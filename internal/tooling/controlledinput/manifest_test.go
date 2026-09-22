package controlledinput

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentAndAncestorDiscoverCompleteTrackedTree(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "Controlled Input Test")
	gitTest(t, root, "config", "user.email", "controlled-input@invalid.example")
	writeTestFile(t, root, "regular.txt", "current\n", 0o644)
	writeTestFile(t, root, "bin/check.sh", "#!/bin/sh\n", 0o755)
	writeTestFile(t, root, ".codex/tracked.txt", "tracked\n", 0o644)
	writeTestFile(t, root, "build/tracked.txt", "tracked build input\n", 0o644)
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "fixture")
	head := gitTest(t, root, "rev-parse", "HEAD")

	current, err := Current(root, "P01")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Entries) != 4 {
		t.Fatalf("complete tracked set not discovered: %#v", current.Entries)
	}
	paths := []string{}
	for _, entry := range current.Entries {
		paths = append(paths, entry.Path)
		if entry.Type != "blob" || entry.LinkTarget != "" {
			t.Fatalf("unexpected controlled entry: %#v", entry)
		}
	}
	for _, required := range []string{".codex/tracked.txt", "build/tracked.txt"} {
		if !contains(paths, required) {
			t.Fatalf("tracked path %s was excluded from complete set %v", required, paths)
		}
	}

	writeTestFile(t, root, "regular.txt", "dirty worktree\n", 0o644)
	dirty, err := Current(root, "P01")
	if err != nil {
		t.Fatal(err)
	}
	ancestor, err := AtCommit(root, head, "P01")
	if err != nil {
		t.Fatal(err)
	}
	if Equal(dirty, ancestor) || digestFor(dirty, "regular.txt") == digestFor(ancestor, "regular.txt") {
		t.Fatal("current worktree bytes were not distinguished from ancestor Git blob")
	}
}

func TestCurrentRejectsWorktreeSymlinkBeforeReadingTarget(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "Controlled Input Test")
	gitTest(t, root, "config", "user.email", "controlled-input@invalid.example")
	writeTestFile(t, root, "input.txt", "tracked\n", 0o644)
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "fixture")
	if err := os.Remove(filepath.Join(root, "input.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-secret"), filepath.Join(root, "input.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Current(root, "P01"); err == nil || !strings.Contains(err.Error(), "worktree symlink") {
		t.Fatalf("worktree symlink was not rejected before target read: %v", err)
	}
}

func TestAncestorRejectsTrackedSymlink(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "Controlled Input Test")
	gitTest(t, root, "config", "user.email", "controlled-input@invalid.example")
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "symlink")
	head := gitTest(t, root, "rev-parse", "HEAD")
	if _, err := AtCommit(root, head, "P02"); err == nil || !strings.Contains(err.Error(), "forbid symlinks") {
		t.Fatalf("tracked ancestor symlink accepted: %v", err)
	}
}

func writeTestFile(t *testing.T, root, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func digestFor(manifest Manifest, path string) string {
	for _, entry := range manifest.Entries {
		if entry.Path == path {
			return entry.SHA256
		}
	}
	return ""
}
