package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func publish(t *testing.T, dir, version string, image []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ImageName), image, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, VersionName), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNothingPublished(t *testing.T) {
	h := Handler(t.TempDir())
	if rec := get(h, "/firmware/manifest"); rec.Code != http.StatusNotFound {
		t.Errorf("manifest of nothing: %d", rec.Code)
	}
	if rec := get(h, "/firmware/"+ImageName); rec.Code != http.StatusNotFound {
		t.Errorf("image of nothing: %d", rec.Code)
	}
}

func TestManifestAndImage(t *testing.T) {
	dir := t.TempDir()
	image := []byte("\xe9\x03\x02\x2f this stands in for a firmware image")
	publish(t, dir, "8269a55", image)
	h := Handler(dir)

	rec := get(h, "/firmware/manifest")
	var m Manifest
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		t.Fatalf("manifest: %d %s", rec.Code, rec.Body.String())
	}
	sum := sha256.Sum256(image)
	if m.Version != "8269a55" || m.Size != int64(len(image)) || m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("manifest %+v", m)
	}

	rec = get(h, "/firmware/"+ImageName)
	if rec.Code != 200 || rec.Body.String() != string(image) {
		t.Errorf("image: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// an interrupted download goes on from where it stopped
	rec = get(h, "/firmware/"+ImageName, "Range", "bytes=10-")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != string(image[10:]) {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}

	// publishing something new changes the manifest
	publish(t, dir, "c0ffee1", []byte("a different image"))
	var m2 Manifest
	json.Unmarshal(get(h, "/firmware/manifest").Body.Bytes(), &m2)
	if m2.Version != "c0ffee1" || m2.SHA256 == m.SHA256 {
		t.Errorf("manifest did not follow the new image: %+v", m2)
	}
}

func TestAnImageWithoutAVersionIsNotOffered(t *testing.T) {
	dir := t.TempDir()
	publish(t, dir, "", []byte("image"))
	if rec := get(Handler(dir), "/firmware/manifest"); rec.Code != http.StatusNotFound {
		t.Errorf("an unversioned image was offered: %d", rec.Code)
	}
}

func TestOtherPathsAndMethods(t *testing.T) {
	h := Handler(t.TempDir())
	if rec := get(h, "/firmware/../etc/passwd"); rec.Code == 200 {
		t.Error("a path outside the firmware directory was served")
	}
	req := httptest.NewRequest(http.MethodPost, "/firmware/manifest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("post: %d", rec.Code)
	}
}
