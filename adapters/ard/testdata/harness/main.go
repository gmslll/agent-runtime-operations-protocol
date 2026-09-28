//go:build ignore

// Command harness is the sole writer of the P31 ARD interoperability report.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-interop-ard"
	checker  = "adapters/ard/testdata/harness/main.go"
	baseline = "ec2a0ee7d6de8a105fac005e22acc2f9e8d517a8"
)

var requiredTests = []string{
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard TestExportPublishesOnlyPublicCatalogMetadata",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard TestImportRejectsImmutableVersionConflict",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard TestPinnedARDV091FixtureImportsWithoutTrustUpgrade",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard TestRoundTripProducesReviewOnlyCandidateNotRuntimeReadiness",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard TestStrictEntryPreservesNamespacedUnknownAndRejectsMalformed",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-ard TestCommandExportsDeterministicPublicARDEntryAndLossReport",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-ard TestCommandRejectsUnsafePathPublisherURLAndCancellation",
}

var requiredFixtures = []string{
	"adapters/ard/testdata/compatibility.json",
	"adapters/ard/testdata/upstream-v0.91-entry.json",
}

type result struct {
	output []byte
	err    error
}

type goEvent struct{ Action, Package, Test, Output string }

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
	inputs, inputErr := staticInputs(root)
	add("p31-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P31 inputs", len(inputs)))
	add("p31-manifest-inventory", validateManifest(root), "exact ARD adapter and CLI ownership with empty runtime inputs")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p31-ard-fixtures", fixtureErr, "official ARD v0.91 schema revision and public discovery invariants are pinned")
	tests := run(root, 90*time.Second, "go", "test", "-race", "-count=1", "-json", "./adapters/ard", "./cmd/arop/internal/commands/export-ard")
	add("p31-go-tests", tests.err, "round-trip, version conflict, unknown fields, loss, and CLI tests pass under race detection")
	add("p31-test-inventory", validateTests(tests.output), "exact seven ARD tests pass without skip or cache")
	cli := run(root, 90*time.Second, "go", "run", "./cmd/arop", "export", "ard", "examples/manifests/publication-v1.json", "example.com", "https://agents.example.com/.well-known/agent-card.json")
	add("p31-cli-export", validateCLI(cli), "arop export ard emits one strict public catalog entry")
	add("p31-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")
	evidence := []report.RuntimeEvidence{
		{Kind: "p31-ard-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p31-ard-fixtures", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
		{Kind: "p31-cli-export", SHA256: report.Hash(cli.output), Bytes: int64(len(cli.output))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P31", Suite: "AROP P31 ARD Catalog Interoperability", Class: "p31.interop.ard", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 2, "runtime_inputs": 0, "go_tests": len(requiredTests), "ard_spec_version": ard.SpecVersion, "ard_schema_commit": ard.SchemaCommit}, AuditNote: "P31 exports only public, versioned catalog metadata and a public A2A Agent Card URL. ARD discovery never grants authorization and never asserts runtime readiness, health, capacity, endpoint topology, leases, or credentials. Imports preserve namespaced extensions but remain review-only and trust-unverified; immutable version conflicts fail closed."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P31/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P31 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P31 checks failed; see build/reports/P31/report.json"))
	}
	fmt.Printf("AROP ARD catalog interoperability passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P31" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-interop-ard" {
				return fmt.Errorf("invalid P31 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p31" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	if !reflect.DeepEqual(owned, []string{"ard-adapter", "arop-cli-export-ard-command"}) || !reflect.DeepEqual(deps, []string{"ard-adapter", "arop-cli-export-ard-command"}) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, deps, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	changed := run(root, 30*time.Second, "git", "diff", "--name-status", "--no-renames", "-z", baseline+"..HEAD", "--")
	if changed.err != nil {
		return nil, changed.err
	}
	values := map[string]bool{"spec/artifact-manifest.yaml": true}
	fields := bytes.Split(changed.output, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		status, name := string(fields[index]), filepath.ToSlash(string(fields[index+1]))
		if status == "" || name == "" {
			continue
		}
		if status[0] != 'A' && status[0] != 'M' && status[0] != 'T' {
			return nil, fmt.Errorf("unsupported P31 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P31 closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P31 input is not regular: %s", name)
		}
		values[name] = true
	}
	paths := make([]string, 0, len(values))
	for value := range values {
		paths = append(paths, value)
	}
	sort.Strings(paths)
	return paths, nil
}

func validateFixtures(root string) ([]byte, error) {
	directory := filepath.Join(root, "adapters/ard/testdata")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	actual := []string{}
	evidence := bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := "adapters/ard/testdata/" + entry.Name()
		actual = append(actual, name)
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid JSON fixture: %s", name)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&evidence, "%s %s %d\n", name, hex.EncodeToString(sum[:]), len(data))
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, requiredFixtures) {
		return nil, fmt.Errorf("fixture inventory=%v want=%v", actual, requiredFixtures)
	}
	compatibility, err := os.ReadFile(filepath.Join(root, requiredFixtures[0]))
	if err != nil {
		return nil, err
	}
	for _, required := range []string{`"spec_version": "0.91"`, ard.SchemaCommit, ard.SchemaSHA256, `"discovery_is_not_runtime_readiness": true`, `"runtime_topology_and_credentials_are_not_exported": true`} {
		if !bytes.Contains(compatibility, []byte(required)) {
			return nil, errors.New("ARD compatibility pin mismatch")
		}
	}
	return evidence.Bytes(), nil
}

