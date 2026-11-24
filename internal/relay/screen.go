package relay

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Styled screen capture for the desk device. Same screen as capture_screen, but analysed:
//
//   - what the agent is doing: working, waiting for the user, done with a reply, or idle;
//   - the agent's own chrome (status bar, prompt box, footers) taken out of the text and returned as
//     structured data (agent, mode, model, context used, folder, branch, credits, time), so the device
//     can put each where it belongs instead of spending its few lines on it;
//   - colour codes kept in the text that remains.
//
// The markers are those of Kiro CLI (v3) and Claude Code, read from their real screens.

// csiRe matches the escape sequences capture-pane -e emits.
var csiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

func stripEscapes(s string) string { return csiRe.ReplaceAllString(s, "") }

// keepStyle removes escape sequences other than colours and attributes (SGR), which the device understands.
func keepStyle(s string) string {
	return csiRe.ReplaceAllStringFunc(s, func(seq string) string {
		if strings.HasSuffix(seq, "m") && !strings.Contains(seq, "?") {
			return seq
		}
		return ""
	})
}

// onlyFrame reports whether a line is nothing but box-drawing characters and spaces.
func onlyFrame(plain string) bool {
	for _, r := range plain {
		if !unicode.IsSpace(r) && !(r >= 0x2500 && r <= 0x259F) {
			return false
		}
	}
	return true
}

// withoutFrame drops box-drawing characters so a line inside a bordered box can be recognised.
func withoutFrame(plain string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x2500 && r <= 0x259F {
			return -1
		}
		return r
	}, plain)
}

const (
	StateWorking = "working"
	StateWaiting = "waiting"
	StateDone    = "done"
	StateIdle    = "idle"
)

// ScreenMeta is what the agent's chrome says. Empty fields are left out; Context is a percentage of
// the context window used, or -1 when the screen does not say.
type ScreenMeta struct {
	Agent   string `json:"agent,omitempty"`   // Kiro's agent profile, e.g. "Default"
	Mode    string `json:"mode,omitempty"`    // "auto", "auto mode on", "accept edits on", ...
	Model   string `json:"model,omitempty"`   // "Opus 4.8"
	Context int    `json:"context"`           // percent used; -1 unknown
	CWD     string `json:"cwd,omitempty"`     // working folder
	Branch  string `json:"branch,omitempty"`  // git branch
	Credits string `json:"credits,omitempty"` // credits the last turn used
	Took    string `json:"took,omitempty"`    // how long the last turn took
	Tokens  string `json:"tokens,omitempty"`  // "9.4k", while busy
	Doing   string `json:"doing,omitempty"`   // the last tool call ("Bash(npm test)") or a line of what it is writing
	Phase   string `json:"phase,omitempty"`   // while busy: "thinking", "tool" (running Doing) or "writing" (Doing is its text)
	Input   string `json:"input,omitempty"`   // what is typed in the input box
}

// Screen is the analysed screen.
type Screen struct {
	Lines   []string
	State   string
	Elapsed string // how long a busy agent has been at it, "1m 30s"
	Meta    ScreenMeta
}

