// Package firmware serves new firmware to the desk device, which updates itself over the air from the relay it already
// trusts (inside the WireGuard tunnel, with the relay token). The relay only hands out a file you put there:
//
//	<dir>/continuum-device.bin   the application image (build/continuum-device.bin)
//	<dir>/version.txt            its version, as the device reports its own (the git describe it was built from)
//
//	GET /firmware/manifest              {"version":"...","size":N,"sha256":"..."}
//	GET /firmware/continuum-device.bin  the image (ranges are supported, so an interrupted download can go on)
package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ImageName   = "continuum-device.bin"
	VersionName = "version.txt"
)

type Manifest struct {
	Version string `json:"version"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

type cached struct {
	mod      time.Time
	size     int64
	manifest Manifest
}

type server struct {
	dir string
	mu  sync.Mutex
	c   cached
}

// Handler serves the manifest and the image from dir.
func Handler(dir string) http.HandlerFunc {
	s := &server{dir: dir}
	return s.serve
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/firmware/") {
	case "manifest":
		m, err := s.manifest()
		if err != nil {
			http.Error(w, "no firmware published", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	case ImageName:
		f, err := os.Open(filepath.Join(s.dir, ImageName))
		if err != nil {
			http.Error(w, "no firmware published", http.StatusNotFound)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, "unreadable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, ImageName, st.ModTime(), f)
	default:
		http.NotFound(w, r)
	}
}

// manifest describes the published image, hashing it again only when the file has changed.
func (s *server) manifest() (Manifest, error) {
	path := filepath.Join(s.dir, ImageName)
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return Manifest{}, os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.manifest.SHA256 != "" && s.c.mod.Equal(st.ModTime()) && s.c.size == st.Size() {
		return s.c.manifest, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Manifest{}, err
	}
	version, _ := os.ReadFile(filepath.Join(s.dir, VersionName))
	m := Manifest{Version: strings.TrimSpace(string(version)), Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}
	if m.Version == "" {
		return Manifest{}, os.ErrNotExist // an image without a version cannot be compared with the running one
	}
	s.c = cached{mod: st.ModTime(), size: st.Size(), manifest: m}
	return m, nil
}
