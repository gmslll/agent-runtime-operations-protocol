//go:build ignore

// Command harness is the sole writer of the P41 release lineage report.
package main

import (
	"bytes"
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
	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/lineage"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make test-release-lineage-tooling"
	checker = "internal/tooling/release/lineage/testdata/harness/main.go"
)

var ownedArtifacts = []string{"cross-commit-lineage-verifier", "public-release-aggregator", "report-provenance-verifier", "release-report-integration"}

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
	inputs, err := staticInputs(root)
	add("p41-static-input-closure", err, fmt.Sprintf("%d tracked lineage, trust, report and workflow inputs", len(inputs)))
	add("p41-manifest-inventory", validateManifest(root), "exact four P41-owned artifacts and empty runtime inputs")

	current, err := currentVerification(root)
	add("p41-current-report", err, "ordinary success is bound to current commit, checker, inputs, runtime and report bytes")
	replayEvidence, err := isolatedReplay(root)
	add("p41-isolated-replay", err, "historical success is re-executed in a detached clean checkout with exact checker outcomes")
	aggregateEvidence, err := aggregateMatrix(root, current)
	add("p41-controlled-ancestry", err, "mixed ancestors aggregate only after current, isolated replay or trusted CI verification")
	negativeEvidence, err := negativeMatrix(root, current)
	add("p41-adversarial-matrix", err, "nonancestor, duplicate phase, unverified ancestor, input drift, unsafe replay command and forged CI identity fail closed")
	add("p41-no-overlay-bridge", sourceGuard(root), "ordinary ancestry contains no overlay or equivalence bridge and cannot synthesize trust")
	add("p41-no-runtime-inputs", nil, "all executable policy is tracked; runtime evidence contains only digests and byte counts")

	evidence := []report.RuntimeEvidence{
		{Kind: "p41-aggregate-matrix", SHA256: report.Hash(aggregateEvidence), Bytes: int64(len(aggregateEvidence))},
		{Kind: "p41-historical-replay", SHA256: report.Hash(replayEvidence), Bytes: int64(len(replayEvidence))},
		{Kind: "p41-negative-matrix", SHA256: report.Hash(negativeEvidence), Bytes: int64(len(negativeEvidence))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P41", Suite: "AROP P41 release lineage tooling", Class: "p41.release.lineage",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 4, "verification_strategies": 3, "runtime_inputs": 0},
		AuditNote: "P41 verifies report bytes, claimed commit/tree, checker, command and static/runtime input closure. Current reports are fully reverified. Historical success requires execution of the exact make target in a detached clean checkout or a P40 detached independent-reviewer provenance whose Sigstore identity binds repository, job_workflow_ref, job_workflow_sha, workflow blob, lock, source and artifact. Aggregation permits independently verified ancestor commits but rejects nonancestors, unverified archive reuse and duplicate phases. It does not bridge final overlays or synthesize trust.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P41/report.json"})
	fatal(err)
	if mode != "current-worktree" || !written.Success || !verified.Success {
		fatal(errors.New("P41 report self-verification failed"))
	}
	fmt.Printf("AROP release lineage tooling passed: %d checks.\n", len(checks))
}

func currentVerification(root string) (lineage.VerifiedCandidate, error) {
	if output, err := run(root, "make", "test-release-evidence-tooling"); err != nil {
		return lineage.VerifiedCandidate{}, fmt.Errorf("refresh P40: %w: %s", err, tail(output))
	}
	return lineage.VerifyCandidate(root, lineage.Candidate{Phase: "P40", ReportPath: "build/reports/P40/report.json", Strategy: lineage.StrategyCurrent}, time.Now().UTC())
}