var (
	elapsedRe      = regexp.MustCompile(`\b(?:\d+h\s*)?(?:\d+m\s*)?\d+s\b`)
	spinnerTimeRe  = regexp.MustCompile(`\(((?:\d+h\s*)?(?:\d+m\s*)?\d+s)\b`) // "(1m 30s · ↓ 9.4k tokens)", not "retrying in 5s"
	tokensRe       = regexp.MustCompile(`[↓↑]\s*([\d.]+k?)\s*tokens`)
	numberedRe     = regexp.MustCompile(`^❯\s*\d{1,2}[.)]\s+\S`)
	cursorRe       = regexp.MustCompile(`^❯\s+\S`)
	choiceRe       = regexp.MustCompile(`\[[yYnN](/[a-zA-Z])+\]|\([yYnN]/[a-zA-Z]\)`)
	kiroStatusRe   = regexp.MustCompile(`^(\S.*?) · (\S.*?) · \S?\s*(\d{1,3})%\s*(.*)$`)
	kiroWorkingRe  = regexp.MustCompile(`(?i)^›\s+kiro is working(?:\s*·\s*([0-9hms ]+?))?\s*(?:·|$)`)
	creditsRe      = regexp.MustCompile(`^▸\s*Credits:\s*turn\s*([\d.]+).*?Time:\s*(.+)$`)
	claudeFooterRe = regexp.MustCompile(`(?i)^[\s⏵⏸]*(.+?)\s*\(shift\+tab to cycle\)(.*)$`)
	claudeModelRe  = regexp.MustCompile(`\b(Opus|Sonnet|Haiku|Fable)\s+[\w.]+`)
	contextLeftRe  = regexp.MustCompile(`(?i)(\d{1,3})%\s*(?:context left|remaining)|context[^\d]{0,30}(\d{1,3})%\s*(?:left|remaining)`)
	toolCallRe     = regexp.MustCompile(`^[⏺●•]\s*[A-Z][A-Za-z_]*\(`)
	replyRe        = regexp.MustCompile(`^[⏺●•]\s*\S`)
	spinnerLineRe  = regexp.MustCompile(`^[✻✶✳✢✽·*●◐◓◑◒⠀-⣿ᗤ]\s+\S`)
)

// kiroPlaceholder is what Kiro's prompt shows when nothing is typed; the other placeholder says it is still starting up.
const (
	kiroPlaceholder = "ask a question or describe a task"
	kiroStarting    = "Initializing"
)

// menuFooters are the key hints under a menu or dialog; they are the surest sign that it wants an answer.
var menuFooters = []string{"esc to close", "to navigate", "enter to confirm", "to select"}

// otherHints are footers that say what keys exist, not what is happening.
var otherHints = []string{"? for shortcuts", "ctrl+t to", "ctrl+o to", "tab to toggle", "/help for", "ctrl+o to expand"}

