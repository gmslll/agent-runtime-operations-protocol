// Command harness is the sole writer of the P16 registry recovery report.
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
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-registry-recovery"
	checker  = "reference/control-plane/internal/app/registrywatch/testdata/harness/main.go"
	waiver   = "reference/control-plane/internal/app/registrywatch/testdata/transition/p16-baseline-transition-waiver.json"
	baseline = "1f8ebc05d503292149a344f3e4c606262a242e8e"
)

var ownedArtifacts = []string{"registry-recovery-service", "registry-watch-fixtures"}

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
		Phase, Command, Report string
	} `json:"acceptance"`
	Constraints []string `json:"constraints"`
}

type discoveredTransition struct{ sources, artifacts []string }

type listedPackage struct {
	Dir                                          string
	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles []string
	EmbedFiles                                   []string
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
	add("p16-static-input-closure", inputErr, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, runtimeErr := declaredRuntimeInputs(root)
	add("p16-runtime-inputs-empty", errors.Join(runtimeErr, requireEmpty(runtimeInputs)), "manifest-declared runtime_inputs is empty")
	add("p16-owned-artifact-inventory", validateOwnedArtifacts(root), "manifest declares exactly two P16-owned artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P16_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p16-transition-waiver", transitionErr, "waiver exactly equals Git, compile/read and manifest-derived closure")
	add("p16-p15-wire-byte-stability", validateP15WireStability(root), "P15 wire, generated models and provenance are byte-identical")
	add("p16-no-production-migration", gitQuiet(root, "diff", "--quiet", baseline+"..HEAD", "--", "reference/control-plane/migrations"), "P16 introduces no migration")
	fixtureEvidence, fixtureErr := validateFixtures(root)
	add("p16-watch-fixtures", fixtureErr, "Watch, compaction, cancellation and restore fixtures are strict and complete")

	scratch, err := os.MkdirTemp("/tmp", "arop-p16-")
	if err == nil {
		err = os.Chmod(scratch, 0o700)
	}
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(scratch)
	scratch, err = filepath.EvalSymlinks(scratch)
	fatal(err)
	cluster, postgresEvidence, postgresErr := startPostgres(scratch)
	if cluster != nil {
		defer cluster.Stop()
	}
	add("p16-private-postgres16", postgresErr, "private Unix-socket-only PostgreSQL 16 is ready")
	goEvidence := []byte(nil)
	var goErr error
	if postgresErr == nil {
		goEvidence, goErr = runGoTests(root, scratch, cluster.DSN(), cluster.bin)
	} else {
		goErr = postgresErr
	}
	add("p16-watch-ha-recovery-tests", goErr, "Watch race, poll, compaction, leader fencing and dual-engine restore pass with race detection")
	add("p16-no-skips-cache-or-no-tests", rejectIncompleteTests(goEvidence), "test stream has no skip/cache/no-tests terminal")

	p15 := run(root, nil, "make", "verify-report", "REPORT=build/reports/P15/report.json")
	if p15.err != nil {
		p15 = run(root, nil, "make", "test-registry-api")
		if p15.err == nil {
			verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P15/report.json")
			p15.output = append(p15.output, verified.output...)
			p15.err = verified.err
		}
	}
	add("p16-p15-regression", p15.err, "P15 registry API and its historical chain pass at the P16 head")

	evidence := []report.RuntimeEvidence{
		{Kind: "p16-fixtures", SHA256: report.Hash(fixtureEvidence), Bytes: int64(len(fixtureEvidence))},
		{Kind: "p16-go-tests", SHA256: report.Hash(goEvidence), Bytes: int64(len(goEvidence))},
		{Kind: "p16-postgres16", SHA256: report.Hash(postgresEvidence), Bytes: int64(len(postgresEvidence))},
		{Kind: "p15-execution", SHA256: report.Hash(p15.output), Bytes: int64(len(p15.output))},
	}
	for _, phase := range []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15"} {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p16-"+strings.ToLower(phase)+"-report", verified.err, phase+" current report verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-regression", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}

	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P16", Suite: "AROP P16 registry recovery", Class: "p16.registry.recovery",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs,
		RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 2, "watch_registered": true, "production_migrations_added": 0, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P16 activates the P15-frozen Watch route over atomic short ledger reads, an in-process notification hint and durable polling for cross-node changes. Long waits hold no application UoW. PostgreSQL session advisory locking and SQLite file locking fence compaction leaders; compaction advances only the logical watermark and never mutates append-only events. Private PostgreSQL 16 and SQLite backup/restore prove exact revision, event and watermark recovery. P15 public wire, generated models, provenance and production migrations remain byte-identical; earlier reports are digest-and-byte runtime evidence only.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P16/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P16 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P16 checks failed; see build/reports/P16/report.json"))
	}
	fmt.Printf("AROP registry recovery passed: %d checks.\n", len(checks))
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

