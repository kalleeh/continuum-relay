package relay

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTmuxTargets(t *testing.T) {
	cases := []struct {
		name string
		rec  *SessionRecord
		want []string
	}{
		{"3", nil, []string{"=3:"}},
		{"cx-orphan", nil, []string{"=cx-orphan:"}},
		{"api", &SessionRecord{Name: "api"}, []string{"=cx-api:"}},
		{"claude", &SessionRecord{Name: "claude", Project: "my-app"}, []string{"=cx-my-app:=claude", "=cx-claude:"}},
		// A project name the client would have rejected falls back to the flat form.
		{"claude", &SessionRecord{Name: "claude", Project: "my app"}, []string{"=cx-claude:"}},
	}
	for _, c := range cases {
		if got := tmuxTargets(c.name, c.rec); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tmuxTargets(%q, %+v) = %v, want %v", c.name, c.rec, got, c.want)
		}
	}
}

func TestLastScreenLines(t *testing.T) {
	screen := "first   \n\n  second\n\t\nthird\n\n\n"
	if got := lastScreenLines(screen, 2); !reflect.DeepEqual(got, []string{"  second", "third"}) {
		t.Errorf("got %q", got)
	}
	if got := lastScreenLines(screen, 0); len(got) != 3 {
		t.Errorf("maxLines 0 should mean the default cap, got %q", got)
	}
	if got := lastScreenLines("", 5); got == nil || len(got) != 0 {
		t.Errorf("empty screen should give an empty, non-nil list (JSON []), got %#v", got)
	}
	long := strings.Repeat("é", maxScreenLineRunes+50)
	if got := lastScreenLines(long, 1); len([]rune(got[0])) != maxScreenLineRunes {
		t.Errorf("long line not capped: %d runes", len([]rune(got[0])))
	}
}

func TestSendKeyRejectsBadInput(t *testing.T) {
	h := NewHub(nil)
	for _, key := range []string{"", "Enter", "C-c", "up; rm -rf /", "Q", "yy", "-", ";", "é"} {
		if err := h.SendKey("api", key); err == nil || !strings.Contains(err.Error(), "unsupported key") {
			t.Errorf("key %q: want unsupported-key error, got %v", key, err)
		}
	}
	for _, name := range []string{"", "a b", "x;y", "../x", strings.Repeat("a", 65)} {
		if err := h.SendKey(name, "up"); err == nil || !strings.Contains(err.Error(), "invalid session name") {
			t.Errorf("name %q: want invalid-name error, got %v", name, err)
		}
		if _, err := h.CaptureScreen(name, 5); err == nil {
			t.Errorf("name %q: capture should fail", name)
		}
	}
}

