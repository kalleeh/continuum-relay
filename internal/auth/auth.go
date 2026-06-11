package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxFailures = 5
const lockoutDuration = 60 * time.Second

type Authenticator struct {
	token    string
	mu       sync.Mutex
	failures map[string][]time.Time // IP → failure timestamps
}

func New(token string) *Authenticator {
	return &Authenticator{
		token:    token,
		failures: make(map[string][]time.Time),
	}
}

// ValidateRequest checks the Authorization: Bearer <token> header.
// Returns false if token is wrong OR if the IP is rate-limited.
func (a *Authenticator) ValidateRequest(r *http.Request) bool {
	return a.validate(clientIP(r), extractBearer(r.Header.Get("Authorization")))
}

// ValidateBasic checks an Authorization: Basic <base64(username:token)> header,
// where username must equal the given value. It shares the same per-IP lockout
// as ValidateRequest, so the terminal WebSocket endpoint (which uses Basic auth)
// gets the same brute-force protection as the Bearer-authenticated relay API.
func (a *Authenticator) ValidateBasic(r *http.Request, username string) bool {
	return a.validate(clientIP(r), extractBasic(r.Header.Get("Authorization"), username))
}

// validate runs the shared rate-limit + constant-time token comparison. provided
// is the bare token already extracted from whichever auth scheme the caller used
// (empty string if the header was missing/malformed — treated as a failure).
func (a *Authenticator) validate(ip, provided string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check rate limit first
	if a.isLocked(ip) {
		return false
	}

	// Check token
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
		a.recordFailure(ip)
		return false
	}

	// Success — clear failures for this IP
	delete(a.failures, ip)
	return true
}

func (a *Authenticator) isLocked(ip string) bool {
	cutoff := time.Now().Add(-lockoutDuration)
	recent := a.failures[ip]
	var still []time.Time
	for _, t := range recent {
		if t.After(cutoff) {
			still = append(still, t)
		}
	}
	if len(still) == 0 {
		delete(a.failures, ip)
	} else {
		a.failures[ip] = still
	}
	return len(still) >= maxFailures
}

func (a *Authenticator) recordFailure(ip string) {
	a.failures[ip] = append(a.failures[ip], time.Now())
}

// UpdateToken atomically replaces the accepted Bearer token.
// The calling WebSocket client must already be authenticated before rotating.
func (a *Authenticator) UpdateToken(newToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = newToken
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && header[:len(prefix)] == prefix {
		return header[len(prefix):]
	}
	return ""
}

// extractBasic decodes an Authorization: Basic header and returns the password
// half iff the username half exactly matches want. Returns "" on any mismatch or
// malformed input, which validate() then treats as an auth failure.
func extractBasic(header, want string) string {
	const prefix = "Basic "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return ""
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok || user != want {
		return ""
	}
	return pass
}

// ClientIP returns the source IP of the request (no port). Exported so callers
// outside this package can key state on the same identity the rate limiter uses
// — e.g. binding a permission request to the client that originated it.
func ClientIP(r *http.Request) string {
	return clientIP(r)
}

func clientIP(r *http.Request) string {
	// Do not trust X-Forwarded-For. The server is accessed directly
	// over WireGuard, not through a reverse proxy.
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
