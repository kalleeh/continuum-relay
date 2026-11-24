package relay

import (
	"os"
	"strings"
	"testing"
)

// Real screens captured from Kiro CLI v3 and Claude Code in each state (testdata/*.txt, from `tmux capture-pane
// -e -p`). When an agent changes its layout these are what catch it: re-capture and look at what changed.
//
// Not covered by a real capture: Claude Code finished with a reply (the Bedrock login on the capture machine
// failed, so no reply was ever produced); screen_test.go has a hand-written one.
func TestRealScreens(t *testing.T) {
	cases := []struct {
		file    string
		state   string
		elapsed string
		check   func(t *testing.T, s Screen)
	}{
		{"kiro-idle.txt", StateWorking, "", func(t *testing.T, s Screen) { // captured while Kiro was still starting
			if s.Meta.Input != "" {
				t.Errorf("the startup placeholder is not typed input: %q", s.Meta.Input)
			}
		}},
		{"kiro-working.txt", StateWorking, "4s", func(t *testing.T, s Screen) {
			if s.Meta.Agent != "Default" || s.Meta.Mode != "auto" || s.Meta.Context != 3 || !strings.HasPrefix(s.Meta.Doing, "Shell sleep") {
				t.Errorf("meta %+v", s.Meta)
			}
			if len(s.Lines) != 0 {
				t.Errorf("a busy agent shows no text: %q", s.Lines)
			}
		}},
		{"kiro-waiting.txt", StateWaiting, "", func(t *testing.T, s Screen) {
			j := strings.Join(s.Lines, "\n")
			for _, want := range []string{"requires approval", "Allow once", "Deny once"} {
				if !strings.Contains(j, want) {
					t.Errorf("%q missing: %q", want, s.Lines)
				}
			}
			if strings.Contains(j, "to navigate") {
				t.Errorf("key hints are chrome: %q", s.Lines)
			}
		}},
		{"kiro-done.txt", StateDone, "", func(t *testing.T, s Screen) {
			if s.Meta.Credits != "0.14" || s.Meta.Took != "15s" || s.Meta.Agent != "Default" || s.Meta.Context != 3 {
				t.Errorf("meta %+v", s.Meta)
			}
			if len(s.Lines) == 0 || strings.Contains(strings.Join(s.Lines, ""), "Credits") || strings.Contains(strings.Join(s.Lines, ""), "Shell") {
				t.Errorf("just the reply: %q", s.Lines)
			}
		}},
		{"claude-idle.txt", StateIdle, "", func(t *testing.T, s Screen) {
			if s.Meta.Mode != "auto mode on" || s.Meta.Model != "Opus 4.8" || s.Meta.Input != "" {
				t.Errorf("meta %+v", s.Meta)
			}
		}},
		{"claude-working.txt", StateWorking, "", func(t *testing.T, s Screen) { // the retry line's "1s" is not an elapsed time
			if s.Meta.Mode != "auto mode on" {
				t.Errorf("meta %+v", s.Meta)
			}
		}},
	}
	for _, c := range cases {
		raw, err := os.ReadFile("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		s := parseScreen(string(raw))
		if s.State != c.state || s.Elapsed != c.elapsed {
			t.Errorf("%s: state %q elapsed %q, want %q %q", c.file, s.State, s.Elapsed, c.state, c.elapsed)
		}
		c.check(t, s)
	}
}
