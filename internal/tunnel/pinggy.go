// Package tunnel provides the pure Go Pinggy SSH tunnel client.
// It connects directly over port 443 via SSH without needing any external
// ssh binary, works on all platforms (Windows, Linux, macOS), and bypasses
// firewall/DPI restrictions under VPN TUN mode.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// resolveHost resolves a hostname to an IPv4 address. If system UDP DNS
// fails or hangs (common in VPN TUN mode), it falls back to public DNS servers over TCP.
func resolveHost(ctx context.Context, host string) (string, error) {
	// 1. Try system resolver first with a short timeout so we don't hang if UDP 53 is blackholed by VPN TUN
	sysCtx, sysCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	ips, err := net.DefaultResolver.LookupHost(sysCtx, host)
	sysCancel()
	if err == nil && len(ips) > 0 {
		for _, ip := range ips {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
				return ip, nil
			}
		}
		return ips[0], nil
	}

	// 2. Fallback to TCP DNS queries to circumvent UDP port 53 blocking/dropping in VPNs
	fallbackServers := []string{
		"8.8.8.8:53",
		"1.1.1.1:53",
		"77.88.8.8:53", // Yandex DNS (fast and reliable in CIS/Russia)
		"77.88.8.1:53",
	}

	for _, srv := range fallbackServers {
		tcpCtx, tcpCancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(dialCtx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(dialCtx, "tcp", srv)
			},
		}
		ips, err := r.LookupHost(tcpCtx, host)
		tcpCancel()
		if err == nil && len(ips) > 0 {
			for _, ip := range ips {
				if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
					return ip, nil
				}
			}
			return ips[0], nil
		}
	}

	return "", fmt.Errorf("не удалось разрешить DNS для %s (проверьте интернет или VPN)", host)
}

// runPureGoPinggy establishes an SSH reverse tunnel to free.pinggy.io:443 in pure Go.
// It needs no external binaries and routes all incoming HTTP traffic to localPort.
func runPureGoPinggy(ctx context.Context, localPort int, upd Update) error {
	upd("starting", "", "Подключаюсь к туннелю (Pinggy SSH 443)…", "Pinggy")

	// 1. Resolve free.pinggy.io
	ip, err := resolveHost(ctx, "free.pinggy.io")
	if err != nil {
		return fmt.Errorf("DNS ошибка: %w", err)
	}
	target := net.JoinHostPort(ip, "443")

	// 2. Establish SSH connection over TCP 443
	config := &ssh.ClientConfig{
		User: "a",
		Auth: []ssh.AuthMethod{
			ssh.Password(""),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к free.pinggy.io:443: %w", err)
	}
	defer rawConn.Close()

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, target, config)
	if err != nil {
		return fmt.Errorf("ошибка SSH рукопожатия с Pinggy: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	// 3. Request remote reverse port forward
	listener, err := client.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("ошибка запроса обратного туннеля Pinggy: %w", err)
	}
	defer listener.Close()

	// 4. Open session shell to retrieve the public URL
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ошибка создания SSH-сессии: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ошибка получения stdout SSH: %w", err)
	}
	if err := session.Shell(); err != nil {
		return fmt.Errorf("ошибка запуска оболочки Pinggy: %w", err)
	}

	var (
		foundURL string
		mu       sync.Mutex
	)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Timer for URL detection
	timer := time.AfterFunc(10*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		if foundURL == "" {
			cancel()
		}
	})
	defer timer.Stop()

	// Scanner to parse public URL
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			if foundURL == "" {
				if u := pinggyURLRe.FindString(line); u != "" {
					foundURL = u
					timer.Stop()
					upd("ready", foundURL, "", "Pinggy")
				}
			}
			mu.Unlock()
		}
	}()

	// 5. Proxy traffic from remote listener to local HTTP server
	go func() {
		for {
			remoteConn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(rc net.Conn) {
				defer rc.Close()
				lc, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 3*time.Second)
				if err != nil {
					return
				}
				defer lc.Close()

				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(rc, lc)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(lc, rc)
					done <- struct{}{}
				}()
				<-done
			}(remoteConn)
		}
	}()

	// 6. Keepalive loop to prevent NAT / VPN drops
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-ticker.C:
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					return
				}
			}
		}
	}()

	// Wait until context cancelled or client disconnected
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Wait()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-doneCh:
		mu.Lock()
		gotURL := foundURL != ""
		mu.Unlock()
		if !gotURL {
			return errors.New("таймаут получения ссылки от Pinggy")
		}
		log.Printf("туннель Pinggy завершил работу: %v", err)
		return nil
	}
}
