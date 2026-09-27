package pullworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileCredentialSourceRotationAndSafety(t *testing.T) {
	directory := t.TempDir()
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := FileCredentialSource{Path: path}
	first, err := source.Credential(context.Background())
	if err != nil || first != "first-token" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	if err := os.WriteFile(path, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := source.Credential(context.Background())
	if err != nil || second != "second-token" {
		t.Fatalf("second=%q err=%v", second, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Credential(context.Background()); err == nil || strings.Contains(err.Error(), "second-token") {
		t.Fatalf("public credential accepted or leaked: %v", err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileCredentialSource{Path: link}).Credential(context.Background()); err == nil {
		t.Fatal("credential symlink accepted")
	}
}
