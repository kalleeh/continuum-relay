package projects

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestProjectPath_RejectsDangerousNames is the regression guard for the bug
// where RemoveProject(".") resolved to the projects dir itself and os.RemoveAll
// wiped every clone. projectPath must reject "." / ".." / traversal and only
// return strict children of ProjectsDir.
func TestProjectPath_RejectsDangerousNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, "projects")

	for _, name := range []string{".", "..", "../evil", "../../etc", "foo/../.."} {
		if _, err := projectPath(name); err == nil {
			t.Errorf("projectPath(%q) = nil error, want rejection", name)
		}
	}

	got, err := projectPath("myrepo")
	if err != nil {
		t.Fatalf("projectPath(%q) unexpected error: %v", "myrepo", err)
	}
	if want := filepath.Join(base, "myrepo"); got != want {
		t.Errorf("projectPath(\"myrepo\") = %q, want %q", got, want)
	}
}

// TestRemoveProject_DotDoesNotWipeProjectsDir is the end-to-end guard: a "."
// delete must error and leave the projects directory (and its contents) intact.
func TestRemoveProject_DotDoesNotWipeProjectsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, "projects")
	if err := os.MkdirAll(filepath.Join(base, "keepme"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProject(".", true); err == nil {
		t.Error("RemoveProject(\".\", force) = nil, want rejection")
	}
	if _, err := os.Stat(filepath.Join(base, "keepme")); err != nil {
		t.Errorf("projects dir was damaged by RemoveProject(\".\"): %v", err)
	}
}

// TestRemoveProject_ForceDeletesChild confirms the happy path still works: a
// real child directory is removed when force bypasses the clean check.
func TestRemoveProject_ForceDeletesChild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "projects", "doomed")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProject("doomed", true); err != nil {
		t.Fatalf("RemoveProject(\"doomed\", true) = %v, want nil", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists after force delete: %v", err)
	}
}

// TestRemoveProject_UnsavedWorkNeedsForce locks in the contract the iOS client
// relies on: a dirty repo (untracked file) is refused with ErrUnsavedWork when
// force is false, and deleted when force is true. The relay maps ErrUnsavedWork
// to the "remove_needs_force" wire code that drives the app's confirm dialog.
func TestRemoveProject_UnsavedWorkNeedsForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "projects", "dirty")

	// A non-repo directory is treated as "not clean" (IsRepo false → Clean false),
	// which is the conservative unsafe-to-delete case — perfect for this test and
	// avoids needing a git binary.
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "scratch.txt"), []byte("wip\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RemoveProject("dirty", false)
	if !errors.Is(err, ErrUnsavedWork) {
		t.Fatalf("RemoveProject(dirty, force=false) = %v, want ErrUnsavedWork", err)
	}
	if _, statErr := os.Stat(work); statErr != nil {
		t.Errorf("dir was deleted despite refusal: %v", statErr)
	}

	if err := RemoveProject("dirty", true); err != nil {
		t.Fatalf("RemoveProject(dirty, force=true) = %v, want nil", err)
	}
	if _, statErr := os.Stat(work); !os.IsNotExist(statErr) {
		t.Errorf("dir still exists after force delete: %v", statErr)
	}
}
