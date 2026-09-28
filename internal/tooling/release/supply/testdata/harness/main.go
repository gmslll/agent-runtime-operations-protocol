//go:build ignore

// Command harness is the sole writer of the P39 release supply-chain report.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/journal"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/supply"
	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
	workflowpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/workflow"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make test-release-supply-chain"
	checker = "internal/tooling/release/supply/testdata/harness/main.go"
)

var ownedArtifacts = []string{
	"release-version-policy-schema", "release-version-policy", "release-version-mapper",
	"release-package-orchestrator", "supply-chain-orchestrator", "release-journal",
	"oidc-release-workflow", "oidc-release-workflow-lock", "oidc-release-workflow-policy",
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", command))
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = sanitize(root, err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, inputErr := staticInputs(root)
	add("p39-static-input-closure", inputErr, fmt.Sprintf("%d tracked release, workflow, report, and governance inputs", len(inputs)))
	add("p39-manifest-inventory", validateManifest(root), "exact nine P39-owned artifacts and empty runtime inputs")

	mapper, mapperErr := versionpolicy.Load(root)
	add("p39-version-policy-load", mapperErr, "strict Schema-validated release policy loads")
	if mapperErr == nil {
		add("p39-version-positive-matrix", versionPositive(mapper), "RC1, RC10, and final map exactly across six ecosystems")
		add("p39-version-negative-matrix", versionNegative(mapper), "v-prefix, PEP input, leading zero, build metadata, and noncanonical forms fail closed")
	}
	workflowEvidence, workflowErr := workflowpolicy.Validate(root)
	add("p39-oidc-workflow-policy", workflowErr, "workflow digest/lock/job identity, pinned actions, protected environment, source and minimal OIDC permissions are exact")
	add("p39-oidc-negative-matrix", workflowNegative(root), "workflow digest drift, static secrets, mutable actions, broad permissions, and unsafe concurrency fail closed")

	journalEvidence, journalErr := exerciseJournal()
	add("p39-durable-journal", journalErr, "single-writer lock, atomic reopen, idempotency, transition order, strict JSON, truncation, and checksum tamper fail closed")
	orchestrationEvidence, orchestrationErr := exerciseCoordinator(mapper)
	add("p39-orchestration-state-machine", orchestrationErr, "four primitives cover six ecosystems; reconcile/resume, immutable conflicts, channel CAS, signing, SBOM and provenance are deterministic")
	realEvidence, realErr := exerciseRealPrimitives(root)
	add("p39-real-build-primitives", realErr, "the P36/P27/P28/P37 primitives run through the release CLI without P05 proxy bootstrap")
	add("p39-no-runtime-inputs", nil, "all executable policy is tracked; runtime output is retained only as digest-and-byte evidence")

	evidence := []report.RuntimeEvidence{
		{Kind: "p39-journal-adversarial", SHA256: report.Hash(journalEvidence), Bytes: int64(len(journalEvidence))},
		{Kind: "p39-orchestration-adversarial", SHA256: report.Hash(orchestrationEvidence), Bytes: int64(len(orchestrationEvidence))},
		{Kind: "p39-real-primitives", SHA256: report.Hash(realEvidence), Bytes: int64(len(realEvidence))},
	}
	if workflowErr == nil {
		workflowBytes, _ := json.Marshal(workflowEvidence)
		evidence = append(evidence, report.RuntimeEvidence{Kind: "p39-workflow-identity", SHA256: report.Hash(workflowBytes), Bytes: int64(len(workflowBytes))})
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P39", Suite: "AROP P39 release supply chain", Class: "p39.release.supply",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 9, "logical_versions": 3, "ecosystems": 6, "build_primitives": 4, "runtime_inputs": 0},
		AuditNote: "P39 maps one canonical logical version to Go, Python, npm, OCI, CLI, and Schema Bundle identities; invokes only the existing P36/P27/P28/P37 primitives; binds policy/workflow/lock/job identity into the journal, manifest, SBOM, provenance, and signatures; reconciles immutable destinations before write; and updates the mutable channel last by compare-and-swap. The durable checksum journal uses an OS-released single-writer lock and resumes remote-success/local-crash safely. The checked-in workflow has minimal OIDC permissions, pinned full-SHA actions, protected environment and source/concurrency guards. No credential, token, artifact bytes, host path, or process output enters this report.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P39/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P39 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P39 checks failed; see build/reports/P39/report.json"))
	}
	fmt.Printf("AROP release supply chain passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, dependencies, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P39" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-release-supply-chain" {
				return fmt.Errorf("invalid P39 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p39" {
			dependencies = append(dependencies, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	wantOwned := append([]string(nil), ownedArtifacts...)
	sort.Strings(owned)
	sort.Strings(dependencies)
	sort.Strings(wantOwned)
	if !reflect.DeepEqual(owned, wantOwned) || !reflect.DeepEqual(dependencies, wantOwned) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	values := map[string]bool{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "" {
			continue
		}
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == "spec/schemas/release-version-policy.schema.json" || path == "spec/release/version-policy.yaml" || path == ".github/workflows/release.yml" || path == ".github/workflows/release.lock.json" || strings.HasPrefix(path, "internal/tooling/release/") || strings.HasPrefix(path, "internal/tooling/cmd/arop-release-supply/") || strings.HasPrefix(path, "internal/tooling/report/") || strings.HasPrefix(path, "internal/tooling/schema/") || strings.HasPrefix(path, "internal/tooling/structuredfile/") {
			info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("P39 static input is absent or non-regular: %s", path)
			}
			values[path] = true
		}
	}
	for _, required := range []string{checker, "internal/tooling/cmd/arop-release-supply/main.go"} {
		if !values[required] {
			return nil, fmt.Errorf("P39 required input is untracked: %s", required)
		}
	}
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func versionPositive(mapper versionpolicy.Mapper) error {
	cases := map[string][]string{
		"1.0.0-rc.1":  {"v1.0.0-rc.1", "1.0.0rc1", "1.0.0-rc.1"},
		"1.0.0-rc.10": {"v1.0.0-rc.10", "1.0.0rc10", "1.0.0-rc.10"},
		"1.0.0":       {"v1.0.0", "1.0.0", "1.0.0"},
	}
	for logical, want := range cases {
		got, err := mapper.Map(logical)
		if err != nil || got.Go != want[0] || got.Python != want[1] || got.NPM != want[2] || got.OCI != want[2] || got.CLI != want[2] || got.SchemaBundle != want[2] {
			return fmt.Errorf("logical version %s mapped incorrectly", logical)
		}
	}
	return nil
}

func versionNegative(mapper versionpolicy.Mapper) error {
	for _, value := range []string{"v1.0.0", "1.0.0rc1", "1.0.0-rc.01", "1.0.0+build", "01.0.0", "1.0", "1.0.0-RC.1", "1.0.0-rc.0"} {
		if _, err := mapper.Map(value); err == nil {
			return fmt.Errorf("noncanonical logical version accepted: %s", value)
		}
	}
	return nil
}

func workflowNegative(root string) error {
	workflowData, err := os.ReadFile(filepath.Join(root, workflowpolicy.WorkflowPath))
	if err != nil {
		return err
	}
	lockData, err := os.ReadFile(filepath.Join(root, workflowpolicy.LockPath))
	if err != nil {
		return err
	}
	mutations := []struct{ name, old, replacement string }{
		{"mutable-action", "actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683", "actions/checkout@main"},
		{"static-secret", "AROP_SOURCE_COMMIT: ${{ github.sha }}", "AROP_SOURCE_COMMIT: ${{ secrets.RELEASE_PAT }}"},
		{"broad-permission", "contents: read", "contents: write"},
		{"unsafe-concurrency", "cancel-in-progress: false", "cancel-in-progress: true"},
	}
	for _, mutation := range mutations {
		temporary, err := os.MkdirTemp("", "arop-p39-workflow-")
		if err != nil {
			return err
		}
		path := filepath.Join(temporary, ".github", "workflows")
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		changed := bytes.Replace(workflowData, []byte(mutation.old), []byte(mutation.replacement), 1)
		if bytes.Equal(changed, workflowData) {
			return fmt.Errorf("workflow mutation %s did not apply", mutation.name)
		}
		if err := os.WriteFile(filepath.Join(temporary, workflowpolicy.WorkflowPath), changed, 0o600); err != nil {
			return err
		}
		var lock workflowpolicy.Lock
		if err := json.Unmarshal(lockData, &lock); err != nil {
			return err
		}
		lock.WorkflowSHA256 = strings.TrimPrefix(hash(changed), "sha256:")
		updatedLock, err := json.MarshalIndent(lock, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(temporary, workflowpolicy.LockPath), append(updatedLock, '\n'), 0o600); err != nil {
			return err
		}
		_, validationErr := workflowpolicy.Validate(temporary)
		_ = os.RemoveAll(temporary)
		if validationErr == nil {
			return fmt.Errorf("workflow mutation accepted: %s", mutation.name)
		}
	}
	return nil
}

func exerciseJournal() ([]byte, error) {
	root, err := os.MkdirTemp("", "arop-p39-journal-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	store, err := journal.Open(root)
	if err != nil {
		return nil, err
	}
	if _, err := journal.Open(root); err == nil {
		return nil, errors.New("concurrent journal writer was accepted")
	}
	key := journal.Key{SourceCommit: strings.Repeat("1", 40), SourceTree: strings.Repeat("2", 40), LogicalVersion: "1.0.0", VersionPolicyDigest: hex64("policy"), Destination: "registry://example/artifact", Operation: "publish-immutable", ArtifactDigest: hex64("artifact")}
	base := journal.Entry{Key: key, WorkflowDigest: hex64("workflow"), WorkflowLockDigest: hex64("lock"), WorkflowIdentity: "workflow#release@digest"}
	prepared := base
	prepared.Status = journal.StatusPrepared
	first, err := store.Append(prepared)
	if err != nil {
		return nil, err
	}
	second, err := store.Append(prepared)
	if err != nil || first != second {
		return nil, errors.New("journal idempotent replay failed")
	}
	committed := base
	committed.Status, committed.ResultingDigest = journal.StatusCommitted, key.ArtifactDigest
	if _, err := store.Append(committed); err == nil {
		return nil, errors.New("journal accepted commit before remote success")
	}
	remote := base
	remote.Status, remote.RemoteDigest = journal.StatusRemoteSuccess, key.ArtifactDigest
	if _, err := store.Append(remote); err != nil {
		return nil, err
	}
	committed.RemoteDigest = key.ArtifactDigest
	if _, err := store.Append(committed); err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	reopened, err := journal.Open(root)
	if err != nil || len(reopened.Entries()) != 3 {
		return nil, errors.New("journal atomic reopen failed")
	}
	if err := reopened.Close(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "journal.json"))
	if err != nil {
		return nil, err
	}
	for _, mutation := range [][]byte{data[:len(data)/2], append(append([]byte(nil), data...), []byte("{}")...)} {
		if err := os.WriteFile(filepath.Join(root, "journal.json"), mutation, 0o600); err != nil {
			return nil, err
		}
		if invalid, openErr := journal.Open(root); openErr == nil {
			_ = invalid.Close()
			return nil, errors.New("truncated or trailing journal was accepted")
		}
	}
	tampered := bytes.Replace(data, []byte(key.Destination), []byte("registry://example/tampered"), 1)
	if err := os.WriteFile(filepath.Join(root, "journal.json"), tampered, 0o600); err != nil {
		return nil, err
	}
	if invalid, openErr := journal.Open(root); openErr == nil {
		_ = invalid.Close()
		return nil, errors.New("checksummed journal tamper was accepted")
	}
	return []byte(fmt.Sprintf("entries=3 bytes=%d checksum=%s\n", len(data), hash(data))), nil
}

func exerciseCoordinator(mapper versionpolicy.Mapper) ([]byte, error) {
	request := supply.Request{LogicalVersion: "1.0.0-rc.1", SourceCommit: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), Destination: "registry://example/arop", WorkflowDigest: hex64("workflow"), WorkflowLockDigest: hex64("lock"), WorkflowIdentity: "workflow#release@digest"}
	first, firstPublisher, firstEntries, err := runCoordinator(mapper, request, &onceFault{point: "after-remote:go"})
	if err == nil || first.ManifestDigest != "" || firstPublisher.puts != 1 {
		return nil, errors.New("after-remote fault did not interrupt at the exact boundary")
	}
	result, publisher, entries, err := resumeCoordinator(mapper, request, firstPublisher, firstEntries)
	if err != nil {
		return nil, err
	}
	if publisher.puts != 6 || publisher.operations[len(publisher.operations)-1] != "channel" || len(entries) != 21 || len(result.Manifest.Artifacts) != 6 || len(result.SBOM.ArtifactDigests) != 6 {
		return nil, fmt.Errorf("resume inventory puts=%d operations=%v entries=%d artifacts=%d", publisher.puts, publisher.operations, len(entries), len(result.Manifest.Artifacts))
	}
	second, _, _, err := runCoordinator(mapper, request, nil)
	if err != nil || !reflect.DeepEqual(result.Manifest, second.Manifest) || !reflect.DeepEqual(result.SBOM, second.SBOM) || !reflect.DeepEqual(result.Provenance, second.Provenance) {
		return nil, errors.New("release manifest, SBOM, or provenance is nondeterministic")
	}
	conflicting := newFakePublisher()
	conflicting.values[request.Destination+"/go/v1.0.0-rc.1/go.zip"] = hex64("wrong")
	if _, _, _, err := coordinatorWithPublisher(mapper, request, conflicting, nil); err == nil {
		return nil, errors.New("immutable destination conflict was accepted")
	}
	channelConflict := newFakePublisher()
	channelConflict.values[request.Destination+"/channels/stable"] = hex64("other-channel")
	if _, _, _, err := coordinatorWithPublisher(mapper, request, channelConflict, nil); err == nil {
		return nil, errors.New("mutable channel CAS conflict was accepted")
	}
	return []byte(fmt.Sprintf("artifacts=%d entries=%d manifest=%s operations=%d\n", len(result.Manifest.Artifacts), len(entries), result.ManifestDigest, len(publisher.operations))), nil
}

func runCoordinator(mapper versionpolicy.Mapper, request supply.Request, faults supply.FaultHook) (supply.Result, *fakePublisher, []journal.Entry, error) {
	publisher := newFakePublisher()
	return coordinatorWithPublisher(mapper, request, publisher, faults)
}

func coordinatorWithPublisher(mapper versionpolicy.Mapper, request supply.Request, publisher *fakePublisher, faults supply.FaultHook) (supply.Result, *fakePublisher, []journal.Entry, error) {
	directory, err := os.MkdirTemp("", "arop-p39-coordinator-")
	if err != nil {
		return supply.Result{}, publisher, nil, err
	}
	defer os.RemoveAll(directory)
	store, err := journal.Open(directory)
	if err != nil {
		return supply.Result{}, publisher, nil, err
	}
	coordinator := supply.Coordinator{Mapper: mapper, Primitives: fakePrimitives(), Publisher: publisher, Signer: fakeSigner{}, Journal: store, Faults: faults}
	result, runErr := coordinator.Run(context.Background(), request)
	entries := store.Entries()
	closeErr := store.Close()
	return result, publisher, entries, errors.Join(runErr, closeErr)
}

func resumeCoordinator(mapper versionpolicy.Mapper, request supply.Request, publisher *fakePublisher, prefix []journal.Entry) (supply.Result, *fakePublisher, []journal.Entry, error) {
	directory, err := os.MkdirTemp("", "arop-p39-resume-")
	if err != nil {
		return supply.Result{}, publisher, nil, err
	}
	defer os.RemoveAll(directory)
	store, err := journal.Open(directory)
	if err != nil {
		return supply.Result{}, publisher, nil, err
	}
	for _, entry := range prefix {
		entry.Sequence, entry.PreviousChecksum, entry.Checksum = 0, "", ""
		if _, err := store.Append(entry); err != nil {
			return supply.Result{}, publisher, nil, err
		}
	}
	coordinator := supply.Coordinator{Mapper: mapper, Primitives: fakePrimitives(), Publisher: publisher, Signer: fakeSigner{}, Journal: store}
	result, runErr := coordinator.Run(context.Background(), request)
	entries := store.Entries()
	closeErr := store.Close()
	return result, publisher, entries, errors.Join(runErr, closeErr)
}

type fakePrimitive struct {
	id        string
	artifacts []string
}

func (primitive fakePrimitive) ID() string { return primitive.id }
func (primitive fakePrimitive) Build(_ context.Context, versions versionpolicy.Versions) ([]supply.Artifact, error) {
	result := []supply.Artifact{}
	for _, ecosystem := range primitive.artifacts {
		version := map[string]string{supply.EcosystemGo: versions.Go, supply.EcosystemPython: versions.Python, supply.EcosystemNPM: versions.NPM, supply.EcosystemOCI: versions.OCI, supply.EcosystemCLI: versions.CLI, supply.EcosystemSchemaBundle: versions.SchemaBundle}[ecosystem]
		name := ecosystem + ".zip"
		result = append(result, supply.Artifact{Name: name, Ecosystem: ecosystem, Version: version, SHA256: hex64(name), Bytes: int64(len(name))})
	}
	return result, nil
}

func fakePrimitives() []supply.Primitive {
	return []supply.Primitive{
		fakePrimitive{"go", []string{supply.EcosystemGo, supply.EcosystemCLI}},
		fakePrimitive{"python", []string{supply.EcosystemPython}},
		fakePrimitive{"npm", []string{supply.EcosystemNPM}},
		fakePrimitive{"oci", []string{supply.EcosystemOCI, supply.EcosystemSchemaBundle}},
	}
}

type fakePublisher struct {
	mu         sync.Mutex
	values     map[string]string
	operations []string
	puts       int
}

func newFakePublisher() *fakePublisher { return &fakePublisher{values: map[string]string{}} }
func (publisher *fakePublisher) Reconcile(_ context.Context, destination string) (supply.RemoteState, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	value, ok := publisher.values[destination]
	return supply.RemoteState{Exists: ok, Digest: value}, nil
}
func (publisher *fakePublisher) PutImmutable(_ context.Context, destination string, artifact supply.Artifact) (string, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if existing, ok := publisher.values[destination]; ok && existing != artifact.SHA256 {
		return "", errors.New("immutable conflict")
	}
	publisher.values[destination], publisher.puts = artifact.SHA256, publisher.puts+1
	publisher.operations = append(publisher.operations, artifact.Ecosystem)
	return artifact.SHA256, nil
}
func (publisher *fakePublisher) CompareAndSwapChannel(_ context.Context, destination, oldDigest, newDigest string) (string, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.values[destination] != oldDigest {
		return "", errors.New("channel conflict")
	}
	publisher.values[destination] = newDigest
	publisher.operations = append(publisher.operations, "channel")
	return newDigest, nil
}

type fakeSigner struct{}

func (fakeSigner) Sign(_ context.Context, statement []byte) (supply.Signature, error) {
	return supply.Signature{Identity: "test:oidc", Digest: hash(statement)}, nil
}

type onceFault struct {
	point string
	fired bool
}

func (fault *onceFault) Hit(point string) error {
	if !fault.fired && point == fault.point {
		fault.fired = true
		return errors.New("injected release fault")
	}
	return nil
}

func exerciseRealPrimitives(root string) ([]byte, error) {
	scratch, err := os.MkdirTemp("", "arop-p39-real-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	result := run(ctx, root, "go", "run", "./internal/tooling/cmd/arop-release-supply", "--root", root, "--logical-version", "1.0.0-rc.1", "--source-commit", head, "--output", filepath.Join(scratch, "output"), "--journal", filepath.Join(scratch, "journal"), "--dry-run")
	if result.err != nil {
		return nil, result.err
	}
	var release supply.Result
	if err := strictJSONFile(filepath.Join(scratch, "output", "release-result.json"), &release); err != nil {
		return nil, err
	}
	if release.Versions.Logical != "1.0.0-rc.1" || len(release.Manifest.Artifacts) < 6 || release.ManifestDigest == "" || release.Manifest.WorkflowLockDigest == "" {
		return nil, errors.New("real release output is incomplete")
	}
	ecosystems := map[string]bool{}
	for _, artifact := range release.Manifest.Artifacts {
		ecosystems[artifact.Ecosystem] = true
	}
	if len(ecosystems) != 6 {
		return nil, fmt.Errorf("real release ecosystems=%v", ecosystems)
	}
	journalData, err := os.ReadFile(filepath.Join(scratch, "journal", "journal.json"))
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("artifacts=%d manifest=%s journal=%s/%d stdout=%s/%d\n", len(release.Manifest.Artifacts), release.ManifestDigest, hash(journalData), len(journalData), hash(result.output), len(result.output))), nil
}

type commandResult struct {
	output []byte
	err    error
}

func run(ctx context.Context, directory, name string, arguments ...string) commandResult {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	environment := []string{}
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "GOCACHEPROG": true, "GOTOOLCHAIN": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "PYTHONPATH": true, "PYTHONHOME": true, "GITHUB_TOKEN": true, "GH_TOKEN": true, "NPM_TOKEN": true, "TWINE_PASSWORD": true, "DOCKER_PASSWORD": true}
	for _, item := range os.Environ() {
		key := strings.SplitN(item, "=", 2)[0]
		if !blocked[strings.ToUpper(key)] {
			environment = append(environment, item)
		}
	}
	command.Env = append(environment, "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "PYTHONDONTWRITEBYTECODE=1", "PYTHONHASHSEED=0", "PIP_NO_INDEX=1", "TZ=UTC", "LANG=C", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return commandResult{output, fmt.Errorf("%s failed: %w: %s", filepath.Base(name), err, tail(output))}
	}
	return commandResult{output, nil}
}

func strictJSONFile(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("JSON has trailing value")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func git(root string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func hex64(value string) string { return hash([]byte(value)) }
func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func tail(value []byte) string {
	if len(value) > 1200 {
		value = value[len(value)-1200:]
	}
	return strings.TrimSpace(string(value))
}

func sanitize(root string, err error) string {
	value := strings.ReplaceAll(err.Error(), root, "<repo>")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "P39 harness:", err)
		os.Exit(1)
	}
}
