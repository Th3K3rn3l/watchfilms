// Package tunnel manages public tunnels (Cloudflare and Pinggy SSH) so a friend
// can reach the local watch room over the internet without a public IP.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	cfURLRe       = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	cfConnRe      = regexp.MustCompile(`(?i)(Registered tunnel connection|Connection [a-f0-9-]+ registered|connIndex=\d+.*location=)`)
	cfFatalRe     = regexp.MustCompile(`(?i)(TLS handshake with edge error|failed to dial.*edge|hard_fail=true|failed to request quick Tunnel|context deadline exceeded|unable to establish connection|no more connections active)`)
	pinggyURLRe   = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.(?:free\.pinggy\.net|run\.pinggy-free\.link|a\.free\.pinggy\.link|pinggy\.link)`)
)

// Update receives tunnel status updates: status ("starting", "ready", "error"),
// public url (if ready), error text, and the provider name ("Cloudflare", "Pinggy").
type Update func(status, url, errText, provider string)

type Config struct {
	LocalPort int
	Mode      string // "auto", "cloudflare", "pinggy"
	CFToken   string // optional Cloudflare Named Tunnel token
	Domain    string // optional custom domain for Cloudflare Named Tunnel (e.g. kino.example.ru)
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

func downloadURL() (string, error) {
	base := "https://github.com/cloudflare/cloudflared/releases/latest/download/"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return base + "cloudflared-linux-amd64", nil
	case "linux/arm64":
		return base + "cloudflared-linux-arm64", nil
	case "linux/386":
		return base + "cloudflared-linux-386", nil
	case "windows/amd64":
		return base + "cloudflared-windows-amd64.exe", nil
	case "windows/386":
		return base + "cloudflared-windows-386.exe", nil
	}
	return "", fmt.Errorf("нет готового cloudflared для %s/%s", runtime.GOOS, runtime.GOARCH)
}

// ensureBinary returns a path to cloudflared: next to the executable, in
// PATH, or downloaded once into the user cache directory.
func ensureBinary(ctx context.Context, upd Update) (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), binName())
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	dir := filepath.Join(cache, "watchfilms")
	p := filepath.Join(dir, binName())
	if st, err := os.Stat(p); err == nil && st.Size() > 1<<20 {
		return p, nil
	}
	url, err := downloadURL()
	if err != nil {
		return "", err
	}
	upd("starting", "", "Скачиваю cloudflared (один раз, ~40 МБ)…", "Cloudflare")
	log.Printf("скачиваю cloudflared: %s", url)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("не удалось скачать cloudflared: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("не удалось скачать cloudflared: HTTP %d", resp.StatusCode)
	}
	tmp := p + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("загрузка cloudflared прервалась: %w", err)
	}
	f.Close()
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return p, nil
}

// findPhysicalLANIPs detects non-virtual physical IPv4 addresses (Wi-Fi, Ethernet).
// Binding outbound connections to a physical IP bypasses local VPN / TUN adapters
// (e.g. sing-box, xray, Clash) where Cloudflare Tunnel mTLS/QUIC is often blocked.
func findPhysicalLANIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifc.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		isVirtual := false
		for _, prefix := range []string{"tun", "tap", "wg", "tailscale", "docker", "veth", "br-", "virbr", "vbox", "vmnet", "sing-box", "xray", "ppp"} {
			if strings.HasPrefix(name, prefix) {
				isVirtual = true
				break
			}
		}
		if isVirtual {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ip4.String())
		}
	}
	return ips
}

// waitReachable polls the public URL to verify that Cloudflare or Pinggy is genuinely
// routing requests to the local server, preventing Error 1033 from being displayed.
func waitReachable(ctx context.Context, url string, timeout time.Duration) bool {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Try IPv4 first to avoid broken/unroutable local IPv6 configs
			c, err := dialer.DialContext(ctx, "tcp4", addr)
			if err == nil {
				return c, nil
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	cl := &http.Client{
		Timeout:   2500 * time.Millisecond,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil
		},
	}
	deadline := time.Now().Add(timeout)
	target := strings.TrimRight(url, "/") + "/healthz"
	for time.Now().Before(deadline) && ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
		if err == nil {
			resp, err := cl.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(800 * time.Millisecond):
		}
	}
	return false
}

// Manager controls the active tunnel provider and manages automatic failover and manual switching.
type Manager struct {
	cfg        Config
	upd        Update
	mu         sync.Mutex
	cancelCur  context.CancelFunc
	switchCh   chan string
	curProv    string
}

func NewManager(cfg Config, upd Update) *Manager {
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	return &Manager{
		cfg:      cfg,
		upd:      upd,
		switchCh: make(chan string, 1),
	}
}

// SwitchNext switches to the alternative tunnel provider (Cloudflare ⇄ Pinggy).
func (m *Manager) SwitchNext() (string, error) {
	m.mu.Lock()
	cur := m.curProv
	cancel := m.cancelCur
	m.mu.Unlock()

	next := "pinggy"
	if strings.EqualFold(cur, "pinggy") {
		next = "cloudflare"
	}
	if cancel != nil {
		cancel()
	}
	select {
	case m.switchCh <- next:
		return next, nil
	default:
		return next, nil
	}
}

// CurrentProvider returns the currently active tunnel provider.
func (m *Manager) CurrentProvider() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.curProv
}

// Run executes the tunnel loop until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	mode := m.cfg.Mode
	for ctx.Err() == nil {
		// Check for dynamic switch request
		select {
		case req := <-m.switchCh:
			mode = req
		default:
		}

		tctx, cancel := context.WithCancel(ctx)
		m.mu.Lock()
		m.cancelCur = cancel
		m.mu.Unlock()

		var err error
		switch strings.ToLower(mode) {
		case "cloudflare":
			m.setCur("Cloudflare")
			m.upd("starting", "", "Открываю ссылку через Cloudflare…", "Cloudflare")
			err = m.runCloudflareWithRetries(tctx)

		case "pinggy":
			m.setCur("Pinggy")
			err = runPureGoPinggy(tctx, m.cfg.LocalPort, m.upd)

		default: // "auto"
			// If a custom Cloudflare token is provided, prioritize Cloudflare!
			if m.cfg.CFToken != "" {
				m.setCur("Cloudflare")
				m.upd("starting", "", "Подключаюсь к Cloudflare Named Tunnel…", "Cloudflare")
				err = m.runCloudflareWithRetries(tctx)
				break
			}

			// In auto mode without token, try Pinggy first because it works over standard port 443
			// without external binaries and works seamlessly under VPN (TUN mode).
			m.setCur("Pinggy")
			err = runPureGoPinggy(tctx, m.cfg.LocalPort, m.upd)
			if tctx.Err() != nil {
				cancel()
				return
			}
			if err != nil {
				log.Printf("туннель: Pinggy не удался (%v), переключаюсь на Cloudflare...", err)
				m.setCur("Cloudflare")
				m.upd("starting", "", "Pinggy недоступен, пробую Cloudflare…", "Cloudflare")
				cfErr := m.runCloudflareOnce(tctx)
				if cfErr != nil {
					err = fmt.Errorf("все сервисы туннелей недоступны (Pinggy: %v, Cloudflare: %w)", err, cfErr)
				} else {
					err = nil
				}
			}
		}

		cancel()
		if ctx.Err() != nil {
			return
		}

		// Check if a switch was requested while running
		select {
		case req := <-m.switchCh:
			mode = req
			continue
		default:
		}

		if err != nil {
			m.upd("error", "", fmt.Sprintf("Ошибка туннеля: %v. Используйте ссылки для локальной сети/VPN ниже или нажмите «Сменить сервис».", err), m.CurrentProvider())
			// Don't spin in an infinite loop! Wait for switch request or back off generously
			select {
			case <-ctx.Done():
				return
			case req := <-m.switchCh:
				mode = req
			case <-time.After(15 * time.Second):
			}
		} else {
			// Normal disconnect (e.g. 60m session limit reached on Pinggy) -> reconnect after 1s
			select {
			case <-ctx.Done():
				return
			case req := <-m.switchCh:
				mode = req
			case <-time.After(1 * time.Second):
			}
		}
	}
}

func (m *Manager) setCur(p string) {
	m.mu.Lock()
	m.curProv = p
	m.mu.Unlock()
}

func (m *Manager) runCloudflareWithRetries(ctx context.Context) error {
	fails := 0
	for ctx.Err() == nil {
		err := m.runCloudflareOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			fails++
			m.upd("error", "", fmt.Sprintf("Ошибка Cloudflare: %v, повтор…", err), "Cloudflare")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(min(fails, 5)) * 2 * time.Second):
			}
		}
	}
	return nil
}

func (m *Manager) runCloudflareOnce(ctx context.Context) error {
	bin, err := ensureBinary(ctx, m.upd)
	if err != nil {
		return err
	}

	// Try with physical LAN bind IP if available to avoid VPN TUN drop
	bindIPs := findPhysicalLANIPs()
	var bindIP string
	if len(bindIPs) > 0 {
		bindIP = bindIPs[0]
	}

	err = runCFProcess(ctx, bin, m.cfg.LocalPort, m.cfg.CFToken, m.cfg.Domain, "http2", bindIP, m.upd)
	// If failed with bind IP and we haven't tried without it, fallback to default routing
	if err != nil && bindIP != "" && ctx.Err() == nil {
		log.Printf("туннель: попытка с bind-ip %s завершилась с ошибкой (%v), пробую без привязки...", bindIP, err)
		err = runCFProcess(ctx, bin, m.cfg.LocalPort, m.cfg.CFToken, m.cfg.Domain, "http2", "", m.upd)
	}
	return err
}

func runCFProcess(ctx context.Context, bin string, port int, cfToken, domain, proto, bindIP string, upd Update) error {
	var args []string
	if cfToken != "" {
		args = []string{"tunnel", "--no-autoupdate", "--protocol", proto}
		if bindIP != "" {
			args = append(args, "--edge-bind-address", bindIP)
		}
		args = append(args, "run", "--token", cfToken)
	} else {
		args = []string{"tunnel", "--no-autoupdate", "--protocol", proto}
		if bindIP != "" {
			args = append(args, "--edge-bind-address", bindIP)
		}
		args = append(args, "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	hideWindow(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = cmd.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("не удалось запустить cloudflared: %w", err)
	}

	var (
		foundURL    string
		connected   bool
		lastErrText string
		mu          sync.Mutex
	)

	// Timer: Cloudflare edge connection must establish within 15 seconds
	timer := time.AfterFunc(15*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		if !connected {
			cancel()
		}
	})
	defer timer.Stop()

	sc := bufio.NewScanner(stderr)
	go func() {
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			if foundURL == "" {
				if u := cfURLRe.FindString(line); u != "" {
					foundURL = u
					upd("starting", "", "Подключаюсь к серверам Cloudflare…", "Cloudflare")
				}
			}
			if cfConnRe.MatchString(line) {
				connected = true
				timer.Stop()
			}
			if cfFatalRe.MatchString(line) {
				lastErrText = line
			}
			mu.Unlock()

			if connected {
				if cfToken != "" {
					targetURL := domain
					if targetURL != "" && !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
						targetURL = "https://" + targetURL
					}
					if targetURL == "" {
						targetURL = "https://(домен настроен в Cloudflare)"
					}
					upd("ready", targetURL, "", "Cloudflare")
					break
				}
				if foundURL != "" {
					// Once connected to edge, verify reachability
					go func(targetURL string) {
						if waitReachable(cctx, targetURL, 8*time.Second) {
							upd("ready", targetURL, "", "Cloudflare")
						} else {
							mu.Lock()
							lastErrText = "ссылка Cloudflare недоступна (Error 1033 / DNS timeout)"
							mu.Unlock()
							cancel()
						}
					}(foundURL)
					break
				}
			}
		}
		// Drain remaining output
		for sc.Scan() {
		}
	}()

	waitErr := cmd.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !connected {
		if lastErrText != "" {
			return errors.New(lastErrText)
		}
		return errors.New("таймаут подключения к edge-серверам Cloudflare (проверьте блокировки/VPN)")
	}
	if lastErrText != "" && waitErr != nil {
		return errors.New(lastErrText)
	}
	return waitErr
}

// Run is the legacy entry point for backwards compatibility.
func Run(ctx context.Context, localPort int, upd func(status, url, errText string)) {
	mgr := NewManager(Config{LocalPort: localPort, Mode: "auto"}, func(s, u, e, p string) {
		upd(s, u, e)
	})
	mgr.Run(ctx)
}
