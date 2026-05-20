package relay

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	relayNames := make(map[string]bool)
	for _, s := range h.sessions {
		rec := s.GetRecord()
		rec.Source = "relay"
		records = append(records, rec)
		relayNames[rec.Name] = true
	}

	// Discover tmux sessions not managed by the relay.
	for _, sys := range discoverTmuxSessions() {
		if !relayNames[sys.Name] {
			records = append(records, sys)
		}
	}
	return records
}

// discoverTmuxSessions runs `tmux list-sessions` and returns records for
// sessions not managed by the relay. These are marked with Source="system".
func discoverTmuxSessions() []SessionRecord {
	// When running as root (LaunchDaemon), tmux connects to root's server by default.
	// We need to query the actual user's tmux server instead.
	var cmd *exec.Cmd
	detectedUser := os.Getenv("CONTINUUM_USER")
	if detectedUser == "" {
		if out, err := exec.Command("stat", "-f", "%Su", "/dev/console").Output(); err == nil {
			detectedUser = strings.TrimSpace(string(out))
		}
	}
	if detectedUser != "" && detectedUser != "root" {
		cmd = exec.Command("su", "-", detectedUser, "-c", "tmux list-sessions -F '#{session_name}\t#{session_activity}\t#{session_attached}'")
	} else {
		cmd = exec.Command("tmux", "list-sessions", "-F", "#{session_name}\t#{session_activity}\t#{session_attached}")
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var records []SessionRecord
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		name := parts[0]
		// Skip sessions with names that don't pass validation (numbered junk, etc.)
		if len(name) > 64 {
			continue
		}

		status := SessionStatus("detached")
		if parts[2] != "0" {
			status = StatusRunning
		}

		var lastActivity time.Time
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			lastActivity = time.Unix(ts, 0)
		}

		records = append(records, SessionRecord{
			Name:         name,
			Type:         "terminal",
			Status:       status,
			LastActivity: lastActivity,
			Source:       "system",
		})
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
	allowed := []string{"/home/", "/root/", "/tmp/", "/var/", "/Users/"}
	for _, prefix := range allowed {
		if strings.HasPrefix(clean, prefix) {
			return nil
		}
	}
	return fmt.Errorf("working directory must be under /home/, /Users/, /root/, /tmp/, or /var/")
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
					h.tokenMu.Lock()
					for i, t := range h.deviceTokens {
						if t == tok {
							h.deviceTokens = append(h.deviceTokens[:i], h.deviceTokens[i+1:]...)
							slog.Info("removed stale APNs device token", "token_suffix", tok[max(0, len(tok)-8):])
							break
						}
					}
					h.tokenMu.Unlock()
				} else {
					slog.Warn("APNs push failed", "err", err, "token_suffix", tok[max(0, len(tok)-8):])
				}
			} else {
				slog.Info("APNs push sent", "session", sessionName)
			}
		}()
	}
}

