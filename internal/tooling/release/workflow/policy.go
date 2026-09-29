// Package workflow validates the immutable OIDC release workflow and its lock.
package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	WorkflowPath = ".github/workflows/release.yml"
	LockPath     = ".github/workflows/release.lock.json"
)

var pinnedAction = regexp.MustCompile(`(?m)^\s*- uses: ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)@([0-9a-f]{40})\s*$`)

type Lock struct {
	SchemaVersion    int               `json:"schema_version"`
	WorkflowPath     string            `json:"workflow_path"`
	WorkflowSHA256   string            `json:"workflow_sha256"`
	Job              string            `json:"job"`
	Environment      string            `json:"environment"`
	Permissions      map[string]string `json:"permissions"`
	Actions          []string          `json:"actions"`
	SourceRef        string            `json:"source_ref"`
	ConcurrencyGroup string            `json:"concurrency_group"`
}

type Evidence struct {
	WorkflowDigest string
	LockDigest     string
	JobIdentity    string
	Actions        []string
}

func Validate(root string) (Evidence, error) {
	workflowData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(WorkflowPath)))
	if err != nil {
		return Evidence{}, fmt.Errorf("read release workflow: %w", err)
	}
	lockData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LockPath)))
	if err != nil {
		return Evidence{}, fmt.Errorf("read release workflow lock: %w", err)
	}
	var lock Lock
	if err := structuredfile.Load(filepath.Join(root, filepath.FromSlash(LockPath)), &lock); err != nil {
		return Evidence{}, fmt.Errorf("decode release workflow lock: %w", err)
	}
	workflowDigest := hash(workflowData)
	lockDigest := hash(lockData)
	if lock.SchemaVersion != 1 || lock.WorkflowPath != WorkflowPath || lock.WorkflowSHA256 != strings.TrimPrefix(workflowDigest, "sha256:") || lock.Job != "release" || lock.Environment != "arop-release" || lock.SourceRef != "refs/heads/main" || lock.ConcurrencyGroup != "arop-release-${{ github.ref }}" {
		return Evidence{}, errors.New("release workflow lock identity drifted")
	}
	if len(lock.Permissions) != 2 || lock.Permissions["contents"] != "read" || lock.Permissions["id-token"] != "write" {
		return Evidence{}, errors.New("release workflow lock permissions are not minimal")
	}
	matches := pinnedAction.FindAllSubmatch(workflowData, -1)
	actions := make([]string, 0, len(matches))
	for _, match := range matches {
		actions = append(actions, string(match[1])+"@"+string(match[2]))
	}
	if len(actions) != 2 || !equalStrings(actions, lock.Actions) {
		return Evidence{}, fmt.Errorf("release workflow actions=%v lock=%v", actions, lock.Actions)
	}
	lower := bytes.ToLower(workflowData)
	for _, forbidden := range []string{"pull_request_target", "permissions: write-all", "contents: write", "packages: write", "github.token", "secrets.", "password", "private key", "_pat", "cancel-in-progress: true"} {
		if bytes.Contains(lower, []byte(forbidden)) {
			return Evidence{}, fmt.Errorf("release workflow contains forbidden capability %q", forbidden)
		}
	}
	required := []string{
		"workflow_dispatch:", "contents: read", "id-token: write", "environment: arop-release",
		"github.ref == 'refs/heads/main'", "github.repository == 'gmslll/agent-runtime-operations-protocol'",
		"persist-credentials: false", "AROP_JOB_WORKFLOW_REF", "AROP_JOB_WORKFLOW_SHA", "--dry-run",
	}
	for _, value := range required {
		if !bytes.Contains(workflowData, []byte(value)) {
			return Evidence{}, fmt.Errorf("release workflow lacks %q", value)
		}
	}
	if _, err := structuredfile.Parse(workflowData, "yaml"); err != nil {
		return Evidence{}, fmt.Errorf("parse release workflow: %w", err)
	}
	return Evidence{WorkflowDigest: workflowDigest, LockDigest: lockDigest, JobIdentity: WorkflowPath + "#release@" + lock.WorkflowSHA256, Actions: append([]string(nil), actions...)}, nil
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}
