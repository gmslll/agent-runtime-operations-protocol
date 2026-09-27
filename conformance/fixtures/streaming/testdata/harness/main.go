// Command harness is the sole writer of the P22 structured streaming report.
package main

import (
	"bytes"
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
	"strconv"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-streaming-resume"
	checker  = "conformance/fixtures/streaming/testdata/harness/main.go"
	waiver   = "conformance/fixtures/streaming/testdata/transition/p22-baseline-transition-waiver.json"
	baseline = "2daaaaaf6aecba3e4180bbfab152f782bf95dc14"
)

var requiredOwnedArtifacts = []string{
	"asyncapi-agent-events", "generated-streaming-go", "generated-streaming-python", "generated-streaming-typescript",
	"go-streaming-client", "streaming-fixtures", "streaming-service", "typescript-streaming-client",
}

var requiredReportDependencies = []string{
	"asyncapi-agent-events", "generated-streaming-go", "generated-streaming-python", "generated-streaming-typescript",
	"go-consumer-sdk", "go-streaming-client", "streaming-fixtures", "streaming-service", "typescript-streaming-client",
}

var requiredCaseInventory = map[string][]string{
	"relay": {
		"initial-replay-then-live-with-no-gap", "last-event-id-reconnect-replays-each-run-sequence-once",
		"expired-cursor-returns-410-with-snapshot-and-latest-sequence", "duplicate-ledger-notification-does-not-duplicate-delivery",
		"slow-consumer-is-disconnected-without-blocking-ledger", "terminal-event-remains-self-contained-after-retention",
	},
	"direct": {
		"producer-sequence-is-independent-from-run-sequence", "last-event-id-resumes-provider-outbox",
		"run-token-is-revalidated-on-every-connection", "disconnect-never-cancels-run",
	},
	"client": {
		"go-client-rejects-gap-duplicate-conflict-and-malformed-sse", "go-client-reconnects-with-last-event-id-and-bounded-backoff",
		"typescript-client-reconnects-with-last-event-id", "cursor-expired-requires-snapshot-recovery",
		"partial-batch-retries-only-unacknowledged-suffix",
	},
	"unicode": {
		"ascii-offset", "chinese-utf8-byte-offset", "emoji-utf8-byte-offset", "combining-mark-utf8-byte-offset",
		"non-boundary-offset-is-rejected",
	},
}

