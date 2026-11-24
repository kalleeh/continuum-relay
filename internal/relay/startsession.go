package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Starting and ending sessions for clients without a terminal (the Continuum desk device).
//
// The phone app starts a session by typing `tmux new-session ...` into a terminal. A device has no
// terminal, so the relay does the same thing itself, but only from a fixed menu: the agent is one
// of the names below (never a command string from the client) and the working directory is a
// project the relay already knows. Like send_key, this grants nothing the terminal endpoint does not.

// sessionAgents maps the wire name of an agent to its session type as the phone app spells it.
var sessionAgents = map[string]string{
	"claude":   "claudeCode",
	"kiro":     "kiro",
	"terminal": "terminal",
}

// launchCommand is what gets typed into the new session's shell, or "" for a plain terminal.
// skipPermissions mirrors the phone app's option: the agent then runs tools without asking.
func launchCommand(agent string, skipPermissions bool) string {
	switch agent {
	case "claude":
		if skipPermissions {
			return "claude --dangerously-skip-permissions"
		}
		return "claude"
	case "kiro":
		// kiro-cli installs into ~/.local/bin, which a fresh tmux shell does not always have on its path.
		cmd := `export PATH="$HOME/.local/bin:$PATH"; kiro-cli chat`
		if skipPermissions {
			cmd += " --trust-all-tools"
		}
		return cmd
	}
	return ""
}

// StartSession creates the tmux session cx-<name> (in cwd, if given), starts the agent in it and
// records it. It refuses a name that is already taken, in the relay's records or in tmux.
func (h *Hub) StartSession(name, cwd, agent string, skipPermissions bool) error {
	if !sessionNameRe.MatchString(name) {
		return errors.New("invalid session name")
	}
	sessionType, ok := sessionAgents[agent]
	if !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if err := validateWorkingDir(cwd); err != nil {
		return fmt.Errorf("invalid cwd: %w", err)
	}
	if _, exists := h.GetSession(name); exists {
		return errors.New("a session with that name already exists")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := "=cx-" + name
	if tmuxCommand(ctx, "has-session", "-t", target).Run() == nil {
		return errors.New("a session with that name already exists")
	}
	args := []string{"new-session", "-d", "-s", "cx-" + name}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	if out, err := tmuxCommand(ctx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux: %s", strings.TrimSpace(string(out)))
	}
	if launch := launchCommand(agent, skipPermissions); launch != "" {
		// -l: typed literally; Enter is a separate key.
		if out, err := tmuxCommand(ctx, "send-keys", "-l", "-t", target+":", "--", launch).CombinedOutput(); err != nil {
			_ = tmuxCommand(ctx, "kill-session", "-t", target).Run()
			return fmt.Errorf("tmux: %s", strings.TrimSpace(string(out)))
		}
		_ = tmuxCommand(ctx, "send-keys", "-t", target+":", "Enter").Run()
	}
	_, err := h.CreateSession(name, cwd, sessionType)
	return err
}

// EndSession kills a session the relay manages (tmux session cx-<name>, or the window of a
// project-backed one) and forgets it. A discovered session that is not Continuum's own is refused.
func (h *Hub) EndSession(name string) error {
	if !sessionNameRe.MatchString(name) {
		return errors.New("invalid session name")
	}
	logical := strings.TrimPrefix(name, "cx-")
	if logical == "" {
		return errors.New("invalid session name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var killArgs []string
	if s, ok := h.GetSession(logical); ok {
		rec := s.GetRecord()
		if rec.Project != "" && sessionNameRe.MatchString(rec.Project) {
			killArgs = []string{"kill-window", "-t", "=cx-" + rec.Project + ":=" + logical}
		}
	} else if !strings.HasPrefix(name, "cx-") {
		return errors.New("only sessions started by Continuum can be ended")
	}
	if killArgs == nil {
		killArgs = []string{"kill-session", "-t", "=cx-" + logical}
	}
	out, err := tmuxCommand(ctx, killArgs...).CombinedOutput()
	h.DeleteSession(logical)
	gone := strings.Contains(string(out), "can't find") || strings.Contains(string(out), "no server running")
	if err != nil && !gone {
		return fmt.Errorf("tmux: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