func loadManifest(root string) (blueprint.Manifest, error) {
	var manifest blueprint.Manifest
	err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest)
	return manifest, err
}

func declaredRuntimeInputs(root string) ([]string, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return nil, err
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p16" {
			return append([]string(nil), artifact.RuntimeInputs...), nil
		}
	}
	return nil, errors.New("phase-report-p16 is missing")
}

func requireEmpty(items []string) error {
	if len(items) != 0 {
		return fmt.Errorf("runtime_inputs=%q", items)
	}
	return nil
}

func validateOwnedArtifacts(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	var found, dependencies []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P16" {
			found = append(found, artifact.ID)
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-registry-recovery" {
				return fmt.Errorf("invalid P16 ownership metadata for %s", artifact.ID)
			}
		}
		if artifact.ID == "phase-report-p16" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
		}
	}
	sort.Strings(found)
	sort.Strings(dependencies)
	want := append([]string(nil), ownedArtifacts...)
	sort.Strings(want)
	if !reflect.DeepEqual(found, want) || !reflect.DeepEqual(dependencies, want) {
		return fmt.Errorf("owned=%v report_dependencies=%v want=%v", found, dependencies, want)
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
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("transition has trailing JSON value")
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P16_TRANSITION") == "1" {
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
		return discoveredTransition{}, errors.New("P16 carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("P16 carrier parent is not the accepted P15 endpoint")
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := changedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}
	compiled, err := compiledClosure(root, sources)
	if err != nil {
		return discoveredTransition{}, err
	}
	for _, source := range sources {
		if strings.HasSuffix(source, ".go") && !compiled[source] {
			return discoveredTransition{}, fmt.Errorf("changed Go source is outside actual compile closure: %s", source)
		}
	}
	manifest, err := loadManifest(root)
	if err != nil {
		return discoveredTransition{}, err
	}
	artifacts, err := affectedArtifacts(manifest, sources)
	return discoveredTransition{sources: sources, artifacts: artifacts}, err
}

