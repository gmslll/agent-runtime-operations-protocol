// Command harness is the sole writer of the P14 registry core report.
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
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-registry-core"
	checker  = "reference/control-plane/internal/domain/registry/testdata/harness/main.go"
	waiver   = "reference/control-plane/internal/domain/registry/testdata/transition/p14-baseline-transition-waiver.json"
	baseline = "958d1d42bb7b4a3f6c015ad97004b434ba07d2d3"
)

var requiredOwnedArtifacts = []string{
	"postgres-migration-registry",
	"registry-core-service",
	"registry-fixtures",
	"schema-registry-event",
	"schema-registry-lease",
	"schema-registry-runtime-instance",
	"sqlite-migration-registry",
}

type commandResult struct {
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
	Baseline struct {
		Rule, Commit string
	} `json:"baseline"`
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

type discoveredTransition struct {
	sources, artifacts []string
}

type listedPackage struct {
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	EmbedFiles   []string
}

type fixtureCases struct {
	SchemaVersion int `json:"schema_version"`
	Valid         []struct {
		Schema, Document string
	} `json:"valid"`
	InvalidMutations []struct {
		Name, Document, Pointer string
		Value                   any
	} `json:"invalid_mutations"`
	DiscoveryEligibilityDimensions []string `json:"discovery_eligibility_dimensions"`
	FencingNegativeDimensions      []string `json:"fencing_negative_dimensions"`
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
	inputs, err := trackedInputs(root)
	add("p14-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, runtimeErr := declaredRuntimeInputs(root)
	add("p14-runtime-inputs-empty", errors.Join(runtimeErr, requireEmpty(runtimeInputs)), "manifest-declared runtime_inputs is empty")
	ownedErr := validateOwnedArtifacts(root)
	add("p14-owned-artifact-inventory", ownedErr, "manifest declares the exact seven P14-owned artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P14_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p14-transition-waiver", transitionErr, "waiver exactly binds Git changes, manifest owners, acceptance and constraints")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p14-schema-fixtures", fixtureErr, "three registry schemas and all declared positive/negative fixtures pass offline validation")
	add("p14-production-catalog", validateCatalog(root), "P13 snapshot is frozen and production catalog ends at paired 0030")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p14-")
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p14-private-scratch", scratchErr, "private scratch created")
	var pg *cluster
	var pgEvidence []byte
	if scratchErr == nil {
		pg, pgEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p14-private-postgres16", err, "private PostgreSQL 16 ready on a private Unix socket")
	var tests commandResult
	if pg != nil {
		tests = runTests(root, scratch, pg.dsn())
	} else {
		tests.err = errors.New("PostgreSQL prerequisite failed")
	}
	add("p14-registry-domain-storage-migrations", tests.err, "domain, dual repositories, exact schemas and migration transition pass with race detection")
	add("p14-no-skips-cache-or-no-tests", rejectIncompleteTests(tests.output), "test stream has no skip/cache/no-tests terminal")

	evidence := []report.RuntimeEvidence{
		{Kind: "p14-go-test", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p14-postgres-toolchain", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))},
		{Kind: "p14-fixture-validation", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
	}
	p13 := run(root, nil, "make", "test-asset-broker")
	if p13.err == nil {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P13/report.json")
		p13.output = append(p13.output, verified.output...)
		p13.err = verified.err
	}
	add("p14-p13-regression", p13.err, "P13 and its P05-P12 regression chain pass on the current head")
	evidence = append(evidence, report.RuntimeEvidence{Kind: "p13-regression", SHA256: report.Hash(p13.output), Bytes: int64(len(p13.output))})
	for _, phase := range []string{"P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12"} {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p14-"+strings.ToLower(phase)+"-regression", verified.err, phase+" current report verified after P13")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-regression", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}
	if pg != nil {
		err = pg.stop()
	} else {
		err = errors.New("private PostgreSQL was not started")
	}
	add("p14-postgres-shutdown", err, "private PostgreSQL stopped")
	if scratchErr == nil {
		err = os.RemoveAll(scratch)
		if err == nil {
			if _, statErr := os.Lstat(scratch); !errors.Is(statErr, os.ErrNotExist) {
				err = errors.New("private scratch still exists after cleanup")
			}
		}
	} else {
		err = errors.New("private scratch was not created")
	}
	add("p14-scratch-cleanup", err, "scratch removed")

	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P14", Suite: "AROP P14 registry core", Class: "p14.registry",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 7, "database_engines": 2, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P14 binds the exact seven manifest-owned artifacts and an independently discovered Git/manifest transition closure. Server time owns lease expiry; generation/session/lease/resource fences reject stale writers; state, append-only event and revision share one UoW; both databases enforce exact schema parity and the same repository/migration matrix. P05-P13 regressions enter only as digest-and-byte runtime evidence and runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P14/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P14 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P14 checks failed; see build/reports/P14/report.json"))
	}
	fmt.Printf("AROP registry core passed: %d checks.\n", len(checks))
}

func declaredRuntimeInputs(root string) ([]string, error) {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	var found []string
	matches := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p14" {
			matches++
			found = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("phase-report-p14 manifest entries=%d", matches)
	}
	return found, nil
}

func requireEmpty(items []string) error {
	if len(items) != 0 {
		return fmt.Errorf("runtime_inputs must be empty, got %q", items)
	}
	return nil
}

func validateOwnedArtifacts(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	var found []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P14" {
			found = append(found, artifact.ID)
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-registry-core" {
				return fmt.Errorf("P14 artifact %s has invalid ownership metadata", artifact.ID)
			}
		}
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, requiredOwnedArtifacts) {
		return fmt.Errorf("P14 owned artifacts=%v want=%v", found, requiredOwnedArtifacts)
	}
	return nil
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	var paths []string
	for _, item := range bytes.Split(result.output, []byte{0}) {
		if len(item) != 0 {
			paths = append(paths, string(item))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var value transition
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("transition has trailing JSON value")
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P14_TRANSITION") == "1" {
		fmt.Printf("sources=%q\nartifacts=%q\n", discovered.sources, discovered.artifacts)
	}
	if err := validateTransitionCandidate(value, discovered); err != nil {
		return err
	}
	return validateTransitionNegatives(value, discovered)
}

