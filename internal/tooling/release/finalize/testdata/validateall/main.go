//go:build ignore

// Command validateall performs the P44 private code-complete review.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make validate-all"
	checker = "internal/tooling/release/finalize/testdata/validateall/main.go"
)

var staticInputs = []string{"internal/tooling/release/lineage/aggregate.go", "internal/tooling/cmd/arop-release-lineage/main.go", "internal/tooling/release/lineage/reporting.go"}

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
	targetEvidence, err := refreshAll(root)
	add("p44-exact-phase-execution", err, "P01-P43 phase policies execute in order at current HEAD; P03/P04 retain independently signed planning lineage")
	if err != nil {
		fatal(err)
	}
	runtimeInputs := reportPaths()
	lineageEvidence, err := verifyAll(root)
	add("p44-report-lineage", err, "exactly 43 reports independently verify their claimed commit, checker, input closure, runtime fan-in and success")
	irEvidence, err := verifyRequirements(root)
	add("p44-ir01-ir14", err, "IR-01 through IR-14 are exact, unique and retained in both signed planning summaries")
	add("p44-clean-environment", cleanStatus(root), "final current worktree is clean and all current reports bind the sanitized runtime")
	add("p44-private-status", nil, "status is private code-complete only; P45/P50/P52 remain external-evidence gates")
	add("p44-no-external-fabrication", externalGateGuard(root), "no domain/package ownership, partner conformance or release approval is synthesized")
	evidence := []report.RuntimeEvidence{{Kind: "p44-ir-traceability", SHA256: report.Hash(irEvidence), Bytes: int64(len(irEvidence))}, {Kind: "p44-lineage-matrix", SHA256: report.Hash(lineageEvidence), Bytes: int64(len(lineageEvidence))}, {Kind: "p44-phase-execution", SHA256: report.Hash(targetEvidence), Bytes: int64(len(targetEvidence))}}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P44", Suite: "AROP P44 private code-complete", Class: "p44.release.readiness", Command: command, CheckerPath: checker, InputPaths: staticInputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"phase_reports": 43, "internal_requirements": 14, "status": "private code-complete", "external_gates_remaining": 3}, AuditNote: "P44 aggregates exactly P01-P43 after executing the signed phase policies in order on the current clean source. P03/P04 are revalidated through their promoted independent-reviewer/project-owner evidence chain rather than regenerated. Each report is verified directly; P43 is not used as a proxy. IR-01 through IR-14 remain traceable. This report declares only private code-complete: it does not claim public namespace ownership, partner conformance, release approval or v1 delivery, and P45/P50/P52 remain mandatory external gates."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P44/report.json"})
	fatal(err)
	if mode != "current-worktree" || !written.Success || !verified.Success {
		fatal(errors.New("P44 report self-verification failed"))
	}
	fmt.Printf("AROP private code-complete verification passed: %d checks.\n", len(checks))
}

func refreshAll(root string) ([]byte, error) {
	results := map[string]string{}
	if output, err := run(root, "make", "validate"); err != nil {
		return nil, fmt.Errorf("P01/P02 current validation: %w: %s", err, tail(output))
	} else {
		results["P01-P02-current"] = report.Hash(output)
	}
	replays := []struct {
		name, commit, target string
		prerequisites        []string
		phases               []string
	}{
		{"P10", "9c4da72a6043e265f75b5b7fb0f0c1a31d2d98d2", "test-identity-secrets", nil, []string{"P10"}},
		{"P23", "5bada508fa5e7035abd1d33817b4de7abf3ee49d", "verify-run-delivery", nil, []string{"P18", "P19", "P20", "P21", "P22", "P23"}},
		{"P24-P26", "6ba1651", "verify-operations-security", []string{"test-worker-service", "test-go-worker-sdk"}, []string{"P24", "P25", "P26"}},
		{"P28", "4daf269", "test-typescript-consumer", nil, []string{"P28"}},
		{"P29", "9a15353", "test-interop-a2a", nil, []string{"P29"}},
		{"P30", "ec2a0ee", "test-interop-mcp", nil, []string{"P30"}},
	}
	for _, replay := range replays {
		commit, err := git(root, "rev-parse", replay.commit+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("%s historical commit: %w", replay.name, err)
		}
		if evidence, ok := existingSuccessfulReports(root, replay.phases, strings.TrimSpace(commit)); ok {
			results[replay.name+"-verified-existing"] = report.Hash(evidence)
			continue
		}
		evidence, err := replayReports(root, replay.commit, replay.target, replay.prerequisites, replay.phases)
		if err != nil {
			return nil, fmt.Errorf("%s historical replay: %w", replay.name, err)
		}
		results[replay.name+"-isolated-replay"] = report.Hash(evidence)
	}
	if output, err := run(root, "make", "release-dry-run", "READ_ONLY=1"); err != nil {
		return nil, fmt.Errorf("P39-P43 current release verification: %w: %s", err, tail(output))
	} else {
		results["P39-P43-current"] = report.Hash(output)
	}
	if output, err := run(root, "make", "test-evidence-lineage"); err != nil {
		return nil, fmt.Errorf("P03/P04 evidence lineage: %w: %s", err, tail(output))
	} else {
		results["P03-P04-lineage"] = report.Hash(output)
	}
	return json.Marshal(results)
}

