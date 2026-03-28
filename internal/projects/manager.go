package projects

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var slugRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

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
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone failed: %w\n%s", err, string(out))
		}
	} else {
		// Re-set URL with token, pull, then strip token
		setURL := exec.CommandContext(ctx, "git", "-C", dest, "remote", "set-url", "origin", authedURL)
		setURL.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := setURL.CombinedOutput(); err != nil {
			return fmt.Errorf("remote set-url failed: %w\n%s", err, string(out))
		}
		pull := exec.CommandContext(ctx, "git", "-C", dest, "pull", "--ff-only")
		pull.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
		if out, err := pull.CombinedOutput(); err != nil {
			return fmt.Errorf("git pull failed: %w\n%s", err, string(out))
		}
	}

	// Strip token from remote URL
	strip := exec.Command("git", "-C", dest, "remote", "set-url", "origin", cleanURL)
	_ = strip.Run()
	return nil
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

// RemoveProject deletes ~/projects/<name>.
func RemoveProject(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid project name: must match [a-zA-Z0-9_.-]+")
	}
	target := filepath.Join(ProjectsDir(), name)
	// Path traversal guard
	if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(ProjectsDir())) {
		return fmt.Errorf("path traversal rejected")
	}
	return os.RemoveAll(target)
}
