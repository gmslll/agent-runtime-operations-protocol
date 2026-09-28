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

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
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
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.Kind == "machine-reports" && artifact.ProducerPhase != "" {
			phase := artifact.ProducerPhase
			if phase >= "P01" && phase <= "P43" {
				targets[phase] = strings.TrimPrefix(artifact.AcceptanceTest, "make-")
			}
		}
	}
	results := map[string]string{}
	for i := 1; i <= 43; i++ {
		phase := fmt.Sprintf("P%02d", i)
		if phase == "P03" || phase == "P04" {
			continue
		}
		target := targets[phase]
		if target == "" {
			return nil, fmt.Errorf("%s acceptance target missing", phase)
		}
		args := []string{target}
		if phase == "P43" {
			args = append(args, "READ_ONLY=1")
		}
		output, err := run(root, "make", args...)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w: %s", phase, target, err, tail(output))
		}
		results[phase] = report.Hash(output)
	}
	if output, err := run(root, "make", "test-evidence-lineage"); err != nil {
		return nil, fmt.Errorf("P03/P04 evidence lineage: %w: %s", err, tail(output))
	} else {
		results["P03-P04-lineage"] = report.Hash(output)
	}
	return json.Marshal(results)
}

func verifyAll(root string) ([]byte, error) {
	results := map[string]string{}
	for i := 1; i <= 43; i++ {
		phase := fmt.Sprintf("P%02d", i)
		path := fmt.Sprintf("build/reports/%s/report.json", phase)
		options := report.VerifyOptions{Root: root, ReportPath: path, AllowAncestor: phase == "P03" || phase == "P04"}
		verified, mode, err := report.Verify(options)
		if err != nil || !verified.Success {
			return nil, fmt.Errorf("%s verification mode=%s: %w", phase, mode, err)
		}
		if phase != "P03" && phase != "P04" && mode != "current-worktree" {
			return nil, fmt.Errorf("%s was not freshly rerun", phase)
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
