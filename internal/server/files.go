package server

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"watchfilms/internal/media"
)

type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
}

type fsListing struct {
	Path   string    `json:"path"`
	Parent string    `json:"parent"`
	Dirs   []fsEntry `json:"dirs"`
	Files  []fsEntry `json:"files"`
	Roots  []fsEntry `json:"roots"`
	Error  string    `json:"error,omitempty"`
}

func roots() []fsEntry {
	var out []fsEntry
	add := func(name, p string) {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, fsEntry{Name: name, Path: p})
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		add("Домой", home)
		add("Загрузки", filepath.Join(home, "Downloads"))
		add("Видео", filepath.Join(home, "Videos"))
		add("Рабочий стол", filepath.Join(home, "Desktop"))
	}
	if runtime.GOOS == "windows" {
		for c := 'C'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			add("Диск "+string(c)+":", d)
		}
	} else {
		add("/", "/")
		if u := os.Getenv("USER"); u != "" {
			add("Флешки", filepath.Join("/media", u))
			add("/run/media", filepath.Join("/run/media", u))
		}
		add("/mnt", "/mnt")
	}
	return out
}

func listDir(p string) fsListing {
	res := fsListing{Roots: roots(), Dirs: []fsEntry{}, Files: []fsEntry{}}
	if p == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p = home
		} else {
			p = "/"
		}
	}
	p = filepath.Clean(p)
	res.Path = p
	if parent := filepath.Dir(p); parent != p {
		res.Parent = parent
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		res.Error = "Не удалось открыть папку: " + err.Error()
		return res
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		full := filepath.Join(p, name)
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(full); err == nil {
				isDir = st.IsDir()
			}
		}
		if isDir {
			res.Dirs = append(res.Dirs, fsEntry{Name: name, Path: full})
		} else if media.IsVideo(name) {
			var size int64
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			if st, err := os.Stat(full); err == nil {
				size = st.Size()
			}
			res.Files = append(res.Files, fsEntry{Name: name, Path: full, Size: size})
		}
	}
	less := func(a []fsEntry) func(i, j int) bool {
		return func(i, j int) bool { return strings.ToLower(a[i].Name) < strings.ToLower(a[j].Name) }
	}
	sort.Slice(res.Dirs, less(res.Dirs))
	sort.Slice(res.Files, less(res.Files))
	return res
}
