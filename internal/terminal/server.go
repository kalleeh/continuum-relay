// REQUIRED go.mod dependencies (DO NOT EDIT go.mod — list here for integration step):
// require (
//     github.com/creack/pty v1.1.21
// )

package terminal

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"nhooyr.io/websocket"
)

// Server is an HTTP+WebSocket terminal server compatible with the ttyd/xterm.js protocol.
// Each WebSocket connection gets its own PTY running the configured command.
type Server struct {
	addr     string       // e.g. "10.100.0.1:7681"
	token    string       // auth token (compared against Basic auth credential, username is "continuum")
	command  []string     // command to run in PTY, e.g. ["tmux", "new-session", "-A", "-s", "main"]
	Listener net.Listener // if set, used instead of net.Listen(addr)
	User     string       // if set, PTY processes run as this user (requires root)
}

// New creates a terminal server.
// addr: listen address, e.g. "10.100.0.1:7681"
// token: auth token for Basic auth (username is always "continuum")
// command: program to run in each PTY session
func New(addr, token string, command []string) *Server {
	return &Server{
		addr:    addr,
		token:   token,
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

	// Validate Basic auth before upgrading to WebSocket.
	if !s.checkAuth(r) {
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
	s.handleConn(r.Context(), conn)
}

// checkAuth validates the Authorization: Basic header.
// Expected credential: "continuum:<token>".
func (s *Server) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return false
	}
	expected := "continuum:" + s.token
	return subtle.ConstantTimeCompare([]byte(decoded), []byte(expected)) == 1
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
func (s *Server) handleConn(ctx context.Context, conn *websocket.Conn) {
	defer conn.CloseNow()

	// Spawn the command with a PTY at the default 80x24 size.
	// Set TERM so tmux/ncurses can find terminal capabilities; the relay
	// runs as a systemd service with a minimal environment where TERM is
	// typically unset, which causes tmux to fail with "terminal does not
	// support clear".
	cmd := exec.CommandContext(ctx, s.command[0], s.command[1:]...)
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
		ptmx.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait() //nolint:errcheck
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	slog.Info("terminal client disconnected", "cmd", s.command[0])
}
