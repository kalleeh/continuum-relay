package webproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		path     string
		wantPort int
		wantRest string
		wantOK   bool
	}{
		{"/proxy/8000/foo/bar", 8000, "foo/bar", true},
		{"/proxy/3000/", 3000, "", true},
		{"/proxy/3000", 3000, "", true},
		{"/proxy/", 0, "", false},
		{"/proxy/abc/x", 0, "", false},
		{"/notproxy/8000/", 0, "", false},
	}
	for _, c := range cases {
		port, rest, ok := parseTarget(c.path)
		if ok != c.wantOK || port != c.wantPort || rest != c.wantRest {
			t.Errorf("parseTarget(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.path, port, rest, ok, c.wantPort, c.wantRest, c.wantOK)
		}
	}
}

func TestHandler_BlocksDisallowedPorts(t *testing.T) {
	h := Handler()
	for _, p := range []int{22, 80, 1023, 7681, 7682, 11434, 51820, 70000} {
		req := httptest.NewRequest(http.MethodGet, "/proxy/"+strconv.Itoa(p)+"/", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
			t.Errorf("port %d: status %d, want 403/400 (blocked)", p, rec.Code)
		}
	}
}

func TestHandler_ProxiesAndStripsAuth(t *testing.T) {
	// Upstream localhost server records what it received.
	var gotAuth string
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()

	// Pull the port the test server bound to.
	u, _ := url.Parse(upstream.URL)
	port := u.Port()

	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/proxy/"+port+"/hello/world", nil)
	req.Header.Set("Authorization", "Bearer secrettoken")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "upstream-ok" {
		t.Fatalf("body = %q, want upstream-ok", rec.Body.String())
	}
	if gotPath != "/hello/world" {
		t.Errorf("upstream path = %q, want /hello/world", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("relay bearer leaked to upstream: %q", gotAuth)
	}
}

func TestHandler_BadGatewayWhenNoUpstream(t *testing.T) {
	h := Handler()
	// 9 is in range and not blocked, but nothing listens there... actually use a
	// high port unlikely to be bound.
	req := httptest.NewRequest(http.MethodGet, "/proxy/59999/", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no upstream)", rec.Code)
	}
}

func TestRewriteLocation(t *testing.T) {
	cases := []struct {
		in   string
		port int
		want string
	}{
		{"/dashboard", 8000, "/proxy/8000/dashboard"},
		{"/proxy/8000/already", 8000, "/proxy/8000/already"},
		{"http://127.0.0.1:8000/x?q=1", 8000, "/proxy/8000/x?q=1"},
		{"http://localhost:8000/y", 8000, "/proxy/8000/y"},
		{"https://example.com/external", 8000, "https://example.com/external"},
	}
	for _, c := range cases {
		if got := rewriteLocation(c.in, c.port); got != c.want {
			t.Errorf("rewriteLocation(%q,%d) = %q, want %q", c.in, c.port, got, c.want)
		}
	}
}

func TestHandler_RewritesRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port := u.Port()

	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/proxy/"+port+"/", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/proxy/"+port+"/login") {
		t.Fatalf("redirect Location = %q, want it rewritten under /proxy/%s/", loc, port)
	}
}