func discoverTransition(root string) (discoveredTransition, error) {
	intro := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	commits := lines(intro.output)
	if intro.err != nil || len(commits) != 1 {
		return discoveredTransition{}, errors.New("transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("transition carrier introduction parent is not the frozen P13 endpoint")
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := parseChangedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}
	compiled, err := discoverCompiledClosure(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return discoveredTransition{}, err
	}
	artifacts, err := discoverAffectedArtifacts(root, manifest, sources, compiled, discoverReadClosure())
	if err != nil {
		return discoveredTransition{}, err
	}
	return discoveredTransition{sources: sources, artifacts: artifacts}, nil
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
			if path != "" && !strings.HasPrefix(path, "build/reports/") {
				paths[path] = true
			}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func discoverCompiledClosure(root string) (map[string]bool, error) {
	closure := map[string]bool{
		"go.mod": true, "go.sum": true,
		"reference/control-plane/go.mod": true, "reference/control-plane/go.sum": true,
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p14-closure-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, err
	}
	work := filepath.Join(temporary, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return nil, err
	}
	listed := runStdout(filepath.Join(root, "reference/control-plane"), map[string]string{"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": temporary}, "go", "list", "-deps", "-test", "-json",
		"./cmd/aropd", "./internal/domain/registry/...", "./internal/domain/registry/testdata/acceptance", "./internal/domain/registry/testdata/harness",
		"./internal/domain/assets/testdata/acceptance", "./internal/domain/assets/testdata/harness",
		"./internal/domain/publication/testdata/harness",
		"./internal/identity/testdata/acceptance", "./internal/identity/testdata/harness", "./internal/storage/migrate/...", "./internal/storage/migrate/testdata/engine-versions/harness")
	if listed.err != nil {
		return nil, listed.err
	}
	decoder := json.NewDecoder(bytes.NewReader(listed.output))
	for {
		var item listedPackage
		if err := decoder.Decode(&item); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(item.Dir)
		if err != nil || absolute != root && !strings.HasPrefix(absolute, root+string(filepath.Separator)) {
			continue
		}
		files := append(append(append(append(append([]string{}, item.GoFiles...), item.CgoFiles...), item.TestGoFiles...), item.XTestGoFiles...), item.EmbedFiles...)
		for _, name := range files {
			relative, err := filepath.Rel(root, filepath.Join(absolute, name))
			if err == nil {
				closure[filepath.ToSlash(relative)] = true
			}
		}
	}
	return closure, nil
}

func discoverReadClosure() map[string]bool {
	paths := []string{
		"Makefile", waiver, "spec/artifact-manifest.yaml",
		"reference/control-plane/internal/storage/migrate/PRODUCTION_CATALOG.md",
		"reference/control-plane/internal/storage/migrate/production_catalog.go",
		"reference/control-plane/migrations/sqlite/0030_registry.sql",
		"reference/control-plane/migrations/postgres/0030_registry.sql",
		"conformance/fixtures/registry/cases.json",
		"schemas/registry/runtime-instance-v1.schema.json",
		"schemas/registry/lease-v1.schema.json",
		"schemas/registry/registry-event-v1.schema.json",
	}
	result := make(map[string]bool, len(paths))
	for _, path := range paths {
		result[path] = true
	}
	return result
}

func discoverAffectedArtifacts(root string, manifest blueprint.Manifest, sources []string, compiled, read map[string]bool) ([]string, error) {
	byID := map[string]blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	found := map[string]bool{}
	for _, source := range sources {
		if source == "Makefile" {
			matched := false
			for _, artifact := range manifest.Artifacts {
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-registry-core" && phaseNumber(artifact.OwnerPhase) <= 14 {
					found[artifact.ID] = true
					matched = true
				}
			}
			if !matched {
				return nil, errors.New("changed Makefile has no P14 acceptance artifact")
			}
			continue
		}
		best := -1
		var matches []blueprint.Artifact
		for _, artifact := range manifest.Artifacts {
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(artifact.OwnerPhase) > 14 {
				continue
			}
			score := artifactPathScore(root, artifact.Path, source, compiled[source] || read[source])
			if score > best {
				best, matches = score, []blueprint.Artifact{artifact}
			} else if score >= 0 && score == best {
				matches = append(matches, artifact)
			}
		}
		if best < 0 || len(matches) == 0 {
			return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
		}
	}
	propagateAffectedArtifacts(found, manifest, byID)
	for _, id := range append(append([]string{}, requiredOwnedArtifacts...), "phase-report-p14") {
		if !found[id] {
			return nil, fmt.Errorf("independent artifact discovery omitted required %s", id)
		}
	}
	result := make([]string, 0, len(found))
	for id := range found {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("discovered artifact %s is absent from manifest", id)
		}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func artifactPathScore(root, artifactPath, source string, consumed bool) int {
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && strings.HasPrefix(source, artifactPath+"/") {
		return 3*len(artifactPath) + 1
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifactPath)))
	if consumed && directory != "." && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
		return 2 * len(directory)
	}
	return -1
}

