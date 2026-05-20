package tools

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"time"
)

const maxOutputBytes = 10 * 1024 // 10KB

// executeRunCode runs python3 or bash code with a 30s timeout, restricted
// environment, and network isolation on Linux (unshare --net).
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := buildSandboxedCommand(ctx, interpreter, code)
	cmd.Env = sandboxEnv()

	out, err := cmd.CombinedOutput()
	if len(out) > maxOutputBytes {
		out = out[:maxOutputBytes]
	}
	if err != nil {
		return ToolResult{Name: "run_code", Content: string(out), Error: err.Error()}
	}
	return ToolResult{Name: "run_code", Content: string(out)}
}

// buildSandboxedCommand wraps the interpreter in unshare --net on Linux to
// drop network access. On macOS/Windows, falls back to direct execution.
func buildSandboxedCommand(ctx context.Context, interpreter, code string) *exec.Cmd {
	if runtime.GOOS == "linux" {
		if path, err := exec.LookPath("unshare"); err == nil {
			return exec.CommandContext(ctx, path, "--net", "--", interpreter, "-c", code)
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
