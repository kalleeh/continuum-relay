package auth

import (
	"crypto/subtle"
	"net/http"
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
	ip := clientIP(r)

	a.mu.Lock()
	defer a.mu.Unlock()

	// Check rate limit first
	if a.isLocked(ip) {
		return false
	}

	// Check token
	provided := extractBearer(r.Header.Get("Authorization"))
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
	a.failures[ip] = still
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