func changedSources(data []byte) ([]string, error) {
	fields, paths := bytes.Split(data, []byte{0}), map[string]bool{}
	for index := 0; index < len(fields) && len(fields[index]) != 0; {
		status := string(fields[index])
		index++
		if status == "" || !strings.Contains("ACMRTD", status[:1]) {
			return nil, fmt.Errorf("unsupported Git status %q", status)
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

func compiledClosure(root string, changed []string) (map[string]bool, error) {
	result := map[string]bool{}
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
				relative, relErr := filepath.Rel(root, filepath.Join(absolute, name))
				if relErr == nil {
					result[filepath.ToSlash(relative)] = true
				}
			}
		}
	}
	rootList := run(root, nil, "go", "list", "-deps", "-test", "-json", "./...", "./conformance/fixtures/registry-watch/testdata/verification")
	if rootList.err != nil {
		return nil, rootList.err
	}
	if err := consume(rootList.output); err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("/tmp", "arop-p16-list-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	work := filepath.Join(temporary, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return nil, err
	}
	nestedRoot := filepath.Join(root, "reference/control-plane")
	nestedEnv := map[string]string{"GOWORK": work, "TMPDIR": temporary}
	nested := run(nestedRoot, nestedEnv, "go", "list", "-deps", "-test", "-json", "./...", "./internal/app/registrywatch/testdata/harness")
	if nested.err != nil {
		return nil, nested.err
	}
	if err := consume(nested.output); err != nil {
		return nil, err
	}
	// Each changed Go source also gets a direct package query. This closes the
	// inventory for command/testdata packages that broad ./... patterns may
	// legitimately omit while still deriving membership from go list itself.
	for _, source := range changed {
		if !strings.HasSuffix(source, ".go") || result[source] {
			continue
		}
		directory, environment, relative := root, map[string]string(nil), source
		if strings.HasPrefix(source, "reference/control-plane/") {
			directory, environment, relative = nestedRoot, nestedEnv, strings.TrimPrefix(source, "reference/control-plane/")
		}
		pattern := "./" + filepath.ToSlash(filepath.Dir(relative))
		direct := run(directory, environment, "go", "list", "-deps", "-test", "-json", pattern)
		if direct.err != nil {
			return nil, direct.err
		}
		if err := consume(direct.output); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func affectedArtifacts(manifest blueprint.Manifest, sources []string) ([]string, error) {
	byID, found := map[string]blueprint.Artifact{}, map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	for _, source := range sources {
		best := -1
		matches := []string{}
		if source == "Makefile" {
			for _, artifact := range manifest.Artifacts {
				if artifact.AcceptanceTest == "make-test-registry-recovery" {
					matches = append(matches, artifact.ID)
				}
			}
		} else {
			for _, artifact := range manifest.Artifacts {
				path := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(artifact.Path)), "/")
				if artifact.PathRole != "concrete" || path == "." || path == "" || source != path && !strings.HasPrefix(source, path+"/") {
					continue
				}
				if len(path) > best {
					best, matches = len(path), []string{artifact.ID}
				} else if len(path) == best {
					matches = append(matches, artifact.ID)
				}
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, id := range matches {
			found[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, artifact := range manifest.Artifacts {
			if found[artifact.ID] || phaseNumber(artifactPhase(artifact)) > 16 {
				continue
			}
			for _, dependency := range artifact.DerivesFrom {
				if found[dependency] {
					found[artifact.ID], changed = true, true
					break
				}
			}
		}
	}
	result := make([]string, 0, len(found))
	for id := range found {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("unknown artifact %s", id)
		}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func artifactPhase(artifact blueprint.Artifact) string {
	for _, value := range []string{artifact.OwnerPhase, artifact.ProducerPhase, artifact.CompletionPhase} {
		if value != "" {
			return value
		}
	}
	return ""
}

func phaseNumber(value string) int {
	if len(value) != 3 || value[0] != 'P' {
		return 1000
	}
	number, err := strconv.Atoi(value[1:])
	if err != nil {
		return 1000
	}
	return number
}

func validateTransitionCandidate(value transition, discovered discoveredTransition) error {
	wantFrom := []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15"}
	if value.SchemaVersion != 1 || value.WaiverID != "P16-REGISTRY-RECOVERY-TRANSITION-001" || value.Status != "validated" || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Transition.ToPhase != "P16" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
		return errors.New("transition identity, phase or baseline is invalid")
	}
	if !reflect.DeepEqual(value.SourceClosure, discovered.sources) || !reflect.DeepEqual(value.AffectedArtifacts, discovered.artifacts) {
		return fmt.Errorf("transition exact closure mismatch: sources=%q artifacts=%q", discovered.sources, discovered.artifacts)
	}
	if !reflect.DeepEqual(value.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(value.Constraints, canonicalConstraints()) {
		return errors.New("transition acceptance or constraints are not exact")
	}
	return nil
}

func canonicalAcceptance() []struct{ Phase, Command, Report string } {
	commands := map[string]string{"P01": "make spec-index-check", "P02": "make blueprint-check", "P05": "make test-go-workspace", "P06": "make test-protocol-foundation", "P07": "make test-codegen-pipeline", "P08": "make test-control-plane-platform", "P09": "make test-storage-migrations", "P10": "make test-identity-secrets", "P11": "make test-publication-contracts", "P12": "make test-publication-service", "P13": "make test-asset-broker", "P14": "make test-registry-core", "P15": "make test-registry-api", "P16": command}
	result := []struct{ Phase, Command, Report string }{}
	for _, phase := range []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15", "P16"} {
		result = append(result, struct{ Phase, Command, Report string }{phase, commands[phase], "build/reports/" + phase + "/report.json"})
	}
	return result
}

func canonicalConstraints() []string {
	return []string{
		"p14-ledger-event-and-watermark-semantics-remain-authoritative",
		"p15-public-wire-generated-models-and-provenance-remain-byte-identical",
		"p16-introduces-no-production-migration",
		"watch-uses-short-reads-and-never-holds-a-uow-while-waiting",
		"snapshot-watch-compaction-resync-and-cancellation-fail-closed",
		"ha-leadership-is-fenced-and-old-leaders-cannot-revive",
		"backup-restore-preserves-ledger-watermark-and-readiness",
		"owner-phase-accountability-does-not-transfer",
		"p16-report-runtime-inputs-remain-empty",
	}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	mutations := []func(*transition){
		func(value *transition) { value.AffectedArtifacts = value.AffectedArtifacts[1:] },
		func(value *transition) { value.AffectedArtifacts = append(value.AffectedArtifacts, "unexpected") },
		func(value *transition) { value.AffectedArtifacts[0] = "substituted" },
		func(value *transition) {
			value.AffectedArtifacts = append(value.AffectedArtifacts, value.AffectedArtifacts[0])
		},
		func(value *transition) { value.SourceClosure = value.SourceClosure[1:] },
		func(value *transition) { value.SourceClosure = append(value.SourceClosure, "unexpected") },
		func(value *transition) { value.SourceClosure[0] = "substituted" },
		func(value *transition) { value.SourceClosure = append(value.SourceClosure, value.SourceClosure[0]) },
		func(value *transition) { value.Acceptance = value.Acceptance[1:] },
		func(value *transition) { value.Acceptance = append(value.Acceptance, value.Acceptance[0]) },
		func(value *transition) { value.Acceptance[0].Command = "make substituted" },
		func(value *transition) { value.Constraints = value.Constraints[1:] },
		func(value *transition) { value.Constraints = append(value.Constraints, "unexpected") },
		func(value *transition) { value.Constraints[0] = "substituted" },
		func(value *transition) { value.Constraints = append(value.Constraints, value.Constraints[0]) },
	}
	if len(valid.AffectedArtifacts) == 0 || len(valid.SourceClosure) == 0 || len(valid.Acceptance) == 0 || len(valid.Constraints) == 0 {
		return errors.New("transition negative probes require non-empty exact sets")
	}
	for index, mutate := range mutations {
		candidate := cloneTransition(valid)
		mutate(&candidate)
		if validateTransitionCandidate(candidate, discovered) == nil {
			return fmt.Errorf("transition negative %d was accepted", index)
		}
	}
	return nil
}

func cloneTransition(value transition) transition {
	encoded, _ := json.Marshal(value)
	var clone transition
	_ = json.Unmarshal(encoded, &clone)
	return clone
}

func validateP15WireStability(root string) error {
	paths := []string{"openapi/discovery-runtime-v1.yaml", "openapi/registry-runtime-v1.yaml", "schemas/registry/discovery-snapshot-v1.schema.json", "schemas/registry/registry-event-v1.schema.json", "sdk/go/generated/registry/registry.gen.go", "sdk/python/src/arop/generated/registry/registry_gen.py", "sdk/typescript/src/generated/registry/registry.gen.ts", "reference/control-plane/internal/app/registryapi/testdata/generated/provenance.json"}
	args := append([]string{"diff", "--quiet", baseline + "..HEAD", "--"}, paths...)
	return gitQuiet(root, args...)
}

func gitQuiet(root string, args ...string) error { return run(root, nil, "git", args...).err }

func validateFixtures(root string) ([]byte, error) {
	paths := []string{"conformance/fixtures/registry-watch/cases.json", "conformance/fixtures/registry-watch/changes.valid.json", "conformance/fixtures/registry-watch/compacted.error.json"}
	var evidence bytes.Buffer
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return evidence.Bytes(), err
		}
		if _, err := structuredfile.Parse(data, ".json"); err != nil {
			return evidence.Bytes(), err
		}
		fmt.Fprintf(&evidence, "%s:%s:%d\n", path, report.Hash(data), len(data))
	}
	casesData, _ := os.ReadFile(filepath.Join(root, paths[0]))
	for _, marker := range []string{"snapshot-watch-race", "duplicate-notification", "cross-node-poll", "compacted-cursor", "cancelled-wait", "restored-ledger"} {
		if strings.Count(string(casesData), marker) != 1 {
			return evidence.Bytes(), fmt.Errorf("fixture case %s is not exact", marker)
		}
	}
	return evidence.Bytes(), nil
}

func runGoTests(root, scratch, dsn, postgresBin string) ([]byte, error) {
	work := filepath.Join(scratch, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return nil, err
	}
	overrides := map[string]string{"GOWORK": work, "TMPDIR": scratch, "AROP_P16_POSTGRES_URL": dsn, "AROP_P16_POSTGRES_BIN": postgresBin, "AROP_P16_SCRATCH": scratch}
	directory := filepath.Join(root, "reference/control-plane")
	primary := run(directory, overrides, "go", "test", "-count=1", "-race", "-json", "./internal/app/registrywatch", "./internal/app/registryapi", "./internal/app/platform/httpadapter", "./internal/domain/registry/storage/sqlite", "./cmd/aropd")
	// The P14 repository test and the P16 coordinator test both rebuild the
	// registry tables. Run them serially against the private cluster so package
	// parallelism cannot turn independent destructive fixtures into a race.
	overrides["AROP_P14_POSTGRES_URL"] = dsn
	overrides["AROP_P14_MIGRATION_ROOT"] = filepath.Join(root, "reference/control-plane/migrations")
	postgres := run(directory, overrides, "go", "test", "-count=1", "-race", "-json", "./internal/domain/registry/storage/postgres")
	output := append(append([]byte(nil), primary.output...), postgres.output...)
	return output, errors.Join(primary.err, postgres.err)
}

func rejectIncompleteTests(output []byte) error {
	if len(output) == 0 {
		return errors.New("test output is empty")
	}
	for _, marker := range [][]byte{[]byte(`"Action":"skip"`), []byte("[no test files]"), []byte("(cached)")} {
		if bytes.Contains(output, marker) {
			return fmt.Errorf("test output contains %q", marker)
		}
	}
	return nil
}

type cluster struct {
	cmd    *exec.Cmd
	log    bytes.Buffer
	socket string
	bin    string
}

func startPostgres(scratch string) (*cluster, []byte, error) {
	tools := map[string]string{}
	var evidence bytes.Buffer
	for _, name := range []string{"initdb", "postgres", "pg_isready", "pg_dump", "pg_restore"} {
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, evidence.Bytes(), err
		}
		version := run("/", nil, path, "--version")
		if version.err != nil || !strings.Contains(string(version.output), "16.") {
			return nil, evidence.Bytes(), fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		tools[name] = path
		fmt.Fprintf(&evidence, "%s:%s\n", name, strings.TrimSpace(string(version.output)))
	}
	data, socket, emptyPass := filepath.Join(scratch, "pgdata"), filepath.Join(scratch, "pgsocket"), filepath.Join(scratch, "pgpass")
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if err := os.WriteFile(emptyPass, nil, 0o600); err != nil {
		return nil, evidence.Bytes(), err
	}
	pgEnv := map[string]string{"PGPASSFILE": emptyPass}
	if result := run(scratch, pgEnv, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p16", "--no-instructions"); result.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	instance := &cluster{socket: socket, bin: filepath.Dir(tools["pg_dump"])}
	instance.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "55436", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	instance.cmd.Dir, instance.cmd.Env, instance.cmd.Stdout, instance.cmd.Stderr = scratch, cleanEnvironment(pgEnv), &instance.log, &instance.log
	if err := instance.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, pgEnv, tools["pg_isready"], "-h", socket, "-p", "55436", "-U", "arop_p16", "-d", "postgres", "-q").err == nil {
			return instance, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	instance.Stop()
	return nil, evidence.Bytes(), errors.New("PostgreSQL 16 did not become ready")
}

func (cluster *cluster) DSN() string {
	return (&url.URL{Scheme: "postgres", User: url.User("arop_p16"), Host: "localhost", Path: "/postgres", RawQuery: url.Values{"host": []string{cluster.socket}, "port": []string{"55436"}, "sslmode": []string{"disable"}}.Encode()}).String()
}

func (cluster *cluster) Stop() {
	if cluster == nil || cluster.cmd == nil || cluster.cmd.Process == nil {
		return
	}
	_ = cluster.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = cluster.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cluster.cmd.Process.Kill()
		<-done
	}
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	process := exec.Command(name, args...)
	process.Dir, process.Env = directory, cleanEnvironment(overrides)
	output, err := process.CombinedOutput()
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
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
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