func validateTests(output []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	seen := []string{}
	packages := map[string]string{}
	for scanner.Scan() {
		var event goEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Action == "fail" || event.Action == "skip" {
			return fmt.Errorf("non-pass Go event: %+v", event)
		}
		if event.Action == "pass" && event.Test != "" {
			seen = append(seen, event.Package+" "+event.Test)
		}
		if event.Test == "" && (event.Action == "pass" || event.Action == "fail" || event.Action == "skip") {
			if _, exists := packages[event.Package]; exists {
				return fmt.Errorf("duplicate package terminal: %s", event.Package)
			}
			packages[event.Package] = event.Action
		}
		if strings.Contains(event.Output, "(cached)") {
			return errors.New("Go test cache marker present")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	sort.Strings(seen)
	want := append([]string(nil), requiredTests...)
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) {
		return fmt.Errorf("test inventory=%v want=%v", seen, want)
	}
	if len(packages) != 2 {
		return fmt.Errorf("package terminals=%v", packages)
	}
	for _, action := range packages {
		if action != "pass" {
			return errors.New("package did not pass")
		}
	}
	return nil
}

func validateCLI(execution result) error {
	if execution.err != nil {
		return execution.err
	}
	entry, _, err := ard.DecodeEntry(execution.output)
	if err != nil {
		return err
	}
	if entry.Identifier != "urn:air:example.com:arop:publication.example" || entry.Type != ard.AgentCardMediaType || entry.Version != "1.0.0" || entry.URL != "https://agents.example.com/.well-known/agent-card.json" {
		return fmt.Errorf("unexpected ARD CLI entry: %+v", entry)
	}
	for _, forbidden := range []string{"runtime_endpoint", "runtime_instances", "deployment_credentials", "authorization_snapshot", "lease", "readiness"} {
		if bytes.Contains(execution.output, []byte(forbidden)) {
			return fmt.Errorf("ARD CLI output leaked %s", forbidden)
		}
	}
	return nil
}

func run(directory string, timeout time.Duration, name string, arguments ...string) result {
	process := exec.Command(name, arguments...)
	process.Dir = directory
	process.Env = cleanEnvironment()
	timer := time.AfterFunc(timeout, func() {
		if process.Process != nil {
			_ = process.Process.Kill()
		}
	})
	output, err := process.CombinedOutput()
	timer.Stop()
	if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, strings.TrimSpace(string(tail)))
	}
	return result{output, err}
}

func cleanEnvironment() []string {
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "GOCACHE": true, "GOCACHEPROG": true, "GOMODCACHE": true, "GOTMPDIR": true, "GOROOT": true, "GOTOOLCHAIN": true, "GOEXPERIMENT": true, "CGO_ENABLED": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true}
	values := []string{}
	for _, entry := range os.Environ() {
		key := entry
		if at := strings.IndexByte(entry, '='); at >= 0 {
			key = entry[:at]
		}
		if !blocked[strings.ToUpper(key)] {
			values = append(values, entry)
		}
	}
	return append(values, "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "TZ=UTC", "LANG=C", "LC_ALL=C")
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(err.Error(), filepath.Clean(os.TempDir()), "<tmp>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 6000 {
		value = value[len(value)-6000:]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
