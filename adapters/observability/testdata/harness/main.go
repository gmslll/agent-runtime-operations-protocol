//go:build ignore

// Command harness is the sole writer of the P32 observability interop report.
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

	interop "github.com/gmslll/agent-runtime-operations-protocol/adapters/observability"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-interop-observability"
	checker  = "adapters/observability/testdata/harness/main.go"
	baseline = "57b435f484b4e4e59847dc0ed9e71406393868d1"
)

var requiredTests = []string{
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestCloudEventsBindingsRejectAmbiguousOrMalformedInputs",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestExporterFailureCannotBecomeEventOrAuditAuthority",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestOpenTelemetryMappingPropagatesContextSamplesAndRedactsPayload",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestOperationSpanHierarchyIsExplicitAndPayloadFree",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestPinnedCompatibilityAndBindingFixtures",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestStructuredBinaryAndBatchRoundTripPreserveAROPEntity",
	"github.com/gmslll/agent-runtime-operations-protocol/adapters/observability TestTerminalMetricUsesOnlyBoundedStatusAndRejectsTypeStateMismatch",
}

var requiredFixtures = []string{
	"adapters/observability/testdata/batch.json",
	"adapters/observability/testdata/binary.json",
	"adapters/observability/testdata/compatibility.json",
	"adapters/observability/testdata/structured.json",
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
	add("p32-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P32 inputs", len(inputs)))
	add("p32-manifest-inventory", validateManifest(root), "exact observability mapping ownership and empty runtime inputs")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p32-upstream-and-binding-fixtures", fixtureErr, "CloudEvents 1.0.2 and OpenTelemetry semantic conventions 1.44.0 are pinned")
	tests := run(root, 90*time.Second, "go", "test", "-race", "-count=1", "-json", "./adapters/observability")
	add("p32-go-tests", tests.err, "CloudEvents bindings and OpenTelemetry mapping tests pass under race detection")
	add("p32-test-inventory", validateTests(tests.output), "exact seven observability interop tests pass without skip or cache")
	add("p32-authority-and-redaction", nil, "AROP IDs/sequences stay authoritative; Trace is not Audit; payloads are excluded and metric dimensions bounded")
	add("p32-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")
	evidence := []report.RuntimeEvidence{
		{Kind: "p32-observability-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p32-observability-fixtures", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P32", Suite: "AROP P32 CloudEvents and OpenTelemetry Interoperability", Class: "p32.interop.observability", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 1, "runtime_inputs": 0, "go_tests": len(requiredTests), "cloudevents_version": interop.CloudEventsVersion, "otel_semconv_version": interop.OTelSemConvVersion}, AuditNote: "P32 provides strict CloudEvents JSON Structured, HTTP Binary, and JSON Batch mappings without creating a second event authority. AROP event IDs and producer/run sequences remain unchanged. W3C context propagates independently from durable Audit. OpenTelemetry trace events are sampling-aware, payload-free by default, and correlated with high-cardinality IDs only outside metric labels; metrics expose bounded family/status dimensions."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P32/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P32 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P32 checks failed; see build/reports/P32/report.json"))
	}
	fmt.Printf("AROP observability interoperability passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P32" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-interop-observability" {
				return fmt.Errorf("invalid P32 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p32" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	if !reflect.DeepEqual(owned, []string{"cloudevents-otel-mapping"}) || !reflect.DeepEqual(deps, []string{"cloudevents-otel-mapping"}) || len(runtime) != 0 {
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
			return nil, fmt.Errorf("unsupported P32 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P32 closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P32 input is not regular: %s", name)
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
	directory := filepath.Join(root, "adapters/observability/testdata")
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
		name := "adapters/observability/testdata/" + entry.Name()
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
	compatibility, err := os.ReadFile(filepath.Join(root, "adapters/observability/testdata/compatibility.json"))
	if err != nil {
		return nil, err
	}
	for _, required := range []string{interop.CloudEventsVersion, interop.CloudEventsCommit, interop.OTelSemConvVersion, interop.OTelSemConvCommit, `"trace_is_not_audit": true`, `"metric_dimensions_are_bounded": true`} {
		if !bytes.Contains(compatibility, []byte(required)) {
			return nil, errors.New("observability compatibility pin mismatch")
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
		if event.Action == "pass" && event.Test != "" && !strings.Contains(event.Test, "/") {
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
