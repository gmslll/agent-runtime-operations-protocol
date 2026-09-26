// Command harness is the sole writer of the P18 run lifecycle report.
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
	command    = "make test-run-lifecycle"
	checker    = "reference/control-plane/internal/domain/run/testdata/harness/main.go"
	waiver     = "reference/control-plane/internal/domain/run/testdata/transition/p18-baseline-transition-waiver.json"
	nextWaiver = "reference/control-plane/internal/domain/dispatch/testdata/transition/p19-baseline-transition-waiver.json"
	baseline   = "e2cc6266530d462d968e6f9a35dd80a045ddc533"
)

var requiredOwnedArtifacts = []string{
	"generated-run-go",
	"generated-run-python",
	"generated-run-typescript",
	"openapi-control-plane-run-fragment",
	"postgres-migration-run",
	"run-fixtures",
	"run-lifecycle-service",
	"schema-command",
	"schema-result",
	"schema-run-request",
	"schema-run-status",
	"sqlite-migration-run",
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
		Delete, Add             bool
	} `json:"invalid_mutations"`
	TerminalStates    []string `json:"terminal_states"`
	NonterminalStates []string `json:"nonterminal_states"`
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
	add("p18-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, runtimeErr := declaredRuntimeInputs(root)
	add("p18-runtime-inputs-empty", errors.Join(runtimeErr, requireEmpty(runtimeInputs)), "manifest-declared runtime_inputs is empty")
	ownedErr := validateOwnedArtifacts(root)
	add("p18-owned-artifact-inventory", ownedErr, "manifest declares the exact twelve P18-owned artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P18_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p18-transition-waiver", transitionErr, "waiver exactly binds Git changes, manifest owners, acceptance and constraints")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p18-schema-fixtures", fixtureErr, "four run schemas and all declared positive/negative fixtures pass offline validation")
	generatedEvidence, generatedErr := validateGeneratedContracts(root)
	add("p18-generated-contracts", generatedErr, "Go, Python and TypeScript run models regenerate without drift")
	add("p18-openapi-contract", validateOpenAPI(root), "run OpenAPI exposes exact create, read and command scope boundaries")
	add("p18-production-catalog", validateCatalog(root), "P14 snapshot is frozen and production catalog ends at paired 0040")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p18-")
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p18-private-scratch", scratchErr, "private scratch created")
	var pg *cluster
	var pgEvidence []byte
	if scratchErr == nil {
		pg, pgEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p18-private-postgres16", err, "private PostgreSQL 16 ready on a private Unix socket")
	var tests commandResult
	if pg != nil {
		tests = runTests(root, scratch, pg.dsn())
	} else {
		tests.err = errors.New("PostgreSQL prerequisite failed")
	}
	add("p18-run-domain-storage-migrations", tests.err, "domain, dual repositories, exact schemas and migration transition pass with race detection")
	add("p18-no-skips-cache-or-no-tests", rejectIncompleteTests(tests.output), "test stream has no skip/cache/no-tests terminal")

	evidence := []report.RuntimeEvidence{
		{Kind: "p18-go-test", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p18-postgres-toolchain", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))},
		{Kind: "p18-fixture-validation", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
		{Kind: "p18-codegen-reproducibility", SHA256: report.Hash(generatedEvidence), Bytes: int64(len(generatedEvidence))},
	}
	p17 := run(root, nil, "make", "verify-registry")
	if p17.err == nil {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P17/report.json")
		p17.output = append(p17.output, verified.output...)
		p17.err = verified.err
	}
	add("p18-p17-regression", p17.err, "P17 and its P14-P16 black-box chain pass on the current head")
	evidence = append(evidence, report.RuntimeEvidence{Kind: "p17-regression", SHA256: report.Hash(p17.output), Bytes: int64(len(p17.output))})
	for _, phase := range []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15", "P16"} {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p18-"+strings.ToLower(phase)+"-regression", verified.err, phase+" current report verified after P17")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-regression", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}
	if pg != nil {
		err = pg.stop()
	} else {
		err = errors.New("private PostgreSQL was not started")
	}
	add("p18-postgres-shutdown", err, "private PostgreSQL stopped")
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
	add("p18-scratch-cleanup", err, "scratch removed")
	sort.Slice(evidence, func(left, right int) bool { return evidence[left].Kind < evidence[right].Kind })

	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P18", Suite: "AROP P18 run lifecycle", Class: "p18.run",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 12, "database_engines": 2, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P18 binds the exact twelve manifest-owned artifacts and an independently discovered Git/manifest transition closure. Authorization snapshots, Run state, outbox, durable audit and stable effect reservations are tenant-bound and mutation paths use one UoW. Cancel and deadline races fail closed, usage and trace fields are wire-safe, and no Dispatcher is introduced. SQLite and PostgreSQL 16 enforce exact 0040 schema parity and the same empty, P14-to-P18, idempotent and dirty-history matrix. P01-P17 regressions enter only as digest-and-byte runtime evidence and runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P18/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P18 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P18 checks failed; see build/reports/P18/report.json"))
	}
	fmt.Printf("AROP run lifecycle passed: %d checks.\n", len(checks))
}

