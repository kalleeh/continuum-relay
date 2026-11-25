package relay

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

const rule = "────────────────────────────────────────"

// Screens below are shaped after real captures of Kiro CLI v3 and Claude Code.

func kiroWorking() string {
	return strings.Join([]string{
		"  › Use the shell tool to run exactly: sleep 12 && echo finished-sleeping. After",
		"    it completes, say done in one short sentence.",
		"• Shell sleep 12 && echo finished-sleeping",
		"    ╰ description=Run sleep then echo message, run_in_background=false",
		"  esc to cancel",
		rule,
		"Default · auto · ◔ 3%                                                          /",
		"›  Kiro is working · 14s · Type to steer · Ctrl+S to queue",
	}, "\n")
}

func kiroDone() string {
	return strings.Join([]string{
		"• Shell sleep 12 && echo finished-sleeping",
		"    ╰ description=Run sleep then echo message, run_in_background=false",
		"    ╰ \x1b[38;5;151mfinished-sleeping\x1b[39m",
		"\x1b[38;5;219m•\x1b[39m Done, the command finished and printed \"finished-sleeping\".",
		"  It took twelve seconds.",
		"▸ Credits: turn 0.10 • session 0.20 | Time: 57s",
		rule,
		"\x1b[95mDefault\x1b[39m · auto · \x1b[32m◔\x1b[39m \x1b[32m3%\x1b[39m                            \x1b[95m~/projects/llm-academy\x1b[39m · (main)",
		"›  ask a question or describe a task ↵",
	}, "\n")
}

func kiroPermission() string {
	return strings.Join([]string{
		"  › Use the shell tool to run exactly: sleep 12 && echo finished-sleeping.",
		"↓ Shell sleep 12 && echo finished-sleeping",
		"    ╰ description=Run sleep then echo message, run_in_background=false",
		rule,
		" Run sleep then echo message requires approval",
		" shell → sleep 12 && echo finished-sleeping",
		" \x1b[1m\x1b[35m❯ Allow once\x1b[0m",
		"   Deny once",
		"   Create allow rule to approve future similar actions",
		"   Create deny rule to block future similar actions",
		rule,
		" esc to close · ↑↓ to navigate · ↵ to select · Tab to edit",
	}, "\n")
}

func claudeIdle() string {
	return strings.Join([]string{
		"▝▜██████▀  Opus 4.8 with high effort · Amazon Bedrock",
		"⚠ 1 MCP server needs authentication · run /mcp",
		"▎ Auto mode is now Claude Code's default permission mode.",
		rule,
		"❯ fix the failing test",
		rule,
		"  ⏵⏵ auto mode on (shift+tab to cycle)",
	}, "\n")
}

func claudeWorking() string {
	return strings.Join([]string{
		"❯ Use the Bash tool to run: sleep 14 && echo finished-sleeping. Then reply with",
		"  one short sentence.",
		"⏺ Bash(sleep 14 && echo finished-sleeping)",
		"✶ Puttering… (1m 30s · ↓ 9.4k tokens · thinking)",
		rule,
		"❯ ",
		rule,
		"  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt",
	}, "\n")
}

func claudeDone() string {
	return strings.Join([]string{
		"❯ Run the tests",
		"⏺ Bash(npm test)",
		"  ⎿  PASS src/a.test.ts",
		"⏺ All 42 tests pass.",
		"  Login now retries once on a 401.",
		"  Want me to commit?",
		rule,
		"❯ ",
		rule,
		"  ⏵⏵ accept edits on (shift+tab to cycle)",
	}, "\n")
}

func claudeDialog() string {
	return strings.Join([]string{
		"  Try the new fullscreen renderer?",
		"  · Flicker-free output",
		"  ❯ 1. Yes, try it",
		"    2. Not now",
		"  Enter to confirm · Esc to cancel",
	}, "\n")
}

