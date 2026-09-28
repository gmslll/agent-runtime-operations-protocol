//go:build ignore

// Command harness is the sole writer of the P33 portable conformance report.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	command  = "make test-portable-conformance"
	checker  = "cmd/arop-conformance/testdata/harness/main.go"
	baseline = "2ddc32394b6a010cc60e54f45717ea2768a6a9ba"
)

var requiredTests = []string{
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner TestCatalogRejectsDuplicateUnknownCycleAndTamperedFixture",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner TestExecuteCLIWritesJSONAndJUnitAndRejectsSymlinkOutput",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner TestRequiredSkipTimeoutAndInvalidTargetResponsesFailClosed",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner TestRunProfileClosureFilteringAndDigestReports",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner TestTargetEnvironmentIsOfflineAndSecretFree",
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/test TestCommandDelegatesToPortableRunnerCLI",
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
	add("p33-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P33 inputs", len(inputs)))
	add("p33-manifest-inventory", validateManifest(root), "exact six P33 artifacts and report closure")
	catalogEvidence, catalogErr := validateCatalog(root)
	add("p33-profile-scenario-catalog", catalogErr, "five unique acyclic profiles close over three digest-pinned scenarios")
	tests := run(root, 120*time.Second, "go", "test", "-race", "-count=1", "-json", "./cmd/arop-conformance/runner", "./cmd/arop/internal/commands/test")
	add("p33-go-tests", tests.err, "portable runner and arop test delegation pass under race detection")
	add("p33-test-inventory", validateTests(tests.output), "exact six top-level tests pass without fail, skip, cache, or no-test terminals")
	smokeEvidence, smokeErr := runSmoke(root)
	add("p33-binary-and-cli-smoke", smokeErr, "standalone runner and arop test share one offline runner and emit digest-bound JSON/JUnit")
	add("p33-no-runtime-inputs", nil, "target output is digest-only runtime evidence; report runtime_inputs remains empty")
	evidence := []report.RuntimeEvidence{
		{Kind: "p33-go-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p33-catalog", SHA256: report.Hash(catalogEvidence), Bytes: int64(len(catalogEvidence))},
		{Kind: "p33-binary-smoke", SHA256: report.Hash(smokeEvidence), Bytes: int64(len(smokeEvidence))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P33", Suite: "AROP P33 Portable Conformance Runner", Class: "p33.conformance.portable", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 6, "runtime_inputs": 0, "go_tests": len(requiredTests), "profiles": 5, "scenarios": 3, "driver_protocol": runner.DriverProtocol}, AuditNote: "P33 runs fully offline from digest-pinned, language-neutral Scenario/Profile documents. Profile and Scenario IDs are unique, graph references exist and are acyclic, required scenarios cannot skip, filters retain dependency closure, and timeouts fail closed. Driver bytes, profile, scenario, and fixture digests enter both JSON and JUnit. The runner selects and records tests but does not redefine protocol behavior or import Reference Control Plane internal packages."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P33/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P33 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P33 checks failed; see build/reports/P33/report.json"))
	}
	fmt.Printf("AROP portable conformance passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P33" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-portable-conformance" {
				return fmt.Errorf("invalid P33 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p33" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	wantOwned := []string{"portable-conformance-runner", "conformance-scenario-schema", "conformance-profile-schema", "conformance-core-scenarios", "conformance-v1-profiles", "arop-cli-test-command"}
	wantDeps := []string{"portable-conformance-runner", "conformance-scenario-schema", "conformance-profile-schema", "conformance-core-scenarios", "conformance-v1-profiles", "arop-cli-test-command", "compatibility-matrix"}
	if !reflect.DeepEqual(owned, wantOwned) || !reflect.DeepEqual(deps, wantDeps) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, deps, runtime)
	}
	return nil
}