func containsAny(s string, list []string) bool {
	low := strings.ToLower(s)
	for _, h := range list {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

type row struct {
	styled, plain, core string
	rule                bool
}

// lead is the line as drawn, without its indentation: unlike core, it keeps box-drawing and block
// characters, which Kiro uses for "╰" sub-lines and Claude Code for "▎" notices.
func (r row) lead() string { return strings.TrimSpace(r.plain) }

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// inputBox finds Claude Code's input box: a "❯" line between two rules, near the bottom. It returns the
// indexes of its first and last row (the rules excluded), or -1.
func inputBox(rows []row) (first, last int) {
	for i := len(rows) - 1; i >= 0; i-- {
		if !rows[i].rule {
			continue
		}
		// i is the closing rule (or a footer-less end); look for the opening rule above it
		for j := i - 1; j >= 0; j-- {
			if rows[j].rule {
				if j+1 < i && strings.HasPrefix(rows[j+1].core, "❯") && !numberedRe.MatchString(rows[j+1].core) {
					return j + 1, i - 1
				}
				break
			}
		}
	}
	return -1, -1
}

// parseScreen analyses raw `capture-pane -e -p` output.
func parseScreen(raw string) Screen {
	var s Screen
	s.Meta.Context = -1
	var rows []row
	for _, line := range strings.Split(raw, "\n") {
		plain := strings.TrimRight(stripEscapes(line), " \t\r")
		core := strings.TrimSpace(withoutFrame(plain))
		if strings.TrimSpace(plain) == "" {
			continue
		}
		rows = append(rows, row{styled: strings.TrimRight(keepStyle(line), " \t\r"), plain: plain, core: core, rule: onlyFrame(plain)})
	}

	boxFirst, boxLast := inputBox(rows)
	var all []row
	working, menuFooter, sawCredits, thinking := false, false, false, false
	creditsAt := -1
	for i, r := range rows {
		switch {
		case r.rule:
			continue
		case boxFirst >= 0 && i >= boxFirst && i <= boxLast: // Claude's input box: the typed text only
			text := strings.TrimSpace(strings.TrimPrefix(r.core, "❯"))
			if s.Meta.Input != "" {
				s.Meta.Input += " "
			}
			s.Meta.Input += text
			continue
		}
		if m := kiroStatusRe.FindStringSubmatch(r.core); m != nil && !strings.HasPrefix(r.core, "›") && !strings.HasPrefix(r.core, "•") {
			s.Meta.Agent, s.Meta.Mode, s.Meta.Context = strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), atoiOr(m[3], -1)
			rest := strings.TrimSpace(m[4])
			if k := strings.Index(rest, " · ("); k >= 0 && strings.HasSuffix(rest, ")") {
				s.Meta.Branch = rest[k+len(" · (") : len(rest)-1]
				rest = rest[:k]
			}
			s.Meta.CWD = rest
			continue
		}
		if m := kiroWorkingRe.FindStringSubmatch(r.plain); m != nil {
			working = true
			if e := strings.TrimSpace(m[1]); e != "" && s.Elapsed == "" {
				s.Elapsed = e
			}
			continue
		}
		if strings.HasPrefix(r.plain, "›") { // Kiro's prompt, at the left edge (a message sent earlier is indented)
			input := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(r.plain, "›")), "↵"))
			switch {
			case strings.HasPrefix(input, kiroStarting): // "Initializing · type to queue a message": not ready yet
				working = true
			case input != kiroPlaceholder:
				s.Meta.Input = input
			}
			continue
		}
		if m := creditsRe.FindStringSubmatch(r.core); m != nil {
			s.Meta.Credits, s.Meta.Took = m[1], strings.TrimSpace(m[2])
			sawCredits, creditsAt = true, len(all)
			continue
		}
		if strings.Contains(r.core, "(esc to cancel)") || strings.EqualFold(r.core, "esc to cancel") {
			working = true
			continue
		}
		if containsAny(r.core, menuFooters) {
			menuFooter = true
			continue
		}
		if m := claudeFooterRe.FindStringSubmatch(r.core); m != nil {
			s.Meta.Mode = strings.TrimSpace(m[1])
			if strings.Contains(strings.ToLower(m[2]), "to interrupt") {
				working = true
			}
			if c := contextLeftRe.FindStringSubmatch(m[2]); c != nil {
				left := atoiOr(c[1]+c[2], -1)
				if left >= 0 {
					s.Meta.Context = 100 - left
				}
			}
			continue
		}
		if containsAny(r.core, otherHints) {
			continue
		}
		if strings.HasPrefix(r.lead(), "▎") || strings.HasPrefix(r.lead(), "⚠") { // notices
			continue
		}
		if first := []rune(r.lead() + " ")[0]; first >= 0x2580 && first <= 0x259F { // the logo banner
			if m := claudeModelRe.FindString(r.core); m != "" && s.Meta.Model == "" {
				s.Meta.Model = m
			}
			continue
		}
		if strings.Contains(strings.ToLower(r.core), "esc to interrupt") { // Claude's older status line
			working = true
		}
		if spinnerLineRe.MatchString(r.core) && (strings.Contains(r.core, "…") || strings.Contains(r.core, "...") ||
			strings.Contains(r.core, "to interrupt") || elapsedRe.MatchString(r.core)) {
			working = true
			if m := spinnerTimeRe.FindStringSubmatch(r.core); m != nil && s.Elapsed == "" {
				s.Elapsed = strings.TrimSpace(m[1])
			}
			if t := tokensRe.FindStringSubmatch(r.core); t != nil {
				s.Meta.Tokens = t[1]
			}
			if strings.Contains(strings.ToLower(r.core), "thinking") || strings.Contains(strings.ToLower(r.core), "thought") {
				thinking = true
			}
			continue
		}
		if s.Meta.Model == "" && strings.Contains(r.core, "·") {
			if m := claudeModelRe.FindString(r.core); m != "" {
				s.Meta.Model = m
			}
		}
		all = append(all, r)
	}

	// what the agent is doing right now: the last tool call, or the text it is writing after it
	toolAt := -1
	for i := len(all) - 1; i >= 0; i-- {
		core := all[i].core
		kiroTool := (strings.HasPrefix(core, "• ") || strings.HasPrefix(core, "↓ ")) && i+1 < len(all) && strings.HasPrefix(all[i+1].lead(), "╰")
		if toolCallRe.MatchString(core) || kiroTool {
			s.Meta.Doing = truncate(strings.TrimSpace(strings.TrimLeft(core, "⏺●•↓ ")), 60)
			toolAt = i
			break
		}
	}
	prose := ""
	for i := len(all) - 1; i > toolAt && i >= 0; i-- { // the agent's own words after its last tool call
		core := all[i].core
		if (strings.HasPrefix(core, "⏺ ") || strings.HasPrefix(core, "● ")) && !toolCallRe.MatchString(core) {
			prose = truncate(strings.TrimSpace(core[len("⏺ "):]), 60)
			break
		}
	}
	switch {
	case thinking:
		s.Meta.Phase = "thinking"
	case prose != "":
		s.Meta.Phase, s.Meta.Doing = "writing", prose
	case toolAt >= 0:
		s.Meta.Phase = "tool"
	default:
		s.Meta.Phase = "thinking"
	}

	waiting := false
	for i := len(all) - 1; i >= 0 && i >= len(all)-14; i-- {
		core := all[i].core
		if numberedRe.MatchString(core) || (menuFooter && cursorRe.MatchString(core)) || (i >= len(all)-6 && choiceRe.MatchString(core)) {
			waiting = true
			break
		}
	}

	var shown []row
	switch {
	case waiting:
		s.State = StateWaiting
		shown = menuBlock(all)
	case working:
		s.State = StateWorking
	default:
		shown, s.State = reply(all, sawCredits, creditsAt)
		if s.Meta.Input != "" && (s.State == StateDone || s.State == StateIdle) {
			shown = append(append([]row{}, shown...), row{styled: "› " + s.Meta.Input, plain: "› " + s.Meta.Input})
		}
	}
	s.Lines = []string{}
	for _, r := range shown {
		line := r.styled
		if u := []rune(r.plain); len(u) > 300 {
			line = string(u[:300]) // the colours of a very long line are not worth the bytes
		}
		s.Lines = append(s.Lines, line)
	}
	// a very long reply is cut at its start, so what is nearest the prompt is kept
	total := 0
	for i := len(s.Lines) - 1; i >= 0; i-- {
		total += len(s.Lines[i]) + 4
		if total > replyByteBudget {
			s.Lines = s.Lines[i+1:]
			break
		}
	}
	return s
}

