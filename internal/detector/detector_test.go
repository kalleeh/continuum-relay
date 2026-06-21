package detector

import (
	"testing"
	"time"
)

func TestClassifyAndTransitions(t *testing.T) {
	tr := New(30 * time.Second)
	now := time.Unix(1_000_000, 0)

	// First poll: one active session (just had activity) → Working, emitted as new.
	c := tr.Update(now, map[string]time.Time{"main": now.Add(-5 * time.Second)})
	if len(c) != 1 || c[0].Name != "main" || c[0].State != Working {
		t.Fatalf("first poll: want 1 working change, got %+v", c)
	}

	// Same state next poll → no change emitted.
	c = tr.Update(now.Add(1*time.Second), map[string]time.Time{"main": now.Add(-5 * time.Second)})
	if len(c) != 0 {
		t.Fatalf("steady state should emit no changes, got %+v", c)
	}

	// Activity goes stale (>30s) → flips to Idle, one change.
	later := now.Add(60 * time.Second)
	c = tr.Update(later, map[string]time.Time{"main": now.Add(-5 * time.Second)})
	if len(c) != 1 || c[0].State != Idle {
		t.Fatalf("want idle transition, got %+v", c)
	}

	// New activity arrives → back to Working.
	c = tr.Update(later.Add(1*time.Second), map[string]time.Time{"main": later})
	if len(c) != 1 || c[0].State != Working {
		t.Fatalf("want working transition, got %+v", c)
	}
}

func TestNewSessionEmittedOnce(t *testing.T) {
	tr := New(30 * time.Second)
	now := time.Unix(2_000_000, 0)
	act := map[string]time.Time{"a": now, "b": now.Add(-90 * time.Second)}
	c := tr.Update(now, act)
	if len(c) != 2 {
		t.Fatalf("two new sessions → two changes, got %d", len(c))
	}
	// Re-poll, unchanged → nothing.
	if c2 := tr.Update(now, act); len(c2) != 0 {
		t.Fatalf("unchanged re-poll should be silent, got %+v", c2)
	}
}

func TestDisappearedSessionForgotten(t *testing.T) {
	tr := New(30 * time.Second)
	now := time.Unix(3_000_000, 0)
	tr.Update(now, map[string]time.Time{"gone": now})
	// Session disappears.
	tr.Update(now.Add(1*time.Second), map[string]time.Time{})
	// Reappears active → should be treated as new and emit a change again.
	c := tr.Update(now.Add(2*time.Second), map[string]time.Time{"gone": now.Add(2 * time.Second)})
	if len(c) != 1 || c[0].State != Working {
		t.Fatalf("reappeared session should re-emit, got %+v", c)
	}
}

func TestForget(t *testing.T) {
	tr := New(30 * time.Second)
	now := time.Unix(4_000_000, 0)
	tr.Update(now, map[string]time.Time{"x": now})
	tr.Forget("x")
	// After Forget, the same active session re-emits as new.
	c := tr.Update(now, map[string]time.Time{"x": now})
	if len(c) != 1 {
		t.Fatalf("after Forget, want re-emit, got %+v", c)
	}
}
