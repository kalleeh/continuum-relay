package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrUnsavedWork is returned by RemoveProject when the target has work that
// only exists on this host (uncommitted/untracked/stashed changes or unpushed
// commits) and force was not set. The relay maps this to a distinct wire error
// code so the client can offer a "delete anyway" retry rather than treating it
// like a hard failure.
var ErrUnsavedWork = errors.New("project has unsaved work")

var slugRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
var gitTokenRe = regexp.MustCompile(`https://[^@]+@`)

type ProjectRecord struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func ProjectsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/ubuntu"
	}
	return filepath.Join(home, "projects")
}

// SyncProject clones or pulls the repo. Token is used in the clone URL then stripped immediately.
func SyncProject(slug, token string) error {
	if !slugRe.MatchString(slug) {
		return fmt.Errorf("invalid slug: must match owner/repo")
	}
	parts := strings.SplitN(slug, "/", 2)
	repoName := parts[1]
	dest := filepath.Join(ProjectsDir(), repoName)
	authedURL := fmt.Sprintf("https://%s@github.com/%s.git", token, slug)
	cleanURL := fmt.Sprintf("https://github.com/%s.git", slug)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := os.Stat(dest); os.IsNotExist(err) {
		// Clone
		if err := os.MkdirAll(ProjectsDir(), 0755); err != nil {
			return fmt.Errorf("mkdir failed: %w", err)
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", authedURL, dest)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(dest) // clean up partial clone
			sanitized := gitTokenRe.ReplaceAll(out, []byte("https://"))
			return fmt.Errorf("git clone failed: %w\n%s", err, sanitized)
		}
	} else {
		// Re-set URL with token, pull, then strip token
		setURL := exec.CommandContext(ctx, "git", "-C", dest, "remote", "set-url", "origin", authedURL)
		setURL.Env = gitEnv()
		if out, err := setURL.CombinedOutput(); err != nil {
			sanitized := gitTokenRe.ReplaceAll(out, []byte("https://"))
			return fmt.Errorf("remote set-url failed: %w\n%s", err, sanitized)
		}
		pull := exec.CommandContext(ctx, "git", "-C", dest, "pull", "--ff-only")
		pull.Env = gitEnv()
		if out, err := pull.CombinedOutput(); err != nil {
			sanitized := gitTokenRe.ReplaceAll(out, []byte("https://"))
			return fmt.Errorf("git pull failed: %w\n%s", err, sanitized)
		}
	}

	// Strip token from remote URL
	strip := exec.Command("git", "-C", dest, "remote", "set-url", "origin", cleanURL)
	strip.Env = gitEnv()
	_ = strip.Run()
	return nil
}

// gitEnv builds a minimal environment for git subprocesses. It deliberately
// does NOT inherit the relay's full os.Environ(): the relay holds CONTINUUM_TOKEN
// and the Bedrock/Ollama/APNs secrets, and a cloned repo can run code in the
// relay's context via git hooks or a credential helper, which would exfiltrate
// them. Only PATH and HOME are passed through (HOME so git finds a global
// config / known CA bundle), plus prompt-suppression so a credential challenge
// fails fast instead of hanging.
func gitEnv() []string {
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"PATH=" + os.Getenv("PATH"),
	}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	return env
}

// ListProjects scans ~/projects/ for directories containing a .git subdirectory.
func ListProjects() ([]ProjectRecord, error) {
	dir := ProjectsDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []ProjectRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records []ProjectRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			records = append(records, ProjectRecord{Name: e.Name(), Path: path})
		}
	}
	if records == nil {
		records = []ProjectRecord{}
	}
	return records, nil
}

// projectPath validates a project name and returns the absolute path to
// ~/projects/<name>, guaranteeing the result is a direct child of the projects
// directory. It rejects "." and ".." (which nameRe's character class otherwise
// permits) and anything that resolves to the projects dir itself — without this
// RemoveProject(".") would os.RemoveAll the entire ~/projects tree.
func projectPath(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("invalid project name: must match [a-zA-Z0-9_.-]+")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid project name")
	}
	base := filepath.Clean(ProjectsDir())
	target := filepath.Clean(filepath.Join(base, name))
	// Must be a strict child of base: base + separator + something. This rejects
	// both the base dir itself and any path that escaped via traversal.
	if target == base || !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path traversal rejected")
	}
	return target, nil
}

// RemoveProject deletes ~/projects/<name>. Unless force is true it refuses to
// delete a clone with un-saved work (uncommitted/untracked/stashed changes or
// unpushed commits), so an accidental delete can't silently destroy work that
// only exists on this host. The server enforces this rather than trusting the
// client to have consulted ProjectStatus first (the two are separate calls, so
// state can change between them — but re-checking here closes the common case).
func RemoveProject(name string, force bool) error {
	target, err := projectPath(name)
	if err != nil {
		return err
	}
	if !force {
		if st, err := projectStatusAt(target); err == nil && !st.Clean {
			return ErrUnsavedWork
		}
	}
	return os.RemoveAll(target)
}
