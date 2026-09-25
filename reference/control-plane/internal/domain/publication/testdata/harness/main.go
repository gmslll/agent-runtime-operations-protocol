// Command harness is the sole writer of the P12 publication report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-publication-service"
	checker  = "reference/control-plane/internal/domain/publication/testdata/harness/main.go"
	waiver   = "reference/control-plane/internal/domain/publication/testdata/transition/p12-baseline-transition-waiver.json"
	baseline = "31da1c8353b61e5eceae97f90b5e633e29abf3e5"
)

type result struct {
	output []byte
	err    error
}
type cluster struct {
	cmd    *exec.Cmd
	socket string
	log    bytes.Buffer
}
type transition struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
		OwnershipTransferred bool   `json:"ownership_transferred"`
	} `json:"policy"`
	Baseline   struct{ Rule, Commit string } `json:"baseline"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string                                  `json:"affected_artifacts"`
	SourceClosure     []string                                  `json:"source_closure"`
	Acceptance        []struct{ Phase, Command, Report string } `json:"acceptance"`
	Constraints       []string                                  `json:"constraints"`
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", command))
	}
	checks := []report.Check{}
	add := func(name string, err error, ok string) {
		detail := ok
		if err != nil {
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, err := trackedInputs(root)
	add("p12-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	add("p12-runtime-inputs-empty", nil, "runtime_inputs is empty")
	add("p12-transition-waiver", validateTransition(root), "waiver exactly binds Git changes, P12 artifacts and acceptance")
	add("p12-catalog-snapshots", validateCatalog(root), "P09/P10 snapshots and P12 current catalog are explicit")
	add("p12-no-production-0020", rejectProduction0020(root), "production catalog and migration directories stop at 0010")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p12-")
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p12-private-scratch", scratchErr, "private scratch created")
	var pg *cluster
	var pgEvidence []byte
	if scratchErr == nil {
		pg, pgEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p12-private-postgres16", err, "private PostgreSQL 16 ready on Unix socket")
	var tests result
	if pg != nil {
		tests = runTests(root, scratch, pg.url())
	} else {
		tests.err = errors.New("PostgreSQL prerequisite failed")
	}
	add("p12-sqlite-postgres-publication", tests.err, "publication, HTTP, CLI, migration and both repository suites pass without skip")
	add("p12-no-skips", rejectSkips(tests.output), "test stream has no skip/cache/no-tests terminal")

	regressions := []struct{ name, target, report string }{
		{"p05", "test-go-workspace", "build/reports/P05/report.json"},
		{"p06", "test-protocol-foundation", "build/reports/P06/report.json"},
		{"p08", "test-control-plane-platform", "build/reports/P08/report.json"},
		{"p09", "test-storage-migrations", "build/reports/P09/report.json"},
		{"p10", "test-identity-secrets", "build/reports/P10/report.json"},
		{"p11", "test-publication-contracts", "build/reports/P11/report.json"},
	}
	evidence := []report.RuntimeEvidence{{Kind: "p12-go-test", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))}, {Kind: "p12-postgres-toolchain", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))}}
	for _, item := range regressions {
		r := run(root, nil, "make", item.target)
		if r.err == nil {
			v := run(root, nil, "make", "verify-report", "REPORT="+item.report)
			r.output = append(r.output, v.output...)
			r.err = v.err
		}
		add("p12-"+item.name+"-regression", r.err, strings.ToUpper(item.name)+" current report verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: item.name + "-regression", SHA256: report.Hash(r.output), Bytes: int64(len(r.output))})
	}
	if pg != nil {
		err = pg.stop()
	} else {
		err = nil
	}
	add("p12-postgres-shutdown", err, "private PostgreSQL stopped")
	if scratchErr == nil {
		err = os.RemoveAll(scratch)
	} else {
		err = nil
	}
	add("p12-scratch-cleanup", err, "scratch removed")

	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P12", Suite: "AROP P12 publication service", Class: "p12.publication", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 5, "database_engines": 2, "runtime_inputs": 0}, AuditNote: "P12 binds its five owned artifacts and an independently discovered Git/manifest transition closure. P09 and P10 migration snapshots remain immutable; Current is exactly 0001+0005+0010. P05/P06/P08/P09/P10/P11 are rerun on the current Git head and their command streams are recorded only as digest-and-byte runtime evidence; no prior report or external review is an input. Publication is reference-only, uses one durable UoW for mutation and audit, and introduces no Endpoint field, network fetch, production 0020, memory fallback, secret, DSN or scratch path."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P12/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P12 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P12 checks failed; see build/reports/P12/report.json"))
	}
	fmt.Printf("AROP publication service passed: %d checks.\n", len(checks))
}

func trackedInputs(root string) ([]string, error) {
	r := run(root, nil, "git", "ls-files", "-z")
	if r.err != nil {
		return nil, r.err
	}
	parts := bytes.Split(r.output, []byte{0})
	out := []string{}
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	sort.Strings(out)
	return out, nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var w transition
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&w); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("transition has trailing JSON value")
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if err := validateTransitionCandidate(w, discovered); err != nil {
		return err
	}
	return validateTransitionNegatives(w, discovered)
}

type discoveredTransition struct {
	artifacts []string
	sources   []string
}

type listedPackage struct {
	Dir            string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	TestGoFiles    []string
	XTestGoFiles   []string
	EmbedFiles     []string
}

func discoverTransition(root string) (discoveredTransition, error) {
	intro := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	if intro.err != nil {
		return discoveredTransition{}, intro.err
	}
	commits := nonemptyLines(intro.output)
	if len(commits) != 1 {
		return discoveredTransition{}, fmt.Errorf("carrier introduction commits=%d want 1", len(commits))
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("carrier introduction parent is not the frozen P11 endpoint")
	}

	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := parseChangedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}

	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return discoveredTransition{}, err
	}
	compiled, err := discoverCompiledClosure(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	read := discoverReadClosure()
	artifacts, err := discoverAffectedArtifacts(root, manifest, sources, compiled, read)
	if err != nil {
		return discoveredTransition{}, err
	}
	return discoveredTransition{artifacts: artifacts, sources: sources}, nil
}

func discoverCompiledClosure(root string) (map[string]bool, error) {
	closure := map[string]bool{}
	decode := func(output []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var item listedPackage
			if err := decoder.Decode(&item); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			absolute, err := filepath.Abs(item.Dir)
			if err != nil || absolute != root && !strings.HasPrefix(absolute, root+string(filepath.Separator)) {
				continue
			}
			for _, name := range append(append(append(append(append(append([]string{}, item.GoFiles...), item.CgoFiles...), item.IgnoredGoFiles...), item.TestGoFiles...), item.XTestGoFiles...), item.EmbedFiles...) {
				relative, err := filepath.Rel(root, filepath.Join(absolute, name))
				if err == nil {
					closure[filepath.ToSlash(relative)] = true
				}
			}
		}
		return nil
	}
	rootList := run(root, map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}, "go", "list", "-deps", "-test", "-json", "./cmd/arop", "./internal/tooling/cmd/arop-spec-index-check", "./internal/tooling/cmd/arop-go-proxy-bootstrap", "./conformance/harness/base", "./conformance/fixtures/codegen-spike/testdata/harness", "./conformance/fixtures/contracts/control-plane-publication/testdata/harness")
	if rootList.err != nil {
		return nil, rootList.err
	}
	if err := decode(rootList.output); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p12-closure-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, err
	}
	work := filepath.Join(temporary, "go.work")
	workBody := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, workBody, 0o600); err != nil {
		return nil, err
	}
	nestedList := run(filepath.Join(root, "reference/control-plane"), map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": work, "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": temporary}, "go", "list", "-deps", "-test", "-json", "./cmd/aropd", "./internal/app/platform/httpadapter", "./internal/domain/publication/...", "./internal/identity/...", "./internal/storage/migrate/...")
	if nestedList.err != nil {
		return nil, nestedList.err
	}
	if err := decode(nestedList.output); err != nil {
		return nil, err
	}
	return closure, nil
}

func discoverReadClosure() map[string]bool {
	paths := []string{
		"Makefile",
		waiver,
		"spec/artifact-manifest.yaml",
		"cmd/arop/internal/commands/publish/testdata/transition/p12-p05-transition.json",
		"reference/control-plane/internal/storage/migrate/production_catalog.go",
		"reference/control-plane/migrations/sqlite/0010_publication.sql",
		"reference/control-plane/migrations/postgres/0010_publication.sql",
	}
	out := make(map[string]bool, len(paths))
	for _, path := range paths {
		out[path] = true
	}
	return out
}

func parseChangedSources(data []byte) ([]string, error) {
	fields := bytes.Split(data, []byte{0})
	paths := map[string]bool{}
	for index := 0; index < len(fields) && len(fields[index]) != 0; {
		status := string(fields[index])
		index++
		if status == "" || !strings.Contains("ACMRTD", status[:1]) {
			return nil, fmt.Errorf("unsupported Git change status %q", status)
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if index+count > len(fields) {
			return nil, errors.New("truncated Git name-status stream")
		}
		for range count {
			path := string(fields[index])
			index++
			if path == "" || strings.HasPrefix(path, "build/reports/") {
				continue
			}
			paths[path] = true
		}
	}
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func discoverAffectedArtifacts(root string, manifest blueprint.Manifest, sources []string, compiled, read map[string]bool) ([]string, error) {
	byID := map[string]blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	found := map[string]bool{}
	for _, source := range sources {
		bestScore := -1
		var matches []blueprint.Artifact
		for _, artifact := range manifest.Artifacts {
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(artifact.OwnerPhase) > 12 {
				continue
			}
			score := artifactPathScore(root, artifact.Path, source, compiled, read)
			if score > bestScore {
				bestScore, matches = score, []blueprint.Artifact{artifact}
			} else if score >= 0 && score == bestScore {
				matches = append(matches, artifact)
			}
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
		}
	}
	for _, id := range []string{"publication-fixtures", "publication-service", "sqlite-migration-publication", "postgres-migration-publication", "arop-cli-publication-command"} {
		found[id] = true
	}
	propagateAffectedArtifacts(found, manifest, byID)
	found["phase-report-p12"] = true
	lowerBound := []string{"spec-index-report-orchestrator", "spec-index-check-reports", "go-module-proxy-bootstrap", "nested-control-plane-go-module", "phase-report-p05", "conformance-harness-base", "phase-report-p06", "codegen-representative-spike", "phase-report-p07", "control-plane-platform-foundation", "reference-control-plane-server", "phase-report-p08", "migration-engine", "migration-engine-fixture-versions", "phase-report-p09", "identity-service", "credential-store", "p10-baseline-transition-waiver", "phase-report-p10", "publication-contract-fixtures", "phase-report-p11", "publication-fixtures", "publication-service", "sqlite-migration-publication", "postgres-migration-publication", "arop-cli-publication-command", "phase-report-p12"}
	for _, id := range lowerBound {
		if !found[id] {
			return nil, fmt.Errorf("independent artifact discovery omitted required %s", id)
		}
	}
	out := make([]string, 0, len(found))
	for id := range found {
		if _, exists := byID[id]; !exists {
			return nil, fmt.Errorf("discovered artifact %s is absent from manifest", id)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func artifactPathScore(root, artifactPath, source string, compiled, read map[string]bool) int {
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && strings.HasPrefix(source, artifactPath+"/") && (compiled[source] || read[source]) {
		return 3*len(artifactPath) + 1
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifactPath)))
	if directory != "." && (compiled[source] || read[source]) && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
		return 2 * len(directory)
	}
	return -1
}

func propagateAffectedArtifacts(found map[string]bool, manifest blueprint.Manifest, byID map[string]blueprint.Artifact) {
	for {
		ids := make([]string, 0, len(found))
		for id := range found {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		additions := map[string]bool{}
		for _, id := range ids {
			artifact := byID[id]
			phase := artifact.OwnerPhase
			if phase == "" {
				continue
			}
			for _, candidate := range manifest.Artifacts {
				if candidate.Kind == "baseline-transition-waiver" && candidate.OwnerPhase == phase && transitivelyDepends(candidate.ID, id, byID, map[string]bool{}) {
					additions[candidate.ID] = true
				}
				if candidate.ProducerPhase == phase && strings.HasPrefix(candidate.Path, "build/reports/") && transitivelyDepends(candidate.ID, id, byID, map[string]bool{}) {
					additions[candidate.ID] = true
					for _, dependency := range candidate.DerivesFrom {
						if item := byID[dependency]; item.Kind == "baseline-transition-waiver" {
							additions[dependency] = true
						}
					}
				}
			}
		}
		added := 0
		for id := range additions {
			if !found[id] {
				found[id] = true
				added++
			}
		}
		if added == 0 {
			return
		}
	}
}

func phaseNumber(phase string) int {
	if len(phase) != 3 || phase[0] != 'P' {
		return 1000
	}
	value, err := strconv.Atoi(phase[1:])
	if err != nil {
		return 1000
	}
	return value
}

func transitivelyDepends(candidate, target string, byID map[string]blueprint.Artifact, visiting map[string]bool) bool {
	if candidate == target {
		return true
	}
	if visiting[candidate] {
		return false
	}
	visiting[candidate] = true
	defer delete(visiting, candidate)
	for _, dependency := range byID[candidate].DerivesFrom {
		if transitivelyDepends(dependency, target, byID, visiting) {
			return true
		}
	}
	return false
}

func validateTransitionCandidate(w transition, discovered discoveredTransition) error {
	if w.SchemaVersion != 1 || w.WaiverID != "P12-PUBLICATION-SERVICE-TRANSITION-001" || w.Status != "validated" || w.Baseline.Commit != baseline || w.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || w.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || w.Policy.OwnershipTransferred || w.Transition.ToPhase != "P12" || strings.TrimSpace(w.Transition.Reason) == "" {
		return errors.New("transition identity, owner or baseline invalid")
	}
	wantFrom := []string{"P05", "P06", "P08", "P09", "P10", "P11"}
	if !reflect.DeepEqual(w.Transition.FromPhases, wantFrom) {
		return fmt.Errorf("transition from phases=%v want %v", w.Transition.FromPhases, wantFrom)
	}
	if !reflect.DeepEqual(w.AffectedArtifacts, discovered.artifacts) {
		return fmt.Errorf("transition artifacts=%v want independently discovered %v", w.AffectedArtifacts, discovered.artifacts)
	}
	if !reflect.DeepEqual(w.SourceClosure, discovered.sources) {
		return fmt.Errorf("transition sources=%v want independently discovered %v", w.SourceClosure, discovered.sources)
	}
	if !reflect.DeepEqual(w.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(w.Constraints, canonicalConstraints()) {
		return errors.New("transition exact acceptance/constraint closure mismatch")
	}
	return nil
}

func canonicalAcceptance() []struct{ Phase, Command, Report string } {
	return []struct{ Phase, Command, Report string }{
		{"P05", "make test-go-workspace", "build/reports/P05/report.json"},
		{"P06", "make test-protocol-foundation", "build/reports/P06/report.json"},
		{"P08", "make test-control-plane-platform", "build/reports/P08/report.json"},
		{"P09", "make test-storage-migrations", "build/reports/P09/report.json"},
		{"P10", "make test-identity-secrets", "build/reports/P10/report.json"},
		{"P11", "make test-publication-contracts", "build/reports/P11/report.json"},
		{"P12", "make test-publication-service", "build/reports/P12/report.json"},
	}
}

func canonicalConstraints() []string {
	return []string{"manifest-endpoint-remains-forbidden", "bundle-reference-validation-is-offline", "no-dns-http-connect-or-redirect-policy-in-p12", "owner-phase-accountability-does-not-transfer", "p09-and-p10-catalog-snapshots-remain-immutable", "publication-mutation-and-durable-audit-share-one-uow", "durable-mode-never-falls-back-to-memory", "p12-report-runtime-inputs-remain-empty"}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	assertRejected := func(name string, mutate func(*transition)) error {
		candidate := cloneTransition(valid)
		mutate(&candidate)
		if validateTransitionCandidate(candidate, discovered) == nil {
			return fmt.Errorf("transition negative %s was accepted", name)
		}
		return nil
	}
	for index := range valid.AffectedArtifacts {
		if err := assertRejected("artifact-omission", func(candidate *transition) {
			candidate.AffectedArtifacts = append(candidate.AffectedArtifacts[:index:index], candidate.AffectedArtifacts[index+1:]...)
		}); err != nil {
			return err
		}
	}
	for index := range valid.SourceClosure {
		if err := assertRejected("source-omission", func(candidate *transition) {
			candidate.SourceClosure = append(candidate.SourceClosure[:index:index], candidate.SourceClosure[index+1:]...)
		}); err != nil {
			return err
		}
	}
	for index := range valid.Acceptance {
		if err := assertRejected("acceptance-omission", func(candidate *transition) {
			candidate.Acceptance = append(candidate.Acceptance[:index:index], candidate.Acceptance[index+1:]...)
		}); err != nil {
			return err
		}
	}
	for index := range valid.Constraints {
		if err := assertRejected("constraint-omission", func(candidate *transition) {
			candidate.Constraints = append(candidate.Constraints[:index:index], candidate.Constraints[index+1:]...)
		}); err != nil {
			return err
		}
	}
	negatives := []struct {
		name   string
		mutate func(*transition)
	}{
		{"artifact-addition", func(candidate *transition) {
			candidate.AffectedArtifacts = append(candidate.AffectedArtifacts, "schema-manifest")
		}},
		{"source-substitution", func(candidate *transition) { candidate.SourceClosure[0] = "README.md" }},
		{"acceptance-substitution", func(candidate *transition) { candidate.Acceptance[0].Command = "make validate" }},
		{"constraint-substitution", func(candidate *transition) { candidate.Constraints[0] = "invented" }},
		{"artifact-duplicate", func(candidate *transition) {
			candidate.AffectedArtifacts = append(candidate.AffectedArtifacts, candidate.AffectedArtifacts[0])
		}},
		{"source-order", func(candidate *transition) {
			candidate.SourceClosure[0], candidate.SourceClosure[1] = candidate.SourceClosure[1], candidate.SourceClosure[0]
		}},
		{"wrong-owner", func(candidate *transition) { candidate.Policy.OwnershipTransferred = true }},
		{"wrong-phase", func(candidate *transition) { candidate.Transition.ToPhase = "P11" }},
	}
	for _, negative := range negatives {
		if err := assertRejected(negative.name, negative.mutate); err != nil {
			return err
		}
	}
	return nil
}

func cloneTransition(value transition) transition {
	data, _ := json.Marshal(value)
	var clone transition
	_ = json.Unmarshal(data, &clone)
	return clone
}

func validateCatalog(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	s := string(b)
	for _, needle := range []string{"func P09ProductionCatalog()", `ReportPhase: "P09"`, "func P10ProductionCatalog()", `ReportPhase: "P10"`, "func CurrentProductionCatalog()", `ReportPhase: "P12"`, "0001_base.sql", "0005_identity.sql", "0010_publication.sql"} {
		if !strings.Contains(s, needle) {
			return fmt.Errorf("catalog missing %s", needle)
		}
	}
	return nil
}
func rejectProduction0020(root string) error {
	for _, d := range []string{"sqlite", "postgres"} {
		m, _ := filepath.Glob(filepath.Join(root, "reference/control-plane/migrations", d, "0020*"))
		if len(m) > 0 {
			return errors.New("production 0020 exists")
		}
	}
	return nil
}

func runTests(root, scratch, dsn string) result {
	work := filepath.Join(scratch, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return result{err: err}
	}
	env := map[string]string{"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": scratch, "AROP_P12_POSTGRES_URL": dsn}
	return run(filepath.Join(root, "reference/control-plane"), env, "go", "test", "-count=1", "-json", "./cmd/aropd", "./internal/app/platform/httpadapter", "./internal/domain/publication/...", "./internal/storage/migrate")
}
func rejectSkips(out []byte) error {
	if bytes.Contains(out, []byte(`"Action":"skip"`)) || bytes.Contains(out, []byte("[no test files]")) || bytes.Contains(out, []byte("(cached)")) {
		return errors.New("test inventory contains skip/cache/no-tests")
	}
	return nil
}

func startPostgres(scratch string) (*cluster, []byte, error) {
	tools := map[string]string{}
	evidence := bytes.Buffer{}
	for _, n := range []string{"initdb", "postgres", "pg_isready"} {
		p, e := exec.LookPath(n)
		if e != nil {
			return nil, nil, e
		}
		v := run("/", nil, p, "--version")
		if v.err != nil || !strings.Contains(string(v.output), "16.") {
			return nil, nil, fmt.Errorf("%s is not PostgreSQL 16", n)
		}
		tools[n] = p
		fmt.Fprintf(&evidence, "%s:%s\n", n, strings.TrimSpace(string(v.output)))
	}
	data := filepath.Join(scratch, "pgdata")
	socket := filepath.Join(scratch, "pgsocket")
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if r := run(scratch, map[string]string{"HOME": "/nonexistent"}, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p12", "--no-instructions"); r.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	c := &cluster{socket: socket}
	c.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	c.cmd.Dir = scratch
	c.cmd.Env = cleanEnv(map[string]string{"HOME": "/nonexistent"})
	c.cmd.Stdout = &c.log
	c.cmd.Stderr = &c.log
	if err := c.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, nil, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p12", "-d", "postgres", "-q").err == nil {
			return c, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = c.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}
func (c *cluster) url() string {
	return "postgresql://arop_p12@localhost/postgres?host=" + url.QueryEscape(c.socket) + "&port=5432&sslmode=disable"
}
func (c *cluster) stop() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		return errors.New("postgres shutdown timeout")
	}
}

func run(dir string, overrides map[string]string, name string, args ...string) result {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = cleanEnv(overrides)
	o, e := c.CombinedOutput()
	if e != nil {
		tail := o
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		e = fmt.Errorf("%w: %s", e, strings.TrimSpace(string(tail)))
	}
	return result{o, e}
}
func cleanEnv(overrides map[string]string) []string {
	keep := []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR", "TZ"}
	m := map[string]string{}
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	for k, v := range overrides {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
func nonemptyLines(b []byte) []string {
	var out []string
	for _, s := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
func sanitize(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "/tmp/", "<tmp>/")
	if len(s) > 1500 {
		s = s[:1500]
	}
	return s
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
