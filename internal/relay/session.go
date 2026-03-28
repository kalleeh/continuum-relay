package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SessionStatus string

const (
	StatusRunning  SessionStatus = "running"
	StatusIdle     SessionStatus = "idle"
	StatusFinished SessionStatus = "finished"
)

type pendingPermission struct {
	id       string
	mode     string // "json" or "text" — determines stdin response format
	response chan bool
}

type SessionRecord struct {
	Name             string        `json:"name"`
	Type             string        `json:"sessionType"`
	Status           SessionStatus `json:"status"`
	WorkingDirectory string        `json:"workingDirectory,omitempty"`
	Project          string        `json:"project,omitempty"`
	LastActivity     time.Time     `json:"lastActivity"`
	LastSnippet      string        `json:"lastOutputSnippet,omitempty"`
}

const recentOutputBuffer = 50 // replay up to last 50 events to new subscribers

type Session struct {
	Record SessionRecord

	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancelCtx context.CancelFunc

	mu           sync.RWMutex
	subscribers  map[string]chan []byte // clientID → output channel
	recentOutput [][]byte              // circular buffer for late-joining subscribers

	pendingPerm  *pendingPermission
	permissionMu sync.Mutex

	hub *Hub // set by Hub.CreateSession for push notification dispatch
}

func NewSession(name, cwd string) *Session {
	return &Session{
		Record: SessionRecord{
			Name:             name,
			Type:             "claudeCode",
			Status:           StatusIdle,
			WorkingDirectory: cwd,
			LastActivity:     time.Now(),
		},
		subscribers: make(map[string]chan []byte),
	}
}

// Start launches the claude process. Returns error if already running.
func (s *Session) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil {
		return nil // already running, clients just attach
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelCtx = cancel

	cmd := exec.CommandContext(ctx, "claude",
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = s.Record.WorkingDirectory
	if cmd.Dir == "" {
		cmd.Dir = "/home/ubuntu"
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}

	s.cmd = cmd
	s.stdin = stdin
	s.Record.Status = StatusRunning

	go s.readOutput(stdout)
	go io.Copy(io.Discard, stderr)
	go s.waitForExit()

	slog.Info("session started", "name", s.Record.Name, "pid", cmd.Process.Pid)
	return nil
}

func (s *Session) readOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large tool outputs
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Update last snippet for session list
		s.mu.Lock()
		s.Record.LastActivity = time.Now()
		// Extract a readable snippet from the stream-json line
		var evt map[string]any
		if json.Unmarshal(line, &evt) == nil {
			// Permission request detection — handled before broadcast, blocks until response
			evtType, _ := evt["type"].(string)
			if evtType == "tool_input_request" || evtType == "permission_request" {
				s.mu.Unlock()
				s.handlePermissionRequest(line, evt, "json")
				continue
			}
			// Text-prompt fallback: detect "[y/N]" or "Do you want" patterns in system events
			if evtType == "system" {
				if msg, _ := evt["message"].(string); strings.Contains(msg, "[y/N]") || strings.Contains(msg, "Do you want") {
					s.mu.Unlock()
					s.handlePermissionRequest(line, evt, "text")
					continue
				}
			}
			if evtType == "result" {
				s.Record.Status = StatusFinished
				// Extract result summary for push notification (first 100 chars)
				resultSummary := ""
				if r, ok := evt["result"].(string); ok {
					if len(r) > 100 {
						resultSummary = r[:100] + "…"
					} else {
						resultSummary = r
					}
				}
				sessionName := s.Record.Name
				hub := s.hub
				s.mu.Unlock()
				if hub != nil {
					go hub.SendPush(sessionName, resultSummary)
				}
				s.broadcast(line)
				continue
			}
		}
		s.mu.Unlock()

		// Broadcast to all subscribers
		s.broadcast(line)
	}
}

func (s *Session) waitForExit() {
	if s.cmd != nil {
		_ = s.cmd.Wait()
	}
	s.mu.Lock()
	s.Record.Status = StatusFinished
	s.cmd = nil
	s.stdin = nil
	s.mu.Unlock()
	slog.Info("session exited", "name", s.Record.Name)
}

func (s *Session) broadcast(data []byte) {
	// Wrap in a server message envelope
	msg, _ := json.Marshal(map[string]any{
		"type":    "session_event",
		"session": s.Record.Name,
		"event":   json.RawMessage(data),
	})

	s.mu.Lock()
	// Buffer for late-joining subscribers
	if len(s.recentOutput) >= recentOutputBuffer {
		s.recentOutput = s.recentOutput[1:]
	}
	s.recentOutput = append(s.recentOutput, msg)
	subs := make(map[string]chan []byte, len(s.subscribers))
	for id, ch := range s.subscribers {
		subs[id] = ch
	}
	s.mu.Unlock()

	for id, ch := range subs {
		select {
		case ch <- msg:
		default:
			slog.Warn("message dropped for slow subscriber", "session", s.Record.Name, "client", id)
		}
	}
}

