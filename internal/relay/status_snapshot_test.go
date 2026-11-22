package relay

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/continuum-app/continuum-relay/internal/detector"
)

func TestStatusSnapshot(t *testing.T) {
	h := NewHub(nil)
	if got := h.StatusSnapshot(); len(got) != 0 {
		t.Fatalf("before the detector runs there is nothing to report, got %d messages", len(got))
	}

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tracker := detector.New(20 * time.Second)
	tracker.Update(now, map[string]time.Time{
		"cx-busy":  now.Add(-2 * time.Second),
		"cx-quiet": now.Add(-5 * time.Minute),
	})
	h.tracker = tracker

	msgs := h.StatusSnapshot()
	if len(msgs) != 2 {
		t.Fatalf("want one message per tracked session, got %d", len(msgs))
	}
	want := []struct{ session, status string }{{"cx-busy", "running"}, {"cx-quiet", "idle"}}
	for i, raw := range msgs {
		var m struct {
			Type, Session, Status, LastActivity string
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != "session_status" || m.Session != want[i].session || m.Status != want[i].status {
			t.Errorf("message %d = %s, want session %s status %s", i, raw, want[i].session, want[i].status)
		}
		if _, err := time.Parse(time.RFC3339, m.LastActivity); err != nil {
			t.Errorf("lastActivity %q is not RFC3339", m.LastActivity)
		}
	}

	// A session that has gone away is no longer reported.
	tracker.Update(now, map[string]time.Time{"cx-busy": now})
	if got := h.StatusSnapshot(); len(got) != 1 {
		t.Errorf("want 1 message after a session disappears, got %d", len(got))
	}
}
