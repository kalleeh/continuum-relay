package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/continuum-app/continuum-relay/internal/apns"
	"github.com/continuum-app/continuum-relay/internal/detector"
)

const maxDeviceTokens = 10 // prevent unbounded growth on reconnects

type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*Session

	tokenMu      sync.Mutex
	deviceTokens []string
	apnsClient   *apns.Client // nil if APNs not configured

	// Per-session Live Activity push tokens (session name → APNs activity token),
	// registered by the iOS client after it starts an activity. Distinct from
	// deviceTokens: these target a specific on-screen Live Activity for
	// background content-state updates.
	activityMu     sync.Mutex
	activityTokens map[string]string

	// Connected clients, for fanning session_status out to foreground apps.
	// Each registers a buffered send channel drained by its own writer.
	clientMu sync.Mutex
	clients  map[string]chan []byte

	// Dedup set for permission-prompt notifications, keyed by request id, so the
	// same prompt never notifies twice. Bounded in NotifyPermission.
	notifyMu        sync.Mutex
	notifiedPermIDs map[string]bool
}

func NewHub(apnsClient *apns.Client) *Hub {
	return &Hub{
		sessions:       make(map[string]*Session),
		apnsClient:     apnsClient,
		activityTokens: make(map[string]string),
		clients:        make(map[string]chan []byte),
	}
}

// RegisterClient adds a connected client's send channel to the broadcast set.
func (h *Hub) RegisterClient(clientID string, ch chan []byte) {
	h.clientMu.Lock()
	h.clients[clientID] = ch
	h.clientMu.Unlock()
}

// UnregisterClient removes a client from the broadcast set.
func (h *Hub) UnregisterClient(clientID string) {
	h.clientMu.Lock()
	delete(h.clients, clientID)
	h.clientMu.Unlock()
}

func (h *Hub) ListSessions() []SessionRecord {
	// Snapshot relay-managed sessions under the lock, then release it BEFORE
	// shelling out to tmux. discoverTmuxSessions runs `su - <user> -c tmux …`,
	// which can hang (e.g. wg0 contention on this host); holding h.mu across it
	// would block every CreateSession/DeleteSession (write lock) indefinitely
	// and wedge all clients, since SessionListJSON is on the hot path.
	h.mu.RLock()
	relayRecords := make([]SessionRecord, 0, len(h.sessions))
	for _, s := range h.sessions {
		rec := s.GetRecord()
		rec.Source = "relay"
		relayRecords = append(relayRecords, rec)
	}
	h.mu.RUnlock()

	// Discover tmux sessions not managed by the relay (no lock held).
	return mergeDiscovered(relayRecords, discoverTmuxSessions())
}

// mergeDiscovered appends discovered ("system") tmux sessions to the relay's
// own records, dropping any discovered session that's already represented by a
// live relay record. A discovered session is a duplicate when either:
//
//   - its name matches a relay record's logical name (same session, e.g. after a
//     relay restart where h.sessions was rebuilt), or
//   - its name matches a relay record's project. A project-backed agent session
//     is hosted in a tmux session named cx-<project> with the agent in a *window*;
//     `tmux list-sessions` sees cx-<project> and strips it to <project>, which
//     never matches the relay record's logical window name. Without the project
//     check that container slips through as a phantom type=terminal/source=system
//     duplicate alongside the real agent session.
//
// Kept pure (no tmux shellout, no lock) so the dedup logic is unit-testable.
func mergeDiscovered(relayRecords, discovered []SessionRecord) []SessionRecord {
	relayNames := make(map[string]bool, len(relayRecords))
	relayProjects := make(map[string]bool, len(relayRecords))
	for _, rec := range relayRecords {
		relayNames[rec.Name] = true
		if rec.Project != "" {
			relayProjects[rec.Project] = true
		}
	}

	records := make([]SessionRecord, 0, len(relayRecords)+len(discovered))
	records = append(records, relayRecords...)
	for _, sys := range discovered {
		if relayNames[sys.Name] || relayProjects[sys.Name] {
			continue
		}
		records = append(records, sys)
	}
	return records
}

