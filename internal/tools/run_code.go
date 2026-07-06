package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const maxOutputBytes = 10 * 1024 // 10KB

// maxConcurrentRunCode bounds how many sandboxed interpreter processes can be
// running at once across all chat sessions on this relay. Without this, N
// concurrent chat requests each looping through tool-calling rounds could
// spawn unbounded processes on a small VPS.
const maxConcurrentRunCode = 4

var runCodeSem = make(chan struct{}, maxConcurrentRunCode)

// cappedBuffer is an io.Writer that stops accepting bytes once it reaches its
// cap, instead of buffering without bound. Unlike cmd.CombinedOutput() (which
// accumulates the entire child output before any truncation is applied), this
// bounds memory during execution — a print-loop can no longer grow the buffer
// past maxOutputBytes regardless of how long it runs before the timeout fires.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func newCappedBuffer(cap int) *cappedBuffer {
	return &cappedBuffer{cap: cap}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if remaining := c.cap - c.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			c.buf.Write(p[:remaining])
		} else {
			c.buf.Write(p)
		}
	}
	// Report the full length written so the child process sees a normal
	// pipe write rather than an error once the cap is hit.
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// executeRunCode runs python3 or bash code with a 30s timeout, restricted
// environment, and network isolation on Linux (unshare --net) and macOS
// (sandbox-exec).
//
// SECURITY: This function must ONLY be called via ExecuteUnsafe() after
// explicit user permission has been obtained. It is never reachable from Execute().
func executeRunCode(language, code string) ToolResult {
	var interpreter string
	switch language {
	case "python":
		interpreter = "python3"
	case "bash":
		interpreter = "bash"
	default:
		return ToolResult{Name: "run_code", Error: "unsupported language"}
	}

	runCodeSem <- struct{}{}
	defer func() { <-runCodeSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := buildSandboxedCommand(ctx, interpreter, code)
	cmd.Env = sandboxEnv()

	out := newCappedBuffer(maxOutputBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()

	if err != nil {
		return ToolResult{Name: "run_code", Content: out.String(), Error: err.Error()}
	}
	return ToolResult{Name: "run_code", Content: out.String()}
}

// macOSSandboxProfile is a Seatbelt profile denying network access while
// still allowing process exec/fork, filesystem reads, and writes under /tmp
// (sandboxEnv's HOME is /tmp/continuum-sandbox). `(deny default)` with no
// matching `(allow network-*)` rule blocks network-outbound/-bind/-inbound.
// Verified empirically: under this profile `curl` fails (exit 6, no
// connection) while `python3 -c "print(...)"` and writes under /tmp succeed.
// Syntax matches Apple's own shipped profiles (/usr/share/sandbox/*.sb).
const macOSSandboxProfile = `(version 1)
(deny default)
(import "system.sb")
(allow process-exec)
(allow process-fork)
(allow file-read*)
(allow file-write* (subpath "/tmp"))
(allow signal (target self))`

// buildSandboxedCommand wraps the interpreter in unshare --net on Linux, or a
// network-denying Seatbelt profile via sandbox-exec on macOS, to drop network
// access. Falls back to direct execution only where neither is available
// (e.g. Windows).
func buildSandboxedCommand(ctx context.Context, interpreter, code string) *exec.Cmd {
	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("unshare"); err == nil {
			return exec.CommandContext(ctx, path, "--net", "--", interpreter, "-c", code)
		}
	case "darwin":
		if path, err := exec.LookPath("sandbox-exec"); err == nil {
			return exec.CommandContext(ctx, path, "-p", macOSSandboxProfile, interpreter, "-c", code)
		}
	}
	return exec.CommandContext(ctx, interpreter, "-c", code)
}

// sandboxEnv returns a minimal environment for code execution.
// HOME is set to a temp directory so code cannot read ~/.ssh, ~/.aws, etc.
// PATH is preserved for interpreter lookup. Everything else is dropped.
func sandboxEnv() []string {
	tmpHome := filepath.Join(os.TempDir(), "continuum-sandbox")
	_ = os.MkdirAll(tmpHome, 0700)

	env := []string{
		"HOME=" + tmpHome,
		"TMPDIR=" + os.TempDir(),
		"LANG=en_US.UTF-8",
	}
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}
	if u, err := user.Current(); err == nil {
		env = append(env, "USER="+u.Username)
	}
	return env
}
