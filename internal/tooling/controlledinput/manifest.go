// Package controlledinput discovers the complete tracked input set used by the
// repository-wide P01/P02 governance checks. The set is derived from Git, not
// from paths claimed by a report producer.
package controlledinput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const Policy = "tracked-repository-tree-v1"

type Entry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	LinkTarget string `json:"link_target"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}

type Manifest struct {
	Policy  string  `json:"policy"`
	Scope   string  `json:"scope"`
	Source  string  `json:"source"`
	Tree    string  `json:"tree"`
	SHA256  string  `json:"sha256"`
	Entries []Entry `json:"entries"`
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func aggregate(entries []Entry) string {
	var out bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&out, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\n", entry.Path, entry.Type, entry.Mode, entry.LinkTarget, entry.SHA256, entry.Bytes)
	}
	return hash(out.Bytes())
}

func command(root string, args ...string) ([]byte, error) {
	cmd := GitCommand(root, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// GitCommand creates a repository-local Git subprocess after removing ambient
// GIT_* overrides that could redirect its index, worktree, object database, or
// configuration away from root.
func GitCommand(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(WithoutGitOverrides(os.Environ()),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd
}

func WithoutGitOverrides(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func validatePath(path string) error {
	clean := filepath.ToSlash(filepath.Clean(path))
	if path == "" || clean != path || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("tracked input path is unsafe: %q", path)
	}
	return nil
}

func entry(path, mode, kind string, data []byte) (Entry, error) {
	if err := validatePath(path); err != nil {
		return Entry{}, err
	}
	if kind != "blob" || (mode != "100644" && mode != "100755") {
		if mode == "120000" {
			return Entry{}, fmt.Errorf("tracked input %s is a symlink; controlled inputs forbid symlinks", path)
		}
		return Entry{}, fmt.Errorf("tracked input %s has unsupported type/mode %s/%s", path, kind, mode)
	}
	return Entry{Path: path, Type: kind, Mode: mode, LinkTarget: "", SHA256: hash(data), Bytes: int64(len(data))}, nil
}

func validateWorktreeFile(root, path, gitMode string) error {
	if err := validatePath(path); err != nil {
		return err
	}
	current := filepath.Clean(root)
	components := strings.Split(filepath.FromSlash(path), string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect controlled input %s component %s: %w", path, component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("controlled input %s traverses worktree symlink %s; controlled inputs forbid symlinks", path, filepath.ToSlash(strings.Join(components[:index+1], string(filepath.Separator))))
		}
		if index < len(components)-1 && !info.IsDir() {
			return fmt.Errorf("controlled input %s parent component %s is not a directory", path, component)
		}
		if index == len(components)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("controlled input %s is not a regular worktree file", path)
		}
		if index == len(components)-1 {
			worktreeExecutable := info.Mode().Perm()&0o111 != 0
			gitExecutable := gitMode == "100755"
			if worktreeExecutable != gitExecutable {
				return fmt.Errorf("controlled input %s executable mode does not match Git mode %s", path, gitMode)
			}
		}
	}
	return nil
}

// Current discovers the exact tracked index path set and hashes bytes from the
// current worktree. Staged additions/deletions therefore cannot hide from the
// manifest, while ignored build/session files never enter it.
func Current(root, scope string) (Manifest, error) {
	if scope != "P01" && scope != "P02" {
		return Manifest{}, fmt.Errorf("controlled input scope must be P01 or P02")
	}
	raw, err := command(root, "ls-files", "-z", "--stage")
	if err != nil {
		return Manifest{}, err
	}
	rootedWorktree, err := os.OpenRoot(root)
	if err != nil {
		return Manifest{}, fmt.Errorf("open controlled worktree root: %w", err)
	}
	defer rootedWorktree.Close()
	entries := []Entry{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		meta := strings.Fields(string(parts[0]))
		if len(parts) != 2 || len(meta) != 3 || meta[2] != "0" {
			return Manifest{}, fmt.Errorf("invalid or unmerged git index record %q", record)
		}
		path := filepath.ToSlash(string(parts[1]))
		kindRaw, kindErr := command(root, "cat-file", "-t", meta[1])
		if kindErr != nil {
			return Manifest{}, fmt.Errorf("inspect controlled Git object %s: %w", path, kindErr)
		}
		kind := strings.TrimSpace(string(kindRaw))
		if _, itemErr := entry(path, meta[0], kind, nil); itemErr != nil {
			return Manifest{}, itemErr
		}
		if fileErr := validateWorktreeFile(root, path, meta[0]); fileErr != nil {
			return Manifest{}, fileErr
		}
		data, readErr := fs.ReadFile(rootedWorktree.FS(), filepath.FromSlash(path))
		if readErr != nil {
			return Manifest{}, fmt.Errorf("read controlled input %s: %w", path, readErr)
		}
		if fileErr := validateWorktreeFile(root, path, meta[0]); fileErr != nil {
			return Manifest{}, fmt.Errorf("revalidate controlled input after read: %w", fileErr)
		}
		item, itemErr := entry(path, meta[0], kind, data)
		if itemErr != nil {
			return Manifest{}, itemErr
		}
		entries = append(entries, item)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	treeRaw, err := command(root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Policy: Policy, Scope: scope, Source: "current-index-worktree", Tree: strings.TrimSpace(string(treeRaw)), SHA256: aggregate(entries), Entries: entries}, nil
}

// AtCommit discovers and hashes the corresponding committed Git tree without
// consulting paths claimed by an archived report.
func AtCommit(root, commit, scope string) (Manifest, error) {
	if scope != "P01" && scope != "P02" {
		return Manifest{}, fmt.Errorf("controlled input scope must be P01 or P02")
	}
	raw, err := command(root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return Manifest{}, err
	}
	entries := []Entry{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		meta := strings.Fields(string(parts[0]))
		if len(parts) != 2 || len(meta) != 3 {
			return Manifest{}, fmt.Errorf("invalid git tree record %q", record)
		}
		path := filepath.ToSlash(string(parts[1]))
		if _, itemErr := entry(path, meta[0], meta[1], nil); itemErr != nil {
			return Manifest{}, itemErr
		}
		data, readErr := command(root, "cat-file", "blob", meta[2])
		if readErr != nil {
			return Manifest{}, fmt.Errorf("read controlled Git blob %s: %w", path, readErr)
		}
		item, itemErr := entry(path, meta[0], meta[1], data)
		if itemErr != nil {
			return Manifest{}, itemErr
		}
		entries = append(entries, item)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	treeRaw, err := command(root, "rev-parse", commit+"^{tree}")
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Policy: Policy, Scope: scope, Source: "git-tree", Tree: strings.TrimSpace(string(treeRaw)), SHA256: aggregate(entries), Entries: entries}, nil
}

func Equal(a, b Manifest) bool {
	if a.Policy != b.Policy || a.Scope != b.Scope || a.Tree != b.Tree || a.SHA256 != b.SHA256 || len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		if a.Entries[i] != b.Entries[i] {
			return false
		}
	}
	return true
}
