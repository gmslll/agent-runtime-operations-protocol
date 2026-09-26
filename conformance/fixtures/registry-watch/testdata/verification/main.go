// Command verification is the read-only P17 registry verifier prebuilt by P16.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make verify-registry"
	checker = "conformance/fixtures/registry-watch/testdata/verification/main.go"
)

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
	add("p17-no-owned-artifacts", validateManifest(root), "P17 owns no artifacts and consumes exactly P14/P15/P16 reports")
	add("p17-tree-identical-carrier", treeIdenticalCarrier(root), "P17 carrier changes no tracked bytes")

	run := execute(root, "make", "test-registry-recovery")
	add("p17-black-box-registry-suite", run.err, "P16 black-box dual-engine suite reruns without code changes")
	evidence := []report.RuntimeEvidence{{Kind: "p16-black-box-rerun", SHA256: report.Hash(run.output), Bytes: int64(len(run.output))}}
	for _, phase := range []string{"P14", "P15", "P16"} {
		path := "build/reports/" + phase + "/report.json"
		verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: path})
		if verifyErr == nil && (mode != "current-worktree" || !verified.Success) {
			verifyErr = fmt.Errorf("mode=%s success=%v", mode, verified.Success)
		}
		add("p17-"+strings.ToLower(phase)+"-report", verifyErr, phase+" report is current and successful")
		if data, readErr := os.ReadFile(filepath.Join(root, path)); readErr == nil {
			evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-report", SHA256: report.Hash(data), Bytes: int64(len(data))})
		}
	}
	runtimeInputs := []string{}
	for _, phase := range []string{"P14", "P15", "P16"} {
		runtimeInputs = append(runtimeInputs, "build/reports/"+phase+"/report.json", "build/reports/"+phase+"/junit.xml")
	}
	inputs := []string{"Makefile", "spec/artifact-manifest.yaml", "conformance/fixtures/registry-watch/cases.json", checker, "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go"}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P17", Suite: "AROP P17 registry verification", Class: "p17.registry.verification",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 0, "logical_inputs": []string{"phase-report-p14", "phase-report-p15", "phase-report-p16"}, "runtime_input_files": 6},
		AuditNote: "P17 changes no tracked bytes after its explicit carrier. It reruns the P16 black-box suite and consumes the exact P14/P15/P16 JSON and JUnit reports as six runtime input files. It does not repair implementation or schema artifacts.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P17/report.json"})
	fatal(err)
	if mode != "current-worktree" || !verified.Success || !written.Success {
		fatal(errors.New("P17 verification did not produce a current successful report"))
	}
	fmt.Printf("AROP registry verification passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	var owned, dependencies, runtimeInputs []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P17" {
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p17" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
			runtimeInputs = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	want := []string{"phase-report-p14", "phase-report-p15", "phase-report-p16"}
	if len(owned) != 0 || len(dependencies) != 0 || !reflect.DeepEqual(runtimeInputs, want) {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func treeIdenticalCarrier(root string) error {
	parent := execute(root, "git", "rev-parse", "HEAD^")
	if parent.err != nil || len(strings.TrimSpace(string(parent.output))) != 40 {
		return errors.New("P17 carrier parent is unavailable")
	}
	return execute(root, "git", "diff", "--quiet", "HEAD^", "HEAD", "--").err
}

type result struct {
	output []byte
	err    error
}

func execute(directory, name string, args ...string) result {
	process := exec.Command(name, args...)
	process.Dir = directory
	process.Env = cleanEnvironment()
	output, err := process.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(tail)))
	}
	return result{output: output, err: err}
}

func cleanEnvironment() []string {
	values := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC"}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 1000 {
		text = text[len(text)-1000:]
	}
	return strings.ReplaceAll(text, filepath.Clean(os.TempDir()), "<tmp>")
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
