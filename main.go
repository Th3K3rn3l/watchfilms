// WatchFilms: watch a local movie together with a friend over the internet.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"watchfilms/internal/p2p"
	"watchfilms/internal/room"
	"watchfilms/internal/server"
	"watchfilms/internal/tunnel"
)

//go:embed web
var webFS embed.FS

func main() {
	port := flag.Int("port", 8787, "локальный порт")
	noTunnel := flag.Bool("no-tunnel", false, "не открывать публичную ссылку (только локальная сеть / VPN)")
	tunnelMode := flag.String("tunnel", "auto", "способ создания ссылки: auto (Cloudflare -> Pinggy), cloudflare, pinggy, none")
	ycfURL := flag.String("signal", os.Getenv("YCF_URL"), "URL Yandex Cloud Function для P2P соединения")
	flag.StringVar(ycfURL, "ycf", os.Getenv("YCF_URL"), "алиас для -signal")
	webURL := flag.String("web", os.Getenv("WEB_URL"), "URL GitHub Pages веб-интерфейса (например, https://<user>.github.io/watchfilms)")
	useP2P := flag.Bool("p2p", true, "использовать P2P прямое соединение через WebRTC")
	cfToken := flag.String("cf-token", os.Getenv("CLOUDFLARE_TUNNEL_TOKEN"), "токен Named Tunnel Cloudflare (если есть свой аккаунт/домен)")
	cfDomain := flag.String("domain", "", "домен для Named Tunnel Cloudflare (например, kino.mysite.ru)")
	noBrowser := flag.Bool("no-browser", false, "не открывать браузер автоматически")
	flag.Parse()

	web, _ := fs.Sub(webFS, "web")
	rm := room.New()
	srv := server.New(rm, web)
	if flag.NArg() > 0 {
		if err := srv.OpenFile(flag.Arg(0)); err != nil {
			log.Printf("не удалось открыть %s: %v", flag.Arg(0), err)
		}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
		if err != nil {
			log.Fatal(err)
		}
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lan []string
	for _, ip := range lanIPs() {
		lan = append(lan, srv.InviteURL(fmt.Sprintf("http://%s:%d", ip, actualPort)))
	}
	rm.SetTunnel(room.Tunnel{Status: "off", LAN: lan})

	hostURL := srv.HostURL(actualPort)
	fmt.Println()
	fmt.Println("  🎬 WatchFilms запущен")
	fmt.Println("  Откройте у себя:   ", hostURL)
	fmt.Println("  Ссылка для друга появится в окне приложения.")
	fmt.Println("  Для выхода нажмите Ctrl+C или закройте это окно.")
	fmt.Println()

	useTunnel := !*noTunnel && *tunnelMode != "none"
	var p2pMgr *p2p.Manager
	if *useP2P {
		p2pMgr = p2p.NewManager(p2p.Config{
			Room:      rm,
			SignalURL: *ycfURL,
			WebURL:    *webURL,
			Update: func(status, url, msg, provider string) {
				t := room.Tunnel{Status: status, Error: msg, Provider: provider, LAN: lan, URL: url}
				if url != "" {
					fmt.Printf("  ⚡ Ссылка для друга (P2P): %s\n", url)
				}
				if status == "error" {
					log.Println("P2P:", msg)
				}
				rm.SetTunnel(t)
			},
		})
		srv.SetP2PSignal = func(newURL string) error {
			if err := p2pMgr.SetSignalURL(newURL); err != nil {
				return err
			}
			go p2pMgr.Run(ctx)
			return nil
		}
		srv.SetP2PWeb = func(newWeb string) error {
			p2pMgr.SetWebURL(newWeb)
			go p2pMgr.Run(ctx)
			return nil
		}
		if *ycfURL != "" {
			go p2pMgr.Run(ctx)
		}
	}

	if useTunnel {
		mgr := tunnel.NewManager(tunnel.Config{
			LocalPort: actualPort,
			Mode:      *tunnelMode,
			CFToken:   *cfToken,
			Domain:    *cfDomain,
		}, func(status, url, msg, provider string) {
			t := room.Tunnel{Status: status, Error: msg, Provider: provider, LAN: lan}
			if url != "" {
				t.URL = srv.InviteURL(url)
				fmt.Printf("  🌍 Ссылка для друга (%s): %s\n", provider, t.URL)
			}
			if status == "error" {
				log.Println("туннель:", msg)
			}
			if p2pMgr == nil || *ycfURL == "" {
				rm.SetTunnel(t)
			}
		})
		srv.SwitchTunnel = mgr.SwitchNext
		go mgr.Run(ctx)
	}
	if !*noBrowser {
		openBrowser(hostURL)
	}

	<-ctx.Done()
	fmt.Println("\n  Завершение…")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
}

func lanIPs() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("откройте вручную: %s", url)
		return
	}
	go cmd.Wait()
}
