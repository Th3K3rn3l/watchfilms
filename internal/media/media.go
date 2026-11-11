// Package media finds playable files and subtitles and converts SRT to WebVTT.
package media

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"watchfilms/internal/room"
)

// VideoExts are the extensions shown in the host file browser.
var VideoExts = map[string]string{
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/mp4",
	".webm": "video/webm", ".mkv": "video/webm", ".ogv": "video/ogg",
}

// IsVideo reports whether the file name has a supported video extension.
func IsVideo(name string) bool {
	_, ok := VideoExts[strings.ToLower(filepath.Ext(name))]
	return ok
}

// ContentType returns the MIME type for a video path.
func ContentType(name string) string {
	if t, ok := VideoExts[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// FindSubtitles returns .srt/.vtt files in the movie folder (and a "Subs"
// subfolder) whose name starts with the movie name, or all of them if the
// folder holds a single movie.
func FindSubtitles(videoPath string) []room.Subtitle {
	dir := filepath.Dir(videoPath)
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath)))
	var found []string
	videos := 0
	scan := func(d string, sub bool) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			ext := strings.ToLower(filepath.Ext(n))
			if !sub && IsVideo(n) {
				videos++
			}
			if ext == ".srt" || ext == ".vtt" {
				found = append(found, filepath.Join(d, n))
			}
		}
	}
	scan(dir, false)
	for _, s := range []string{"Subs", "subs", "Subtitles", "subtitles"} {
		scan(filepath.Join(dir, s), true)
	}
	var out []room.Subtitle
	for _, p := range found {
		n := strings.ToLower(filepath.Base(p))
		inSubDir := filepath.Dir(p) != dir
		if videos > 1 && !inSubDir && !strings.HasPrefix(n, base) {
			continue
		}
		label := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if lower := strings.ToLower(label); strings.HasPrefix(lower, base) && len(lower) == len(label) {
			if rest := strings.Trim(label[len(base):], " ._-"); rest != "" {
				label = rest
			}
		}
		out = append(out, room.Subtitle{Label: label, Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	for i := range out {
		out[i].ID = i
	}
	return out
}

var srtTime = regexp.MustCompile(`(\d{1,2}:\d{2}:\d{2}),(\d{1,3})`)

// ToVTT converts subtitle bytes (SRT or VTT) to WebVTT, fixing BOM, CRLF and
// legacy Windows-1251 encoding common for Russian subtitles.
func ToVTT(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		data = cp1251ToUTF8(data)
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if strings.HasPrefix(strings.TrimSpace(s), "WEBVTT") {
		return []byte(s)
	}
	s = srtTime.ReplaceAllStringFunc(s, func(m string) string {
		sm := srtTime.FindStringSubmatch(m)
		ms := sm[2]
		for len(ms) < 3 {
			ms += "0"
		}
		return sm[1] + "." + ms
	})
	return []byte("WEBVTT\n\n" + strings.TrimSpace(s) + "\n")
}

func cp1251ToUTF8(b []byte) []byte {
	var buf strings.Builder
	buf.Grow(len(b) * 2)
	for _, c := range b {
		switch {
		case c < 0x80:
			buf.WriteByte(c)
		case c >= 0xC0:
			buf.WriteRune(rune(c) - 0xC0 + 0x410)
		case c == 0xA8:
			buf.WriteRune('Ё')
		case c == 0xB8:
			buf.WriteRune('ё')
		case c == 0x96 || c == 0x97:
			buf.WriteRune('—')
		case c == 0x85:
			buf.WriteRune('…')
		case c == 0xAB:
			buf.WriteRune('«')
		case c == 0xBB:
			buf.WriteRune('»')
		case c == 0x93 || c == 0x94:
			buf.WriteRune('"')
		case c == 0xB9:
			buf.WriteRune('№')
		default:
			buf.WriteRune(' ')
		}
	}
	return []byte(buf.String())
}
