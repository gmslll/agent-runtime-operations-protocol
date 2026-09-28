// Command verification is the P38 read-only resilience verifier prebuilt by P37.
package main

import (
	"bytes"
	"context"
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
	command        = "make verify-resilience"
	checker        = "deployments/production-reference/testdata/verification/main.go"
	carrierSubject = "verify(p38): establish read-only resilience carrier"
)

var phases = []string{"P09", "P10", "P12", "P13", "P14", "P16", "P18", "P19", "P20", "P21", "P24", "P27", "P34", "P35", "P37"}

var requiredChecks = map[string][]string{
	"P09": {"p09-migration-inventory", "p09-postgres16-toolchain"},
	"P10": {"p10-production-catalog", "p10-private-postgres"},
	"P12": {"p12-sqlite-postgres-publication"},
	"P13": {"p13-sqlite-postgres-asset-broker"},
	"P14": {"p14-registry-domain-storage-migrations"},
	"P16": {"p16-watch-ha-recovery-tests"},
	"P18": {"p18-run-domain-storage-migrations"},
	"P19": {"p19-dispatch-domain-storage-migrations"},
	"P20": {"p20-event-domain-storage-migrations"},
	"P21": {"p21-root-provider-consumer-reference-tests", "p21-control-plane-proxy-tests"},
	"P24": {"p24-dual-store-worker-tests"},
	"P27": {"p27-python-tests", "p27-test-inventory"},
	"P34": {"p34-sqlite-control-plane-profile", "p34-postgres-control-plane-profile"},
	"P35": {"p35-postgres16-multi-node-failover"},
	"P37": {"p37-live-postgres-and-ha"},
}

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
	add("p38-manifest-inventory", validateManifest(root), "P38 owns no artifacts and consumes exactly fifteen phase reports")
	add("p38-tree-identical-carrier", validateCarrier(root), "P38 starts at an explicit tree-identical carrier after the prebuilt verifier")

	// P24 replays the complete Control Plane and Go Agent durability chain;
	// P27 supplies the independent Python SQLite path; P37 reruns P34/P35 and
	// the production PostgreSQL deployment path. The reports below, rather than
	// these process logs, are the authoritative runtime inputs.
	runs := []struct {
		name, target string
	}{
		{"p38-control-plane-and-go-agent-rerun", "test-worker-service"},
		{"p38-python-agent-rerun", "test-python-provider"},
		{"p38-production-and-ha-rerun", "production-reference-smoke"},
	}
	evidence := []report.RuntimeEvidence{}
	for _, item := range runs {
		result := execute(root, 70*time.Minute, "make", item.target)
		add(item.name, result.err, item.target+" completed without skip or fallback")
		evidence = append(evidence, report.RuntimeEvidence{Kind: item.name, SHA256: report.Hash(result.output), Bytes: int64(len(result.output))})
	}

	runtimeInputs := []string{}
	for _, phase := range phases {
		path := "build/reports/" + phase + "/report.json"
		verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: path})
		if verifyErr == nil && (mode != "current-worktree" || !verified.Success) {
			verifyErr = fmt.Errorf("mode=%s success=%v", mode, verified.Success)
		}
		if verifyErr == nil {
			verifyErr = validateResilienceMarkers(phase, verified)
		}
		add("p38-"+strings.ToLower(phase)+"-report", verifyErr, phase+" is current, successful, and contains its signed resilience markers")
		for _, name := range []string{"junit.xml", "report.json"} {
			runtimeInputs = append(runtimeInputs, "build/reports/"+phase+"/"+name)
		}
		if data, readErr := os.ReadFile(filepath.Join(root, path)); readErr == nil {
			evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-report", SHA256: report.Hash(data), Bytes: int64(len(data))})
		}
	}
	sort.Strings(runtimeInputs)
	add("p38-runtime-input-inventory", validateRuntimeInputInventory(runtimeInputs), "exact JSON and JUnit files for fifteen logical inputs are bound")
	add("p38-tracked-tree-unchanged", trackedTreeClean(root), "verification changed no tracked implementation, schema, migration, or planning bytes")

	inputs := []string{"Makefile", checker, "internal/tooling/report/verifier.go", "internal/tooling/report/writer.go", "spec/artifact-manifest.yaml"}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P38", Suite: "AROP P38 read-only resilience verification", Class: "p38.resilience.verification",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 0, "logical_inputs": append([]string(nil), phases...), "runtime_input_files": len(runtimeInputs), "storage_engines": 2, "agent_sqlite_implementations": 2},
		AuditNote: "P38 is read-only. It binds the exact JSON and JUnit outputs from fifteen signed phases and independently verifies current commit, runtime, checker, input digests, successful terminal sets, dual-database migrations, backup/restore, Registry recovery, Go/Python Agent SQLite durability, deterministic faults, two-node PostgreSQL failover, and the production deployment profile. Process output is digest-only evidence. No migration, implementation, credential, DSN, database content, absolute path, or log is added by this phase.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P38/report.json"})
	fatal(err)
	if mode != "current-worktree" || !verified.Success || !written.Success {
		fatal(errors.New("P38 verification did not produce a current successful report"))
	}
	fmt.Printf("AROP resilience verification passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, dependencies, runtimeInputs := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P38" {
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p38" {
			dependencies = append(dependencies, artifact.DerivesFrom...)
			runtimeInputs = append(runtimeInputs, artifact.RuntimeInputs...)
		}
	}
	want := make([]string, len(phases))
	for i, phase := range phases {
		want[i] = "phase-report-" + strings.ToLower(phase)
	}
	if len(owned) != 0 || len(dependencies) != 0 || !reflect.DeepEqual(runtimeInputs, want) {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func validateCarrier(root string) error {
	result := execute(root, 30*time.Second, "git", "log", "--format=%H%x00%s%x00")
	if result.err != nil {
		return result.err
	}
	carrier := ""
	parts := bytes.Split(result.output, []byte{0})
	for index := 0; index+1 < len(parts); index += 2 {
		if strings.TrimSpace(string(parts[index+1])) == carrierSubject {
			carrier = strings.TrimSpace(string(parts[index]))
			break
		}
	}
	if len(carrier) != 40 {
		return errors.New("P38 carrier is missing")
	}
	subject := execute(root, 30*time.Second, "git", "show", "-s", "--format=%s", carrier)
	if subject.err != nil || strings.TrimSpace(string(subject.output)) != carrierSubject {
		return errors.New("P38 carrier subject is not exact")
	}
	if ancestor := execute(root, 30*time.Second, "git", "merge-base", "--is-ancestor", carrier, "HEAD"); ancestor.err != nil {
		return errors.New("P38 carrier is not an ancestor of HEAD")
	}
	parent := execute(root, 30*time.Second, "git", "rev-parse", carrier+"^")
	if parent.err != nil || len(strings.TrimSpace(string(parent.output))) != 40 {
		return errors.New("P38 carrier parent is unavailable")
	}
	if diff := execute(root, 30*time.Second, "git", "diff", "--quiet", strings.TrimSpace(string(parent.output)), carrier, "--"); diff.err != nil {
		return errors.New("P38 carrier changes tracked bytes")
	}
	if prebuilt := execute(root, 30*time.Second, "git", "cat-file", "-e", strings.TrimSpace(string(parent.output))+":"+checker); prebuilt.err != nil {
		return errors.New("P38 verifier was not prebuilt before its read-only carrier")
	}
	return nil
}

func validateResilienceMarkers(phase string, value *report.Report) error {
	names := map[string]bool{}
	for _, check := range value.Checks {
		if check.Passed {
			names[check.Name] = true
		}
	}
	for _, required := range requiredChecks[phase] {
		if !names[required] {
			return fmt.Errorf("%s lacks successful resilience check %s", phase, required)
		}
	}
	return nil
}

func validateRuntimeInputInventory(paths []string) error {
	want := []string{}
	for _, phase := range phases {
		want = append(want, "build/reports/"+phase+"/junit.xml", "build/reports/"+phase+"/report.json")
	}
	sort.Strings(want)
	if !reflect.DeepEqual(paths, want) {
		return fmt.Errorf("runtime inputs=%v", paths)
	}
	return nil
}

func trackedTreeClean(root string) error {
	result := execute(root, 30*time.Second, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if result.err != nil {
		return result.err
	}
	if len(bytes.TrimSpace(result.output)) != 0 {
		return errors.New("tracked or untracked repository state changed during read-only verification")
	}
	return nil
}

func execute(directory string, timeout time.Duration, name string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(ctx, name, args...)
	process.Dir = directory
	process.Env = cleanEnvironment()
	output, err := process.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("%s timed out", filepath.Base(name))
	} else if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment() []string {
	values := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "LANG": "C", "LC_ALL": "C", "TZ": "UTC"}
	for _, key := range []string{"HOME", "PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(err.Error(), filepath.Clean(os.TempDir()), "<tmp>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 1200 {
		value = value[len(value)-1200:]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