func claudePermission() string {
	return strings.Join([]string{
		"╭──────────────────────────────╮",
		"│ Bash command                 │",
		"│   rm -rf build               │",
		"│ Do you want to proceed?      │",
		"│ \x1b[36m❯ 1. Yes\x1b[0m                    │",
		"│   2. Yes, and don't ask again │",
		"│   3. No, tell Claude what to do │",
		"╰──────────────────────────────╯",
	}, "\n")
}

func TestKiroWorking(t *testing.T) {
	s := parseScreen(kiroWorking())
	if s.State != StateWorking || s.Elapsed != "14s" {
		t.Fatalf("state %q elapsed %q", s.State, s.Elapsed)
	}
	m := s.Meta
	if m.Agent != "Default" || m.Mode != "auto" || m.Context != 3 || m.CWD != "/" {
		t.Errorf("meta %+v", m)
	}
	if m.Doing != "Shell sleep 12 && echo finished-sleeping" {
		t.Errorf("doing %q", m.Doing)
	}
	if len(s.Lines) != 0 {
		t.Errorf("a busy agent shows no text: %q", s.Lines)
	}
}

func TestKiroIdlePromptIsNotAMenu(t *testing.T) { // the bug: "›" was taken for a menu cursor
	s := parseScreen(strings.Join([]string{"Welcome to Kiro CLI V3", rule, "Default · auto · ◔ 1%    /", "›  ask a question or describe a task ↵"}, "\n"))
	if s.State == StateWaiting || s.State == StateWorking {
		t.Fatalf("an idle Kiro is %q", s.State)
	}
	if s.Meta.Input != "" || s.Meta.Context != 1 {
		t.Errorf("meta %+v", s.Meta)
	}
}

func TestKiroDone(t *testing.T) {
	s := parseScreen(kiroDone())
	if s.State != StateDone {
		t.Fatalf("state %q", s.State)
	}
	m := s.Meta
	if m.Credits != "0.10" || m.Took != "57s" || m.CWD != "~/projects/llm-academy" || m.Branch != "main" || m.Agent != "Default" {
		t.Errorf("meta %+v", m)
	}
	if len(s.Lines) != 2 || !strings.Contains(s.Lines[0], "Done, the command finished") || !strings.Contains(s.Lines[0], "\x1b[38;5;219m") ||
		strings.Contains(strings.Join(s.Lines, ""), "Shell") || strings.Contains(strings.Join(s.Lines, ""), "Credits") {
		t.Errorf("only the reply, in colour: %q", s.Lines)
	}
}

func TestKiroWaiting(t *testing.T) {
	s := parseScreen(kiroPermission())
	if s.State != StateWaiting {
		t.Fatalf("state %q", s.State)
	}
	j := strings.Join(s.Lines, "\n")
	for _, want := range []string{"requires approval", "Allow once", "Deny once", "Create deny rule"} {
		if !strings.Contains(j, want) {
			t.Errorf("%q missing from %q", want, s.Lines)
		}
	}
	if strings.Contains(j, "esc to close") || strings.Contains(j, "to navigate") {
		t.Errorf("the key hints are chrome: %q", s.Lines)
	}
}

func TestKiroTypedInputIsKept(t *testing.T) {
	raw := kiroDone()
	raw = strings.Replace(raw, "›  ask a question or describe a task ↵", "›  fix the failing test", 1)
	s := parseScreen(raw)
	if s.Meta.Input != "fix the failing test" || !strings.Contains(s.Lines[len(s.Lines)-1], "fix the failing test") {
		t.Errorf("input %q lines %q", s.Meta.Input, s.Lines)
	}
}

func TestClaudeIdleAndInput(t *testing.T) {
	s := parseScreen(claudeIdle())
	if s.State == StateWaiting || s.State == StateWorking {
		t.Fatalf("state %q", s.State)
	}
	if s.Meta.Mode != "auto mode on" || s.Meta.Model != "Opus 4.8" || s.Meta.Input != "fix the failing test" {
		t.Errorf("meta %+v", s.Meta)
	}
	if strings.Contains(strings.Join(s.Lines, ""), "MCP server") || strings.Contains(strings.Join(s.Lines, ""), "Auto mode is now") {
		t.Errorf("notices are noise: %q", s.Lines)
	}
}

