package relay

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLaunchCommand(t *testing.T) {
	cases := []struct {
		agent string
		skip  bool
		want  string
	}{
		{"claude", false, "claude"},
		{"claude", true, "claude --dangerously-skip-permissions"},
		{"kiro", false, `export PATH="$HOME/.local/bin:$PATH"; kiro-cli chat`},
		{"kiro", true, `export PATH="$HOME/.local/bin:$PATH"; kiro-cli chat --trust-all-tools`},
		{"terminal", false, ""},
		{"rm -rf /", true, ""}, // an unknown agent can never produce a command
	}
	for _, c := range cases {
		if got := launchCommand(c.agent, c.skip); got != c.want {
			t.Errorf("launchCommand(%q, %v) = %q, want %q", c.agent, c.skip, got, c.want)
		}
	}
}

func TestStartSessionRejectsBadInput(t *testing.T) {
	h := NewHub(nil)
	for _, c := range []struct{ name, cwd, agent string }{
		{"bad name", "", "claude"},
		{"ok", "", "bash -c evil"},
		{"ok", "/etc", "claude"},
		{"ok", "/home/u/../../etc", "claude"},
	} {
		if err := h.StartSession(c.name, c.cwd, c.agent, false); err == nil {
			t.Errorf("StartSession(%q, %q, %q) should fail", c.name, c.cwd, c.agent)
		}
	}
	if err := h.EndSession("../x"); err == nil {
		t.Error("EndSession with a bad name should fail")
	}
	if err := h.EndSession("hermes"); err == nil || !strings.Contains(err.Error(), "started by Continuum") {
		t.Errorf("a discovered session must not be endable, got %v", err)
	}
}

func TestStartAndEndSessionAgainstTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sockDir, err := os.MkdirTemp("/tmp", "cxs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	t.Setenv("TMUX_TMPDIR", sockDir)
	t.Setenv("TMUX", "")
	ctx := context.Background()
	t.Cleanup(func() { _ = tmuxCommand(ctx, "kill-server").Run() })
	exists := func(name string) bool { return tmuxCommand(ctx, "has-session", "-t", "="+name).Run() == nil }

	workDir := t.TempDir()
	if !strings.HasPrefix(workDir, "/var/") && !strings.HasPrefix(workDir, "/tmp/") && !strings.HasPrefix(workDir, "/home/") {
		workDir = sockDir // macOS temp dirs live under /var/folders; fall back to an allowed path
	}
	h := NewHub(nil)
	if err := h.StartSession("fresh", workDir, "terminal", false); err != nil {
		t.Fatal(err)
	}
	if !exists("cx-fresh") {
		t.Fatal("tmux session cx-fresh was not created")
	}
	if _, ok := h.GetSession("fresh"); !ok {
		t.Error("the session was not recorded")
	}
	if err := h.StartSession("fresh", workDir, "terminal", false); err == nil {
		t.Error("starting the same name twice should fail")
	}

	// The launch command is typed into the shell: use `cat -v`-free proof with a harmless agent override.
	if err := h.StartSession("typed", workDir, "claude", false); err != nil {
		t.Fatal(err)
	}
	var screen string
	for i := 0; i < 40; i++ {
		out, _ := tmuxCommand(ctx, "capture-pane", "-p", "-t", "=cx-typed:").Output()
		screen = string(out)
		if strings.Contains(screen, "claude") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(screen, "claude") {
		t.Errorf("the launch command did not reach the pane: %q", screen)
	}

	if err := h.EndSession("fresh"); err != nil {
		t.Fatal(err)
	}
	if exists("cx-fresh") {
		t.Error("cx-fresh should be gone")
	}
	if _, ok := h.GetSession("fresh"); ok {
		t.Error("the record should be gone")
	}
	if err := h.EndSession("typed"); err != nil || exists("cx-typed") {
		t.Errorf("ending typed: %v", err)
	}
	// ending something already gone is not an error the user needs to see
	if err := h.EndSession("cx-never-existed"); err != nil {
		t.Errorf("ending a missing cx- session: %v", err)
	}
}

func TestHelloAnswersWithTheProtocol(t *testing.T) {
	m := helloMessage()
	if m["type"] != "hello" || m["protocol"] != ProtocolVersion {
		t.Fatalf("hello = %v", m)
	}
	have := map[string]bool{}
	for _, f := range ProtocolFeatures {
		have[f] = true
	}
	for _, want := range []string{"start_session", "end_session", "styled_screen", "screen_history", "list_status"} {
		if !have[want] {
			t.Errorf("feature %q is missing", want)
		}
	}
}