// legacySessionRe matches iOS-app-generated tmux session names in the
// pre-cx- scheme: <type>-<digits>, e.g. "claudeCode-6195", "terminal-6738".
// This is safe to use for discovery because the random suffix makes collisions
// with personal sessions like "hermes" or "main" extremely unlikely.
var legacySessionRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]+-\d+$`)

// tmuxDiscoveryCommand builds the `tmux list-sessions` invocation used for
// discovery. Crucially it must reach the *user's* tmux server.
//
// The relay's systemd unit sets PrivateTmp=true, giving the relay an isolated
// /tmp — so a bare `tmux` (default socket /tmp/tmux-<uid>/default) sees an
// empty namespace and finds nothing. PrivateTmp only remaps /tmp and /var/tmp,
// NOT /run/user/<uid>, which stays visible across the namespace. So we point
// tmux at a socket dir under /run/user/<uid> via TMUX_TMPDIR — verified
// reachable from the relay's namespace. The PTY spawn (terminal/server.go) sets
// the SAME TMUX_TMPDIR so the app's sessions land on this discoverable socket.
//
// Falls back to a bare `tmux` (no override) on macOS or when there's no
// per-user runtime dir, where PrivateTmp isn't in play.
func tmuxDiscoveryCommand(ctx context.Context, format string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", format)
	if dir := tmuxSocketDir(); dir != "" {
		cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+dir)
	}
	return cmd
}

// tmuxSocketDir returns the TMUX_TMPDIR the relay and its PTY spawns should use
// so tmux sockets live somewhere visible despite PrivateTmp. Empty string means
// "use tmux's default" (macOS / no per-user runtime dir).
func tmuxSocketDir() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if uid := os.Getuid(); uid > 0 {
		dir := fmt.Sprintf("/run/user/%d", uid)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return ""
}

// discoverTmuxSessions runs `tmux list-sessions` and returns records for
// sessions not managed by the relay. These are marked with Source="system".
func discoverTmuxSessions() []SessionRecord {
	// When running as root (LaunchDaemon), tmux connects to root's server by default.
	// We need to query the actual user's tmux server instead.
	// Bound the whole discovery: `su`/`tmux` can hang (wg0 contention, a wedged
	// tmux server). A timeout means a stuck tmux yields an empty session list
	// rather than blocking the caller (ListSessions) forever.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const format = "#{session_name}\t#{session_activity}\t#{session_attached}"
	cmd := tmuxDiscoveryCommand(ctx, format)
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
		// Surface relay-managed sessions only. Two naming schemes are supported:
		//   1. cx-<type> prefix — used after the iOS app is rebuilt with cx- prefix
		//   2. <type>-<digits> pattern — legacy scheme used by current iOS app
		//      (e.g. claudeCode-6195, terminal-6738); safe to surface because the
		//      random suffix makes collisions with user sessions extremely unlikely.
		// User sessions like "hermes" and "main" are excluded by both checks.
		isCxPrefixed := strings.HasPrefix(name, "cx-")
		isLegacyNamed := legacySessionRe.MatchString(name)
		if !isCxPrefixed && !isLegacyNamed {
			continue
		}
		if isCxPrefixed {
			// Strip the "cx-" prefix before returning to the client so the app sees
			// the logical name (e.g. "terminal") rather than the internal tmux name.
			name = strings.TrimPrefix(name, "cx-")
		}
		// Skip sessions with names that don't pass validation (too long, etc.)
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
	delete(h.sessions, name)
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

// RegisterActivity stores the Live Activity push token for a session, so the
// detector loop can push background content-state updates to that specific
// on-screen activity. An empty token unregisters (e.g. the activity ended).
func (h *Hub) RegisterActivity(sessionName, token string) {
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	if token == "" {
		delete(h.activityTokens, sessionName)
		return
	}
	h.activityTokens[sessionName] = token
	slog.Info("live activity registered", "session", sessionName, "token_suffix", token[max(0, len(token)-8):])
}

// PublishStatus is the detector's single dispatch point for a status change:
// it broadcasts session_status to connected clients (foreground updates) AND
// pushes an APNs Live Activity content-state update (background updates).
func (h *Hub) PublishStatus(name string, status SessionStatus, lastActivity time.Time) {
	msg, _ := json.Marshal(map[string]any{
		"type":         "session_status",
		"session":      name,
		"status":       string(status),
		"lastActivity": lastActivity.UTC().Format(time.RFC3339),
	})
	h.clientMu.Lock()
	for _, ch := range h.clients {
		select {
		case ch <- msg:
		default: // slow client — drop this status update rather than block
		}
	}
	h.clientMu.Unlock()

	h.pushLiveActivity(name, status, lastActivity)
}

// pushLiveActivity sends a Live Activity content-state update for the session,
// if APNs is configured and the app registered an activity token for it.
func (h *Hub) pushLiveActivity(name string, status SessionStatus, lastActivity time.Time) {
	if h.apnsClient == nil {
		return
	}
	h.activityMu.Lock()
	token := h.activityTokens[name]
	h.activityMu.Unlock()
	if token == "" {
		return
	}
	contentState := map[string]any{
		"status":         statusDisplayLabel(status),
		"isRunning":      status == StatusRunning,
		"lastActivityAt": lastActivity.UTC().Format(time.RFC3339),
	}
	go func() {
		// 8h stale window: the activity self-ticks its timer, so even if no
		// further push arrives the OS keeps it sensible, then marks it stale.
		if err := h.apnsClient.SendLiveActivity(token, contentState, 8*time.Hour); err != nil {
			if strings.Contains(err.Error(), "410") {
				h.activityMu.Lock()
				delete(h.activityTokens, name)
				h.activityMu.Unlock()
				slog.Info("dropped stale live-activity token", "session", name)
			} else {
				slog.Warn("live activity push failed", "session", name, "err", err)
			}
		}
	}()
}

// statusDisplayLabel maps a relay status to the human label the iOS widget shows.
func statusDisplayLabel(s SessionStatus) string {
	switch s {
	case StatusRunning:
		return "Running"
	case StatusIdle:
		return "Idle"
	case StatusFinished:
		return "Finished"
	default:
		return string(s)
	}
}

// SessionActivitySnapshot returns the current session→last-activity map from
// tmux, for the detector to classify. Reuses discoverTmuxSessions' parsing.
func SessionActivitySnapshot() map[string]time.Time {
	out := make(map[string]time.Time)
	for _, rec := range discoverTmuxSessions() {
		out[rec.Name] = rec.LastActivity
	}
	return out
}

// RunDetector polls tmux session activity on an interval and publishes a
// session_status (working/idle) on every classified transition. Blocks until
// ctx is cancelled. idleAfter is how long a session must be quiet before it's
// classified idle; poll is the tmux sampling cadence.
func (h *Hub) RunDetector(ctx context.Context, poll, idleAfter time.Duration) {
	tracker := detector.New(idleAfter)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	slog.Info("status detector started", "poll", poll, "idleAfter", idleAfter)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, ch := range tracker.Update(time.Now(), SessionActivitySnapshot()) {
				status := StatusIdle
				if ch.State == detector.Working {
					status = StatusRunning
				}
				h.PublishStatus(ch.Name, status, ch.LastActivity)
			}
		}
	}
}

// NotifyPermission fires a single "tool permission needed" alert for a session.
// This is the one reliably-detectable attention event: the Ollama chat proxy
// CREATES the permission request server-side (unique 128-bit id), so we know the
// exact moment a prompt is waiting. Deduped per requestID so a retry/re-entry
// never double-notifies, and collapse-id coalesces on the device.
//
// (Claude Code/Kiro/Goose permission prompts render only inside the tmux TUI —
// the relay can't see them — so this covers Ollama sessions only.)
func (h *Hub) NotifyPermission(sessionName, toolName, requestID string) {
	if h.apnsClient == nil {
		return
	}
	// Dedup: skip if we already notified for this exact request id.
	h.notifyMu.Lock()
	if h.notifiedPermIDs == nil {
		h.notifiedPermIDs = make(map[string]bool)
	}
	if h.notifiedPermIDs[requestID] {
		h.notifyMu.Unlock()
		return
	}
	h.notifiedPermIDs[requestID] = true
	// Bound the set so it can't grow unbounded over a long-lived relay.
	if len(h.notifiedPermIDs) > 256 {
		h.notifiedPermIDs = map[string]bool{requestID: true}
	}
	h.notifyMu.Unlock()

	body := "A tool is waiting for your approval"
	if toolName != "" {
		body = toolName + " is waiting for your approval"
	}
	h.sendAlert(apns.Alert{
		Title:     sessionName,
		Body:      body,
		SessionID: sessionName,
		Category:  "SESSION_PERMISSION",
		// One pending-permission notification per session at a time; a new prompt
		// replaces the previous one on the device rather than stacking.
		CollapseID: "perm:" + sessionName,
	})
}

// sendAlert fans an alert out to every registered device token, dropping tokens
// APNs reports as permanently dead (ErrTokenInvalid).
func (h *Hub) sendAlert(alert apns.Alert) {
	h.tokenMu.Lock()
	tokens := make([]string, len(h.deviceTokens))
	copy(tokens, h.deviceTokens)
	h.tokenMu.Unlock()

	for _, tok := range tokens {
		tok := tok
		go func() {
			err := h.apnsClient.Send(tok, alert)
			if err == nil {
				slog.Info("APNs alert sent", "session", alert.SessionID)
				return
			}
			if errors.Is(err, apns.ErrTokenInvalid) {
				h.tokenMu.Lock()
				for i, t := range h.deviceTokens {
					if t == tok {
						h.deviceTokens = append(h.deviceTokens[:i], h.deviceTokens[i+1:]...)
						slog.Info("removed dead APNs device token", "token_suffix", tok[max(0, len(tok)-8):])
						break
					}
				}
				h.tokenMu.Unlock()
				return
			}
			slog.Warn("APNs alert failed", "err", err, "token_suffix", tok[max(0, len(tok)-8):])
		}()
	}
}
