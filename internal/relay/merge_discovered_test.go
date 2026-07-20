package relay

import "testing"

// Names of the records returned, in order, for compact assertions.
func names(recs []SessionRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Name
	}
	return out
}

func TestMergeDiscovered_DropsProjectContainerDuplicate(t *testing.T) {
	// The reported bug: a project-backed Claude Code session is hosted in tmux
	// session cx-myproject with the agent in a window named claudeCode-1234.
	// The relay record carries Project="myproject"; discovery surfaces the
	// container by its real tmux name "cx-myproject" (never stripped), which
	// must dedup against the record's project via the logical name.
	relay := []SessionRecord{
		{Name: "claudeCode-1234", Type: "claudeCode", Project: "myproject", Source: "relay"},
	}
	discovered := []SessionRecord{
		{Name: "cx-myproject", Type: "terminal", Source: "system"},
	}

	got := names(mergeDiscovered(relay, discovered))
	want := []string{"claudeCode-1234"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("project container should be suppressed: got %v, want %v", got, want)
	}
}

func TestMergeDiscovered_KeepsOrphanedCxSessionByRealName(t *testing.T) {
	// After a relay restart, an app-created cx- session has no relay record.
	// It must surface under its REAL tmux name so bare-name attach works —
	// stripping it to "104" would point the client at a session that doesn't
	// exist in tmux.
	got := names(mergeDiscovered(nil, []SessionRecord{
		{Name: "cx-104", Type: "terminal", Source: "system"},
	}))
	if len(got) != 1 || got[0] != "cx-104" {
		t.Fatalf("orphaned cx- session should surface by real name: got %v", got)
	}
}

func TestMergeDiscovered_DropsNameDuplicate(t *testing.T) {
	// Restart-recovery case: relay rebuilt h.sessions and the discovered tmux
	// session shares the same logical name.
	relay := []SessionRecord{{Name: "terminal-6738", Type: "terminal", Source: "relay"}}
	discovered := []SessionRecord{{Name: "terminal-6738", Type: "terminal", Source: "system"}}

	if got := names(mergeDiscovered(relay, discovered)); len(got) != 1 {
		t.Fatalf("name duplicate should be suppressed: got %v", got)
	}
}

func TestMergeDiscovered_KeepsGenuineSystemSession(t *testing.T) {
	// A real pre-existing user session (no matching relay name or project) must
	// still surface so the "show system sessions" toggle has something to show.
	relay := []SessionRecord{
		{Name: "claudeCode-1234", Type: "claudeCode", Project: "myproject", Source: "relay"},
	}
	discovered := []SessionRecord{
		{Name: "cx-myproject", Type: "terminal", Source: "system"}, // phantom — dropped
		{Name: "hermes-42", Type: "terminal", Source: "system"},    // genuine — kept
	}

	got := names(mergeDiscovered(relay, discovered))
	if len(got) != 2 || got[1] != "hermes-42" {
		t.Fatalf("genuine system session should be kept: got %v", got)
	}
}

func TestMergeDiscovered_NoProjectStillDedupsByName(t *testing.T) {
	// A flat (non-project) agent session uses tmux session cx-<sessionName>;
	// its logical name is exactly the relay record's name — dedup via the
	// logical name covers it, and the empty Project must not suppress
	// unrelated sessions.
	relay := []SessionRecord{{Name: "claudeCode-9", Type: "claudeCode", Source: "relay"}}
	discovered := []SessionRecord{
		{Name: "cx-claudeCode-9", Type: "terminal", Source: "system"}, // same logical name — dropped
		{Name: "other", Type: "terminal", Source: "system"},           // kept
	}

	got := names(mergeDiscovered(relay, discovered))
	if len(got) != 2 || got[1] != "other" {
		t.Fatalf("got %v, want [claudeCode-9 other]", got)
	}
}