type transition struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
		OwnershipTransferred bool   `json:"ownership_transferred"`
	} `json:"policy"`
	Baseline   struct{ Rule, Commit string } `json:"baseline"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string                                  `json:"affected_artifacts"`
	SourceClosure     []string                                  `json:"source_closure"`
	Acceptance        []struct{ Phase, Command, Report string } `json:"acceptance"`
	Constraints       []string                                  `json:"constraints"`
}

type discoveredTransition struct{ sources, artifacts []string }
type commandResult struct {
	output []byte
	err    error
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
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, err := trackedInputs(root)
	add("p22-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, err := declaredRuntimeInputs(root)
	add("p22-runtime-inputs-empty", errors.Join(err, requireEmpty(runtimeInputs)), "manifest runtime_inputs is empty")
	add("p22-owned-artifact-inventory", validateOwnedArtifacts(root), "manifest declares the exact eight P22-owned artifacts")
	add("p22-report-dependency-closure", validateReportDependencies(root), "phase report derives from the exact nine signed artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P22_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p22-transition-waiver", transitionErr, "waiver exactly binds Git changes, compilation, manifest owners, acceptance and constraints")
	casesEvidence, err := validateCases(root)
	add("p22-case-inventory", err, "relay, direct, client and Unicode cases are exact")
	asyncEvidence, err := validateAsyncAPI(root)
	add("p22-asyncapi-contract", err, "AsyncAPI binds relay/direct sequence domains, Event Batch, SSE resume and no-cancel semantics")
	codegenEvidence, err := validateGeneratedContracts(root)
	add("p22-generated-contracts", err, "the signed generator reproduces all three language models and provenance byte-for-byte")
	languageEvidence, err := runLanguageProbes(root)
	add("p22-language-probes", err, "Python and TypeScript strict probes pass with bounded filesystem permissions")

	rootTests := run(root, nil, "go", "test", "-race", "-count=1", "./sdk/go/generated/streaming", "./sdk/go/provider", "./sdk/go/consumer")
	add("p22-root-streaming-tests", rootTests.err, "generated, Provider batch/direct and Consumer resume tests pass under race detection")
	add("p22-root-test-terminals", rejectIncompleteTests(rootTests.output), "root test stream has no skip, cache or no-tests terminal")
	rootVet := run(root, nil, "go", "vet", "./sdk/go/generated/streaming", "./sdk/go/provider", "./sdk/go/consumer")
	add("p22-root-streaming-vet", rootVet.err, "streaming Go packages pass go vet")

	nestedTests, nestedVet := runNested(root)
	add("p22-control-plane-streaming-tests", nestedTests.err, "relay service, storage reader, live HTTP middleware, composition and catalog tests pass under race detection")
	add("p22-control-plane-test-terminals", rejectIncompleteTests(nestedTests.output), "Control Plane test stream has no skip, cache or no-tests terminal")
	add("p22-control-plane-streaming-vet", nestedVet.err, "Control Plane streaming composition passes go vet")

	p21 := run(root, nil, "make", "test-direct-proxy-provider")
	if p21.err == nil {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P21/report.json")
		p21.output = append(p21.output, verified.output...)
		p21.err = verified.err
	}
	add("p22-p21-regression", p21.err, "P21 and its predecessor chain pass at the frozen P22 carrier endpoint")

	evidence := []report.RuntimeEvidence{
		{Kind: "p22-asyncapi-validation", SHA256: report.Hash(asyncEvidence), Bytes: int64(len(asyncEvidence))},
		{Kind: "p22-case-inventory", SHA256: report.Hash(casesEvidence), Bytes: int64(len(casesEvidence))},
		{Kind: "p22-codegen", SHA256: report.Hash(codegenEvidence), Bytes: int64(len(codegenEvidence))},
		{Kind: "p22-control-plane-tests", SHA256: report.Hash(nestedTests.output), Bytes: int64(len(nestedTests.output))},
		{Kind: "p22-control-plane-vet", SHA256: report.Hash(nestedVet.output), Bytes: int64(len(nestedVet.output))},
		{Kind: "p22-language-probes", SHA256: report.Hash(languageEvidence), Bytes: int64(len(languageEvidence))},
		{Kind: "p22-root-tests", SHA256: report.Hash(rootTests.output), Bytes: int64(len(rootTests.output))},
		{Kind: "p22-root-vet", SHA256: report.Hash(rootVet.output), Bytes: int64(len(rootVet.output))},
		{Kind: "p21-regression", SHA256: report.Hash(p21.output), Bytes: int64(len(p21.output))},
	}
	for _, phase := range append([]string{"P01", "P02"}, phases(5, 21)...) {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p22-"+strings.ToLower(phase)+"-report", verified.err, phase+" report is current and verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-report-verification", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P22", Suite: "AROP P22 structured streaming", Class: "p22.streaming",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 8, "sequence_domains": 2, "language_models": 3, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P22 binds the exact eight manifest-owned artifacts plus the completed Go Consumer SDK and an independently discovered Git/compile/manifest transition closure. Relay SSE uses durable Run Sequence while Direct SSE uses Provider Sequence; each connection reauthenticates, Last-Event-ID is bounded and retention expiry returns an explicit Snapshot recovery reference. The Provider durable Event Batch planner retries only the unacknowledged suffix under stable idempotency bytes. Slow consumers fail independently, disconnect never cancels a Run, terminal events remain self-contained, UTF-8 offsets are byte-based, three language models are regenerated deterministically, predecessor reports enter only as digest-and-byte runtime evidence, and runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P22/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P22 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P22 checks failed; see build/reports/P22/report.json"))
	}
	fmt.Printf("AROP structured streaming passed: %d checks.\n", len(checks))
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	paths := []string{}
	for _, item := range bytes.Split(result.output, []byte{0}) {
		if len(item) != 0 {
			paths = append(paths, string(item))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func declaredRuntimeInputs(root string) ([]string, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return nil, err
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p22" {
			return append([]string(nil), artifact.RuntimeInputs...), nil
		}
	}
	return nil, errors.New("phase-report-p22 missing")
}

func requireEmpty(values []string) error {
	if len(values) != 0 {
		return fmt.Errorf("runtime_inputs=%q want empty", values)
	}
	return nil
}

func validateOwnedArtifacts(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	found := []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P22" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-streaming-resume" {
				return fmt.Errorf("invalid P22 owner metadata: %s", artifact.ID)
			}
			found = append(found, artifact.ID)
		}
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, requiredOwnedArtifacts) {
		return fmt.Errorf("P22 owned artifacts=%v want=%v", found, requiredOwnedArtifacts)
	}
	return nil
}

func validateReportDependencies(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p22" {
			found := append([]string(nil), artifact.DerivesFrom...)
			sort.Strings(found)
			if !reflect.DeepEqual(found, requiredReportDependencies) {
				return fmt.Errorf("P22 report dependencies=%v want=%v", found, requiredReportDependencies)
			}
			return nil
		}
	}
	return errors.New("phase-report-p22 missing")
}

func validateCases(root string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(root, "conformance/fixtures/streaming/cases.json"))
	if err != nil {
		return nil, err
	}
	var value struct {
		SchemaVersion   int `json:"schema_version"`
		Relay, Direct   []string
		Client, Unicode []string
	}
	if err = strictJSON(data, &value); err != nil {
		return nil, err
	}
	actual := map[string][]string{"relay": value.Relay, "direct": value.Direct, "client": value.Client, "unicode": value.Unicode}
	if value.SchemaVersion != 1 || !reflect.DeepEqual(actual, requiredCaseInventory) {
		return nil, fmt.Errorf("streaming cases=%v want=%v", actual, requiredCaseInventory)
	}
	return data, nil
}

func validateAsyncAPI(root string) ([]byte, error) {
	path := filepath.Join(root, "asyncapi/agent-events-v1.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err = structuredfile.Load(path, &document); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	text := string(encoded)
	for _, required := range []string{
		`"asyncapi":"3.0.0"`, "/v1/agent-runs/{run_id}/events", "/v1/runs/{run_id}/events", "/v1/agent-runs/{run_id}/events:batch",
		"text/event-stream", "Last-Event-ID", "runsequence", "producersequence", "STREAM_CURSOR_EXPIRED", "utf-8-bytes",
		"retry-only-unacknowledged-suffix-with-original-event-ids", "disconnect_cancels_run", "event-envelope-v1.schema.json",
	} {
		if !strings.Contains(text, required) {
			return nil, fmt.Errorf("AsyncAPI omits %q", required)
		}
	}
	for _, forbidden := range []string{"WebSocket", "grpc", "browserDirect", "disconnect_cancels_run\":true"} {
		if strings.Contains(text, forbidden) {
			return nil, fmt.Errorf("AsyncAPI contains forbidden surface %q", forbidden)
		}
	}
	return append(data, encoded...), nil
}

func validateGeneratedContracts(root string) ([]byte, error) {
	base := filepath.Join(root, "build", "codegen")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	output, err := os.MkdirTemp(base, "p22-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(output)
	relative, _ := filepath.Rel(root, output)
	relative = filepath.ToSlash(relative)
	nodeModules, err := filepath.EvalSymlinks(filepath.Join(root, "node_modules"))
	if err != nil {
		return nil, errors.New("resolve locked node_modules")
	}
	generated := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"}, "node", "--permission", "--allow-fs-read=.", "--allow-fs-read="+nodeModules, "--allow-fs-write=build/codegen", "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", "conformance/fixtures/streaming/pipeline.json", "--output", relative, "--result", relative+"/provenance.json")
	if generated.err != nil {
		return generated.output, generated.err
	}
	paths := []string{
		"sdk/go/generated/streaming/streaming.gen.go", "sdk/go/generated/streaming/streaming_contract_test.go",
		"sdk/python/src/arop/generated/streaming/probe.py", "sdk/python/src/arop/generated/streaming/streaming_gen.py",
		"sdk/typescript/src/generated/streaming/probe.ts", "sdk/typescript/src/generated/streaming/streaming.gen.ts",
	}
	for _, path := range paths {
		actual, readErr := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		tracked, trackedErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil || trackedErr != nil || !bytes.Equal(actual, tracked) {
			return generated.output, fmt.Errorf("generated streaming contract drift: %s", path)
		}
	}
	actual, err := os.ReadFile(filepath.Join(output, "provenance.json"))
	if err != nil {
		return generated.output, err
	}
	tracked, err := os.ReadFile(filepath.Join(root, "conformance/fixtures/streaming/generated/provenance.json"))
	if err != nil || !bytes.Equal(actual, tracked) {
		return generated.output, errors.New("streaming provenance drift")
	}
	return append(generated.output, actual...), validateProvenanceFiles(root, actual)
}

func validateProvenanceFiles(root string, data []byte) error {
	var value struct {
		SchemaVersion int `json:"schema_version"`
		Generator     struct {
			Files []fileEvidence `json:"files"`
		} `json:"generator"`
		Configuration                fileEvidence `json:"configuration"`
		Resources, Fixtures, Outputs []fileEvidence
		Checks                       []struct {
			ID, Detail string
			Passed     bool
		}
	}
	if err := strictJSON(data, &value); err != nil {
		return err
	}
	if value.SchemaVersion != 1 || len(value.Outputs) != 6 || len(value.Checks) != 6 {
		return errors.New("streaming provenance inventory is incomplete")
	}
	files := append(append(append(append([]fileEvidence{}, value.Generator.Files...), value.Configuration), value.Resources...), value.Fixtures...)
	for _, item := range append(files, value.Outputs...) {
		actual, err := fileDigest(root, item.Path)
		if err != nil || actual.SHA256 != item.SHA256 || actual.Bytes != item.Bytes || actual.Mode != item.Mode {
			return fmt.Errorf("provenance file mismatch: %s", item.Path)
		}
	}
	for _, check := range value.Checks {
		if !check.Passed || check.ID == "" || check.Detail == "" {
			return errors.New("streaming provenance contains a false or empty check")
		}
	}
	return nil
}

type fileEvidence struct {
	Path, SHA256, Mode string
	Bytes              int64
}

func fileDigest(root, path string) (fileEvidence, error) {
	if filepath.IsAbs(path) || filepath.Clean(path) != filepath.FromSlash(path) || strings.HasPrefix(path, "../") {
		return fileEvidence{}, errors.New("provenance path escapes root")
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || !info.Mode().IsRegular() {
		return fileEvidence{}, errors.New("provenance path is not regular")
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return fileEvidence{}, err
	}
	digest := sha256.Sum256(data)
	mode := "100644"
	if info.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}
	return fileEvidence{Path: path, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(data)), Mode: mode}, nil
}

func runLanguageProbes(root string) ([]byte, error) {
	python := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}, "python3", "-I", "-B", "sdk/python/src/arop/generated/streaming/probe.py")
	evidence := append([]byte(nil), python.output...)
	if python.err != nil {
		return evidence, python.err
	}
	base := filepath.Join(root, "build", "codegen")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return evidence, err
	}
	output, err := os.MkdirTemp(base, "p22-ts-")
	if err != nil {
		return evidence, err
	}
	defer os.RemoveAll(output)
	nodeModules, err := filepath.EvalSymlinks(filepath.Join(root, "node_modules"))
	if err != nil {
		return evidence, err
	}
	compile := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC"}, "node", "--permission", "--allow-fs-read=.", "--allow-fs-read="+nodeModules, "--allow-fs-write="+output, "--disable-proto=throw", "--no-addons", "node_modules/typescript/bin/tsc", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--strict", "--noUncheckedIndexedAccess", "--exactOptionalPropertyTypes", "--rootDir", "sdk/typescript/src", "--outDir", output, "sdk/typescript/src/generated/streaming/probe.ts", "sdk/typescript/src/streaming/probe.ts")
	evidence = append(evidence, compile.output...)
	if compile.err != nil {
		return evidence, compile.err
	}
	for _, probe := range []string{"generated/streaming/probe.js", "streaming/probe.js"} {
		result := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC"}, "node", "--permission", "--allow-fs-read="+output, "--disable-proto=throw", "--no-addons", filepath.Join(output, probe))
		evidence = append(evidence, result.output...)
		if result.err != nil {
			return evidence, result.err
		}
	}
	return evidence, nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var value transition
	if err = strictJSON(data, &value); err != nil {
		return err
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P22_TRANSITION") == "1" {
		encoded, _ := json.MarshalIndent(map[string]any{"sources": discovered.sources, "artifacts": discovered.artifacts}, "", "  ")
		fmt.Println(string(encoded))
	}
	if err = validateTransitionCandidate(value, discovered); err != nil {
		return err
	}
	return validateTransitionNegatives(value, discovered)
}

func discoverTransition(root string) (discoveredTransition, error) {
	intro := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	commits := strings.Fields(string(intro.output))
	if intro.err != nil || len(commits) != 1 {
		return discoveredTransition{}, errors.New("transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("transition carrier introduction parent is not the frozen P21 endpoint")
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "--name-status", "-z", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := parseChangedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}
	compiled, err := discoverCompiledSources(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	if err = requireChangedGoSourcesCompiled(sources, compiled); err != nil {
		return discoveredTransition{}, err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	artifacts, err := discoverArtifacts(root, manifest, sources)
	return discoveredTransition{sources: sources, artifacts: artifacts}, err
}

func parseChangedSources(data []byte) ([]string, error) {
	fields, paths := bytes.Split(data, []byte{0}), map[string]bool{}
	for index := 0; index < len(fields) && len(fields[index]) != 0; {
		status := string(fields[index])
		index++
		if status == "" || !strings.Contains("ACMRTD", status[:1]) {
			return nil, fmt.Errorf("unsupported Git status %q", status)
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if index+count > len(fields) {
			return nil, errors.New("truncated Git name-status stream")
		}
		for range count {
			path := string(fields[index])
			index++
			if path == "" {
				return nil, errors.New("empty Git path in name-status stream")
			}
			if !strings.HasPrefix(path, "build/reports/") {
				paths[path] = true
			}
		}
	}
	return keys(paths), nil
}

func discoverArtifacts(root string, manifest blueprint.Manifest, sources []string) ([]string, error) {
	found, byID := map[string]bool{}, map[string]blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	for _, source := range sources {
		if source == "Makefile" {
			for _, artifact := range manifest.Artifacts {
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-streaming-resume" {
					found[artifact.ID] = true
				}
			}
			continue
		}
		best, matches := -1, []blueprint.Artifact{}
		for _, artifact := range manifest.Artifacts {
			phase := artifact.OwnerPhase
			if phase == "" {
				phase = artifact.ProducerPhase
			}
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(phase) > 22 {
				continue
			}
			score := artifactScore(root, artifact.Path, source)
			if score > best {
				best, matches = score, []blueprint.Artifact{artifact}
			} else if score >= 0 && score == best {
				matches = append(matches, artifact)
			}
		}
		if best < 0 || len(matches) == 0 {
			return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
		}
	}
	for {
		added := false
		for _, artifact := range manifest.Artifacts {
			phase := artifact.OwnerPhase
			if phase == "" {
				phase = artifact.ProducerPhase
			}
			if artifact.PathRole != "concrete" || phaseNumber(phase) > 22 || found[artifact.ID] {
				continue
			}
			for id := range found {
				if depends(artifact.ID, id, byID, map[string]bool{}) {
					found[artifact.ID], added = true, true
					break
				}
			}
		}
		if !added {
			break
		}
	}
	for _, required := range append(append([]string{}, requiredOwnedArtifacts...), "phase-report-p22") {
		if !found[required] {
			return nil, fmt.Errorf("artifact discovery omitted required %s", required)
		}
	}
	return keys(found), nil
}

func artifactScore(root, artifactPath, source string) int {
	artifactPath = filepath.ToSlash(filepath.Clean(artifactPath))
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && strings.HasPrefix(source, artifactPath+"/") {
		return 3*len(artifactPath) + 1
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifactPath)))
	if directory != "." && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
		return 2 * len(directory)
	}
	if source == "reference/control-plane/go.sum" && artifactPath == "reference/control-plane/go.mod" || source == "go.sum" && artifactPath == "go.mod" {
		return 1
	}
	return -1
}

func depends(candidate, target string, byID map[string]blueprint.Artifact, visiting map[string]bool) bool {
	if candidate == target {
		return true
	}
	if visiting[candidate] {
		return false
	}
	visiting[candidate] = true
	defer delete(visiting, candidate)
	for _, dependency := range byID[candidate].DerivesFrom {
		if depends(dependency, target, byID, visiting) {
			return true
		}
	}
	return false
}

func discoverCompiledSources(root string) (map[string]bool, error) {
	result := map[string]bool{}
	rootList := run(root, nil, "go", "list", "-deps", "-test", "-json", "./sdk/go/provider", "./sdk/go/consumer", "./sdk/go/generated/streaming")
	if rootList.err != nil {
		return nil, rootList.err
	}
	if err := collectListedSources(root, rootList.output, result); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p22-list-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	work := filepath.Join(temporary, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n"
	if err = os.WriteFile(work, []byte(body), 0o600); err != nil {
		return nil, err
	}
	nested := run(filepath.Join(root, "reference/control-plane"), map[string]string{"GOWORK": work, "TMPDIR": temporary}, "go", "list", "-deps", "-test", "-json", "./internal/app/streaming/...", "./internal/app/platform/httpadapter", "./cmd/aropd")
	if nested.err != nil {
		return nil, nested.err
	}
	if err = collectListedSources(root, nested.output, result); err != nil {
		return nil, err
	}
	return result, nil
}

func collectListedSources(root string, data []byte, result map[string]bool) error {
	var item struct {
		Dir                                                      string
		GoFiles, CgoFiles, TestGoFiles, XTestGoFiles, EmbedFiles []string
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		for _, name := range append(append(append(append(append([]string{}, item.GoFiles...), item.CgoFiles...), item.TestGoFiles...), item.XTestGoFiles...), item.EmbedFiles...) {
			absolute := filepath.Join(item.Dir, name)
			relative, err := filepath.Rel(root, absolute)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				result[filepath.ToSlash(relative)] = true
			}
		}
	}
}

func requireChangedGoSourcesCompiled(sources []string, compiled map[string]bool) error {
	productionRoots := []string{"sdk/go/provider/", "sdk/go/consumer/", "sdk/go/generated/streaming/", "reference/control-plane/internal/app/streaming/", "reference/control-plane/internal/app/platform/httpadapter/", "reference/control-plane/cmd/aropd/"}
	for _, source := range sources {
		if !strings.HasSuffix(source, ".go") || strings.Contains(source, "/testdata/harness/") {
			continue
		}
		for _, prefix := range productionRoots {
			if strings.HasPrefix(source, prefix) && !compiled[source] {
				return fmt.Errorf("changed Go source is absent from actual compile closure: %s", source)
			}
		}
	}
	return nil
}

func validateTransitionCandidate(value transition, discovered discoveredTransition) error {
	wantFrom := append([]string{"P01", "P02"}, phases(5, 21)...)
	if value.SchemaVersion != 1 || value.WaiverID != "P22-STRUCTURED-STREAMING-TRANSITION-001" || value.Status != "validated" || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Transition.ToPhase != "P22" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
		return errors.New("transition identity, baseline or ownership is invalid")
	}
	if !reflect.DeepEqual(value.SourceClosure, discovered.sources) || !reflect.DeepEqual(value.AffectedArtifacts, discovered.artifacts) {
		return fmt.Errorf("transition exact closure mismatch: sources=%q artifacts=%q", discovered.sources, discovered.artifacts)
	}
	if !reflect.DeepEqual(value.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(value.Constraints, canonicalConstraints()) {
		return errors.New("transition acceptance or constraints mismatch")
	}
	return nil
}

func canonicalAcceptance() []struct{ Phase, Command, Report string } {
	commands := map[string]string{"P01": "make spec-index-check", "P02": "make blueprint-check", "P05": "make test-go-workspace", "P06": "make test-protocol-foundation", "P07": "make test-codegen-pipeline", "P08": "make test-control-plane-platform", "P09": "make test-storage-migrations", "P10": "make test-identity-secrets", "P11": "make test-publication-contracts", "P12": "make test-publication-service", "P13": "make test-asset-broker", "P14": "make test-registry-core", "P15": "make test-registry-api", "P16": "make test-registry-recovery", "P17": "make verify-registry", "P18": "make test-run-lifecycle", "P19": "make test-dispatch-ticket", "P20": "make test-event-ledger", "P21": "make test-direct-proxy-provider", "P22": command}
	result := []struct{ Phase, Command, Report string }{}
	for _, phase := range append([]string{"P01", "P02"}, phases(5, 22)...) {
		result = append(result, struct{ Phase, Command, Report string }{phase, commands[phase], "build/reports/" + phase + "/report.json"})
	}
	return result
}

func canonicalConstraints() []string {
	return []string{
		"relay-sse-uses-run-sequence-and-direct-sse-uses-producer-sequence",
		"last-event-id-is-strict-js-safe-and-revalidated-on-every-connection",
		"expired-cursor-returns-410-with-latest-sequence-and-snapshot-reference",
		"replay-precedes-live-delivery-without-gap-or-duplicate",
		"slow-consumer-failure-never-blocks-ledger-or-other-consumers",
		"stream-disconnect-never-cancels-run",
		"terminal-event-remains-self-contained-after-delta-retention",
		"event-batch-retries-only-unacknowledged-suffix-under-stable-idempotency-bytes",
		"event-batch-ack-never-marks-beyond-accepted-through-producer-sequence",
		"utf8-output-offsets-are-byte-offsets-in-all-language-clients",
		"browser-typescript-client-is-relay-only-and-never-accepts-run-token",
		"p20-ledger-and-p21-provider-history-remain-immutable",
		"owner-phase-accountability-does-not-transfer",
		"p22-report-runtime-inputs-remain-empty",
	}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	type mutation struct {
		name string
		fn   func(*transition)
	}
	mutations := []mutation{}
	appendGroup := func(name string, length int, omit, substitute func(*transition, int), add, duplicate func(*transition)) {
		for index := 0; index < length; index++ {
			i := index
			mutations = append(mutations, mutation{name + "-omission", func(value *transition) { omit(value, i) }}, mutation{name + "-substitution", func(value *transition) { substitute(value, i) }})
		}
		mutations = append(mutations, mutation{name + "-addition", add}, mutation{name + "-duplicate", duplicate})
	}
	appendGroup("source", len(valid.SourceClosure), func(v *transition, i int) { v.SourceClosure = append(v.SourceClosure[:i:i], v.SourceClosure[i+1:]...) }, func(v *transition, i int) { v.SourceClosure[i] = "unexpected/source" }, func(v *transition) { v.SourceClosure = append(v.SourceClosure, "unexpected/source") }, func(v *transition) { v.SourceClosure = append(v.SourceClosure, v.SourceClosure[0]) })
	appendGroup("artifact", len(valid.AffectedArtifacts), func(v *transition, i int) {
		v.AffectedArtifacts = append(v.AffectedArtifacts[:i:i], v.AffectedArtifacts[i+1:]...)
	}, func(v *transition, i int) { v.AffectedArtifacts[i] = "unexpected-artifact" }, func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, "unexpected-artifact") }, func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, v.AffectedArtifacts[0]) })
	appendGroup("acceptance", len(valid.Acceptance), func(v *transition, i int) { v.Acceptance = append(v.Acceptance[:i:i], v.Acceptance[i+1:]...) }, func(v *transition, i int) { v.Acceptance[i].Command = "make unexpected" }, func(v *transition) {
		v.Acceptance = append(v.Acceptance, struct{ Phase, Command, Report string }{"P99", "make unexpected", "build/reports/P99/report.json"})
	}, func(v *transition) { v.Acceptance = append(v.Acceptance, v.Acceptance[0]) })
	appendGroup("constraint", len(valid.Constraints), func(v *transition, i int) { v.Constraints = append(v.Constraints[:i:i], v.Constraints[i+1:]...) }, func(v *transition, i int) { v.Constraints[i] = "unexpected-constraint" }, func(v *transition) { v.Constraints = append(v.Constraints, "unexpected-constraint") }, func(v *transition) { v.Constraints = append(v.Constraints, v.Constraints[0]) })
	for _, item := range mutations {
		candidate := cloneTransition(valid)
		item.fn(&candidate)
		if validateTransitionCandidate(candidate, discovered) == nil {
			return fmt.Errorf("transition negative accepted: %s", item.name)
		}
	}
	return nil
}

func cloneTransition(value transition) transition {
	data, _ := json.Marshal(value)
	var clone transition
	_ = json.Unmarshal(data, &clone)
	return clone
}

func runNested(root string) (commandResult, commandResult) {
	temporary, err := os.MkdirTemp("/tmp", "arop-p22-work-")
	if err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	defer os.RemoveAll(temporary)
	if err = os.Chmod(temporary, 0o700); err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	work := filepath.Join(temporary, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n"
	if err = os.WriteFile(work, []byte(body), 0o600); err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	overrides := map[string]string{"GOWORK": work, "TMPDIR": temporary}
	directory := filepath.Join(root, "reference/control-plane")
	packages := []string{"./internal/app/streaming/...", "./internal/app/platform/httpadapter", "./internal/storage/migrate", "./cmd/aropd"}
	return run(directory, overrides, "go", append([]string{"test", "-race", "-count=1"}, packages...)...), run(directory, overrides, "go", append([]string{"vet"}, packages...)...)
}

func rejectIncompleteTests(output []byte) error {
	text := string(output)
	for _, forbidden := range []string{"--- SKIP:", "\t[no test files]", "(cached)", "FAIL\t", "--- FAIL:"} {
		if strings.Contains(text, forbidden) {
			return fmt.Errorf("test output contains forbidden terminal %q", forbidden)
		}
	}
	return nil
}

func loadManifest(root string) (blueprint.Manifest, error) {
	var manifest blueprint.Manifest
	err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest)
	return manifest, err
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func phases(first, last int) []string {
	values := make([]string, 0, last-first+1)
	for phase := first; phase <= last; phase++ {
		values = append(values, fmt.Sprintf("P%02d", phase))
	}
	return values
}

func phaseNumber(phase string) int {
	if len(phase) != 3 || phase[0] != 'P' {
		return 1000
	}
	value, err := strconv.Atoi(phase[1:])
	if err != nil {
		return 1000
	}
	return value
}

func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	cmd := exec.Command(name, args...)
	cmd.Dir = directory
	cmd.Env = cleanEnvironment(overrides)
	output, err := cmd.CombinedOutput()
	return commandResult{output: output, err: commandError(name, args, output, err)}
}

func cleanEnvironment(overrides map[string]string) []string {
	banned := []string{"GOFLAGS=", "GOENV=", "GOWORK=", "GOCACHE=", "GOCACHEPROG=", "GOMODCACHE=", "GOTMPDIR=", "GOROOT=", "GOTOOLCHAIN=", "GOEXPERIMENT=", "CGO_ENABLED=", "NODE_OPTIONS=", "NODE_PATH=", "NPM_CONFIG_NODE_OPTIONS=", "PYTHONHOME=", "PYTHONPATH=", "PYTHONSTARTUP=", "PYTHONINSPECT=", "PYTHONWARNINGS=", "PYTHONUSERBASE=", "PGHOST=", "PGPORT=", "PGPASSWORD=", "PGPASSFILE="}
	result := []string{}
	for _, entry := range os.Environ() {
		reject := false
		for _, prefix := range banned {
			if strings.HasPrefix(entry, prefix) {
				reject = true
				break
			}
		}
		if !reject {
			result = append(result, entry)
		}
	}
	defaults := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}
	for key, value := range overrides {
		defaults[key] = value
	}
	for key, value := range defaults {
		result = append(result, key+"="+value)
	}
	return result
}

func commandError(name string, args []string, output []byte, err error) error {
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(output))
	if len(text) > 6000 {
		text = text[len(text)-6000:]
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(text) > 6000 {
		text = text[:6000]
	}
	return text
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