func isolatedReplay(root string) ([]byte, error) {
	temp, err := secureTempDir("arop-p41-replay-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	repository := filepath.Join(temp, "repo")
	if output, err := run("", "git", "clone", "--no-local", "--quiet", root, repository); err != nil {
		return nil, fmt.Errorf("clone fixture: %w: %s", err, tail(output))
	}
	if output, err := run(repository, "make", "test-release-evidence-tooling"); err != nil {
		return nil, fmt.Errorf("historical report fixture: %w: %s", err, tail(output))
	}
	claimed, err := git(repository, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	_, _ = git(repository, "config", "user.name", "AROP P41")
	_, _ = git(repository, "config", "user.email", "p41@example.invalid")
	if _, err := git(repository, "commit", "--allow-empty", "-m", "later release commit"); err != nil {
		return nil, err
	}
	verified, err := lineage.VerifyCandidate(repository, lineage.Candidate{Phase: "P40", ReportPath: "build/reports/P40/report.json", Strategy: lineage.StrategyIsolatedReplay}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if verified.ClaimedCommit != claimed || verified.VerificationRef == "" {
		return nil, errors.New("isolated replay did not preserve claimed commit")
	}
	return json.Marshal(verified)
}

func aggregateMatrix(root string, current lineage.VerifiedCandidate) ([]byte, error) {
	if current.ClaimedCommit == "" {
		return nil, errors.New("current verification unavailable")
	}
	parent, err := git(root, "rev-parse", "HEAD^")
	if err != nil {
		return nil, err
	}
	tree, err := git(root, "rev-parse", parent+"^{tree}")
	if err != nil {
		return nil, err
	}
	historical := current
	historical.Phase = "P39"
	historical.Strategy = lineage.StrategyTrustedCI
	historical.ClaimedCommit, historical.ClaimedTree = parent, tree
	historical.ProvenanceHash, historical.VerificationRef = "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	aggregate, err := lineage.BuildAggregate(lineage.AggregateOptions{Root: root, Candidates: []lineage.VerifiedCandidate{current, historical}})
	if err != nil {
		return nil, err
	}
	if err := lineage.RequiredPhases(aggregate, []string{"P39", "P40"}); err != nil {
		return nil, err
	}
	return json.Marshal(aggregate)
}

func negativeMatrix(root string, current lineage.VerifiedCandidate) ([]byte, error) {
	results := map[string]bool{}
	reject := func(name string, err error) error {
		results[name] = err != nil
		if err == nil {
			return fmt.Errorf("negative %s was accepted", name)
		}
		return nil
	}
	dup := current
	_, err := lineage.BuildAggregate(lineage.AggregateOptions{Root: root, Candidates: []lineage.VerifiedCandidate{current, dup}})
	if err2 := reject("duplicate-phase", err); err2 != nil {
		return nil, err2
	}
	unverified := current
	unverified.Phase, unverified.Strategy, unverified.VerificationRef = "P39", lineage.StrategyIsolatedReplay, ""
	_, err = lineage.BuildAggregate(lineage.AggregateOptions{Root: root, Candidates: []lineage.VerifiedCandidate{unverified}})
	if err2 := reject("unverified-ancestor", err); err2 != nil {
		return nil, err2
	}
	nonancestor := current
	nonancestor.Phase, nonancestor.ClaimedCommit = "P39", strings.Repeat("f", 40)
	_, err = lineage.BuildAggregate(lineage.AggregateOptions{Root: root, Candidates: []lineage.VerifiedCandidate{nonancestor}})
	if err2 := reject("nonancestor", err); err2 != nil {
		return nil, err2
	}
	_, err = lineage.VerifyCandidate(root, lineage.Candidate{Phase: "P40", ReportPath: "build/reports/P40/report.json", Strategy: lineage.StrategyIsolatedReplay}, time.Now().UTC())
	if err2 := reject("replay-current-without-history", err); err2 != nil {
		return nil, err2
	}
	raw, err := os.ReadFile(filepath.Join(root, "build/reports/P40/report.json"))
	if err != nil {
		return nil, err
	}
	var parsed report.Report
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	forged, _ := json.Marshal(lineage.CIProvenance{SchemaVersion: 1, ReportSHA256: report.Hash(raw), ArtifactSHA256: report.Hash(raw), SourceCommit: parsed.Provenance.Git.Head, SourceTree: strings.Repeat("0", 40), Repository: "attacker/repo", JobWorkflowRef: "attacker/repo/.github/workflows/release.yml@refs/heads/main", JobWorkflowSHA: strings.Repeat("0", 40)})
	envelope := releaseevidence.Envelope{}
	trust := releaseevidence.VerifiedTrust{}
	_, err = lineage.VerifyCandidate(root, lineage.Candidate{Phase: "P40", ReportPath: "build/reports/P40/report.json", Strategy: lineage.StrategyTrustedCI, ProvenancePayload: forged, Envelope: &envelope, Trust: &trust, Expected: releaseevidence.ExpectedBindings{Kind: "ci_report_provenance", Role: "independent_reviewer"}}, time.Now().UTC())
	if err2 := reject("forged-ci-identity", err); err2 != nil {
		return nil, err2
	}
	return json.Marshal(results)
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P41" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-release-lineage-tooling" {
				return fmt.Errorf("invalid P41 artifact %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "release-lineage-tooling-reports" {
			deps, runtime = append(deps, artifact.DerivesFrom...), append(runtime, artifact.RuntimeInputs...)
		}
	}
	want := append([]string{}, ownedArtifacts...)
	sort.Strings(owned)
	sort.Strings(deps)
	sort.Strings(want)
	if !reflect.DeepEqual(owned, want) || !reflect.DeepEqual(deps, want) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v deps=%v runtime=%v", owned, deps, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	output, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, err
	}
	values := map[string]bool{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == ".github/workflows/release.lock.json" || path == "spec/schemas/check-report.schema.json" || strings.HasPrefix(path, "internal/tooling/release/lineage/") || strings.HasPrefix(path, "internal/tooling/cmd/arop-release-lineage/") || strings.HasPrefix(path, "internal/tooling/release/evidence/") || strings.HasPrefix(path, "internal/tooling/report/") || strings.HasPrefix(path, "internal/tooling/controlledinput/") || strings.HasPrefix(path, "internal/tooling/schema/") || strings.HasPrefix(path, "internal/tooling/structuredfile/") {
			if path != "" {
				info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
				if statErr != nil || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("P41 input is absent or nonregular: %s", path)
				}
				values[path] = true
			}
		}
	}
	if !values[checker] {
		return nil, errors.New("P41 harness is untracked")
	}
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func sourceGuard(root string) error {
	for _, path := range []string{"internal/tooling/release/lineage/git.go", "internal/tooling/release/lineage/aggregate.go", "internal/tooling/release/lineage/reporting.go", "internal/tooling/cmd/arop-release-lineage/main.go"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(data))
		for _, forbidden := range []string{"insecure-ignore", "manual-trusted-channel", "allow_ancestor", "final overlay", "equivalence bridge"} {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("%s contains forbidden trust/overlay bypass %q", path, forbidden)
			}
		}
	}
	return nil
}

func secureTempDir(pattern string) (string, error) {
	path, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func run(directory, name string, arguments ...string) ([]byte, error) {
	command := exec.Command(name, arguments...)
	command.Dir = directory
	command.Env = sanitizedEnvironment(os.Environ())
	return command.CombinedOutput()
}

func git(root string, arguments ...string) (string, error) {
	output, err := run(root, "git", arguments...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, tail(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func sanitizedEnvironment(values []string) []string {
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "TRUST_ROOT": true, "GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true}
	result := []string{}
	for _, value := range values {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !blocked[key] {
			result = append(result, value)
		}
	}
	return append(result, "GOFLAGS=-mod=readonly", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
}

func tail(value []byte) string {
	if len(value) > 1200 {
		value = value[len(value)-1200:]
	}
	return strings.TrimSpace(string(value))
}

func sanitize(root string, err error) string {
	value := strings.ReplaceAll(err.Error(), root, "<repo>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "P41 harness:", err)
		os.Exit(1)
	}
}