func validateCatalog(root string) ([]byte, error) {
	if _, err := runner.LoadCatalog(root); err != nil {
		return nil, err
	}
	var profiles runner.ProfileCatalog
	if err := structuredfile.Load(filepath.Join(root, "conformance/profiles/v1/core.yaml"), &profiles); err != nil {
		return nil, err
	}
	actualProfiles := []string{}
	for _, profile := range profiles.Profiles {
		actualProfiles = append(actualProfiles, profile.ID)
	}
	wantProfiles := []string{"core-provider", "streaming", "managed-runtime", "pull-worker", "control-plane"}
	if !reflect.DeepEqual(actualProfiles, wantProfiles) {
		return nil, fmt.Errorf("profiles=%v want=%v", actualProfiles, wantProfiles)
	}
	entries, err := os.ReadDir(filepath.Join(root, "conformance/scenarios/core"))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	evidence := bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, "conformance/scenarios/core", entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var scenario runner.Scenario
		if err := protocolcore.DecodeAuthoring(data, &scenario); err != nil {
			return nil, err
		}
		ids = append(ids, scenario.ID)
		fmt.Fprintf(&evidence, "%s %s %d\n", scenario.ID, report.Hash(data), len(data))
	}
	sort.Strings(ids)
	wantIDs := []string{"core.event-envelope.valid", "core.manifest.valid", "core.trace-context.vectors"}
	if !reflect.DeepEqual(ids, wantIDs) {
		return nil, fmt.Errorf("scenario IDs=%v want=%v", ids, wantIDs)
	}
	return evidence.Bytes(), nil
}

func runSmoke(root string) ([]byte, error) {
	scratch, err := os.MkdirTemp("", "arop-p33-smoke-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	scratch, err = filepath.EvalSymlinks(scratch)
	if err != nil {
		return nil, err
	}
	driver := filepath.Join(scratch, "driver")
	standalone := filepath.Join(scratch, "arop-conformance")
	cli := filepath.Join(scratch, "arop")
	for output, pkg := range map[string]string{driver: "./cmd/arop-conformance/testdata/driver", standalone: "./cmd/arop-conformance", cli: "./cmd/arop"} {
		built := run(root, 90*time.Second, "go", "build", "-o", output, pkg)
		if built.err != nil {
			return nil, built.err
		}
	}
	jsonPath, junitPath := filepath.Join(scratch, "report.json"), filepath.Join(scratch, "junit.xml")
	standaloneRun := run(root, 60*time.Second, standalone, "--root", root, "--profile", "control-plane", "--target", driver, "--json", jsonPath, "--junit", junitPath)
	if standaloneRun.err != nil {
		return nil, standaloneRun.err
	}
	reportData, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var conformance runner.Report
	if err := protocolcore.DecodeAuthoring(reportData, &conformance); err != nil {
		return nil, err
	}
	if !conformance.Passed || conformance.PassedCount != 3 || len(conformance.Results) != 3 || !strings.HasPrefix(conformance.TargetSHA256, "sha256:") || !strings.HasPrefix(conformance.ProfileSHA256, "sha256:") {
		return nil, fmt.Errorf("unexpected standalone report: %+v", conformance)
	}
	for _, item := range conformance.Results {
		if !strings.HasPrefix(item.ScenarioSHA256, "sha256:") || item.FixtureSHA256 == "" || item.FixtureBytes <= 0 {
			return nil, fmt.Errorf("incomplete result evidence: %+v", item)
		}
	}
	junit, err := os.ReadFile(junitPath)
	if err != nil {
		return nil, err
	}
	var rootXML struct{ XMLName xml.Name }
	if err := xml.Unmarshal(junit, &rootXML); err != nil || rootXML.XMLName.Local != "testsuite" || !bytes.Contains(junit, []byte(conformance.TargetSHA256)) || !bytes.Contains(junit, []byte(conformance.Results[0].FixtureSHA256)) {
		return nil, errors.New("JUnit report omitted required digests")
	}
	cliRun := run(root, 60*time.Second, cli, "test", "--root", root, "--profile", "core-provider", "--scenario", "core.manifest.valid", "--target", driver, "--json", "-")
	if cliRun.err != nil {
		return nil, cliRun.err
	}
	var cliReport runner.Report
	if err := protocolcore.DecodeAuthoring(cliRun.output, &cliReport); err != nil || !cliReport.Passed || len(cliReport.Results) != 1 || cliReport.TargetSHA256 != conformance.TargetSHA256 {
		return nil, fmt.Errorf("arop test did not use the same runner: %+v err=%v", cliReport, err)
	}
	evidence := bytes.Buffer{}
	for name, data := range map[string][]byte{"standalone-json": reportData, "standalone-junit": junit, "arop-test-json": cliRun.output} {
		sum := sha256.Sum256(data)
		fmt.Fprintf(&evidence, "%s %s %d\n", name, hex.EncodeToString(sum[:]), len(data))
	}
	return evidence.Bytes(), nil
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
			return nil, fmt.Errorf("unsupported P33 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P33 closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P33 input is unavailable or not regular: %s", name)
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