func TestClaudeWorking(t *testing.T) {
	s := parseScreen(claudeWorking())
	if s.State != StateWorking || s.Elapsed != "1m 30s" || s.Meta.Tokens != "9.4k" {
		t.Fatalf("state %q elapsed %q tokens %q", s.State, s.Elapsed, s.Meta.Tokens)
	}
	if s.Meta.Doing != "Bash(sleep 14 && echo finished-sleeping)" || s.Meta.Mode != "auto mode on" {
		t.Errorf("meta %+v", s.Meta)
	}
	if len(s.Lines) != 0 || strings.Contains(strings.Join(s.Lines, ""), "Puttering") {
		t.Errorf("lines %q", s.Lines)
	}
}

func TestClaudeWorkingFromTheFooterAlone(t *testing.T) { // the new layout has no "(esc to interrupt)" in the status line
	s := parseScreen(strings.Join([]string{"❯ do it", "✻ 403 retrying in 5s · attempt 3/10", rule, "❯ ", rule, "  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt"}, "\n"))
	if s.State != StateWorking {
		t.Errorf("state %q", s.State)
	}
}

func TestClaudeDone(t *testing.T) {
	s := parseScreen(claudeDone())
	if s.State != StateDone || s.Meta.Mode != "accept edits on" {
		t.Fatalf("state %q meta %+v", s.State, s.Meta)
	}
	if len(s.Lines) != 3 || !strings.HasPrefix(s.Lines[0], "⏺ All 42 tests pass.") {
		t.Errorf("the reply, not the tool calls: %q", s.Lines)
	}
}

func TestClaudeWaiting(t *testing.T) {
	for name, raw := range map[string]string{"dialog": claudeDialog(), "permission": claudePermission()} {
		s := parseScreen(raw)
		if s.State != StateWaiting {
			t.Errorf("%s: state %q", name, s.State)
		}
		if !strings.Contains(strings.Join(s.Lines, "\n"), "1. Yes") {
			t.Errorf("%s: options missing: %q", name, s.Lines)
		}
	}
	if j := strings.Join(parseScreen(claudeDialog()).Lines, "\n"); strings.Contains(j, "Enter to confirm") {
		t.Errorf("hint line is chrome: %q", j)
	}
	if !strings.Contains(strings.Join(parseScreen(claudePermission()).Lines, "\n"), "\x1b[36m") {
		t.Error("colour of the selected option lost")
	}
}

func TestYesNoPromptIsWaiting(t *testing.T) {
	if s := parseScreen("Run npm test?\nAllow this action? [y/n/t]: "); s.State != StateWaiting {
		t.Errorf("state %q", s.State)
	}
}

func TestAShellIsIdle(t *testing.T) {
	if s := parseScreen("~/projects >"); s.State != StateIdle || len(s.Lines) != 1 {
		t.Errorf("state %q lines %q", s.State, s.Lines)
	}
}

func TestUserEchoWithCursorGlyphIsNotAMenu(t *testing.T) { // Claude echoes your message after "❯"
	s := parseScreen("❯ Use the Bash tool to run it\n  and then reply\n⏺ Done.\n" + rule + "\n❯ \n" + rule)
	if s.State == StateWaiting {
		t.Errorf("state %q", s.State)
	}
}

