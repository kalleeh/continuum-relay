package projects

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Deterministic identity + no global config interference.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepoWithUpstream creates a bare "remote" and a working clone of it with one
// pushed commit on the default branch, returning the working-clone path.
func newRepoWithUpstream(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	git(t, root, "init", "--bare", "-b", "main", remote)

	work := filepath.Join(root, "work")
	git(t, root, "clone", remote, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "README.md")
	git(t, work, "commit", "-m", "initial")
	git(t, work, "push", "-u", "origin", "main")
	return work
}

func TestProjectStatus_Clean(t *testing.T) {
	work := newRepoWithUpstream(t)
	st, err := projectStatusAt(work)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.IsRepo {
		t.Fatal("expected IsRepo true")
	}
	if !st.Clean {
		t.Fatalf("expected clean, got %+v", st)
	}
	if st.Uncommitted != 0 || st.Untracked != 0 || st.Stashes != 0 || len(st.Unpushed) != 0 {
		t.Fatalf("expected all-zero, got %+v", st)
	}
}

func TestProjectStatus_Uncommitted(t *testing.T) {
	work := newRepoWithUpstream(t)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, _ := projectStatusAt(work)
	if st.Clean || st.Uncommitted == 0 {
		t.Fatalf("expected uncommitted>0, got %+v", st)
	}
}

func TestProjectStatus_Untracked(t *testing.T) {
	work := newRepoWithUpstream(t)
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	st, _ := projectStatusAt(work)
	if st.Clean || st.Untracked == 0 {
		t.Fatalf("expected untracked>0, got %+v", st)
	}
}

func TestProjectStatus_UnpushedCommits(t *testing.T) {
	work := newRepoWithUpstream(t)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("y\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "f.txt")
	git(t, work, "commit", "-m", "local only")
	st, _ := projectStatusAt(work)
	if st.Clean {
		t.Fatalf("expected unsafe (unpushed), got clean: %+v", st)
	}
	if len(st.Unpushed) == 0 {
		t.Fatalf("expected an unpushed branch, got %+v", st)
	}
	var mainAhead int
	for _, b := range st.Unpushed {
		if b.Branch == "main" {
			mainAhead = b.Ahead
		}
	}
	if mainAhead != 1 {
		t.Fatalf("expected main ahead 1, got %d (%+v)", mainAhead, st.Unpushed)
	}
}

func TestProjectStatus_BranchNoUpstream(t *testing.T) {
	work := newRepoWithUpstream(t)
	git(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "g.txt"), []byte("z\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "g.txt")
	git(t, work, "commit", "-m", "feature work")
	st, _ := projectStatusAt(work)
	if st.Clean {
		t.Fatalf("expected unsafe (branch with no upstream), got clean: %+v", st)
	}
	found := false
	for _, b := range st.Unpushed {
		if b.Branch == "feature" && !b.HasUpstream {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected feature branch with HasUpstream=false, got %+v", st.Unpushed)
	}
}

func TestProjectStatus_Stash(t *testing.T) {
	work := newRepoWithUpstream(t)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("wip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "stash")
	st, _ := projectStatusAt(work)
	if st.Clean || st.Stashes == 0 {
		t.Fatalf("expected stashes>0, got %+v", st)
	}
}

func TestProjectStatus_NotARepo(t *testing.T) {
	dir := t.TempDir()
	st, err := projectStatusAt(dir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.IsRepo {
		t.Fatalf("expected IsRepo false, got %+v", st)
	}
	if st.Clean {
		t.Fatal("a non-repo must NOT be reported clean (treat as unsafe/unknown)")
	}
}
