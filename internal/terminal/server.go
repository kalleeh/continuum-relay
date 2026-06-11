// REQUIRED go.mod dependencies (DO NOT EDIT go.mod — list here for integration step):
// require (
//     github.com/creack/pty v1.1.21
// )

package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/continuum-app/continuum-relay/internal/auth"
	"github.com/creack/pty"
	"nhooyr.io/websocket"
)

// Server is an HTTP+WebSocket terminal server compatible with the ttyd/xterm.js protocol.
// Each WebSocket connection gets its own PTY running the configured command.
type Server struct {
	addr     string              // e.g. "10.100.0.1:7681"
	auth     *auth.Authenticator // shared with the relay API: Basic-auth check + per-IP lockout, and sees token rotations
	command  []string            // command to run in PTY, e.g. ["tmux", "new-session", "-A", "-s", "main"]
	Listener net.Listener        // if set, used instead of net.Listen(addr)
	User     string              // if set, PTY processes run as this user (requires root)
}

// New creates a terminal server.
// addr: listen address, e.g. "10.100.0.1:7681"
// authenticator: shared with the relay API so the terminal validates against the
//
//	live token (rotations via rotate_token take effect immediately) and shares
//	the same per-IP brute-force lockout. The Basic-auth username is "continuum".
//
// command: program to run in each PTY session
func New(addr string, authenticator *auth.Authenticator, command []string) *Server {
	return &Server{
		addr:    addr,
		auth:    authenticator,
		command: command,
	}
}

// Run starts the HTTP server and blocks until the context is cancelled or it fails.
func (s *Server) Run(ctx context.Context) error {
	var ln net.Listener
	var err error
	if s.Listener != nil {
		ln = s.Listener
	} else {
		ln, err = net.Listen("tcp", s.addr)
		if err != nil {
			return err
		}
	}
	slog.Info("terminal server listening", "addr", s.addr)
	srv := &http.Server{
		Handler:     s,
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// ServeHTTP implements http.Handler. Serves only /ws; all other paths return 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ws" {
		http.NotFound(w, r)
		return
	}

	// Validate Basic auth before upgrading to WebSocket. Shared authenticator
	// enforces the same per-IP lockout as the relay API and reflects token
	// rotations immediately.
	if !s.auth.ValidateBasic(r, "continuum") {
		slog.Warn("terminal auth failed", "ip", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Basic realm="continuum"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Accept WebSocket with the tty subprotocol that the iOS client sends.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"tty"},
	})
	if err != nil {
		slog.Error("websocket accept failed", "err", err)
		return
	}
	conn.SetReadLimit(1 << 20) // 1MB per message

	slog.Info("terminal client connected", "ip", r.RemoteAddr)
	s.handleConn(r.Context(), conn, r.RemoteAddr)
}

