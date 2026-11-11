package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"watchfilms/internal/media"
	"watchfilms/internal/room"
)

func setup(t *testing.T) (*Server, *httptest.Server, string) {
	web := fstest.MapFS{"index.html": {Data: []byte("app")}, "denied.html": {Data: []byte("denied")}}
	s := New(room.New(), web)
	dir := t.TempDir()
	p := filepath.Join(dir, "film.mp4")
	os.WriteFile(p, []byte(strings.Repeat("0123456789", 1000)), 0o644)
	os.WriteFile(filepath.Join(dir, "film.ru.srt"), []byte("1\r\n00:00:01,500 --> 00:00:02,000\r\nПривет\r\n"), 0o644)
	if err := s.OpenFile(p); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, p
}

func client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func TestAuthAndRange(t *testing.T) {
	s, ts, _ := setup(t)
	id := s.Room.Media().ID

	// No token: denied everywhere.
	resp, _ := http.Get(ts.URL + "/api/video/" + id)
	if resp.StatusCode != 403 {
		t.Fatalf("video without token: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/")
	if resp.StatusCode != 403 {
		t.Fatalf("index without token: %d", resp.StatusCode)
	}

	// Guest token sets a cookie and allows streaming with Range.
	c := client()
	resp, _ = c.Get(ts.URL + "/?t=" + s.GuestToken)
	if resp.StatusCode != 200 {
		t.Fatalf("guest index: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/video/"+id, nil)
	req.Header.Set("Range", "bytes=9990-")
	resp, _ = c.Do(req)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(body) != "0123456789" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}

	// Guest is not host: no file browser.
	resp, _ = c.Get(ts.URL + "/api/fs")
	if resp.StatusCode != 403 {
		t.Fatalf("guest fs: %d", resp.StatusCode)
	}
	// Requests through the tunnel never count as host, even with the host token.
	h := client()
	h.Get(ts.URL + "/?h=" + s.HostToken)
	resp, _ = h.Get(ts.URL + "/api/fs")
	if resp.StatusCode != 200 {
		t.Fatalf("host fs: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", ts.URL+"/api/fs", nil)
	req.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
	resp, _ = h.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("tunnel host fs: %d", resp.StatusCode)
	}

	// Subtitles converted to VTT.
	resp, _ = c.Get(ts.URL + "/api/subs/" + id + "/0.vtt")
	body, _ = io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "WEBVTT") || !strings.Contains(string(body), "00:00:01.500") {
		t.Fatalf("vtt: %q", body)
	}
	if s.Room.Media().Subtitles[0].Label != "ru" {
		t.Fatalf("label: %q", s.Room.Media().Subtitles[0].Label)
	}
}

func TestCP1251(t *testing.T) {
	out := string(media.ToVTT([]byte{'1', '\n', 0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2}))
	if !strings.Contains(out, "Привет") {
		t.Fatalf("cp1251: %q", out)
	}
}
