package projects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// UnpushedBranch describes a local branch that has work not on its remote.
type UnpushedBranch struct {
	Branch      string `json:"branch"`
	Ahead       int    `json:"ahead"`       // commits ahead of upstream (0 when no upstream)
	HasUpstream bool   `json:"hasUpstream"` // false → branch was never pushed anywhere
}

// ProjectGitStatus is the git state of a project clone, used to decide whether
// deleting it would lose work. Clean is true only when there is nothing that
// would be lost: no uncommitted/untracked/stashed changes and no unpushed work.
type ProjectGitStatus struct {
	Name        string           `json:"name"`
	IsRepo      bool             `json:"isRepo"`
	Clean       bool             `json:"clean"`
	Uncommitted int              `json:"uncommitted"` // tracked files modified/staged/deleted
	Untracked   int              `json:"untracked"`   // new files git isn't tracking
	Stashes     int              `json:"stashes"`     // git stash entries
	Unpushed    []UnpushedBranch `json:"unpushed"`    // branches ahead of / without upstream
}

// ProjectStatus inspects ~/projects/<name> and reports its git state.
// Validates the name and guards against path traversal, mirroring RemoveProject.
func ProjectStatus(name string) (ProjectGitStatus, error) {
	target, err := projectPath(name)
	if err != nil {
		return ProjectGitStatus{}, err
	}
	st, err := projectStatusAt(target)
	st.Name = name
	return st, err
}

// projectStatusAt does the actual git inspection for a directory. Split out from
// ProjectStatus (which adds name validation) so it can be unit-tested directly.
func projectStatusAt(dir string) (ProjectGitStatus, error) {
	st := ProjectGitStatus{Unpushed: []UnpushedBranch{}}

	// Not a git repo → IsRepo false, Clean false (treat as unsafe/unknown so the
	// caller warns rather than blind-deleting something it can't reason about).
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return st, nil
	}
	st.IsRepo = true

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. Working-tree state via porcelain v1. Each line's first two chars are the
	//    XY status code; "??" means untracked, anything else is a tracked change.
	out, err := gitOut(ctx, dir, "status", "--porcelain")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "??") {
			st.Untracked++
		} else {
			st.Uncommitted++
		}
	}

	// 2. Stash entries (one per line).
	if so, err := gitOut(ctx, dir, "stash", "list"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(so), "\n") {
			if line != "" {
				st.Stashes++
			}
		}
	}

	// 3. Per-branch upstream tracking. For each local branch, %(upstream) is empty
	//    when it has no upstream; %(upstream:track) carries "[ahead N]" when ahead.
	bo, err := gitOut(ctx, dir, "for-each-ref",
		"--format=%(refname:short)%00%(upstream)%00%(upstream:track)", "refs/heads")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(strings.TrimSpace(bo), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) < 3 {
			continue
		}
		branch, upstream, track := parts[0], parts[1], parts[2]
		if upstream == "" {
			// Never pushed anywhere → its commits exist only here.
			st.Unpushed = append(st.Unpushed, UnpushedBranch{Branch: branch, Ahead: 0, HasUpstream: false})
			continue
		}
		if ahead := parseAhead(track); ahead > 0 {
			st.Unpushed = append(st.Unpushed, UnpushedBranch{Branch: branch, Ahead: ahead, HasUpstream: true})
		}
	}

	st.Clean = st.Uncommitted == 0 && st.Untracked == 0 && st.Stashes == 0 && len(st.Unpushed) == 0
	return st, nil
}

// parseAhead extracts N from an "[ahead N]" / "[ahead N, behind M]" track string.
func parseAhead(track string) int {
	const marker = "ahead "
	i := strings.Index(track, marker)
	if i < 0 {
		return 0
	}
	rest := track[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, _ := strconv.Atoi(rest[:j])
	return n
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Use the same minimal env as SyncProject: dir is a user-cloned repo, and
	// git can run repo-controlled code (hooks, credential helpers) that would
	// otherwise inherit the relay's secrets via os.Environ().
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	return string(out), err
}