func replayReports(root, revision, target string, prerequisites, phases []string) (evidence []byte, resultErr error) {
	commit, err := git(root, "rev-parse", revision+"^{commit}")
	if err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp("", "arop-p44-replay-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(temp)
	added := false
	defer func() {
		if added {
			_, removeErr := git(root, "worktree", "remove", "--force", temp)
			if resultErr == nil && removeErr != nil {
				resultErr = removeErr
			}
		}
		_ = os.RemoveAll(temp)
	}()
	if _, err := git(root, "worktree", "add", "--detach", temp, commit); err != nil {
		return nil, err
	}
	added = true
	if err := seedSuccessfulReports(root, temp); err != nil {
		return nil, err
	}
	// A replay must produce its own requested reports.  Seeding predecessor
	// evidence is necessary for historical fan-in, but retaining a stale report
	// for the phase under replay can make a target consume later evidence instead
	// of exercising its historical writer.
	for _, phase := range phases {
		if err := os.RemoveAll(filepath.Join(temp, "build", "reports", phase)); err != nil {
			return nil, err
		}
	}
	if output, err := run(temp, "npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
		return nil, fmt.Errorf("locked dependencies: %w: %s", err, tail(output))
	}
	for _, prerequisite := range prerequisites {
		output, err := run(temp, "make", prerequisite)
		if err != nil {
			return nil, fmt.Errorf("make prerequisite %s: %w: %s", prerequisite, err, tail(output))
		}
	}
	output, err := run(temp, "make", target)
	if err != nil {
		return nil, fmt.Errorf("make %s: %w: %s%s", target, err, tail(output), failedReportDiagnostics(temp, phases))
	}
	bindings := map[string]string{"commit": commit, "target": target, "output_sha256": report.Hash(output)}
	for _, phase := range phases {
		for _, name := range []string{"junit.xml", "report.json"} {
			relative := filepath.Join("build", "reports", phase, name)
			data, err := os.ReadFile(filepath.Join(temp, relative))
			if err != nil {
				return nil, fmt.Errorf("%s did not produce %s", target, relative)
			}
			destination := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(destination, data, 0o600); err != nil {
				return nil, err
			}
			bindings[phase+"/"+name] = report.Hash(data)
		}
	}
	return json.Marshal(bindings)
}

func failedReportDiagnostics(root string, phases []string) string {
	lines := []string{}
	for _, phase := range phases {
		data, err := os.ReadFile(filepath.Join(root, "build", "reports", phase, "report.json"))
		if err != nil {
			continue
		}
		var value struct {
			Success bool `json:"success"`
			Checks  []struct {
				Name   string `json:"name"`
				Passed bool   `json:"passed"`
				Detail string `json:"detail"`
			} `json:"checks"`
		}
		if json.Unmarshal(data, &value) != nil || value.Success {
			continue
		}
		for _, check := range value.Checks {
			if !check.Passed {
				lines = append(lines, phase+"/"+check.Name+": "+check.Detail)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nfailed report checks:\n" + strings.Join(lines, "\n")
}

func existingSuccessfulReports(root string, phases []string, expectedCommit string) ([]byte, bool) {
	values := map[string]string{}
	for _, phase := range phases {
		path := filepath.Join(root, "build", "reports", phase, "report.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		var value struct {
			Success    bool `json:"success"`
			Provenance struct {
				Git struct {
					Head string `json:"head"`
				} `json:"git"`
			} `json:"provenance"`
		}
		if json.Unmarshal(data, &value) != nil || !value.Success || (expectedCommit != "" && value.Provenance.Git.Head != expectedCommit) {
			return nil, false
		}
		if _, err := os.Stat(filepath.Join(root, "build", "reports", phase, "junit.xml")); err != nil {
			return nil, false
		}
		values[phase] = report.Hash(data)
	}
	data, _ := json.Marshal(values)
	return data, true
}

func seedSuccessfulReports(root, destinationRoot string) error {
	for phaseNumber := 1; phaseNumber <= 43; phaseNumber++ {
		phase := fmt.Sprintf("P%02d", phaseNumber)
		if _, ok := existingSuccessfulReports(root, []string{phase}, ""); !ok {
			continue
		}
		for _, name := range []string{"junit.xml", "report.json"} {
			relative := filepath.Join("build", "reports", phase, name)
			data, err := os.ReadFile(filepath.Join(root, relative))
			if err != nil {
				return err
			}
			destination := filepath.Join(destinationRoot, relative)
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(destination, data, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyAll(root string) ([]byte, error) {
	results := map[string]string{}
	for i := 1; i <= 43; i++ {
		phase := fmt.Sprintf("P%02d", i)
		path := fmt.Sprintf("build/reports/%s/report.json", phase)
		options := report.VerifyOptions{Root: root, ReportPath: path, AllowAncestor: true}
		verified, mode, err := report.Verify(options)
		if err != nil || !verified.Success {
			return nil, fmt.Errorf("%s verification mode=%s: %w", phase, mode, err)
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		results[phase] = report.Hash(data)
	}
	return json.Marshal(results)
}

func reportPaths() []string {
	paths := make([]string, 0, 86)
	for i := 1; i <= 43; i++ {
		phase := fmt.Sprintf("P%02d", i)
		paths = append(paths, fmt.Sprintf("build/reports/%s/junit.xml", phase), fmt.Sprintf("build/reports/%s/report.json", phase))
	}
	return paths
}

func verifyRequirements(root string) ([]byte, error) {
	var document struct {
		Requirements []struct {
			ID string `json:"id" yaml:"id"`
		} `json:"requirements" yaml:"requirements"`
	}
	if err := structuredfile.Load(filepath.Join(root, "spec/requirements.yaml"), &document); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, item := range document.Requirements {
		if strings.HasPrefix(item.ID, "IR-") {
			ids = append(ids, item.ID)
		}
	}
	sort.Strings(ids)
	want := []string{}
	for i := 1; i <= 14; i++ {
		want = append(want, fmt.Sprintf("IR-%02d", i))
	}
	if strings.Join(ids, "\n") != strings.Join(want, "\n") {
		return nil, fmt.Errorf("IR inventory=%v", ids)
	}
	for _, path := range []string{"spec/evidence/P03-planning-audit-summary.json", "spec/evidence/P04-user-gate-summary.json"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		for _, id := range want {
			if strings.Count(string(data), `"id": "`+id+`"`) != 1 {
				return nil, fmt.Errorf("%s does not bind %s exactly once", path, id)
			}
		}
	}
	return json.Marshal(ids)
}

func externalGateGuard(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "docs/DEVELOPMENT_PLAN.md"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"P45 — External public configuration gate", "P50 — External conformance review gate", "P52 — External v1 release approval", "blocked-external-evidence"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("external gate marker missing: %s", required)
		}
	}
	return nil
}
func cleanStatus(root string) error {
	output, err := git(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if output != "" {
		return fmt.Errorf("worktree dirty: %s", output)
	}
	return nil
}
func run(dir, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = safeEnv(os.Environ())
	return c.CombinedOutput()
}
func safeEnv(values []string) []string {
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "TRUST_ROOT": true, "EVIDENCE_BUNDLE": true, "ATTESTATION": true, "GITHUB_TOKEN": true, "GH_TOKEN": true, "NPM_TOKEN": true, "NODE_AUTH_TOKEN": true, "TWINE_PASSWORD": true, "DOCKER_PASSWORD": true}
	result := []string{}
	for _, value := range values {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !blocked[key] {
			result = append(result, value)
		}
	}
	return append(result, "GOFLAGS=-mod=readonly", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
}
func git(root string, args ...string) (string, error) {
	output, err := run(root, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func tail(value []byte) string {
	if len(value) > 1600 {
		value = value[len(value)-1600:]
	}
	return strings.TrimSpace(string(value))
}
func sanitize(root string, err error) string {
	value := strings.ReplaceAll(err.Error(), root, "<repo>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 600 {
		value = value[:600]
	}
	return value
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "P44 validate-all:", err)
		os.Exit(1)
	}
}