func propagateAffectedArtifacts(found map[string]bool, manifest blueprint.Manifest, byID map[string]blueprint.Artifact) {
	for {
		added := 0
		for _, candidate := range manifest.Artifacts {
			phase := candidate.OwnerPhase
			if phase == "" {
				phase = candidate.ProducerPhase
			}
			if phaseNumber(phase) > 14 || candidate.PathRole != "concrete" {
				continue
			}
			for id := range found {
				if transitivelyDepends(candidate.ID, id, byID, map[string]bool{}) && !found[candidate.ID] {
					found[candidate.ID] = true
					added++
					break
				}
			}
		}
		if added == 0 {
			return
		}
	}
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

func validateTransitionCandidate(value transition, discovered discoveredTransition) error {
	wantFrom := []string{"P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13"}
	if value.SchemaVersion != 1 || value.WaiverID != "P14-REGISTRY-CORE-TRANSITION-001" || value.Status != "validated" || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Transition.ToPhase != "P14" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
		return errors.New("transition identity, owner or baseline is invalid")
	}
	if !reflect.DeepEqual(value.SourceClosure, discovered.sources) || !reflect.DeepEqual(value.AffectedArtifacts, discovered.artifacts) {
		return fmt.Errorf("transition exact closure mismatch: sources=%q artifacts=%q", discovered.sources, discovered.artifacts)
	}
	if !reflect.DeepEqual(value.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(value.Constraints, canonicalConstraints()) {
		return errors.New("transition acceptance or constraint closure mismatch")
	}
	return nil
}

func canonicalAcceptance() []struct{ Phase, Command, Report string } {
	return []struct{ Phase, Command, Report string }{
		{"P05", "make test-go-workspace", "build/reports/P05/report.json"},
		{"P06", "make test-protocol-foundation", "build/reports/P06/report.json"},
		{"P07", "make test-codegen-pipeline", "build/reports/P07/report.json"},
		{"P08", "make test-control-plane-platform", "build/reports/P08/report.json"},
		{"P09", "make test-storage-migrations", "build/reports/P09/report.json"},
		{"P10", "make test-identity-secrets", "build/reports/P10/report.json"},
		{"P11", "make test-publication-contracts", "build/reports/P11/report.json"},
		{"P12", "make test-publication-service", "build/reports/P12/report.json"},
		{"P13", "make test-asset-broker", "build/reports/P13/report.json"},
		{"P14", command, "build/reports/P14/report.json"},
	}
}

func canonicalConstraints() []string {
	return []string{
		"registry-time-and-lease-expiry-use-only-the-server-clock",
		"revision-event-watermark-and-state-mutate-in-one-uow",
		"stale-session-lease-or-generation-is-fenced",
		"session-identifiers-are-never-reused",
		"draining-remains-latched-across-reregistration",
		"discovery-is-fail-closed-and-conjunctive",
		"public-integers-never-exceed-the-js-safe-maximum",
		"p09-p10-p12-and-p13-catalog-snapshots-remain-immutable",
		"production-catalog-ends-at-0030",
		"owner-phase-accountability-does-not-transfer",
		"p14-report-runtime-inputs-remain-empty",
	}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	type mutation struct {
		name string
		fn   func(*transition)
	}
	mutations := []mutation{}
	appendGroup := func(name string, length int, omit, substitute func(*transition, int), add, duplicate func(*transition)) {
		for index := 0; index < length; index++ {
			i := index
			mutations = append(mutations,
				mutation{name + "-omission", func(value *transition) { omit(value, i) }},
				mutation{name + "-substitution", func(value *transition) { substitute(value, i) }},
			)
		}
		mutations = append(mutations, mutation{name + "-addition", add}, mutation{name + "-duplicate", duplicate})
	}
	appendGroup("source", len(valid.SourceClosure),
		func(v *transition, i int) { v.SourceClosure = append(v.SourceClosure[:i:i], v.SourceClosure[i+1:]...) },
		func(v *transition, i int) { v.SourceClosure[i] = "unexpected/source" },
		func(v *transition) { v.SourceClosure = append(v.SourceClosure, "unexpected/source") },
		func(v *transition) { v.SourceClosure = append(v.SourceClosure, v.SourceClosure[0]) })
	appendGroup("artifact", len(valid.AffectedArtifacts),
		func(v *transition, i int) {
			v.AffectedArtifacts = append(v.AffectedArtifacts[:i:i], v.AffectedArtifacts[i+1:]...)
		},
		func(v *transition, i int) { v.AffectedArtifacts[i] = "unexpected-artifact" },
		func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, "unexpected-artifact") },
		func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, v.AffectedArtifacts[0]) })
	appendGroup("acceptance", len(valid.Acceptance),
		func(v *transition, i int) { v.Acceptance = append(v.Acceptance[:i:i], v.Acceptance[i+1:]...) },
		func(v *transition, i int) { v.Acceptance[i].Command = "make unexpected" },
		func(v *transition) {
			v.Acceptance = append(v.Acceptance, struct{ Phase, Command, Report string }{"P99", "make unexpected", "build/reports/P99/report.json"})
		},
		func(v *transition) { v.Acceptance = append(v.Acceptance, v.Acceptance[0]) })
	appendGroup("constraint", len(valid.Constraints),
		func(v *transition, i int) { v.Constraints = append(v.Constraints[:i:i], v.Constraints[i+1:]...) },
		func(v *transition, i int) { v.Constraints[i] = "unexpected-constraint" },
		func(v *transition) { v.Constraints = append(v.Constraints, "unexpected-constraint") },
		func(v *transition) { v.Constraints = append(v.Constraints, v.Constraints[0]) })
	for _, candidate := range mutations {
		copy := cloneTransition(valid)
		candidate.fn(&copy)
		if validateTransitionCandidate(copy, discovered) == nil {
			return fmt.Errorf("transition negative %s was accepted", candidate.name)
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

func validateFixtures(root string) ([]byte, error) {
	var cases fixtureCases
	if err := structuredfile.Load(filepath.Join(root, "conformance/fixtures/registry/cases.json"), &cases); err != nil {
		return nil, err
	}
	if cases.SchemaVersion != 1 || len(cases.Valid) != 3 || len(cases.InvalidMutations) < 8 || !reflect.DeepEqual(cases.DiscoveryEligibilityDimensions, []string{"lease_alive", "healthy", "ready", "enabled", "not_draining", "capacity_available", "protocol_compatible", "binding_matches"}) || !reflect.DeepEqual(cases.FencingNegativeDimensions, []string{"stale_session", "stale_lease", "stale_generation", "reused_session", "stale_resource_version", "stale_heartbeat_sequence"}) {
		return nil, errors.New("registry fixture inventory is incomplete")
	}
	type validDocument struct {
		schema string
		value  any
	}
	documents := map[string]validDocument{}
	var evidence bytes.Buffer
	for _, item := range cases.Valid {
		value, raw, err := schema.ValidatePath(root, item.Schema, item.Document)
		if err != nil {
			return nil, err
		}
		documents[filepath.Base(item.Document)] = validDocument{schema: item.Schema, value: value}
		fmt.Fprintf(&evidence, "valid:%s:%s:%d\n", item.Schema, report.Hash(raw), len(raw))
	}
	seen := map[string]bool{}
	for _, item := range cases.InvalidMutations {
		if item.Name == "" || seen[item.Name] {
			return nil, errors.New("registry mutation names are not unique")
		}
		seen[item.Name] = true
		document, ok := documents[item.Document]
		if !ok {
			return nil, fmt.Errorf("mutation %s references unknown document", item.Name)
		}
		encoded, _ := json.Marshal(document.value)
		var copy any
		if json.Unmarshal(encoded, &copy) != nil || setJSONPointer(copy, item.Pointer, item.Value) != nil {
			return nil, fmt.Errorf("mutation %s has invalid pointer", item.Name)
		}
		if schema.ValidateFile(root, document.schema, copy) == nil {
			return nil, fmt.Errorf("invalid registry mutation %s was accepted", item.Name)
		}
		fmt.Fprintf(&evidence, "invalid:%s\n", item.Name)
	}
	return evidence.Bytes(), nil
}

func setJSONPointer(value any, pointer string, replacement any) error {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return errors.New("pointer must select a property")
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	current := value
	for index, raw := range parts {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return errors.New("pointer traverses non-object")
		}
		if index == len(parts)-1 {
			object[part] = replacement
			return nil
		}
		current, ok = object[part]
		if !ok {
			return errors.New("pointer property is missing")
		}
	}
	return errors.New("pointer is empty")
}

func validateCatalog(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"func P09ProductionCatalog()", "func P10ProductionCatalog()", "func P12ProductionCatalog()", "func P13ProductionCatalog()", "func CurrentProductionCatalog()", `ReportPhase: "P14"`, "0030_registry.sql"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("catalog missing %s", required)
		}
	}
	start, end := strings.Index(text, "func P13ProductionCatalog()"), strings.Index(text, "func CurrentProductionCatalog()")
	if start < 0 || end <= start || strings.Count(text[start:end], "0020_asset.sql") != 2 || strings.Contains(text[start:end], "0030_registry.sql") {
		return errors.New("P13 catalog snapshot is not exactly complete through 0020")
	}
	current := text[end:]
	if strings.Count(current, "0030_registry.sql") != 2 {
		return errors.New("P14 production catalog does not contain exactly paired 0030 migrations")
	}
	return nil
}

