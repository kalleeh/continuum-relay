package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Remote control of a session's TUI for clients without a terminal (the
// Continuum desk device): send one of a few named keys, and read back the last
// lines on screen so the user can see the menu they are answering.
//
// This grants nothing the terminal endpoint doesn't already: the same token
// opens a full PTY on :7681. Keys are restricted to a fixed set of named keys
// plus single characters a-z / 0-9 (for "[y/n]" style prompts), so nothing a
// client sends can be interpreted by tmux as anything but that one key.

// tmuxKeyNames maps the wire names accepted in send_key to tmux key names.
var tmuxKeyNames = map[string]string{
	"up":     "Up",
	"down":   "Down",
	"enter":  "Enter",
	"escape": "Escape",
}

const (
	maxScreenLines     = 30
	maxScreenLineRunes = 300
)

// tmuxCommand builds a tmux invocation that reaches the user's tmux server and
// uses a UTF-8 locale (see tmuxDiscoveryCommand for why both matter).
func tmuxCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	env := append(os.Environ(), "LC_ALL=C.UTF-8")
	if dir := tmuxSocketDir(); dir != "" {
		env = append(env, "TMUX_TMPDIR="+dir)
	}
	cmd.Env = env
	return cmd
}

// tmuxTargets returns the tmux targets a session name may live at, most
// specific first, mirroring how the iOS client creates sessions
// (TerminalAdapter): a project-backed agent is a window named <name> in session
// cx-<project>; a flat relay session is cx-<name>; a discovered session is
// addressed by its bare tmux name. "=" forces exact matches, so a name can
// never select a different session by prefix.
func tmuxTargets(name string, rec *SessionRecord) []string {
	if rec == nil {
		return []string{"=" + name + ":"}
	}
	var targets []string
	if rec.Project != "" && sessionNameRe.MatchString(rec.Project) {
		targets = append(targets, "=cx-"+rec.Project+":="+name)
	}
	return append(targets, "=cx-"+name+":")
}

func (h *Hub) tmuxTargetsFor(name string) []string {
	if s, ok := h.GetSession(name); ok {
		rec := s.GetRecord()
		return tmuxTargets(name, &rec)
	}
	return tmuxTargets(name, nil)
}

// runOnSession runs a tmux subcommand against the first target that exists.
// args must contain the placeholder "{target}" where the target goes.
func (h *Hub) runOnSession(name string, args ...string) ([]byte, error) {
	if !sessionNameRe.MatchString(name) {
		return nil, errors.New("invalid session name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, target := range h.tmuxTargetsFor(name) {
		resolved := make([]string, len(args))
		for i, a := range args {
			if a == "{target}" {
				a = target
			}
			resolved[i] = a
		}
		if out, err := tmuxCommand(ctx, resolved...).Output(); err == nil {
			return out, nil
		}
	}
	return nil, errors.New("session not found in tmux")
}

// SendKey presses one key in the session's active pane: a named key from
// tmuxKeyNames, or a single lowercase letter or digit typed literally.
func (h *Hub) SendKey(name, key string) error {
	if tmuxKey, ok := tmuxKeyNames[key]; ok {
		_, err := h.runOnSession(name, "send-keys", "-t", "{target}", tmuxKey)
		return err
	}
	if len(key) == 1 && (key[0] >= 'a' && key[0] <= 'z' || key[0] >= '0' && key[0] <= '9') {
		// -l: literal, so the character is never looked up as a tmux key name.
		_, err := h.runOnSession(name, "send-keys", "-l", "-t", "{target}", key)
		return err
	}
	return fmt.Errorf("unsupported key %q", key)
}

// CaptureScreen returns the last non-empty lines currently visible in the
// session's active pane, as plain text, oldest first.
func (h *Hub) CaptureScreen(name string, maxLines int) ([]string, error) {
	out, err := h.runOnSession(name, "capture-pane", "-p", "-t", "{target}")
	if err != nil {
		return nil, err
	}
	return lastScreenLines(string(out), maxLines), nil
}

func lastScreenLines(screen string, maxLines int) []string {
	if maxLines < 1 || maxLines > maxScreenLines {
		maxLines = maxScreenLines
	}
	lines := []string{}
	for _, line := range strings.Split(screen, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxScreenLineRunes {
			line = string(r[:maxScreenLineRunes])
		}
		lines = append(lines, line)
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}