// End to end against a real tmux server on a private socket: keys sent through
// the Hub arrive in the pane, and the pane's text comes back.
func TestSendKeyAndCaptureAgainstTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A short path: tmux's socket path has a tight length limit that t.TempDir() exceeds on macOS.
	sockDir, err := os.MkdirTemp("/tmp", "cxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	t.Setenv("TMUX_TMPDIR", sockDir)
	t.Setenv("TMUX", "")
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if out, err := tmuxCommand(ctx, args...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	t.Cleanup(func() { _ = tmuxCommand(ctx, "kill-server").Run() })

	// `cat -v` echoes each key as visible text: Up is ^[[A, Down ^[[B, Escape ^[.
	run("new-session", "-d", "-s", "cx-flat", "-x", "80", "-y", "24", "cat -v")
	run("new-session", "-d", "-s", "cx-proj", "-x", "80", "-y", "24", "sh")
	run("new-window", "-t", "cx-proj", "-n", "agent", "cat -v")
	run("select-window", "-t", "cx-proj:^") // back to the first window: the agent window is NOT the active one
	run("new-session", "-d", "-s", "cx-flat-other", "-x", "80", "-y", "24", "cat -v")
	run("new-session", "-d", "-s", "bare7", "-x", "80", "-y", "24", "cat -v")

	h := NewHub(nil)
	if _, err := h.CreateSession("flat", "", "claudeCode"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateSession("agent", "/home/u/projects/proj", "claudeCode"); err != nil {
		t.Fatal(err)
	}

	screen := func(name string) string {
		t.Helper()
		var lines []string
		var err error
		for i := 0; i < 40; i++ { // the pane updates asynchronously
			if lines, err = h.CaptureScreen(name, 10); err != nil {
				t.Fatalf("capture %s: %v", name, err)
			}
			if len(lines) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return strings.Join(lines, "\n")
	}
	press := func(name string, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if err := h.SendKey(name, k); err != nil {
				t.Fatalf("send %s to %s: %v", k, name, err)
			}
		}
	}

	press("flat", "up", "down", "enter")
	if got := screen("flat"); !strings.Contains(got, "^[[A^[[B") && !strings.Contains(got, "^[OA^[OB") {
		t.Errorf("flat session did not receive up+down, screen: %q", got)
	}
	// "flat" must not have leaked into the session it is a prefix of.
	if lines, _ := h.CaptureScreen("cx-flat-other", 10); len(lines) != 0 {
		t.Errorf("keys leaked into cx-flat-other: %q", lines)
	}

	press("agent", "escape", "enter") // project-backed: window "agent" in cx-proj
	if got := screen("agent"); !strings.Contains(got, "^[") {
		t.Errorf("project window did not receive escape, screen: %q", got)
	}

	press("bare7", "down", "enter") // discovered session, bare name
	if got := screen("bare7"); !strings.Contains(got, "^[[B") && !strings.Contains(got, "^[OB") {
		t.Errorf("discovered session did not receive down, screen: %q", got)
	}

	press("bare7", "y", "7") // single characters, typed literally
	if got := screen("bare7"); !strings.Contains(got, "y7") {
		t.Errorf("discovered session did not receive literal y7, screen: %q", got)
	}

	if err := h.SendKey("missing", "up"); err == nil {
		t.Error("sending to a session that does not exist should fail")
	}
}

func TestLatestActivity(t *testing.T) {
	cases := map[string]int64{
		"1700000000":                        1700000000,
		"1700000000 1700000050 1700000020 ": 1700000050,
		"1700000050 ":                       1700000050,
		"":                                  0,
		"garbage 12 x":                      12,
	}
	for field, want := range cases {
		got := latestActivity(field)
		if want == 0 && !got.IsZero() || want != 0 && got.Unix() != want {
			t.Errorf("latestActivity(%q) = %v, want unix %d", field, got, want)
		}
	}
}

// Output in a background window must advance the session's last activity:
// that is the only signal that an unattended agent is working.
func TestDiscoveryActivityFollowsPaneOutput(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sockDir, err := os.MkdirTemp("/tmp", "cxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	t.Setenv("TMUX_TMPDIR", sockDir)
	t.Setenv("TMUX", "")
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if out, err := tmuxCommand(ctx, args...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	t.Cleanup(func() { _ = tmuxCommand(ctx, "kill-server").Run() })

	run("new-session", "-d", "-s", "quiet", "-x", "80", "-y", "24", "cat")
	run("new-session", "-d", "-s", "busy", "-x", "80", "-y", "24", "cat")
	run("new-window", "-t", "busy", "-n", "agent", "while true; do date; sleep 0.5; done")
	run("select-window", "-t", "busy:^") // the noisy window is in the background

	activity := func() map[string]time.Time {
		out := map[string]time.Time{}
		for _, rec := range discoverTmuxSessions() {
			out[rec.Name] = rec.LastActivity
		}
		return out
	}
	before := activity()
	if before["busy"].IsZero() || before["quiet"].IsZero() {
		t.Fatalf("sessions not discovered: %v", before)
	}
	time.Sleep(2500 * time.Millisecond)
	after := activity()
	if !after["busy"].After(before["busy"]) {
		t.Errorf("busy session's activity did not advance: %v -> %v", before["busy"], after["busy"])
	}
	if !after["quiet"].Equal(before["quiet"]) {
		t.Errorf("quiet session's activity moved: %v -> %v", before["quiet"], after["quiet"])
	}
}
