// Command verification is the read-only P23 Run and Delivery verifier prebuilt by P22.
package main

import (
	"encoding/json"
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
	command    = "make verify-run-delivery"
	checker    = "conformance/fixtures/streaming/testdata/verification/main.go"
	p23Carrier = "5bada508fa5e7035abd1d33817b4de7abf3ee49d"
)

var phases = []string{"P18", "P19", "P20", "P21", "P22"}

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
	add("p23-no-owned-artifacts", validateManifest(root), "P23 owns no artifacts and consumes exactly P18-P22 reports")
	add("p23-tree-identical-carrier", treeIdenticalCarrier(root), "P23 carrier changes no tracked bytes")

	run := executeAtCarrier(root)
	add("p23-black-box-run-delivery-suite", run.err, "P22 black-box Run, Attempt, Ticket, Event, Provider and Streaming suite reruns without code changes")
	evidence := []report.RuntimeEvidence{{Kind: "p22-black-box-rerun", SHA256: report.Hash(run.output), Bytes: int64(len(run.output))}}
	for _, phase := range phases {
		path := "build/reports/" + phase + "/report.json"
		verifyErr := validateArchivedReport(root, phase)
		add("p23-"+strings.ToLower(phase)+"-report", verifyErr, phase+" report is successful and bound to the immutable P23 carrier archive")
		if data, readErr := os.ReadFile(filepath.Join(root, path)); readErr == nil {
			evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-report", SHA256: report.Hash(data), Bytes: int64(len(data))})
		}
	}
	runtimeInputs := []string{}
	for _, phase := range phases {
		runtimeInputs = append(runtimeInputs, "build/reports/"+phase+"/report.json", "build/reports/"+phase+"/junit.xml")
	}
	inputs := []string{"Makefile", "spec/artifact-manifest.yaml", "conformance/fixtures/streaming/cases.json", checker, "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go"}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P23", Suite: "AROP P23 Run and Delivery verification", Class: "p23.run.delivery.verification",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 0, "logical_inputs": []string{"phase-report-p18", "phase-report-p19", "phase-report-p20", "phase-report-p21", "phase-report-p22"}, "runtime_input_files": 10},
		AuditNote: "P23 changes no tracked bytes after its explicit carrier. It reruns the P22 black-box Run and Delivery suite and consumes the exact P18-P22 JSON and JUnit reports as ten runtime input files. It does not repair implementation, schema or migration artifacts.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P23/report.json"})
	fatal(err)
	if mode != "current-worktree" || !verified.Success || !written.Success {
		fatal(errors.New("P23 verification did not produce a current successful report"))
	}
	fmt.Printf("AROP Run and Delivery verification passed: %d checks.\n", len(checks))
}

func validateArchivedReport(root, phase string) error {
	data, err := os.ReadFile(filepath.Join(root, "build", "reports", phase, "report.json"))
	if err != nil {
		return err
	}
	var value report.Report
	if err = json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.SchemaVersion != 1 || !value.Success || value.Provenance.Git.Head != p23Carrier || value.Provenance.Git.Dirty || len(value.Provenance.Git.DirtyEntries) != 0 || len(value.Checks) == 0 {
		return fmt.Errorf("invalid archived %s report lifecycle", phase)
	}
	for _, check := range value.Checks {
		if !check.Passed {
			return fmt.Errorf("archived %s check failed: %s", phase, check.Name)
		}
	}
	if _, err = os.Stat(filepath.Join(root, "build", "reports", phase, "junit.xml")); err != nil {
		return err
	}
	return nil
}

func executeAtCarrier(root string) (out result) {
	temporary, err := os.MkdirTemp("/tmp", "arop-p23-archive-")
	if err != nil {
		return result{err: err}
	}
	_ = os.Remove(temporary)
	added := execute(root, "git", "worktree", "add", "--detach", temporary, p23Carrier)
	if added.err != nil {
		return added
	}
	defer func() {
		removed := execute(root, "git", "worktree", "remove", "--force", temporary)
		if out.err == nil && removed.err != nil {
			out.err = removed.err
		}
	}()
	rootTests := execute(temporary, "go", "test", "-race", "-count=1", "./sdk/go/generated/streaming", "./sdk/go/provider", "./sdk/go/consumer")
	if rootTests.err != nil {
		return rootTests
	}
	workRoot, err := os.MkdirTemp("/tmp", "arop-p23-go-work-")
	if err != nil {
		return result{output: rootTests.output, err: err}
	}
	defer os.RemoveAll(workRoot)
	work := filepath.Join(workRoot, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(temporary, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + temporary + "\n"
	if err = os.WriteFile(work, []byte(body), 0o600); err != nil {
		return result{output: rootTests.output, err: err}
	}
	nested := executeWithOverrides(filepath.Join(temporary, "reference/control-plane"), map[string]string{"GOWORK": work, "TMPDIR": workRoot}, "go", "test", "-race", "-count=1", "./internal/domain/run", "./internal/domain/dispatch", "./internal/domain/event", "./internal/app/streaming", "./internal/app/platform/httpadapter", "./cmd/aropd")
	out.output = append(rootTests.output, nested.output...)
	out.err = nested.err
	return out
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	var owned, dependencies, runtimeInputs []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P23" {
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p23" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
			runtimeInputs = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	want := []string{"phase-report-p18", "phase-report-p19", "phase-report-p20", "phase-report-p21", "phase-report-p22"}
	if len(owned) != 0 || len(dependencies) != 0 || !reflect.DeepEqual(runtimeInputs, want) {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func treeIdenticalCarrier(root string) error {
	carrier := execute(root, "git", "rev-parse", p23Carrier+"^{commit}")
	if carrier.err != nil || strings.TrimSpace(string(carrier.output)) != p23Carrier {
		return errors.New("P23 carrier is unavailable")
	}
	parent := execute(root, "git", "rev-parse", p23Carrier+"^")
	if parent.err != nil || len(strings.TrimSpace(string(parent.output))) != 40 {
		return errors.New("P23 carrier parent is unavailable")
	}
	if err := execute(root, "git", "merge-base", "--is-ancestor", p23Carrier, "HEAD").err; err != nil {
		return errors.New("P23 carrier is not an ancestor of HEAD")
	}
	return execute(root, "git", "diff", "--quiet", strings.TrimSpace(string(parent.output)), p23Carrier, "--").err
}

type result struct {
	output []byte
	err    error
}

func execute(directory, name string, args ...string) result {
	return executeWithOverrides(directory, nil, name, args...)
}

func executeWithOverrides(directory string, overrides map[string]string, name string, args ...string) result {
	process := exec.Command(name, args...)
	process.Dir = directory
	environment := cleanEnvironment()
	for key, value := range overrides {
		prefix := key + "="
		filtered := environment[:0]
		for _, entry := range environment {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, prefix+value)
	}
	process.Env = environment
	output, err := process.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 6000 {
			tail = tail[len(tail)-6000:]
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
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	return strings.ReplaceAll(text, filepath.Clean(os.TempDir()), "<tmp>")
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