// handleConn spawns a PTY process and bridges it to the WebSocket connection.
//
// Wire protocol:
//
//	Server → Client:
//	  [0x30] + raw PTY output bytes  (terminal data)
//	  [0x31] + UTF-8 title string    (window title; not emitted by this implementation)
//
//	Client → Server:
//	  [0x30] + keystroke bytes       (stdin to PTY)
//	  [0x31] + JSON {"columns":N,"rows":N}  (resize PTY)
func (s *Server) handleConn(ctx context.Context, conn *websocket.Conn, remoteAddr string) {
	defer conn.CloseNow()

	// Spawn the command with a PTY at the default 80x24 size.
	// Set TERM so tmux/ncurses can find terminal capabilities; the relay
	// runs as a systemd service with a minimal environment where TERM is
	// typically unset, which causes tmux to fail with "terminal does not
	// support clear".
	// Use exec.Command (not exec.CommandContext) so the PTY process is NOT killed
	// when the WebSocket context is cancelled. This is the key to session persistence:
	// when the client disconnects, closing the PTY master sends SIGHUP to the
	// foreground process (tmux), which causes tmux to detach — preserving the
	// session — rather than dying. The next WebSocket connection will re-attach.
	//
	// On Linux, wrap the spawn in `systemd-run --user --scope` so the PTY (and
	// tmux + every child) lands in the user manager's cgroup rather than this
	// service's. Without this, an `unattended-upgrades` → needrestart restart
	// of continuum-relay.service tears down the entire cgroup and kills every
	// active session. Placing PTYs in `user@<uid>.service` insulates them from
	// relay restarts entirely.
	//
	// The wrapper also clears AmbientCapabilities and CapabilityBoundingSet, so
	// children no longer inherit CAP_NET_ADMIN from the unit's
	// AmbientCapabilities= setting (which is needed by the relay's wireguard-go
	// TUN setup but has no business being in tmux/claude/MCPs).
	//
	// Disable with CONTINUUM_NO_USER_SCOPE=1 (e.g. for dev environments where
	// the user manager isn't running). On macOS the relay is a LaunchDaemon
	// and PTY children are already isolated via setsid/launchd's own model.
	cmd, userScoped := buildPTYCommand(s.command)
	env := os.Environ()
	hasTerm := false
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}

	// Mark the PTY as launched by Continuum so user rc files can opt out of
	// behavior that conflicts with the relay (e.g. auto-attaching to a personal
	// tmux session, which would intercept the iOS adapter's `tmux new-session`
	// keystrokes). TERM_PROGRAM is the standard convention used by VS Code,
	// iTerm2, Warp, etc.; CONTINUUM_RELAY is an unambiguous secondary marker
	// that survives even if a user spawns another terminal program inside the
	// session (which would clobber TERM_PROGRAM).
	env = append(env, "TERM_PROGRAM=Continuum", "CONTINUUM_RELAY=1")

	// Strip the relay's own secrets before handing the environment to the PTY.
	// The relay inherits CONTINUUM_TOKEN, the Bedrock/Ollama API keys, and the
	// APNs signing material from its systemd EnvironmentFiles; without this any
	// session could `env | grep TOKEN` and read the relay's master credential
	// (which grants peer management and token rotation — a full compromise that
	// survives rotation since the holder can rotate the token themselves). The
	// in-session `continuum-relay` CLI does not depend on these being present:
	// loadToken() falls back to reading /etc/continuum/env (mode 0600).
	env = stripSecrets(env)

	// Drop privileges to the configured user for PTY sessions.
	// The relay runs as root or with CAP_NET_ADMIN as a service user (e.g. ubuntu);
	// either way, sessions should run as the configured user. Always set HOME/Dir
	// so the shell picks up the right rc files. Only attach a setuid Credential
	// when we actually need to switch user — assigning Credential triggers
	// setgroups() in the child, which requires CAP_SETGID even when the target
	// uid matches the current uid. That would fail with EPERM under a hardened
	// systemd unit that grants only CAP_NET_ADMIN.
	if s.User != "" {
		if u, err := user.Lookup(s.User); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			if uid > 0 {
				cmd.Dir = u.HomeDir
				// Override HOME in env so the shell finds the right config
				for i, e := range env {
					if strings.HasPrefix(e, "HOME=") {
						env[i] = "HOME=" + u.HomeDir
						break
					}
				}
				if uid != os.Getuid() {
					cmd.SysProcAttr = &syscall.SysProcAttr{
						Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
					}
				}
				// `systemd-run --user` needs to reach the user manager's bus
				// socket. The relay's systemd unit has PrivateTmp=yes which
				// hides /run/user/<uid> from a default $TMPDIR view, but
				// XDG_RUNTIME_DIR points to the real path on the host and
				// systemd resolves it directly. Set it explicitly because
				// the relay's environment may not have it. Only do this when the
				// user-scope wrapper is actually used — on the plain-exec path
				// (macOS, CONTINUUM_NO_USER_SCOPE, no systemd-run) this dir may
				// not exist and would mislead XDG-aware tools in the session.
				if userScoped && !envHas(env, "XDG_RUNTIME_DIR") {
					env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid))
				}
			}
		}
	}

	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		slog.Error("pty start failed", "err", err, "cmd", s.command)
		conn.Close(websocket.StatusInternalError, "pty failed")
		return
	}
	defer func() {
		// Close the PTY master — this sends SIGHUP to the foreground process (tmux),
		// which causes tmux to detach cleanly and preserves the session for re-attach.
		// Do NOT call cmd.Process.Kill() here: that would destroy the tmux session.
		ptmx.Close()
		cmd.Wait() //nolint:errcheck
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// WebSocket ping/pong heartbeat — surfaces half-open sockets within ~30s.
	// Mirrors the relay subsystem (internal/relay/client.go). Without this,
	// an iOS client suspended in the background can hold a dead socket open
	// indefinitely, leaving the user staring at a frozen terminal until they
	// back out and re-enter the session.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// PTY output → WebSocket.
	// Frames are prefixed with 0x30 to match the xterm.js/ttyd binary protocol.
	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				msg := make([]byte, n+1)
				msg[0] = 0x30
				copy(msg[1:], buf[:n])
				if writeErr := conn.Write(ctx, websocket.MessageBinary, msg); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// WebSocket input → PTY stdin (main loop).
	connectedAt := time.Now()
	stdinLogged := false // set once we've seen a non-empty stdin line (the tmux command)
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case 0x30: // stdin data
			if len(msg) > 1 {
				// Log stdin messages until we capture the first real command (ends in \r).
				// This lets us see exactly what the iOS app types into the shell on connect.
				if !stdinLogged {
					preview := strings.TrimRight(string(msg[1:]), "\r\n")
					if len(preview) > 0 {
						stdinLogged = true
						if len(preview) > 300 {
							preview = preview[:300] + "…"
						}
						slog.Debug("terminal stdin cmd", "ip", remoteAddr, "data", preview)
					}
				}
				ptmx.Write(msg[1:]) //nolint:errcheck
			}
		case 0x31: // resize request
			var sz struct {
				Columns uint16 `json:"columns"`
				Rows    uint16 `json:"rows"`
			}
			if json.Unmarshal(msg[1:], &sz) == nil && sz.Columns > 0 && sz.Rows > 0 {
				// Clamp to a sane upper bound. Real terminals don't go past
				// a few hundred columns; absurd values waste memory in
				// downstream consumers (tmux, ncurses) and could be sent
				// by a buggy or hostile client.
				if sz.Columns > 9999 {
					sz.Columns = 9999
				}
				if sz.Rows > 9999 {
					sz.Rows = 9999
				}
				pty.Setsize(ptmx, &pty.Winsize{Cols: sz.Columns, Rows: sz.Rows}) //nolint:errcheck
			}
		}
	}

	slog.Info("terminal client disconnected", "cmd", s.command[0], "duration", time.Since(connectedAt).Round(time.Millisecond))
}

