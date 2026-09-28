//go:build ignore

// Command harness is the sole writer of the P30 MCP interoperability report.
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

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-interop-mcp"
	checker  = "adapters/mcp/testdata/harness/main.go"
	baseline = "9a15353dda190f4e7b3d5b47aa7ecc350f823bcd"
)

var requiredTests = []string{
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestDenyDependencyErrorAndCancelAreFailClosedAndRedacted",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestFixturesPinMCPRevisionWithoutEndpointOrCredential",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestReadToolPropagatesTraceAndRequiresAuthorization",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestResourceReadIsAuthorizedAndCannotCarryWriteEffects",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestWriteToolFailsClosedWithoutEffectOrDurableJournal",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/mcp TestWriteToolUsesStableEffectJournalAcrossAttempts",
}

var requiredFixtures = []string{
	"adapters/mcp/testdata/compatibility.json",
	"adapters/mcp/testdata/resource-read.json",
	"adapters/mcp/testdata/tool-call.json",
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
	add("p30-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P30 inputs", len(inputs)))
	add("p30-manifest-inventory", validateManifest(root), "exact mcp-adapter ownership and empty runtime inputs")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p30-mcp-fixtures", fixtureErr, "MCP 2026-07-28 tool/resource boundary fixtures are pinned")
	tests := run(root, 90*time.Second, "go", "test", "-race", "-count=1", "-json", "./adapters/mcp")
	add("p30-go-tests", tests.err, "authorization, trace, resource, cancel, error, and effect tests pass under race detection")
	add("p30-test-inventory", validateTests(tests.output), "exact six MCP tests pass without skip or cache")
	add("p30-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")
	evidence := []report.RuntimeEvidence{
		{Kind: "p30-mcp-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p30-mcp-fixtures", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P30", Suite: "AROP P30 MCP Interoperability", Class: "p30.interop.mcp", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 1, "runtime_inputs": 0, "go_tests": len(requiredTests), "mcp_protocol_revision": "2026-07-28"}, AuditNote: "P30 keeps MCP tool/resource semantics intact while requiring an already-authorized AROP Run/Attempt boundary. W3C trace context uses MCP metadata; local authorization remains mandatory; write/irreversible calls require a stable effect_id and durable reservation. MCP transport state, endpoint credentials, registry discovery, and tool annotations never create or weaken AROP authority."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P30/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P30 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P30 checks failed; see build/reports/P30/report.json"))
	}
	fmt.Printf("AROP MCP interoperability passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P30" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-interop-mcp" {
				return fmt.Errorf("invalid P30 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p30" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	if !reflect.DeepEqual(owned, []string{"mcp-adapter"}) || !reflect.DeepEqual(deps, []string{"mcp-adapter"}) || len(runtime) != 0 {
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
			return nil, fmt.Errorf("unsupported P30 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P30 closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P30 input is not regular: %s", name)
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
	directory := filepath.Join(root, "adapters/mcp/testdata")
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
		name := "adapters/mcp/testdata/" + entry.Name()
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
	if !bytes.Contains(compatibility, []byte(`"protocol_revision": "2026-07-28"`)) || !bytes.Contains(compatibility, []byte(`"write_and_irreversible_require_stable_effect_id": true`)) {
		return nil, errors.New("MCP compatibility pin mismatch")
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
	if len(packages) != 1 {
		return fmt.Errorf("package terminals=%v", packages)
	}
	for _, action := range packages {
		if action != "pass" {
			return errors.New("package did not pass")
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
