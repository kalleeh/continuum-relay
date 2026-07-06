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
// The actual PTY lives in the terminal subsystem (port 7681) — Session only
// tracks the name, working directory, and status. Session output flows
// entirely over the terminal WebSocket as raw VT100 bytes; there is no
// subprocess and no per-session pub/sub here.
type Session struct {
	Record SessionRecord

	mu sync.RWMutex
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