func declaredRuntimeInputs(root string) ([]string, error) {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	var found []string
	matches := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p18" {
			matches++
			found = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("phase-report-p18 manifest entries=%d", matches)
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
		if artifact.OwnerPhase == "P18" {
			found = append(found, artifact.ID)
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-run-lifecycle" {
				return fmt.Errorf("P18 artifact %s has invalid ownership metadata", artifact.ID)
			}
		}
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, requiredOwnedArtifacts) {
		return fmt.Errorf("P18 owned artifacts=%v want=%v", found, requiredOwnedArtifacts)
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
	if os.Getenv("AROP_PRINT_P18_TRANSITION") == "1" {
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
		return discoveredTransition{}, errors.New("transition carrier introduction parent is not the frozen P17 endpoint")
	}
	endpoint, err := transitionEndpoint(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+".."+endpoint)
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

func transitionEndpoint(root string) (string, error) {
	introduction := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", nextWaiver)
	commits := lines(introduction.output)
	if introduction.err != nil || len(commits) != 1 {
		return "", errors.New("P19 transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil {
		return "", parent.err
	}
	endpoint := strings.TrimSpace(string(parent.output))
	if endpoint != "9a092c363c90f6b13f3873990a569fbe24653c2a" {
		return "", fmt.Errorf("P18 historical endpoint=%s", endpoint)
	}
	if ancestor := run(root, nil, "git", "merge-base", "--is-ancestor", endpoint, "HEAD"); ancestor.err != nil {
		return "", errors.New("P18 historical endpoint is not an ancestor of HEAD")
	}
	return endpoint, nil
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
	temporary, err := os.MkdirTemp("/tmp", "arop-p18-closure-")
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
		"./cmd/aropd", "./internal/domain/run/...", "./internal/domain/run/testdata/acceptance", "./internal/domain/run/testdata/harness",
		"./internal/domain/registry/...", "./internal/domain/registry/testdata/acceptance", "./internal/domain/registry/testdata/harness",
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
		"internal/tooling/cmd/arop-go-proxy-bootstrap/main.go",
		"conformance/fixtures/registry-watch/testdata/verification/main.go",
		"reference/control-plane/internal/app/platform/testdata/harness/main.go",
		"reference/control-plane/internal/app/registrywatch/testdata/harness/main.go",
		"reference/control-plane/internal/domain/assets/testdata/harness/main.go",
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/harness/main.go",
		"reference/control-plane/internal/domain/run/testdata/transition/p18-p05-transition.json",
		"reference/control-plane/internal/storage/migrate/PRODUCTION_CATALOG.md",
		"reference/control-plane/internal/storage/migrate/production_catalog.go",
		"reference/control-plane/migrations/sqlite/0040_run.sql",
		"reference/control-plane/migrations/postgres/0040_run.sql",
		"conformance/fixtures/run/cases.json",
		"conformance/fixtures/run/command.valid.json",
		"conformance/fixtures/run/generated/provenance.json",
		"conformance/fixtures/run/pipeline.json",
		"conformance/fixtures/run/result.invalid.json",
		"conformance/fixtures/run/result.valid.json",
		"conformance/fixtures/run/run-request.invalid.json",
		"conformance/fixtures/run/run-request.valid.json",
		"conformance/fixtures/run/run-status.forward.json",
		"conformance/fixtures/run/run-status.invalid.json",
		"conformance/fixtures/run/run-status.valid.json",
		"conformance/fixtures/codegen-spike/expected/go/models.gen.go.golden",
		"conformance/fixtures/codegen-spike/expected/go/models_gen_test.go.golden",
		"conformance/fixtures/codegen-spike/expected/python/models_gen.py.golden",
		"conformance/fixtures/codegen-spike/expected/python/probe.py.golden",
		"conformance/fixtures/codegen-spike/expected/typescript/models.gen.ts.golden",
		"conformance/fixtures/codegen-spike/expected/typescript/probe.ts.golden",
		"conformance/fixtures/contracts/control-plane-publication/generated/provenance.json",
		"examples/assets/generated/provenance.json",
		"reference/control-plane/internal/app/registryapi/testdata/generated/provenance.json",
		"openapi/fragments/control-plane/runs-v1.yaml",
		"schemas/runtime/run-request-v1.schema.json",
		"schemas/runtime/run-status-v1.schema.json",
		"schemas/runtime/command-v1.schema.json",
		"schemas/runtime/result-v1.schema.json",
		"scripts/generate.mjs",
		"sdk/go/generated/run/run.gen.go",
		"sdk/go/generated/run/run_contract_test.go",
		"sdk/go/generated/asset/asset.gen.go",
		"sdk/go/generated/control-plane/publication.gen.go",
		"sdk/go/generated/registry/registry.gen.go",
		"sdk/python/src/arop/generated/run/run_gen.py",
		"sdk/python/src/arop/generated/asset/asset_gen.py",
		"sdk/python/src/arop/generated/control-plane/publication_gen.py",
		"sdk/python/src/arop/generated/registry/registry_gen.py",
		"sdk/typescript/src/generated/run/run.gen.ts",
		"sdk/typescript/src/generated/asset/asset.gen.ts",
		"sdk/typescript/src/generated/control-plane/publication.gen.ts",
		"sdk/typescript/src/generated/registry/registry.gen.ts",
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
	found, err := mapSourceArtifacts(root, manifest, sources, compiled, read)
	if err != nil {
		return nil, err
	}
	propagateAffectedArtifacts(found, manifest, byID)
	if err := requireArtifactLowerBound(found, append(append([]string{}, requiredOwnedArtifacts...), "phase-report-p18")); err != nil {
		return nil, err
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

func requireArtifactLowerBound(found map[string]bool, required []string) error {
	for _, id := range required {
		if !found[id] {
			return fmt.Errorf("independent artifact discovery omitted required %s", id)
		}
	}
	return nil
}

func mapSourceArtifacts(root string, manifest blueprint.Manifest, sources []string, compiled, read map[string]bool) (map[string]bool, error) {
	found := map[string]bool{}
	for _, source := range sources {
		if source == "Makefile" {
			matched := false
			for _, artifact := range manifest.Artifacts {
				phase := artifact.OwnerPhase
				if phase == "" {
					phase = artifact.ProducerPhase
				}
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-run-lifecycle" && phaseNumber(phase) <= 18 {
					found[artifact.ID] = true
					matched = true
				}
			}
			if !matched {
				return nil, errors.New("changed Makefile has no P18 acceptance artifact")
			}
			continue
		}
		best := -1
		var matches []blueprint.Artifact
		for _, artifact := range manifest.Artifacts {
			phase := artifact.OwnerPhase
			if phase == "" {
				phase = artifact.ProducerPhase
			}
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(phase) > 18 {
				continue
			}
			score := artifactPathScore(root, artifact.Path, source, compiled, read)
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
	return found, nil
}

func artifactPathScore(root, artifactPath, source string, compiled, read map[string]bool) int {
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	consumed := compiled[source] || read[source]
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && consumed && strings.HasPrefix(source, artifactPath+"/") {
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
			if phaseNumber(phase) > 18 || candidate.PathRole != "concrete" {
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
	wantFrom := []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15", "P16", "P17"}
	if value.SchemaVersion != 1 || value.WaiverID != "P18-RUN-LIFECYCLE-TRANSITION-001" || value.Status != "validated" || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Transition.ToPhase != "P18" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
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
		{"P15", "make test-registry-api", "build/reports/P15/report.json"},
		{"P16", "make test-registry-recovery", "build/reports/P16/report.json"},
		{"P17", "make verify-registry", "build/reports/P17/report.json"},
		{"P18", command, "build/reports/P18/report.json"},
	}
}

func canonicalConstraints() []string {
	return []string{
		"authorization-snapshot-run-outbox-and-durable-audit-mutate-in-one-uow",
		"run-create-and-command-idempotency-is-tenant-scoped-and-request-bound",
		"write-and-irreversible-effects-require-a-stable-effect-id",
		"effect-reservation-is-tenant-bound-and-semantic-digest-fenced",
		"cancel-deadline-and-first-terminal-state-races-fail-closed",
		"run-trace-deadline-and-usage-fields-are-durable-and-wire-safe",
		"public-integers-never-exceed-the-js-safe-maximum",
		"p09-p10-p12-p13-and-p14-catalog-snapshots-remain-immutable",
		"production-catalog-ends-at-0040",
		"dispatcher-attempt-ticket-and-event-ingest-remain-absent",
		"owner-phase-accountability-does-not-transfer",
		"p18-report-runtime-inputs-remain-empty",
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
	if err := structuredfile.Load(filepath.Join(root, "conformance/fixtures/run/cases.json"), &cases); err != nil {
		return nil, err
	}
	if cases.SchemaVersion != 1 || len(cases.Valid) != 4 || len(cases.InvalidMutations) != 5 || !reflect.DeepEqual(cases.TerminalStates, []string{"succeeded", "failed", "cancelled", "timed_out"}) || !reflect.DeepEqual(cases.NonterminalStates, []string{"queued", "dispatching", "running", "waiting_input", "cancel_requested"}) {
		return nil, errors.New("run fixture inventory is incomplete")
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
			return nil, errors.New("run mutation names are not unique")
		}
		seen[item.Name] = true
		document, ok := documents[item.Document]
		if !ok {
			return nil, fmt.Errorf("mutation %s references unknown document", item.Name)
		}
		encoded, _ := json.Marshal(document.value)
		var copy any
		if json.Unmarshal(encoded, &copy) != nil || mutateJSONPointer(copy, item.Pointer, item.Value, item.Delete, item.Add) != nil {
			return nil, fmt.Errorf("mutation %s has invalid pointer", item.Name)
		}
		if schema.ValidateFile(root, document.schema, copy) == nil {
			return nil, fmt.Errorf("invalid run mutation %s was accepted", item.Name)
		}
		fmt.Fprintf(&evidence, "invalid:%s\n", item.Name)
	}
	return evidence.Bytes(), nil
}

func mutateJSONPointer(value any, pointer string, replacement any, remove, add bool) error {
	if remove && add {
		return errors.New("pointer mutation cannot add and remove")
	}
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
			_, exists := object[part]
			if add && exists {
				return errors.New("pointer add property already exists")
			}
			if !add && !exists {
				return errors.New("pointer property is missing")
			}
			if remove {
				delete(object, part)
			} else {
				object[part] = replacement
			}
			return nil
		}
		current, ok = object[part]
		if !ok {
			return errors.New("pointer property is missing")
		}
	}
	return errors.New("pointer is empty")
}

func validateGeneratedContracts(root string) ([]byte, error) {
	base := filepath.Join(root, "build", "codegen")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("build/codegen must be a real directory")
	}
	realRoot, rootErr := filepath.EvalSymlinks(root)
	realBase, baseErr := filepath.EvalSymlinks(base)
	if rootErr != nil || baseErr != nil || realBase != filepath.Join(realRoot, "build", "codegen") {
		return nil, errors.New("build/codegen escapes repository root")
	}
	output, err := os.MkdirTemp(base, "p18-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(output)
	relative, err := filepath.Rel(root, output)
	if err != nil {
		return nil, err
	}
	relative = filepath.ToSlash(relative)
	generated := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"}, "node", "--permission", "--allow-fs-read=.", "--allow-fs-write=build/codegen", "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", "conformance/fixtures/run/pipeline.json", "--output", relative, "--result", relative+"/provenance.json")
	if generated.err != nil {
		return generated.output, generated.err
	}
	for _, path := range []string{"sdk/go/generated/run/run.gen.go", "sdk/python/src/arop/generated/run/run_gen.py", "sdk/typescript/src/generated/run/run.gen.ts"} {
		actual, readErr := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		if readErr != nil {
			return generated.output, readErr
		}
		tracked, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil || !bytes.Equal(actual, tracked) {
			return generated.output, fmt.Errorf("generated run contract drift: %s", path)
		}
	}
	actual, err := os.ReadFile(filepath.Join(output, "provenance.json"))
	if err != nil {
		return generated.output, err
	}
	tracked, err := os.ReadFile(filepath.Join(root, "conformance/fixtures/run/generated/provenance.json"))
	if err != nil || !bytes.Equal(actual, tracked) {
		return generated.output, errors.New("run provenance drift")
	}
	evidence := append(generated.output, actual...)
	goCompile := run(root, map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}, "go", "test", "-count=1", "./sdk/go/generated/run")
	evidence = append(evidence, goCompile.output...)
	if goCompile.err != nil {
		return evidence, goCompile.err
	}
	// Do not use `python -m py_compile` here. py_compile explicitly writes a
	// bytecode file even with -B, while -I ignores PYTHONPYCACHEPREFIX.  That
	// combination used to dirty the repository during the acceptance run and
	// invalidated every report produced after this check.  Compiling the source
	// bytes exercises the same syntax boundary without a filesystem side effect.
	pythonCompile := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}, "python3", "-I", "-B", "-c", `import pathlib,sys; path=sys.argv[1]; compile(pathlib.Path(path).read_bytes(), path, "exec")`, "sdk/python/src/arop/generated/run/run_gen.py")
	evidence = append(evidence, pythonCompile.output...)
	if pythonCompile.err != nil {
		return evidence, pythonCompile.err
	}
	typeScriptCompile := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "NODE_PATH": ""}, "node", "--permission", "--allow-fs-read=.", "--disable-proto=throw", "--no-addons", "node_modules/typescript/bin/tsc", "--noEmit", "--strict", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--exactOptionalPropertyTypes", "--noUncheckedIndexedAccess", "sdk/typescript/src/generated/run/run.gen.ts")
	evidence = append(evidence, typeScriptCompile.output...)
	if typeScriptCompile.err != nil {
		return evidence, typeScriptCompile.err
	}
	return evidence, nil
}

