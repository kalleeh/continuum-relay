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
	// container as name="myproject", type="terminal", source="system".
	relay := []SessionRecord{
		{Name: "claudeCode-1234", Type: "claudeCode", Project: "myproject", Source: "relay"},
	}
	discovered := []SessionRecord{
		{Name: "myproject", Type: "terminal", Source: "system"},
	}

	got := names(mergeDiscovered(relay, discovered))
	want := []string{"claudeCode-1234"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("project container should be suppressed: got %v, want %v", got, want)
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
		{Name: "myproject", Type: "terminal", Source: "system"}, // phantom — dropped
		{Name: "hermes-42", Type: "terminal", Source: "system"}, // genuine — kept
	}

	got := names(mergeDiscovered(relay, discovered))
	if len(got) != 2 || got[1] != "hermes-42" {
		t.Fatalf("genuine system session should be kept: got %v", got)
	}
}

func TestMergeDiscovered_NoProjectStillDedupsByName(t *testing.T) {
	// A flat (non-project) agent session uses tmux session cx-<sessionName>,
	// which strips back to exactly the relay name — dedup by name covers it and
	// the empty Project must not suppress unrelated sessions.
	relay := []SessionRecord{{Name: "claudeCode-9", Type: "claudeCode", Source: "relay"}}
	discovered := []SessionRecord{
		{Name: "claudeCode-9", Type: "terminal", Source: "system"}, // same name — dropped
		{Name: "other", Type: "terminal", Source: "system"},        // kept
	}

	got := names(mergeDiscovered(relay, discovered))
	if len(got) != 2 || got[1] != "other" {
		t.Fatalf("got %v, want [claudeCode-9 other]", got)
	}
}