func (s *Session) broadcastRaw(data []byte) {
	s.mu.Lock()
	if len(s.recentOutput) >= recentOutputBuffer {
		s.recentOutput = s.recentOutput[1:]
	}
	s.recentOutput = append(s.recentOutput, data)
	subs := make(map[string]chan []byte, len(s.subscribers))
	for id, ch := range s.subscribers {
		subs[id] = ch
	}
	s.mu.Unlock()
	for id, ch := range subs {
		select {
		case ch <- data:
		default:
			slog.Warn("message dropped for slow subscriber", "session", s.Record.Name, "client", id)
		}
	}
}

func (s *Session) handlePermissionRequest(line []byte, evt map[string]any, mode string) {
	id, _ := evt["id"].(string)
	if id == "" {
		id = fmt.Sprintf("perm-%d", time.Now().UnixNano())
	}
	toolName, _ := evt["tool_name"].(string)
	if toolName == "" {
		toolName = "Tool"
	}
	inputBytes, _ := json.Marshal(evt["input"])

	msg, _ := json.Marshal(map[string]any{
		"type":      "permission_request",
		"session":   s.Record.Name,
		"id":        id,
		"tool_name": toolName,
		"input":     json.RawMessage(inputBytes),
	})

	respCh := make(chan bool, 1)
	s.permissionMu.Lock()
	s.pendingPerm = &pendingPermission{id: id, mode: mode, response: respCh}
	s.permissionMu.Unlock()

	s.broadcastRaw(msg)

	// Block readOutput goroutine until iOS responds or timeout
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	select {
	case allow := <-respCh:
		s.permissionMu.Lock()
		s.pendingPerm = nil
		s.permissionMu.Unlock()
		s.sendPermissionDecision(allow, mode)
	case <-timer.C:
		s.permissionMu.Lock()
		s.pendingPerm = nil
		s.permissionMu.Unlock()
		slog.Info("permission request timed out, auto-denying", "session", s.Record.Name)
		s.sendPermissionDecision(false, mode)
	}
}

func (s *Session) sendPermissionDecision(allow bool, mode string) {
	s.mu.RLock()
	w := s.stdin
	s.mu.RUnlock()
	if w == nil {
		return
	}
	if mode == "json" {
		decision := "n"
		if allow {
			decision = "y"
		}
		envelope, _ := json.Marshal(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": decision},
				},
			},
		})
		_, _ = w.Write(append(envelope, '\n'))
	} else {
		response := "n\n"
		if allow {
			response = "y\n"
		}
		_, _ = w.Write([]byte(response))
	}
}

func (s *Session) RespondToPermission(id string, allow bool) bool {
	s.permissionMu.Lock()
	defer s.permissionMu.Unlock()
	if s.pendingPerm == nil || s.pendingPerm.id != id {
		return false
	}
	s.pendingPerm.response <- allow
	return true
}

func (s *Session) Subscribe(clientID string) <-chan []byte {
	ch := make(chan []byte, 128)
	s.mu.Lock()
	s.subscribers[clientID] = ch
	// Replay recent output to catch up (handles init event race)
	recent := make([][]byte, len(s.recentOutput))
	copy(recent, s.recentOutput)
	s.mu.Unlock()
	// Send buffered events before live ones
	go func() {
		for _, msg := range recent {
			ch <- msg
		}
	}()
	return ch
}

func (s *Session) Unsubscribe(clientID string) {
	s.mu.Lock()
	if ch, ok := s.subscribers[clientID]; ok {
		close(ch)
		delete(s.subscribers, clientID)
	}
	s.mu.Unlock()
}

func (s *Session) Send(message string) error {
	s.mu.RLock()
	w := s.stdin
	s.mu.RUnlock()
	if w == nil {
		return nil
	}
	// claude --input-format stream-json expects JSON messages on stdin
	envelope, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": message},
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = w.Write(append(envelope, '\n'))
	return err
}

func (s *Session) Interrupt() {
	s.mu.RLock()
	cmd := s.cmd
	s.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGINT)
	}
}

func (s *Session) Stop() {
	s.mu.Lock()
	cancel := s.cancelCtx
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// projectFromCWD extracts the project name from a working directory path.
// e.g. /home/ubuntu/projects/my-app → "my-app", /home/ubuntu → ""
func projectFromCWD(cwd string) string {
	const marker = "/projects/"
	idx := strings.Index(cwd, marker)
	if idx < 0 {
		return ""
	}
	rest := cwd[idx+len(marker):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func (s *Session) GetRecord() SessionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.Record
	r.Project = projectFromCWD(r.WorkingDirectory)
	return r
}