func validateOpenAPI(root string) error {
	var document map[string]any
	if err := structuredfile.Load(filepath.Join(root, "openapi/fragments/control-plane/runs-v1.yaml"), &document); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	text := string(encoded)
	for _, required := range []string{"/v1/agent-runs", "/v1/agent-runs/{run_id}", "/v1/agent-runs/{run_id}/commands", "run:create", "run:read", "run:command", "run-request-v1.schema.json", "run-status-v1.schema.json", "command-v1.schema.json"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("run OpenAPI missing %s", required)
		}
	}
	return nil
}

func validateCatalog(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"func P09ProductionCatalog()", "func P10ProductionCatalog()", "func P12ProductionCatalog()", "func P13ProductionCatalog()", "func P14ProductionCatalog()", "func CurrentProductionCatalog()", `ReportPhase: "P18"`, "0030_registry.sql", "0040_run.sql"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("catalog missing %s", required)
		}
	}
	start, end := strings.Index(text, "func P14ProductionCatalog()"), strings.Index(text, "func CurrentProductionCatalog()")
	if start < 0 || end <= start || strings.Count(text[start:end], "0030_registry.sql") != 2 || strings.Contains(text[start:end], "0040_run.sql") {
		return errors.New("P14 catalog snapshot is not exactly complete through 0030")
	}
	if strings.Count(text[end:], "0040_run.sql") != 2 || !strings.Contains(text[end:], `ReportPhase: "P18"`) {
		return errors.New("P18 production catalog is not exactly complete through 0040")
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
		"AROP_P18_SCRATCH": realScratch, "AROP_P18_MIGRATION_ROOT": filepath.Join(root, "reference/control-plane/migrations"), "AROP_P18_POSTGRES_URL": dsn,
		"AROP_TEST_POSTGRES_DSN": dsn,
	}
	return run(filepath.Join(root, "reference/control-plane"), environment, "go", "test", "-count=1", "-race", "-json",
		"./cmd/aropd", "./internal/app/platform/httpadapter", "./internal/domain/run", "./internal/domain/run/storage/internalstore", "./internal/domain/run/testdata/acceptance", "./internal/domain/run/testdata/harness", "./internal/storage/migrate")
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
	if result := run(scratch, pgEnv, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p18", "--no-instructions"); result.err != nil {
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
		if run(scratch, pgEnv, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p18", "-d", "postgres", "-q").err == nil {
			return instance, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = instance.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}

func (instance *cluster) dsn() string {
	return "postgresql://arop_p18@localhost/postgres?host=" + url.QueryEscape(instance.socket) + "&port=5432&sslmode=disable"
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
