// Package server exposes the web UI, the video stream and the WebSocket.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"watchfilms/internal/media"
	"watchfilms/internal/room"
)

// Server bundles the room with HTTP handlers.
type Server struct {
	Room         *room.Room
	GuestToken   string
	HostToken    string
	web          fs.FS
	SwitchTunnel func() (string, error)
	SetP2PSignal func(string) error
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// New creates a server with fresh random tokens.
func New(r *room.Room, web fs.FS) *Server {
	return &Server{Room: r, GuestToken: token(), HostToken: token(), web: web}
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func cookie(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// isLocal is true only for direct loopback requests to 127.0.0.1 / localhost.
// Tunnels (ssh -R, cloudflared) also connect to loopback, but preserve the
// public Host header or add forwarding headers.
func isLocal(r *http.Request) bool {
	if r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	h := r.Host
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if h != "" && h != "127.0.0.1" && h != "localhost" && !strings.HasPrefix(h, "127.") {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) isHost(r *http.Request) bool {
	return isLocal(r) && eq(cookie(r, "wf_h"), s.HostToken)
}

func (s *Server) authed(r *http.Request) bool {
	return s.isHost(r) || eq(cookie(r, "wf_t"), s.GuestToken)
}

// HostURL is opened in the host's browser.
func (s *Server) HostURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/?h=%s", port, s.HostToken)
}

// InviteURL builds the link for the friend.
func (s *Server) InviteURL(base string) string {
	return strings.TrimRight(base, "/") + "/?t=" + s.GuestToken
}

func setCookie(w http.ResponseWriter, name, val string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// Handler returns the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static := http.FileServer(http.FS(s.web))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if h := q.Get("h"); h != "" && isLocal(r) && eq(h, s.HostToken) {
			setCookie(w, "wf_h", h)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if t := q.Get("t"); t != "" && eq(t, s.GuestToken) {
			setCookie(w, "wf_t", t)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !s.authed(r) {
			b, _ := fs.ReadFile(s.web, "denied.html")
			w.WriteHeader(http.StatusForbidden)
			w.Write(b)
			return
		}
		b, _ := fs.ReadFile(s.web, "index.html")
		w.Write(b)
	})

	mux.HandleFunc("GET /api/video/{id}", s.serveVideo)
	mux.HandleFunc("GET /api/subs/{id}/{n}", s.serveSubs)
	mux.HandleFunc("GET /api/fs", s.hostOnly(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, listDir(r.URL.Query().Get("path")))
	}))
	mux.HandleFunc("POST /api/open", s.hostOnly(s.openMedia))
	mux.HandleFunc("POST /api/tunnel/switch", s.hostOnly(func(w http.ResponseWriter, r *http.Request) {
		if s.SwitchTunnel == nil {
			http.Error(w, "туннель выключен", http.StatusBadRequest)
			return
		}
		newProv, err := s.SwitchTunnel()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"provider": newProv})
	}))
	mux.HandleFunc("POST /api/p2p/config", s.hostOnly(func(w http.ResponseWriter, r *http.Request) {
		if s.SetP2PSignal == nil {
			http.Error(w, "P2P не поддерживается", http.StatusBadRequest)
			return
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.SetP2PSignal(body.URL); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "url": body.URL})
	}))
	mux.HandleFunc("GET /ws", s.serveWS)
	mux.Handle("GET /", static)
	return mux
}

func (s *Server) hostOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isHost(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) serveVideo(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	m := s.Room.Media()
	if m == nil || r.PathValue("id") != m.ID {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(m.Path)
	if err != nil {
		http.Error(w, "файл недоступен", http.StatusGone)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", media.ContentType(m.Path))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// ServeContent implements Range / 206 so any file size streams and seeks.
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) serveSubs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	m := s.Room.Media()
	n, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("n"), ".vtt"))
	if m == nil || r.PathValue("id") != m.ID || err != nil || n < 0 || n >= len(m.Subtitles) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(m.Subtitles[n].Path)
	if err != nil {
		http.Error(w, "субтитры недоступны", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Write(media.ToVTT(data))
}

// OpenFile makes path the current movie.
func (s *Server) OpenFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("файл не найден")
	}
	if st.IsDir() || !media.IsVideo(abs) {
		return fmt.Errorf("это не видеофайл")
	}
	s.Room.SetMedia(&room.Media{ID: token()[:10], Name: filepath.Base(abs), Size: st.Size(),
		Path: abs, Subtitles: media.FindSubtitles(abs)})
	return nil
}

func (s *Server) openMedia(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.OpenFile(req.Path); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
