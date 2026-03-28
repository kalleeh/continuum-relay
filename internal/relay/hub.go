package relay

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/continuum-app/continuum-relay/internal/apns"
)

const maxDeviceTokens = 10 // prevent unbounded growth on reconnects

type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*Session

	tokenMu      sync.Mutex
	deviceTokens []string
	apnsClient   *apns.Client // nil if APNs not configured
}

func NewHub(apnsClient *apns.Client) *Hub {
	return &Hub{
		sessions:   make(map[string]*Session),
		apnsClient: apnsClient,
	}
}

func (h *Hub) ListSessions() []SessionRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()
	records := make([]SessionRecord, 0, len(h.sessions))
	for _, s := range h.sessions {
		records = append(records, s.GetRecord())
	}
	return records
}

func validateWorkingDir(dir string) error {
	if dir == "" {
		return nil
	}
	clean := filepath.Clean(dir)
	if strings.Contains(clean, "..") {
		return fmt.Errorf("working directory must not contain ..")
	}
	allowed := []string{"/home/", "/root/", "/tmp/", "/var/"}
	for _, prefix := range allowed {
		if strings.HasPrefix(clean, prefix) {
			return nil
		}
	}
	return fmt.Errorf("working directory must be under /home/, /root/, /tmp/, or /var/")
}

func (h *Hub) CreateSession(name, cwd, sessionType string) (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.sessions[name]; exists {
		return h.sessions[name], nil // attach to existing
	}

	if cwd != "" {
		if err := validateWorkingDir(cwd); err != nil {
			return nil, fmt.Errorf("invalid cwd: %w", err)
		}
	}

	s := NewSession(name, cwd)
	s.hub = h
	s.Record.Type = sessionType
	h.sessions[name] = s

	if sessionType == "claudeCode" {
		if err := s.Start(); err != nil {
			delete(h.sessions, name)
			return nil, fmt.Errorf("failed to start session: %w", err)
		}
	}
	return s, nil
}

func (h *Hub) GetSession(name string) (*Session, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.sessions[name]
	return s, ok
}

func (h *Hub) DeleteSession(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[name]; ok {
		s.Stop()
		delete(h.sessions, name)
	}
}

func (h *Hub) SessionListJSON() []byte {
	records := h.ListSessions()
	msg, _ := json.Marshal(map[string]any{
		"type":     "session_list",
		"sessions": records,
	})
	return msg
}

// RegisterDevice stores an APNs device token for push delivery.
// Deduplicates and caps at maxDeviceTokens.
func (h *Hub) RegisterDevice(token string) {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	for _, t := range h.deviceTokens {
		if t == token {
			return // already registered
		}
	}
	if len(h.deviceTokens) >= maxDeviceTokens {
		// Drop the oldest token to make room
		h.deviceTokens = h.deviceTokens[1:]
	}
	h.deviceTokens = append(h.deviceTokens, token)
	slog.Info("device registered for push", "token_suffix", token[max(0, len(token)-8):])
}

// SendPush fires an APNs SESSION_FINISHED notification to all registered devices.
// Called from the session goroutine; does not block.
func (h *Hub) SendPush(sessionName, resultSummary string) {
	if h.apnsClient == nil {
		return
	}
	h.tokenMu.Lock()
	tokens := make([]string, len(h.deviceTokens))
	copy(tokens, h.deviceTokens)
	h.tokenMu.Unlock()

	title := sessionName
	body := resultSummary
	if body == "" {
		body = "Session finished"
	}

	for _, tok := range tokens {
		tok := tok
		go func() {
			if err := h.apnsClient.Send(tok, title, body, sessionName); err != nil {
				if strings.Contains(err.Error(), "410") {
					// APNs says token is invalid or expired; remove it
					go func() {
						h.tokenMu.Lock()
						defer h.tokenMu.Unlock()
						for i, t := range h.deviceTokens {
							if t == tok {
								h.deviceTokens = append(h.deviceTokens[:i], h.deviceTokens[i+1:]...)
								slog.Info("removed stale APNs device token", "token_suffix", tok[max(0, len(tok)-8):])
								break
							}
						}
					}()
				} else {
					slog.Warn("APNs push failed", "err", err, "token_suffix", tok[max(0, len(tok)-8):])
				}
			} else {
				slog.Info("APNs push sent", "session", sessionName)
			}
		}()
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
