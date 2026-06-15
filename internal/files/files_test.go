package files

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(dir), dir
}

func TestResolve_RejectsTraversalAndPaths(t *testing.T) {
	s, _ := newTestServer(t)
	bad := []string{
		"", ".", "..", "../x", "../../etc/passwd",
		"a/b", "/etc/passwd", "foo/../bar", "x\\y",
		strings.Repeat("a", 256), // over 255
		"bad*name", "semi;colon",
	}
	for _, name := range bad {
		if _, err := s.resolve(name); err == nil {
			t.Errorf("resolve(%q) = nil error, want rejection", name)
		}
	}
}

func TestResolve_AcceptsSafeNames(t *testing.T) {
	s, dir := newTestServer(t)
	good := []string{"report.pdf", "my file.txt", "a_b-c.1", "data.csv"}
	for _, name := range good {
		full, err := s.resolve(name)
		if err != nil {
			t.Errorf("resolve(%q) unexpected error: %v", name, err)
			continue
		}
		if filepath.Dir(full) != filepath.Clean(dir) {
			t.Errorf("resolve(%q) = %q, not directly under %q", name, full, dir)
		}
	}
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	content := []byte("hello continuum shared files")

	// Build a multipart upload.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "greeting.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.HandleUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// List should now show the file.
	lreq := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	lrec := httptest.NewRecorder()
	s.HandleList(lrec, lreq)
	if !strings.Contains(lrec.Body.String(), "greeting.txt") {
		t.Fatalf("list missing uploaded file: %s", lrec.Body.String())
	}

	// Download should return the exact bytes.
	dreq := httptest.NewRequest(http.MethodGet, "/api/files/download?name=greeting.txt", nil)
	drec := httptest.NewRecorder()
	s.HandleDownload(drec, dreq)
	if drec.Code != http.StatusOK {
		t.Fatalf("download status = %d", drec.Code)
	}
	if !bytes.Equal(drec.Body.Bytes(), content) {
		t.Fatalf("download mismatch: got %q want %q", drec.Body.Bytes(), content)
	}
}

func TestDownload_RejectsBadName(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files/download?name=../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	s.HandleDownload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("download bad name status = %d, want 400", rec.Code)
	}
}

func TestDownload_RejectsSymlink(t *testing.T) {
	s, dir := newTestServer(t)
	// Plant a symlink inside the shared dir pointing outside it.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/files/download?name=innocent.txt", nil)
	rec := httptest.NewRecorder()
	s.HandleDownload(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("download followed a symlink out of the shared dir (status %d) — should be rejected", rec.Code)
	}
	// And the symlink must not appear in listings.
	lrec := httptest.NewRecorder()
	s.HandleList(lrec, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	if strings.Contains(lrec.Body.String(), "innocent.txt") {
		t.Fatalf("symlink leaked into listing: %s", lrec.Body.String())
	}
}

func TestDelete(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "trash.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/files?name=trash.txt", nil)
	rec := httptest.NewRecorder()
	s.HandleDelete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "trash.txt")); !os.IsNotExist(err) {
		t.Fatalf("file still exists after delete")
	}
}

func TestUpload_RejectsOversize(t *testing.T) {
	s, _ := newTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "big.bin")
	// Write just over the cap.
	chunk := bytes.Repeat([]byte("A"), 1<<20)
	for i := 0; i < (MaxUploadBytes/len(chunk))+2; i++ {
		fw.Write(chunk)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.HandleUpload(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("oversize upload accepted (status %d) — should be rejected", rec.Code)
	}
}
