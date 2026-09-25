package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockedNodeModulesUsesWorktreeOwner(t *testing.T) {
	repository := t.TempDir()
	root := filepath.Join(repository, ".worktrees", "p12")
	shared := filepath.Join(repository, "node_modules")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := lockedNodeModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != shared {
		t.Fatalf("lockedNodeModules()=%q want %q", got, shared)
	}
}

func TestLockedNodeModulesRejectsUnrelatedExternalDirectory(t *testing.T) {
	repository := t.TempDir()
	root := filepath.Join(repository, ".worktrees", "p12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "node_modules")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}

	if got, err := lockedNodeModules(root); err == nil {
		t.Fatalf("lockedNodeModules()=%q unexpectedly accepted unrelated %q", got, external)
	}
}