const (
	waitingBlockMax = 9
	replyBlockMax   = 200       // lines of a reply the device can scroll through
	replyByteBudget = 40 * 1024 // and the most it is sent: the device's receive buffer is 64 KB
	idleTail        = 9
	maxHistory      = 1000 // scrollback lines a client may ask to look through
)

// menuBlock picks the question and its options: from a few lines above the first option cursor to the
// end of the options.
func menuBlock(all []row) []row {
	cursor := -1
	for i := len(all) - 1; i >= 0 && i >= len(all)-14; i-- {
		if strings.HasPrefix(all[i].core, "❯") || choiceRe.MatchString(all[i].core) {
			cursor = i
		}
	}
	if cursor < 0 {
		cursor = len(all) - 1
	}
	start := cursor - 3
	if start < 0 {
		start = 0
	}
	end := cursor + 1
	for end < len(all) && end-start < waitingBlockMax {
		end++
	}
	if end-start > waitingBlockMax {
		end = start + waitingBlockMax
	}
	return all[start:end]
}

// reply picks the agent's last reply: Kiro prints a "Credits" line when a turn ends and its reply
// starts with a bullet; Claude Code starts a reply with ⏺. Tool calls are not the reply.
func reply(all []row, sawCredits bool, creditsAt int) ([]row, string) {
	tail := func() []row {
		from := len(all) - idleTail
		if from < 0 {
			from = 0
		}
		return all[from:]
	}
	if sawCredits {
		var block []row
		for i := creditsAt - 1; i >= 0 && len(block) < replyBlockMax; i-- {
			core := all[i].core
			if strings.HasPrefix(all[i].lead(), "╰") || strings.HasPrefix(core, "› ") || toolCallRe.MatchString(core) {
				break
			}
			if (strings.HasPrefix(core, "• ") || strings.HasPrefix(core, "↓ ")) && i+1 < len(all) && strings.HasPrefix(all[i+1].lead(), "╰") {
				break // a tool call
			}
			block = append([]row{all[i]}, block...)
		}
		for k, r := range block { // start at the reply's own bullet, not at a continued message above it
			if strings.HasPrefix(r.core, "• ") {
				return block[k:], StateDone
			}
		}
		if len(block) > 0 {
			return block, StateDone
		}
		return tail(), StateDone
	}
	for i := len(all) - 1; i >= 0; i-- {
		if replyRe.MatchString(all[i].core) && !toolCallRe.MatchString(all[i].core) {
			from := i
			if len(all)-from > replyBlockMax {
				from = len(all) - replyBlockMax
			}
			return all[from:], StateDone
		}
	}
	return tail(), StateIdle
}

