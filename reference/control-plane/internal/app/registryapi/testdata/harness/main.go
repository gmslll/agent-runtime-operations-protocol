// Command harness is the sole writer of the P15 registry API report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-registry-api"
	checker  = "reference/control-plane/internal/app/registryapi/testdata/harness/main.go"
	waiver   = "reference/control-plane/internal/app/registryapi/testdata/transition/p15-baseline-transition-waiver.json"
	baseline = "abb436d62ea9455f6691f6703837ef433ba5225f"
)

var requiredOwnedArtifacts = []string{
	"arop-cli-registration-command",
	"generated-registry-go",
	"generated-registry-python",
	"generated-registry-typescript",
	"go-registry-sdk",
	"openapi-discovery-runtime",
	"openapi-registry-runtime",
	"registry-api-service",
	"schema-discovery-snapshot",
}

type commandResult struct {
	output []byte
	err    error
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
		Rule   string `json:"rule"`
		Commit string `json:"commit"`
	} `json:"baseline"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string `json:"affected_artifacts"`
	SourceClosure     []string `json:"source_closure"`
	Acceptance        []struct {
		Phase   string `json:"phase"`
		Command string `json:"command"`
		Report  string `json:"report"`
	} `json:"acceptance"`
	Constraints []string `json:"constraints"`
}

type discoveredTransition struct {
	sources   []string
	artifacts []string
}

type listedPackage struct {
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	EmbedFiles   []string
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

	inputs, inputErr := trackedInputs(root)
	add("p15-static-input-closure", inputErr, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, runtimeErr := declaredRuntimeInputs(root)
	add("p15-runtime-inputs-empty", errors.Join(runtimeErr, requireEmpty(runtimeInputs)), "manifest-declared runtime_inputs is empty")
	add("p15-owned-artifact-inventory", validateOwnedArtifacts(root), "manifest declares the exact nine P15-owned artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P15_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p15-transition-waiver", transitionErr, "waiver exactly equals the independently discovered Git and manifest closure")

	contractEvidence, contractErr := validateContracts(root)
	add("p15-schema-openapi-contracts", contractErr, "snapshot schema, both OpenAPI contracts, offline refs and Watch boundary pass")
	generatedEvidence, generatedErr := validateGenerated(root)
	add("p15-generated-three-language-models", generatedErr, "clean generation has zero drift and Go/Python/TypeScript compile")
	goEvidence, goErr := runGoTests(root)
	add("p15-api-sdk-cli-tests", goErr, "API, outer auth boundary, SDK, lease helper, CLI and production composition pass with race detection")
	add("p15-no-skips-cache-or-no-tests", rejectIncompleteTests(goEvidence), "test stream has no skip/cache/no-tests terminal")

	p14 := run(root, nil, "make", "test-registry-core")
	if p14.err == nil {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P14/report.json")
		p14.output = append(p14.output, verified.output...)
		p14.err = verified.err
	}
	add("p15-p14-regression", p14.err, "P14 registry core and its P05-P13 chain pass on the current head")

	evidence := []report.RuntimeEvidence{
		{Kind: "p15-contract-validation", SHA256: report.Hash(contractEvidence), Bytes: int64(len(contractEvidence))},
		{Kind: "p15-generated-models", SHA256: report.Hash(generatedEvidence), Bytes: int64(len(generatedEvidence))},
		{Kind: "p15-go-tests", SHA256: report.Hash(goEvidence), Bytes: int64(len(goEvidence))},
		{Kind: "p14-execution", SHA256: report.Hash(p14.output), Bytes: int64(len(p14.output))},
	}
	for _, phase := range []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14"} {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p15-"+strings.ToLower(phase)+"-report", verified.err, phase+" current report verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-regression", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}

	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P15", Suite: "AROP P15 registry API", Class: "p15.registry.api",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs,
		RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 9, "implemented_routes": 6, "watch_registered": false, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P15 binds the exact nine manifest-owned artifacts and an independently discovered baseline transition. Snapshot is implemented while Watch remains an authenticated but unregistered P16 route. Credential scopes and tenant identity enter through P10 authentication, mutations own one durable UoW, server-authoritative fencing/lease results drive the Go helper, and generated models are reproduced and compiled in all three languages. Earlier reports enter only as digest-and-byte runtime evidence; runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P15/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P15 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P15 checks failed; see build/reports/P15/report.json"))
	}
	fmt.Printf("AROP registry API passed: %d checks.\n", len(checks))
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

func declaredRuntimeInputs(root string) ([]string, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return nil, err
	}
	var found []string
	matches := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p15" {
			matches++
			found = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("phase-report-p15 manifest entries=%d", matches)
	}
	return found, nil
}

func requireEmpty(items []string) error {
	if len(items) != 0 {
		return fmt.Errorf("runtime_inputs must be empty, got %q", items)
	}
	return nil
}

func loadManifest(root string) (blueprint.Manifest, error) {
	var manifest blueprint.Manifest
	err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest)
	return manifest, err
}

func validateOwnedArtifacts(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	var found []string
	var reportDependencies []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P15" {
			found = append(found, artifact.ID)
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-registry-api" {
				return fmt.Errorf("P15 artifact %s has invalid ownership metadata", artifact.ID)
			}
		}
		if artifact.ID == "phase-report-p15" {
			reportDependencies = append([]string(nil), artifact.DerivesFrom...)
		}
	}
	sort.Strings(found)
	want := append([]string(nil), requiredOwnedArtifacts...)
	sort.Strings(want)
	if !reflect.DeepEqual(found, want) {
		return fmt.Errorf("P15 owned artifacts=%v want=%v", found, want)
	}
	wantDependencies := append([]string(nil), requiredOwnedArtifacts...)
	sort.Strings(reportDependencies)
	sort.Strings(wantDependencies)
	if !reflect.DeepEqual(reportDependencies, wantDependencies) {
		return fmt.Errorf("phase-report-p15 derives_from=%v want=%v", reportDependencies, wantDependencies)
	}
	return nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return err
	}
	var value transition
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("transition has trailing JSON value")
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P15_TRANSITION") == "1" {
		fmt.Printf("sources=%q\nartifacts=%q\n", discovered.sources, discovered.artifacts)
	}
	if err := validateTransitionCandidate(value, discovered); err != nil {
		return err
	}
	return validateTransitionNegatives(value, discovered)
}

func discoverTransition(root string) (discoveredTransition, error) {
	introduction := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	commits := lines(introduction.output)
	if introduction.err != nil || len(commits) != 1 {
		return discoveredTransition{}, errors.New("transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("transition carrier introduction parent is not the frozen P14 endpoint")
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := parseChangedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	compiled, err := discoverCompiledClosure(root)
	if err != nil {
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
	closure := map[string]bool{"go.mod": true, "go.sum": true, "reference/control-plane/go.mod": true, "reference/control-plane/go.sum": true}
	consume := func(output []byte) error {
		decoder := json.NewDecoder(bytes.NewReader(output))
		for {
			var item listedPackage
			if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
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
	}
	rootList := run(root, nil, "go", "list", "-deps", "-test", "-json", "./cmd/arop/...", "./sdk/go/registry")
	if rootList.err != nil {
		return nil, rootList.err
	}
	if err := consume(rootList.output); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p15-closure-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	work := filepath.Join(temporary, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return nil, err
	}
	nestedList := run(filepath.Join(root, "reference/control-plane"), map[string]string{"GOWORK": work, "TMPDIR": temporary}, "go", "list", "-deps", "-test", "-json", "./internal/app/registryapi", "./internal/app/platform/httpadapter", "./cmd/aropd")
	if nestedList.err != nil {
		return nil, nestedList.err
	}
	if err := consume(nestedList.output); err != nil {
		return nil, err
	}
	return closure, nil
}

func discoverReadClosure() map[string]bool {
	paths := []string{
		"Makefile", checker, waiver, "scripts/generate.mjs",
		"conformance/fixtures/registry/pipeline.json",
		"conformance/fixtures/registry/discovery-snapshot.valid.json",
		"conformance/fixtures/registry/discovery-snapshot.invalid.json",
		"conformance/fixtures/registry/discovery-snapshot.forward.json",
		"schemas/registry/discovery-snapshot-v1.schema.json",
		"openapi/registry-runtime-v1.yaml", "openapi/discovery-runtime-v1.yaml",
		"reference/control-plane/internal/app/registryapi/testdata/generated/provenance.json",
		"conformance/fixtures/contracts/control-plane-publication/generated/provenance.json",
		"examples/assets/generated/provenance.json",
		"reference/control-plane/internal/app/platform/testdata/harness/main.go",
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/harness/main.go",
		"sdk/go/generated/asset/asset.gen.go",
		"sdk/go/generated/control-plane/publication.gen.go",
		"sdk/python/src/arop/generated/registry/registry_gen.py",
		"sdk/python/src/arop/generated/asset/asset_gen.py",
		"sdk/python/src/arop/generated/control-plane/publication_gen.py",
		"sdk/typescript/src/generated/registry/registry.gen.ts",
		"sdk/typescript/src/generated/asset/asset.gen.ts",
		"sdk/typescript/src/generated/control-plane/publication.gen.ts",
		"reference/control-plane/internal/domain/registry/testdata/harness/main.go",
		"conformance/fixtures/codegen-spike/expected/go/models.gen.go.golden",
		"conformance/fixtures/codegen-spike/expected/go/models_gen_test.go.golden",
		"conformance/fixtures/codegen-spike/expected/python/models_gen.py.golden",
		"conformance/fixtures/codegen-spike/expected/python/probe.py.golden",
		"conformance/fixtures/codegen-spike/expected/typescript/models.gen.ts.golden",
		"conformance/fixtures/codegen-spike/expected/typescript/probe.ts.golden",
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
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-registry-api" && phaseNumber(ownerPhase(artifact)) <= 15 {
					found[artifact.ID] = true
					matched = true
				}
			}
			if !matched {
				return nil, errors.New("changed Makefile has no P15 acceptance artifact")
			}
			continue
		}
		best := -1
		var matches []blueprint.Artifact
		for _, artifact := range manifest.Artifacts {
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(ownerPhase(artifact)) > 15 {
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
			futureOwners := aggregateFutureOwners(manifest, byID, source, compiled[source] || read[source])
			if len(futureOwners) == 0 {
				return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
			}
			for _, id := range futureOwners {
				found[id] = true
			}
			continue
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
		}
	}
	for {
		added := 0
		for _, candidate := range manifest.Artifacts {
			if candidate.PathRole != "concrete" || phaseNumber(ownerPhase(candidate)) > 15 || found[candidate.ID] {
				continue
			}
			for id := range found {
				if transitivelyDepends(candidate.ID, id, byID, map[string]bool{}) {
					found[candidate.ID] = true
					added++
					break
				}
			}
		}
		if added == 0 {
			break
		}
	}
	for _, id := range append(append([]string{}, requiredOwnedArtifacts...), "phase-report-p15") {
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

func ownerPhase(artifact blueprint.Artifact) string {
	if artifact.OwnerPhase != "" {
		return artifact.OwnerPhase
	}
	return artifact.ProducerPhase
}

func artifactPathScore(root, artifactPath, source string, consumed bool) int {
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && consumed && strings.HasPrefix(source, artifactPath+"/") {
		return 3*len(artifactPath) + 1
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifactPath)))
	if consumed && directory != "." && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
		return 2 * len(directory)
	}
	return -1
}

func aggregateFutureOwners(manifest blueprint.Manifest, byID map[string]blueprint.Artifact, source string, consumed bool) []string {
	if !consumed {
		return nil
	}
	bestLength := -1
	owners := map[string]bool{}
	for _, aggregate := range manifest.Artifacts {
		if aggregate.Path == "" || (aggregate.PathRole != "aggregate" && aggregate.PathRole != "container") || source != aggregate.Path && !strings.HasPrefix(source, aggregate.Path+"/") {
			continue
		}
		if len(aggregate.Path) < bestLength {
			continue
		}
		if len(aggregate.Path) > bestLength {
			bestLength = len(aggregate.Path)
			owners = map[string]bool{}
		}
		for _, futureID := range aggregate.FutureArtifacts {
			future := byID[futureID]
			if future.ID != "" && future.PathRole == "concrete" && phaseNumber(ownerPhase(future)) <= 15 {
				owners[future.ID] = true
			}
		}
	}
	result := make([]string, 0, len(owners))
	for id := range owners {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
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
	wantFrom := []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14"}
	if value.SchemaVersion != 1 || value.WaiverID != "P15-REGISTRY-API-TRANSITION-001" || value.Status != "validated" || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Transition.ToPhase != "P15" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
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

func canonicalAcceptance() []struct {
	Phase   string `json:"phase"`
	Command string `json:"command"`
	Report  string `json:"report"`
} {
	return []struct {
		Phase   string `json:"phase"`
		Command string `json:"command"`
		Report  string `json:"report"`
	}{
		{"P01", "make spec-index-check", "build/reports/P01/report.json"},
		{"P02", "make blueprint-check", "build/reports/P02/report.json"},
		{"P05", "make test-go-workspace", "build/reports/P05/report.json"},
		{"P06", "make test-protocol-foundation", "build/reports/P06/report.json"},
		{"P07", "make test-codegen-pipeline", "build/reports/P07/report.json"},
		{"P08", "make test-control-plane-platform", "build/reports/P08/report.json"},
		{"P09", "make test-storage-migrations", "build/reports/P09/report.json"},
		{"P10", "make test-identity-secrets", "build/reports/P10/report.json"},
		{"P11", "make test-publication-contracts", "build/reports/P11/report.json"},
		{"P12", "make test-publication-service", "build/reports/P12/report.json"},
		{"P13", "make test-asset-broker", "build/reports/P13/report.json"},
		{"P14", "make test-registry-core", "build/reports/P14/report.json"},
		{"P15", command, "build/reports/P15/report.json"},
	}
}

func canonicalConstraints() []string {
	return []string{
		"p14-domain-ledger-and-persistence-semantics-remain-authoritative",
		"snapshot-is-implemented-and-watch-route-is-not-registered-before-p16",
		"registry-credentials-never-enter-reports-logs-or-cli-output",
		"route-specific-authentication-authorization-and-tenant-binding-fail-closed",
		"sdk-generation-fencing-drain-and-keepalive-use-server-authoritative-results",
		"owner-phase-accountability-does-not-transfer",
		"p15-report-runtime-inputs-remain-empty",
	}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	type mutation struct {
		name string
		fn   func(*transition)
	}
	var mutations []mutation
	appendGroup := func(name string, length int, omit, substitute func(*transition, int), add, duplicate func(*transition)) {
		for index := 0; index < length; index++ {
			i := index
			mutations = append(mutations, mutation{name + "-omission", func(value *transition) { omit(value, i) }}, mutation{name + "-substitution", func(value *transition) { substitute(value, i) }})
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
		func(v *transition) { v.Acceptance = append(v.Acceptance, canonicalAcceptance()[0]) },
		func(v *transition) { v.Acceptance = append(v.Acceptance, v.Acceptance[0]) })
	appendGroup("constraint", len(valid.Constraints),
		func(v *transition, i int) { v.Constraints = append(v.Constraints[:i:i], v.Constraints[i+1:]...) },
		func(v *transition, i int) { v.Constraints[i] = "unexpected-constraint" },
		func(v *transition) { v.Constraints = append(v.Constraints, "unexpected-constraint") },
		func(v *transition) { v.Constraints = append(v.Constraints, v.Constraints[0]) })
	for _, mutation := range mutations {
		candidate := cloneTransition(valid)
		mutation.fn(&candidate)
		if validateTransitionCandidate(candidate, discovered) == nil {
			return fmt.Errorf("transition negative %s was accepted", mutation.name)
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

func validateContracts(root string) ([]byte, error) {
	var evidence bytes.Buffer
	for _, path := range []string{"openapi/registry-runtime-v1.yaml", "openapi/discovery-runtime-v1.yaml"} {
		value, data, err := structuredfile.LoadAny(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		document, ok := value.(map[string]any)
		if !ok || document["openapi"] != "3.1.0" {
			return nil, fmt.Errorf("%s is not OpenAPI 3.1", path)
		}
		fmt.Fprintf(&evidence, "%s:%s:%d\n", path, report.Hash(data), len(data))
	}
	valid, validData, err := schema.ValidatePath(root, "schemas/registry/discovery-snapshot-v1.schema.json", "conformance/fixtures/registry/discovery-snapshot.valid.json")
	if err != nil || valid == nil {
		return nil, errors.Join(err, errors.New("valid discovery snapshot rejected"))
	}
	if _, _, err := schema.ValidatePath(root, "schemas/registry/discovery-snapshot-v1.schema.json", "conformance/fixtures/registry/discovery-snapshot.invalid.json"); err == nil {
		return nil, errors.New("unsafe discovery revision accepted")
	}
	forward, _, err := structuredfile.LoadAny(filepath.Join(root, "conformance/fixtures/registry/discovery-snapshot.forward.json"))
	if err != nil || forward == nil {
		return nil, errors.Join(err, errors.New("forward discovery fixture rejected by strict parser"))
	}
	fmt.Fprintf(&evidence, "snapshot:%s:%d\n", report.Hash(validData), len(validData))
	return evidence.Bytes(), validateOpenAPIBoundary(root)
}

func validateOpenAPIBoundary(root string) error {
	registryValue, _, err := structuredfile.LoadAny(filepath.Join(root, "openapi/registry-runtime-v1.yaml"))
	if err != nil {
		return err
	}
	discoveryValue, _, err := structuredfile.LoadAny(filepath.Join(root, "openapi/discovery-runtime-v1.yaml"))
	if err != nil {
		return err
	}
	registryPaths := asMap(asMap(registryValue)["paths"])
	if !reflect.DeepEqual(sortedMapKeys(registryPaths), []string{"/v1/registry/instances/{instance_id}", "/v1/registry/instances/{instance_id}/drain", "/v1/registry/leases/{lease_id}/keepalive"}) {
		return errors.New("registry OpenAPI path inventory mismatch")
	}
	want := map[string]string{"registerRuntimeInstance": "registry:register", "compareAndSwapRuntimeOperatorState": "registry:operate", "deregisterRuntimeInstance": "registry:write", "drainRuntimeInstance": "registry:write", "keepaliveRuntimeLease": "registry:write"}
	for id, scope := range want {
		op, ok := operationByID(registryPaths, id)
		if !ok || !reflect.DeepEqual(stringsFromAny(op["x-arop-required-scopes"]), []string{scope}) {
			return fmt.Errorf("registry operation %s scope mismatch", id)
		}
	}
	discoveryPaths := asMap(asMap(discoveryValue)["paths"])
	if !reflect.DeepEqual(sortedMapKeys(discoveryPaths), []string{"/v1/discovery/agents/{agent_id}/instances", "/v1/discovery/changes"}) {
		return errors.New("discovery OpenAPI path inventory mismatch")
	}
	snapshot, ok := operationByID(discoveryPaths, "discoverRuntimeInstances")
	if !ok || !reflect.DeepEqual(stringsFromAny(snapshot["x-arop-required-scopes"]), []string{"registry:discover"}) {
		return errors.New("snapshot scope mismatch")
	}
	watch, ok := operationByID(discoveryPaths, "watchRuntimeDiscoveryChanges")
	if !ok || watch["x-arop-implementation-phase"] != "P16" || !reflect.DeepEqual(stringsFromAny(watch["x-arop-required-scopes"]), []string{"registry:discover"}) {
		return errors.New("Watch phase or scope mismatch")
	}
	return nil
}

func operationByID(paths map[string]any, id string) (map[string]any, bool) {
	for _, path := range paths {
		for _, operation := range asMap(path) {
			candidate := asMap(operation)
			if candidate["operationId"] == id {
				return candidate, true
			}
		}
	}
	return nil, false
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringsFromAny(value any) []string {
	items, _ := value.([]any)
	result := make([]string, len(items))
	for index, item := range items {
		result[index], _ = item.(string)
	}
	return result
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateGenerated(root string) ([]byte, error) {
	codegenRoot := filepath.Join(root, "build", "codegen")
	if err := os.MkdirAll(codegenRoot, 0o755); err != nil {
		return nil, err
	}
	output, err := os.MkdirTemp(codegenRoot, "p15-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(output)
	if err := os.Chmod(output, 0o700); err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, output)
	if err != nil {
		return nil, err
	}
	relative = filepath.ToSlash(relative)
	result := filepath.Join(output, "provenance.json")
	node := run(root, nil, "node", "--permission", "--allow-fs-read="+root, "--allow-fs-write="+codegenRoot, "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", "conformance/fixtures/registry/pipeline.json", "--output", relative, "--result", relative+"/provenance.json")
	if node.err != nil {
		return nil, node.err
	}
	paths := []string{
		"sdk/go/generated/registry/registry.gen.go",
		"sdk/python/src/arop/generated/registry/registry_gen.py",
		"sdk/typescript/src/generated/registry/registry.gen.ts",
	}
	var evidence bytes.Buffer
	for _, path := range paths {
		generated, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		tracked, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || !bytes.Equal(generated, tracked) {
			return nil, errors.Join(err, fmt.Errorf("generated output drift: %s", path))
		}
		fmt.Fprintf(&evidence, "%s:%s:%d\n", path, report.Hash(generated), len(generated))
	}
	generatedProvenance, err := os.ReadFile(result)
	if err != nil {
		return nil, err
	}
	trackedProvenance, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/app/registryapi/testdata/generated/provenance.json"))
	if err != nil || !bytes.Equal(generatedProvenance, trackedProvenance) {
		return nil, errors.Join(err, errors.New("generated provenance drift"))
	}
	fmt.Fprintf(&evidence, "provenance:%s:%d\n", report.Hash(generatedProvenance), len(generatedProvenance))
	python := run(root, map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}, "python3", "-c", `compile(open("sdk/python/src/arop/generated/registry/registry_gen.py", encoding="utf-8").read(), "registry_gen.py", "exec")`)
	if python.err != nil {
		return nil, python.err
	}
	typescript := run(root, nil, filepath.Join(root, "node_modules/.bin/tsc"), "--noEmit", "--strict", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--skipLibCheck", "sdk/typescript/src/generated/registry/registry.gen.ts")
	if typescript.err != nil {
		return nil, typescript.err
	}
	return evidence.Bytes(), nil
}

func runGoTests(root string) ([]byte, error) {
	rootTests := run(root, nil, "go", "test", "-count=1", "-race", "-json", "./sdk/go/registry", "./cmd/arop/internal/commands/register", "./cmd/arop")
	if rootTests.err != nil {
		return rootTests.output, rootTests.err
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p15-tests-")
	if err != nil {
		return rootTests.output, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return rootTests.output, err
	}
	work := filepath.Join(temporary, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return rootTests.output, err
	}
	nested := run(filepath.Join(root, "reference/control-plane"), map[string]string{"GOWORK": work, "TMPDIR": temporary}, "go", "test", "-count=1", "-race", "-json", "./internal/app/registryapi", "./internal/app/platform/httpadapter", "./cmd/aropd")
	output := append(append([]byte(nil), rootTests.output...), nested.output...)
	return output, nested.err
}

func rejectIncompleteTests(output []byte) error {
	for _, marker := range [][]byte{[]byte(`"Action":"skip"`), []byte("[no test files]"), []byte("(cached)")} {
		if bytes.Contains(output, marker) {
			return errors.New("test inventory contains skip/cache/no-tests")
		}
	}
	return nil
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

func cleanEnvironment(overrides map[string]string) []string {
	values := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC"}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR"} {
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
