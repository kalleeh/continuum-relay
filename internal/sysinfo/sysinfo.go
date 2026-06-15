package sysinfo

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// Info holds server environment details for the iOS client.
type Info struct {
	OS               string   `json:"os"`               // "linux", "darwin"
	Arch             string   `json:"arch"`             // "amd64", "arm64"
	Hostname         string   `json:"hostname"`         // machine hostname
	User             string   `json:"user"`             // effective username
	Home             string   `json:"home"`             // user home directory
	Shell            string   `json:"shell"`            // login shell path
	ShellCommand     []string `json:"shellCommand"`     // full command to spawn a login shell
	TmuxPath         string   `json:"tmuxPath"`         // absolute path to tmux
	DefaultWorkDir   string   `json:"defaultWorkDir"`   // recommended working directory
	ProjectsDir      string   `json:"projectsDir"`      // recommended projects root
	SharedDir        string   `json:"sharedDir"`        // file-transfer shared dir (sibling of projects, never inside a repo)
}

// Detect probes the current system and returns an Info struct.
func Detect() Info {
	info := Info{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	info.Hostname, _ = os.Hostname()
	info.User = detectUser()
	info.Home = detectHome(info.User)
	info.Shell = detectShell(info.User)
	info.ShellCommand = detectShellCommand(info.User, info.Shell)
	info.TmuxPath = detectTmux()
	info.DefaultWorkDir = detectWorkDir(info.Home)
	info.ProjectsDir = detectProjectsDir(info.Home)
	info.SharedDir = detectSharedDir(info.Home)

	return info
}

// detectSharedDir returns the file-transfer shared directory and ensures it
// exists. It lives at $HOME/continuum-shared — deliberately a sibling of the
// projects tree, never inside a git working copy, so uploaded files can't
// pollute a repository's status or get accidentally committed. Mode 0700:
// single-user box, no reason for it to be group/other-readable.
func detectSharedDir(home string) string {
	dir := filepath.Join(home, "continuum-shared")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func detectUser() string {
	// Prefer the actual user even when running as root (sudo)
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u := os.Getenv("CONTINUUM_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		if u.Username == "root" {
			// On macOS, find the console (GUI) user
			if runtime.GOOS == "darwin" {
				if out, err := exec.Command("stat", "-f", "%Su", "/dev/console").Output(); err == nil {
					consoleUser := strings.TrimSpace(string(out))
					if consoleUser != "" && consoleUser != "root" {
						return consoleUser
					}
				}
			}
			// On Linux servers, the deploy creates an "ubuntu" user
			if _, err := user.Lookup("ubuntu"); err == nil {
				return "ubuntu"
			}
		}
		return u.Username
	}
	return "root"
}

func detectHome(username string) string {
	// Try the user's actual home
	if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "darwin" {
		return "/Users/" + username
	}
	return "/home/" + username
}

// detectShellCommand returns the command to spawn a login shell.
// On macOS, uses /usr/bin/login -f <user> to avoid provenance restrictions
// that block direct fork/exec of /bin/zsh from launchd-spawned processes.
// On Linux, uses the shell directly with -l for login mode.
func detectShellCommand(username, shell string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"/bin/zsh", "-l"}
	}
	return []string{shell, "-l"}
}

func detectShell(username string) string {
	// Try user's configured shell
	if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
		// On macOS/Linux, shell is in passwd but Go doesn't expose it directly.
		// Use dscl on macOS, getent on Linux.
		switch runtime.GOOS {
		case "darwin":
			out, err := exec.Command("dscl", ".", "-read", "/Users/"+username, "UserShell").Output()
			if err == nil {
				parts := strings.Fields(string(out))
				if len(parts) >= 2 {
					return parts[len(parts)-1]
				}
			}
		case "linux":
			out, err := exec.Command("getent", "passwd", username).Output()
			if err == nil {
				fields := strings.Split(strings.TrimSpace(string(out)), ":")
				if len(fields) >= 7 && fields[6] != "" {
					return fields[6]
				}
			}
		}
	}
	// Fallback
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/bash"
}

func detectTmux() string {
	// Find tmux in PATH and common locations
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	// Check common locations
	candidates := []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
		"/bin/tmux",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "tmux" // fallback to bare name, hope PATH resolves it
}

func detectWorkDir(home string) string {
	// Prefer ~/projects if it exists (created by deploy.sh on Linux)
	projects := filepath.Join(home, "projects")
	if fi, err := os.Stat(projects); err == nil && fi.IsDir() {
		return projects
	}
	// On macOS, check ~/Projects or ~/Developer
	if runtime.GOOS == "darwin" {
		for _, dir := range []string{"Projects", "Developer", "Code"} {
			p := filepath.Join(home, dir)
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				return p
			}
		}
	}
	return home
}

func detectProjectsDir(home string) string {
	candidates := []string{"projects", "Projects", "Developer", "Code", "repos", "src"}
	for _, dir := range candidates {
		p := filepath.Join(home, dir)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return filepath.Join(home, "projects")
}