// captureTTL is how long a capture is reused: the desk device asks twice a second while its terminal view is open,
// and a capture costs a tmux process, so identical requests close together share one.
const captureTTL = 300 * time.Millisecond

type capturedScreen struct {
	at     time.Time
	screen Screen
}

var (
	captureMu    sync.Mutex
	captureCache = map[string]capturedScreen{}
)

// CaptureScreenStyled is CaptureScreen, analysed. history > 0 also looks that many lines back into the
// terminal's scrollback, so a reply that has scrolled off the screen can still be read in full. A result is
// reused for captureTTL, however many clients (or polls) ask.
var busySince = map[string]time.Time{} // when each session was first seen busy; guarded by captureMu

func formatElapsed(d time.Duration) string {
	sec := int(d.Seconds())
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh %dm", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%ds", sec)
}

func (h *Hub) CaptureScreenStyled(name string, history int) (Screen, error) {
	if history > maxHistory {
		history = maxHistory
	}
	key := name + "|" + strconv.Itoa(history)
	captureMu.Lock()
	if c, ok := captureCache[key]; ok && time.Since(c.at) < captureTTL {
		captureMu.Unlock()
		return c.screen, nil
	}
	captureMu.Unlock()

	args := []string{"capture-pane", "-e", "-p"}
	if history > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(history))
	}
	out, err := h.runOnSession(name, append(args, "-t", "{target}")...)
	if err != nil {
		return Screen{}, err
	}
	screen := parseScreen(string(out))

	captureMu.Lock()
	now := time.Now()
	// how long it has been busy, for when the agent's own counter is not on screen (between its updates)
	if screen.State == StateWorking {
		since, ok := busySince[name]
		if !ok {
			since = now
			busySince[name] = since
		}
		if screen.Elapsed == "" {
			screen.Elapsed = formatElapsed(now.Sub(since))
		}
	} else {
		delete(busySince, name)
	}
	for k, c := range captureCache { // forget what is old, so the cache cannot grow
		if now.Sub(c.at) > 5*time.Second {
			delete(captureCache, k)
		}
	}
	captureCache[key] = capturedScreen{at: now, screen: screen}
	captureMu.Unlock()
	return screen, nil
}
