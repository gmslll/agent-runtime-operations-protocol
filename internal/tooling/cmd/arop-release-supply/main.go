// Command arop-release-supply performs a fail-closed, resumable release dry run.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/journal"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/supply"
	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
	workflowpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/workflow"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	versionFlag := flag.String("logical-version", "", "canonical logical version without v prefix")
	sourceFlag := flag.String("source-commit", "", "exact 40-character source commit")
	outputFlag := flag.String("output", "", "empty release output directory")
	journalFlag := flag.String("journal", "", "durable release journal directory")
	expectedChannelFlag := flag.String("expected-channel-digest", "", "expected old stable-channel digest")
	dryRunFlag := flag.Bool("dry-run", false, "build, sign, and publish only to an in-memory test destination")
	flag.Parse()
	if flag.NArg() != 0 || !*dryRunFlag {
		fatal(errors.New("unexpected arguments or missing mandatory --dry-run"))
	}
	root, err := structuredfile.FindRoot(*rootFlag)
	fatal(err)
	if *versionFlag == "" || *sourceFlag == "" || *outputFlag == "" || *journalFlag == "" {
		fatal(errors.New("--logical-version, --source-commit, --output, and --journal are required"))
	}
	fatal(requireCleanHead(root, *sourceFlag))
	sourceTree, err := git(root, "rev-parse", *sourceFlag+"^{tree}")
	fatal(err)
	mapper, err := versionpolicy.Load(root)
	fatal(err)
	workflowEvidence, err := workflowpolicy.Validate(root)
	fatal(err)
	output, err := emptyDirectory(*outputFlag)
	fatal(err)
	primitives, err := supply.LocalPrimitives(root, filepath.Join(output, "artifacts"))
	fatal(err)
	store, err := journal.Open(*journalFlag)
	fatal(err)
	defer func() { fatal(store.Close()) }()
	publisher := &memoryPublisher{values: map[string]string{}}
	coordinator := supply.Coordinator{
		Mapper: mapper, Primitives: primitives, Publisher: publisher,
		Signer: digestSigner{identity: "dry-run:oidc-workload-identity"}, Journal: store,
	}
	result, err := coordinator.Run(context.Background(), supply.Request{
		LogicalVersion: *versionFlag, SourceCommit: *sourceFlag, SourceTree: sourceTree,
		Destination: "dry-run://arop-release", ExpectedChannelDigest: *expectedChannelFlag,
		WorkflowDigest: workflowEvidence.WorkflowDigest, WorkflowLockDigest: workflowEvidence.LockDigest,
		WorkflowIdentity: workflowEvidence.JobIdentity,
	})
	fatal(err)
	fatal(writeJSON(filepath.Join(output, "artifact-manifest.json"), result.Manifest))
	fatal(writeJSON(filepath.Join(output, "sbom.spdx.json"), result.SBOM))
	fatal(writeJSON(filepath.Join(output, "provenance.json"), result.Provenance))
	fatal(writeJSON(filepath.Join(output, "release-result.json"), result))
	encoded, err := json.Marshal(map[string]any{
		"dry_run": true, "logical_version": result.Versions.Logical,
		"artifact_count": len(result.Manifest.Artifacts), "manifest_digest": result.ManifestDigest,
		"channel_digest": result.ChannelDigest,
	})
	fatal(err)
	fmt.Println(string(encoded))
}

type memoryPublisher struct{ values map[string]string }

func (publisher *memoryPublisher) Reconcile(_ context.Context, destination string) (supply.RemoteState, error) {
	digest, exists := publisher.values[destination]
	return supply.RemoteState{Exists: exists, Digest: digest}, nil
}

func (publisher *memoryPublisher) PutImmutable(_ context.Context, destination string, artifact supply.Artifact) (string, error) {
	if existing, ok := publisher.values[destination]; ok && existing != artifact.SHA256 {
		return "", errors.New("immutable dry-run destination conflict")
	}
	publisher.values[destination] = artifact.SHA256
	return artifact.SHA256, nil
}

func (publisher *memoryPublisher) CompareAndSwapChannel(_ context.Context, destination, oldDigest, newDigest string) (string, error) {
	if publisher.values[destination] != oldDigest {
		return "", errors.New("dry-run channel compare-and-swap conflict")
	}
	publisher.values[destination] = newDigest
	return newDigest, nil
}

type digestSigner struct{ identity string }

func (signer digestSigner) Sign(_ context.Context, statement []byte) (supply.Signature, error) {
	value := sha256.Sum256(statement)
	return supply.Signature{Identity: signer.identity, Digest: "sha256:" + hex.EncodeToString(value[:])}, nil
}

func requireCleanHead(root, source string) error {
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil || head != source {
		return errors.New("source commit must equal current HEAD")
	}
	status, err := git(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("release source worktree must be clean")
	}
	return nil
}

func emptyDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil || abs == string(filepath.Separator) {
		return "", errors.New("release output path is invalid")
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || resolved != abs {
		return "", errors.New("release output must not traverse symlinks")
	}
	entries, err := os.ReadDir(abs)
	if err != nil || len(entries) != 0 {
		return "", errors.New("release output directory must be empty")
	}
	return abs, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func git(root string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "arop-release-supply:", err)
		os.Exit(1)
	}
}
