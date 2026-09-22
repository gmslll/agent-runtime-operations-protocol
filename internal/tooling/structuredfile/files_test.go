package structuredfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
	}{
		{"top-level duplicate", `{"a":1,"a":2}`},
		{"nested duplicate", `{"a":{"b":1,"b":2}}`},
		{"trailing object", `{"a":1} {}`},
		{"trailing null", `{"a":1} null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data), "json"); err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
		})
	}
}

func TestStrictYAML(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
	}{
		{"top-level duplicate", "a: 1\na: 2\n"},
		{"nested duplicate", "a:\n  b: 1\n  b: 2\n"},
		{"multiple documents", "a: 1\n---\nb: 2\n"},
		{"alias", "a: &v 1\nb: *v\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data), "yaml"); err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
		})
	}
}

func TestCanonicalRepositoryContainment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	insideDir := filepath.Join(root, "..inside")
	if err := os.MkdirAll(insideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(insideDir, "evidence.json")
	if err := os.WriteFile(inside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "evidence.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireInsideFile(root, inside, "inside"); err != nil {
		t.Fatalf("real inside file rejected: %v", err)
	}
	if _, err := RequireOutsideFile(root, outside, "outside"); err != nil {
		t.Fatalf("real outside file rejected: %v", err)
	}
	if _, err := RequireOutsideFile(root, inside, "dot-dot-inside"); err == nil {
		t.Fatal("inside file whose name starts with '..' was accepted as outside")
	}

	outsideLinkBack := filepath.Join(outsideDir, "back-to-repository")
	if err := os.Symlink(inside, outsideLinkBack); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireOutsideFile(root, outsideLinkBack, "outside-symlink-back"); err == nil {
		t.Fatal("outside symlink resolving into repository was accepted")
	}
	insideLinkOut := filepath.Join(root, "link-out")
	if err := os.Symlink(outside, insideLinkOut); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireInsideFile(root, insideLinkOut, "inside-symlink-out"); err == nil {
		t.Fatal("inside symlink resolving outside repository was accepted")
	}
}
