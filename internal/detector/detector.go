// Package detector classifies tmux session activity as "working" vs "idle"
// using tmux's own per-session #{session_activity} timestamp — the same signal
// the relay already reads when listing sessions. This is deliberately simpler
// and more robust than scraping pane content or /proc state: tmux bumps
// session_activity whenever any window in the session produces output, across
// all session types (claude/kiro/goose/shell), with no target-window mapping
// or prompt-string guessing.
//
// The Tracker (pure, no tmux/IO) is the classifier; the relay package owns the
// polling loop and dispatch so this stays unit-testable.
package detector

import (
	"sync"
	"time"
)

// State is the coarse activity classification. "working" = output seen
// recently; "idle" = quiet past the idle window. We intentionally do NOT try to
// distinguish "waiting for input" here — that needs fragile per-agent prompt
// detection and is deferred (see the plan).
type State string

const (
	Working State = "working"
	Idle    State = "idle"
)

// Change reports a session whose classified State flipped since the last poll.
type Change struct {
	Name         string
	State        State
	LastActivity time.Time // the session's most recent activity time
}

// Tracker holds the last-seen activity timestamp + derived state per session
// and reports transitions. Safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	idleAfter time.Duration
	seen      map[string]entry
}

type entry struct {
	lastActivity time.Time
	state        State
}

// New returns a Tracker that classifies a session as Idle once its activity
// timestamp has not advanced for idleAfter.
func New(idleAfter time.Duration) *Tracker {
	return &Tracker{idleAfter: idleAfter, seen: make(map[string]entry)}
}

// Update feeds the current snapshot of session→lastActivity (as tmux reports it)
// and returns the set of sessions whose State changed. `now` is passed in so the
// classifier is deterministic and testable.
//
// Rules:
//   - A session is Working if its last activity is within idleAfter of now.
//   - Otherwise Idle.
//   - A brand-new session emits a Change for its initial state (so a freshly
//     active session pushes immediately).
//   - Sessions that disappeared from the snapshot are dropped silently (the
//     session list / end-activity path handles removal).
func (t *Tracker) Update(now time.Time, activity map[string]time.Time) []Change {
	t.mu.Lock()
	defer t.mu.Unlock()

	var changes []Change
	for name, last := range activity {
		state := Idle
		if now.Sub(last) < t.idleAfter {
			state = Working
		}
		prev, existed := t.seen[name]
		if !existed || prev.state != state {
			changes = append(changes, Change{Name: name, State: state, LastActivity: last})
		}
		t.seen[name] = entry{lastActivity: last, state: state}
	}
	// Forget sessions no longer present so a recreated session is treated as new.
	for name := range t.seen {
		if _, ok := activity[name]; !ok {
			delete(t.seen, name)
		}
	}
	return changes
}

// Forget drops a session from tracking (e.g. when its Live Activity ends), so if
// it reappears it re-emits an initial Change.
func (t *Tracker) Forget(name string) {
	t.mu.Lock()
	delete(t.seen, name)
	t.mu.Unlock()
}