func TestElapsedParsing(t *testing.T) {
	for in, want := range map[string]string{"1m 30s": "1m 30s", "12s": "12s", "1h 2m 3s": "1h 2m 3s"} {
		if got := strings.TrimSpace(elapsedRe.FindString("x (" + in + " · y)")); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestLongReplyIsKeptForScrolling(t *testing.T) {
	var b strings.Builder
	b.WriteString("  › explain it\n")
	for i := 1; i <= 150; i++ {
		b.WriteString("• line " + strconv.Itoa(i) + " of a long explanation of how it works\n")
	}
	b.WriteString("▸ Credits: turn 0.20 • session 0.30 | Time: 1m 2s\n" + rule + "\nDefault · auto · ◔ 9%    /\n›  ask a question or describe a task ↵\n")
	s := parseScreen(b.String())
	if s.State != StateDone {
		t.Fatalf("state %q", s.State)
	}
	if len(s.Lines) != 150 || !strings.Contains(s.Lines[0], "line 1 ") || !strings.Contains(s.Lines[149], "line 150") {
		t.Errorf("the whole reply should be kept: %d lines, first %q", len(s.Lines), s.Lines[0])
	}
}

func TestVeryLongReplyIsCutAtItsStart(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 190; i++ {
		b.WriteString("⏺ " + strings.Repeat("x", 280) + " " + strconv.Itoa(i) + "\n")
	}
	b.WriteString(rule + "\n❯ \n" + rule + "\n  ⏵⏵ auto mode on (shift+tab to cycle)\n")
	s := parseScreen(b.String())
	total := 0
	for _, l := range s.Lines {
		total += len(l)
	}
	if total > replyByteBudget+len(s.Lines)*4 || len(s.Lines) >= 190 {
		t.Errorf("not trimmed to the budget: %d lines, %d bytes", len(s.Lines), total)
	}
	if !strings.HasSuffix(s.Lines[len(s.Lines)-1], " 190") {
		t.Errorf("the end of the reply, next to the prompt, must be what is kept")
	}
}

// Captures are shared for a moment, so a polling client costs one tmux process per interval, not one per request.
func TestCaptureIsCachedBriefly(t *testing.T) {
	captureMu.Lock()
	captureCache = map[string]capturedScreen{
		"cached|0": {at: time.Now(), screen: Screen{State: StateDone}},
		"stale|0":  {at: time.Now().Add(-time.Second), screen: Screen{State: StateDone}},
	}
	captureMu.Unlock()
	h := NewHub(nil)
	if s, err := h.CaptureScreenStyled("cached", 0); err != nil || s.State != StateDone {
		t.Errorf("a fresh result should be reused without touching tmux: %v %v", s, err)
	}
	if _, err := h.CaptureScreenStyled("stale", 0); err == nil {
		t.Error("a stale result must be captured again (there is no such session, so it errors)")
	}
	if _, err := h.CaptureScreenStyled("cached", 5000); err == nil {
		t.Error("another history length is another request (and is capped, not trusted)")
	}
}

func TestWorkingPhase(t *testing.T) {
	cases := []struct {
		name, screen, phase, doing string
	}{
		{"claude thinking", "⏺ Bash(ls)\n  ⎿  a b\n\n✶ Puttering… (1m 30s · ↓ 9.4k tokens · thinking)\n\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n",
			"thinking", "Bash(ls)"},
		{"claude running a tool", "⏺ Bash(npm test)\n\n✶ Puttering… (12s · ↓ 1.2k tokens)\n\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n",
			"tool", "Bash(npm test)"},
		{"claude writing", "⏺ Bash(ls)\n⏺ I found two files and will now edit the first\n\n✶ Puttering… (12s · ↓ 1.2k tokens)\n\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n",
			"writing", "I found two files and will now edit the first"},
		{"nothing yet", "✶ Puttering… (3s · ↓ 12 tokens)\n\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n", "thinking", ""},
	}
	// the last few things it did come with it, oldest first
	long := parseScreen("⏺ Read(a.go)\n⏺ Update(a.go)\n⏺ I will run the tests now\n⏺ Bash(go test)\n\n✶ Puttering… (12s · ↓ 1.2k tokens)\n\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt\n")
	if got := strings.Join(long.Meta.Recent, ","); got != "T|Read(a.go),T|Update(a.go),W|I will run the tests now,T|Bash(go test)" {
		t.Errorf("recent: %q", got)
	}
	for _, c := range cases {
		s := parseScreen(c.screen)
		if s.State != StateWorking || s.Meta.Phase != c.phase || s.Meta.Doing != c.doing {
			t.Errorf("%s: state %q phase %q doing %q", c.name, s.State, s.Meta.Phase, s.Meta.Doing)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", 90 * time.Second: "1m 30s", 3700 * time.Second: "1h 1m"} {
		if got := formatElapsed(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