func runTests(root, scratch, dsn string) commandResult {
	realScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return commandResult{err: errors.New("resolve private scratch")}
	}
	work := filepath.Join(realScratch, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return commandResult{err: err}
	}
	environment := map[string]string{
		"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": realScratch,
		"AROP_P14_SCRATCH": realScratch, "AROP_P14_MIGRATION_ROOT": filepath.Join(root, "reference/control-plane/migrations"), "AROP_P14_POSTGRES_URL": dsn,
	}
	return run(filepath.Join(root, "reference/control-plane"), environment, "go", "test", "-count=1", "-race", "-json",
		"./internal/domain/registry", "./internal/domain/registry/storage/sqlite", "./internal/domain/registry/storage/postgres", "./internal/domain/registry/testdata/acceptance", "./internal/storage/migrate")
}

func rejectIncompleteTests(output []byte) error {
	for _, marker := range [][]byte{[]byte(`"Action":"skip"`), []byte("[no test files]"), []byte("(cached)")} {
		if bytes.Contains(output, marker) {
			return errors.New("test inventory contains skip/cache/no-tests")
		}
	}
	return nil
}

func startPostgres(scratch string) (*cluster, []byte, error) {
	tools := map[string]string{}
	var evidence bytes.Buffer
	for _, name := range []string{"initdb", "postgres", "pg_isready"} {
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, nil, err
		}
		version := run("/", nil, path, "--version")
		if version.err != nil || !strings.Contains(string(version.output), "16.") {
			return nil, nil, fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		tools[name] = path
		fmt.Fprintf(&evidence, "%s:%s\n", name, strings.TrimSpace(string(version.output)))
	}
	data := filepath.Join(scratch, "pgdata")
	socket := filepath.Join(scratch, "pgsocket")
	emptyPass := filepath.Join(scratch, "pgpass")
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if err := os.WriteFile(emptyPass, nil, 0o600); err != nil {
		return nil, evidence.Bytes(), err
	}
	pgEnv := map[string]string{"PGPASSFILE": emptyPass}
	if result := run(scratch, pgEnv, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p14", "--no-instructions"); result.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	instance := &cluster{socket: socket}
	instance.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	instance.cmd.Dir = scratch
	instance.cmd.Env = cleanEnvironment(pgEnv)
	instance.cmd.Stdout, instance.cmd.Stderr = &instance.log, &instance.log
	if err := instance.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, pgEnv, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p14", "-d", "postgres", "-q").err == nil {
			return instance, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = instance.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}

func (instance *cluster) dsn() string {
	return "postgresql://arop_p14@localhost/postgres?host=" + url.QueryEscape(instance.socket) + "&port=5432&sslmode=disable"
}

func (instance *cluster) stop() error {
	if instance == nil || instance.cmd == nil || instance.cmd.Process == nil {
		return nil
	}
	if err := instance.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- instance.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = instance.cmd.Process.Kill()
		<-done
		return errors.New("postgres shutdown timeout")
	}
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Env = cleanEnvironment(overrides)
	output, err := command.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func runStdout(directory string, overrides map[string]string, name string, args ...string) commandResult {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Env = cleanEnvironment(overrides)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		tail := stderr.Bytes()
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: stdout.Bytes(), err: err}
}

func cleanEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR", "TZ"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
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

func lines(data []byte) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func sanitize(err error) string {
	value := strings.ReplaceAll(err.Error(), "/tmp/", "<tmp>/")
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
