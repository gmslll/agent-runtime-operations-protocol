//go:build ignore

// Command harness is the sole writer of the P29 A2A interoperability report.
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

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-interop-a2a"
	checker  = "adapters/a2a/testdata/harness/main.go"
	baseline = "4daf269f93bd189b10096d5ca53103c69e0d6752"
)

var requiredOwned = []string{"a2a-adapter", "arop-cli-export-a2a-command"}
var requiredFixtures = []string{"adapters/a2a/testdata/compatibility.json", "adapters/a2a/testdata/upstream-v1.0.1-agent-card.json", "adapters/a2a/testdata/upstream-v1.0.1-task.json"}
var requiredTests = []string{
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestCompatibilityPinAndStrictUpstreamFixtures",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestContentMappingRejectsUntrustedURLAndInlineBytes",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestExportAgentCardPreservesPublicIdentityAndHidesRuntimeSecurity",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestExportEventPreservesDeltaSnapshotAndLifecycleSemantics",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestExportTaskPreservesTerminalSnapshotAndTimeoutLoss",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestImportTaskStateDoesNotTurnCancelRequestIntoConfirmation",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/cancel_requested",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/cancelled",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/dispatching",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/failed",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/queued",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/running",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/succeeded",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/timed_out",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestRunTaskStateMappingIsExplicit/waiting_input",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState/duplicate",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState/state",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState/trailing",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState/unknown",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-a2a TestCommandExportsDeterministicCardAndSeparateLossReport",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-a2a TestCommandRejectsUnsafePathEndpointAndCancellation",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-a2a TestCommandRejectsUnsafePathEndpointAndCancellation/endpoint",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-a2a TestCommandRejectsUnsafePathEndpointAndCancellation/escape",
}

type commandResult struct {
	output []byte
	err    error
}
type goEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
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
	inputs, inputErr := staticInputs(root)
	add("p29-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P29 inputs", len(inputs)))
	add("p29-manifest-inventory", validateManifest(root), "exact two P29-owned artifacts and empty runtime inputs")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p29-upstream-fixtures", fixtureErr, "A2A v1.0.1 fixtures and normative proto digest are pinned")
	tests := run(root, 90*time.Second, "go", "test", "-count=1", "-json", "./adapters/a2a", "./cmd/arop/internal/commands/export-a2a")
	add("p29-go-tests", tests.err, "A2A card/task/content/loss and CLI tests pass")
	add("p29-test-inventory", validateTestInventory(tests.output), "exact 25 A2A test terminals pass without skip or cache")
	cli := run(root, 60*time.Second, "go", "run", "./cmd/arop", "export", "a2a", "examples/manifests/publication-v1.json", "https://agents.example.invalid/a2a/publication")
	add("p29-cli-smoke", validateCLI(cli), "arop export a2a emits a deterministic credential-free v1.0 Agent Card")
	add("p29-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")
	evidence := []report.RuntimeEvidence{{Kind: "p29-a2a-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))}, {Kind: "p29-cli-export", SHA256: report.Hash(cli.output), Bytes: int64(len(cli.output))}, {Kind: "p29-upstream-fixtures", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))}}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P29", Suite: "AROP P29 A2A Interoperability", Class: "p29.interop.a2a", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 2, "runtime_inputs": 0, "go_tests": len(requiredTests), "upstream_fixture_release": "1.0.1"}, AuditNote: "P29 pins the A2A v1.0.1 release and maps Agent Card, Task, Message Part, Artifact, cancellation and terminal semantics without changing AROP Core. Every mapping is classified exact, extended, lossy, or unsupported; the adapter retains a digest-bound original-object reference and never exports run tokens, registry leases, private routing, authorization snapshots, or effect reservations."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P29/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P29 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P29 checks failed; see build/reports/P29/report.json"))
	}
	fmt.Printf("AROP A2A interoperability passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P29" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-interop-a2a" {
				return fmt.Errorf("invalid P29 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p29" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	sort.Strings(deps)
	want := append([]string(nil), requiredOwned...)
	sort.Strings(want)
	if !reflect.DeepEqual(owned, want) || !reflect.DeepEqual(deps, want) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, deps, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	result := run(root, 30*time.Second, "git", "diff", "--name-status", "--no-renames", "-z", baseline+"..HEAD", "--")
	if result.err != nil {
		return nil, result.err
	}
	values := map[string]bool{"spec/artifact-manifest.yaml": true}
	fields := bytes.Split(result.output, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		status, name := string(fields[index]), filepath.ToSlash(string(fields[index+1]))
		if status == "" || name == "" {
			continue
		}
		if status[0] != 'A' && status[0] != 'M' && status[0] != 'T' {
			return nil, fmt.Errorf("unsupported P29 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P29 static closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P29 input is not regular: %s", name)
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
	actual := []string{}
	directory := filepath.Join(root, "adapters/a2a/testdata")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	evidence := bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := "adapters/a2a/testdata/" + entry.Name()
		if strings.HasSuffix(name, ".json") {
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
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, requiredFixtures) {
		return nil, fmt.Errorf("fixture inventory=%v want=%v", actual, requiredFixtures)
	}
	compatibility, err := os.ReadFile(filepath.Join(root, requiredFixtures[0]))
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(compatibility, []byte(`"fixture_release": "1.0.1"`)) || !bytes.Contains(compatibility, []byte(`"normative_proto_sha256": "e195bf96ab630c69797851970203e1b2b6b19528f2e9803b7d904b91a5104016"`)) {
		return nil, errors.New("upstream pin mismatch")
	}
	return evidence.Bytes(), nil
}

func validateTestInventory(output []byte) error {
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

func validateCLI(result commandResult) error {
	if result.err != nil {
		return result.err
	}
	if bytes.Contains(bytes.ToLower(result.output), []byte("run_token")) || bytes.Contains(bytes.ToLower(result.output), []byte("runtime_lease")) {
		return errors.New("CLI output leaked AROP runtime security")
	}
	var card a2a.AgentCard
	decoder := json.NewDecoder(bytes.NewReader(result.output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&card); err != nil {
		return err
	}
	if card.Name != "Publication Example" || card.Version != "1.0.0" || len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolVersion != "1.0" || len(card.Skills) != 1 {
		return fmt.Errorf("unexpected Agent Card: %+v", card)
	}
	return nil
}

func run(directory string, timeout time.Duration, name string, arguments ...string) commandResult {
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
	return commandResult{output: output, err: err}
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
	values = append(values, "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "TZ=UTC", "LANG=C", "LC_ALL=C")
	return values
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
