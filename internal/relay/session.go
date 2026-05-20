package relay

import (
	"strings"
	"sync"
	"time"
)

type SessionStatus string

const (
	StatusRunning  SessionStatus = "running"
	StatusIdle     SessionStatus = "idle"
	StatusFinished SessionStatus = "finished"
)

type SessionRecord struct {
	Name             string        `json:"name"`
	Type             string        `json:"sessionType"`
	Status           SessionStatus `json:"status"`
	WorkingDirectory string        `json:"workingDirectory,omitempty"`
	Project          string        `json:"project,omitempty"`
	LastActivity     time.Time     `json:"lastActivity"`
	LastSnippet      string        `json:"lastOutputSnippet,omitempty"`
	Source           string        `json:"source,omitempty"` // "relay" (app-created) or "system" (discovered tmux)
}

// Session is a relay-side bookkeeping record for a tmux-backed session.
// The actual PTY lives in the terminal subsystem (port 7681) — Session
// only tracks the name, working directory, and any subscribers that want
// to be notified when the session list changes. There is no subprocess.
type Session struct {
	Record SessionRecord

	mu          sync.RWMutex
	subscribers map[string]chan []byte // clientID → notification channel

	hub *Hub // set by Hub.CreateSession; reserved for future push-notification dispatch
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

func (s *Session) Subscribe(clientID string) <-chan []byte {
	ch := make(chan []byte, 128)
	s.mu.Lock()
	s.subscribers[clientID] = ch
	s.mu.Unlock()
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
