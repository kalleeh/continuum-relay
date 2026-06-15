// Package files exposes an authenticated HTTP API over a single shared
// directory (sysinfo.SharedDir, e.g. $HOME/continuum-shared) so the iOS client
// can transfer files to and from the server. Every session sees the same
// directory; it lives outside the projects tree so uploads never pollute a git
// repo.
//
// Access is filename-only: callers name a file in the shared dir, never a path.
// There is no traversal, no subdirectories, and the resolved target is verified
// to stay strictly inside the shared dir before any open. Uploads stream to disk
// (no full-buffer) and are size-capped.
package files

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// MaxUploadBytes caps a single uploaded file. Streamed to disk, so this bounds
// disk use per upload rather than memory. 100 MB covers PDFs/datasets/most
// media without risking filling a small VPS in one shot.
const MaxUploadBytes = 100 * 1024 * 1024

// validName allows a single path-less filename: letters, digits, dot, dash,
// underscore, space. No separators, so "../x", "/etc/x", "a/b" are all rejected
// before they ever reach the filesystem.
var validName = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,255}$`)

// Server serves the shared-directory file API. Routes are registered by the
// relay server, which also applies bearer auth before dispatching here.
type Server struct {
	dir string
}

// New returns a file Server bound to dir (the shared directory). dir is created
// if missing.
func New(dir string) *Server {
	_ = os.MkdirAll(dir, 0o700)
	return &Server{dir: dir}
}

// FileInfo is one entry in the directory listing.
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"` // unix seconds
}

// resolve validates name and returns the absolute path inside the shared dir.
// It rejects anything that isn't a bare, safe filename and double-checks the
// joined path still sits directly under dir (defense in depth against any regex
// gap).
func (s *Server) resolve(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid filename")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid filename")
	}
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid filename")
	}
	full := filepath.Join(s.dir, name)
	// filepath.Dir(full) must equal the shared dir exactly — no nesting/escape.
	if filepath.Dir(full) != filepath.Clean(s.dir) {
		return "", fmt.Errorf("invalid filename")
	}
	return full, nil
}

// HandleList responds with a JSON array of the files in the shared dir
// (non-recursive; directories and symlinks are skipped).
func (s *Server) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		http.Error(w, "could not read shared directory", http.StatusInternalServerError)
		return
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// HandleDownload streams a file from the shared dir. Supports Range requests
// (http.ServeContent) so large files resume and seek. ?name=<file>.
func (s *Server) HandleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	full, err := s.resolve(r.URL.Query().Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Lstat(full)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	// O_NOFOLLOW: if the entry was swapped for a symlink between Lstat and open,
	// fail rather than follow it out of the shared dir.
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		http.Error(w, "could not open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	name := filepath.Base(full)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// HandleUpload accepts a multipart/form-data POST with a "file" part and writes
// it into the shared dir, streamed to disk. The filename is taken from the part
// header (validated) and capped at MaxUploadBytes.
func (s *Server) HandleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Cap the whole request body as a backstop; the per-file copy is also
	// limited below so a lying Content-Length can't be trusted.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1<<20)

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "malformed multipart body", http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		full, err := s.resolve(part.FileName())
		if err != nil {
			_ = part.Close()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// O_EXCL would reject overwrites; we intentionally allow overwrite (O_TRUNC)
		// so re-uploading a corrected file works. O_NOFOLLOW prevents writing
		// through a pre-planted symlink.
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			_ = part.Close()
			http.Error(w, "could not create file", http.StatusInternalServerError)
			return
		}
		// Limit the copy itself: +1 byte so we can detect "over the cap".
		written, copyErr := io.Copy(f, io.LimitReader(part, MaxUploadBytes+1))
		_ = part.Close()
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(full)
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}
		if written > MaxUploadBytes {
			_ = os.Remove(full)
			http.Error(w, "file exceeds 100MB limit", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FileInfo{
			Name:    filepath.Base(full),
			Size:    written,
			ModTime: time.Now().Unix(),
		})
		return
	}
	http.Error(w, "no file part in upload", http.StatusBadRequest)
}

// HandleDelete removes a file from the shared dir. ?name=<file>.
func (s *Server) HandleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	full, err := s.resolve(r.URL.Query().Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Lstat + reject symlink so we delete only a real file in the shared dir,
	// never the target of a planted symlink.
	info, err := os.Lstat(full)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	if err := os.Remove(full); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
