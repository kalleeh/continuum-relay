package tools

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestValidateFetchURL(t *testing.T) {
	ok := []string{
		"https://example.com",
		"http://example.com/path?q=1",
		"https://10.0.0.1:8080/x", // hygiene only — host shape, not SSRF policy
	}
	for _, u := range ok {
		if err := validateFetchURL(u); err != nil {
			t.Errorf("validateFetchURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"file:///etc/passwd",
		"gopher://example.com",
		"data:text/plain,hi",
		"ftp://example.com",
		"not a url",
		"/relative/path",
		"https://", // no host
	}
	for _, u := range bad {
		if err := validateFetchURL(u); err == nil {
			t.Errorf("validateFetchURL(%q) = nil, want rejection", u)
		}
	}
	// Over-length URL is rejected.
	long := "https://example.com/" + string(make([]byte, maxFetchURLLen))
	if err := validateFetchURL(long); err == nil {
		t.Error("over-length url accepted")
	}
}

// TestWebFetch_OnlyDialsOllama pins the invariant the whole SSRF analysis rests
// on: the relay never dials the model-supplied URL — it only POSTs to
// ollama.com, which performs the actual fetch. If anyone rewires executeWebFetch
// to honor OLLAMA_HOST or fetch locally, this test fails and an SSRF guard must
// be added before it can pass again.
func TestWebFetch_OnlyDialsOllama(t *testing.T) {
	var dialed []string
	orig := ollamaAPIClient
	t.Cleanup(func() { ollamaAPIClient = orig })

	ollamaAPIClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialed = append(dialed, addr)
				// Fail the dial: we only care about WHERE it tried to connect.
				return nil, &net.OpError{Op: "dial", Err: errStub{}}
			},
		},
	}

	t.Setenv("OLLAMA_API_KEY", "test-key")
	// A URL an attacker would choose for SSRF — metadata endpoint.
	_ = executeWebFetch("http://169.254.169.254/latest/meta-data/")

	if len(dialed) == 0 {
		t.Fatal("no dial attempt recorded")
	}
	for _, addr := range dialed {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if host != "ollama.com" {
			t.Fatalf("relay dialed %q; it must only ever dial ollama.com", host)
		}
	}
}

type errStub struct{}

func (errStub) Error() string   { return "stub dial failure" }
func (errStub) Timeout() bool   { return false }
func (errStub) Temporary() bool { return false }