// buildPTYCommand returns the exec.Cmd that starts the user's shell for a
// PTY session. On Linux it wraps the command in `systemd-run --user --scope`
// so the spawned tmux + children land in user@<uid>.service's cgroup, not
// continuum-relay.service's; that lets sessions survive `systemctl restart
// continuum-relay` (e.g. needrestart-driven library upgrades). The wrapper
// also clears AmbientCapabilities/CapabilityBoundingSet so children do not
// inherit CAP_NET_ADMIN from the relay unit.
//
// Falls back to plain exec.Command when:
//   - GOOS != linux (macOS LaunchDaemon path: launchd already isolates per-job)
//   - CONTINUUM_NO_USER_SCOPE=1 (escape hatch for dev/test)
//   - systemd-run is not on PATH
//
// If `--user --scope` itself fails at exec time (no user manager running, no
// linger configured), the PTY simply fails to start and the WebSocket gets
// a 500. Operators can either enable linger (`loginctl enable-linger ubuntu`)
// or set CONTINUUM_NO_USER_SCOPE=1.
// buildPTYCommand returns the command and a bool reporting whether the
// `systemd-run --user --scope` wrapper was applied. The bool gates the
// XDG_RUNTIME_DIR injection in handleConn — that variable is only meaningful
// when the wrapper is used, and setting it on the plain-exec path (macOS,
// CONTINUUM_NO_USER_SCOPE, or no systemd-run) points XDG-aware tools at a path
// that may not exist.
func buildPTYCommand(command []string) (*exec.Cmd, bool) {
	if runtime.GOOS != "linux" || os.Getenv("CONTINUUM_NO_USER_SCOPE") == "1" {
		return exec.Command(command[0], command[1:]...), false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		// Falling back to plain exec means PTYs land in the relay's own cgroup
		// and will NOT survive a relay restart. Warn loudly — this silently
		// defeats the session-survival design otherwise.
		slog.Warn("systemd-run not found on PATH; PTY sessions will run in the relay's cgroup and will NOT survive a relay restart")
		return exec.Command(command[0], command[1:]...), false
	}
	// NOTE: do NOT pass exec-context properties (NoNewPrivileges,
	// AmbientCapabilities, CapabilityBoundingSet) here. A `--scope` unit adopts
	// an already-forked process and has no exec context, so systemd rejects
	// these with "Unknown assignment: …" and systemd-run exits non-zero —
	// killing every PTY at spawn (~6ms). They are service-only properties.
	// The PTY does not inherit the relay's CAP_NET_ADMIN regardless: ambient
	// caps are not passed across the systemd-run/dbus spawn, and the relay's
	// own NoNewPrivileges=true already bounds what the scope can escalate to.
	args := []string{
		"--user",
		"--scope",
		"--quiet",
		"--collect", // garbage-collect the transient scope when it exits
		"--",
	}
	args = append(args, command...)
	return exec.Command("systemd-run", args...), true
}

// secretEnvPrefixes are the environment-variable name prefixes the relay must
// not leak into PTY sessions: its own auth token, the Bedrock/Ollama API keys,
// and the APNs signing credentials. Matched by prefix so both exact names
// (CONTINUUM_TOKEN) and families (AWS_*, BEDROCK_*, APNS_*) are covered.
var secretEnvPrefixes = []string{
	"CONTINUUM_TOKEN=",
	"OLLAMA_API_KEY=",
	"BEDROCK_",
	"AWS_",
	"APNS_",
}

// stripSecrets returns env with any entry whose name matches a secret prefix
// removed. The input slice is not modified.
func stripSecrets(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		secret := false
		for _, p := range secretEnvPrefixes {
			if strings.HasPrefix(e, p) {
				secret = true
				break
			}
		}
		if !secret {
			out = append(out, e)
		}
	}
	return out
}

// envHas reports whether env contains a "KEY=" entry (any value).
func envHas(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
