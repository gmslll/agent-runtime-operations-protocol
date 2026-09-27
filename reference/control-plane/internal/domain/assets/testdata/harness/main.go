// Command harness is the sole writer of the P13 asset broker report.
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
	"strings"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command     = "make test-asset-broker"
	checker     = "reference/control-plane/internal/domain/assets/testdata/harness/main.go"
	waiver      = "reference/control-plane/internal/domain/assets/testdata/transition/p13-baseline-transition-waiver.json"
	baseline    = "3db6ee93a693d62d47e2d2fd25c5de43749f2e7d"
	p14Carrier  = "reference/control-plane/internal/domain/registry/testdata/transition/p14-baseline-transition-waiver.json"
	p13Endpoint = "958d1d42bb7b4a3f6c015ad97004b434ba07d2d3"
)

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
	add("p13-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, runtimeInputsErr := declaredRuntimeInputs(root)
	add("p13-runtime-inputs-empty", errors.Join(runtimeInputsErr, requireEmpty(runtimeInputs)), "manifest-declared runtime_inputs is empty")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P13_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p13-transition-waiver", transitionErr, "waiver exactly binds Git changes and manifest owners")
	add("p13-catalog-snapshots", validateCatalog(root), "P09/P10/P12 snapshots and P13 current catalog are exact")
	add("p13-generated-contracts", validateGeneratedContracts(root), "asset schema/OpenAPI/models/provenance compile and regenerate without drift")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p13-")
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p13-private-scratch", scratchErr, "private scratch created")
	var pg *cluster
	var pgEvidence []byte
	if scratchErr == nil {
		pg, pgEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p13-private-postgres16", err, "private PostgreSQL 16 ready on a private Unix socket")
	var tests commandResult
	if pg != nil {
		tests = runTests(root, scratch, pg.dsn())
	} else {
		tests.err = errors.New("PostgreSQL prerequisite failed")
	}
	add("p13-sqlite-postgres-asset-broker", tests.err, "asset domain, HTTP, composition, migration and both repositories pass")
	add("p13-no-skips-cache-or-no-tests", rejectIncompleteTests(tests.output), "test stream has no skip/cache/no-tests terminal")
	if pg != nil {
		err = pg.stop()
	} else {
		err = errors.New("private PostgreSQL was not started")
	}
	add("p13-postgres-shutdown", err, "private PostgreSQL stopped before historical regressions")

	evidence := []report.RuntimeEvidence{
		{Kind: "p13-go-test", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p13-postgres-toolchain", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))},
	}
	p07 := run(root, nil, "make", "test-codegen-pipeline")
	if p07.err == nil {
		verify := run(root, nil, "make", "verify-report", "REPORT=build/reports/P07/report.json")
		p07.output = append(p07.output, verify.output...)
		p07.err = verify.err
	}
	add("p13-p07-regression", p07.err, "P07 code generation report is current")
	evidence = append(evidence, report.RuntimeEvidence{Kind: "p07-regression", SHA256: report.Hash(p07.output), Bytes: int64(len(p07.output))})
	p12 := run(root, nil, "make", "test-publication-service")
	if p12.err == nil {
		verify := run(root, nil, "make", "verify-report", "REPORT=build/reports/P12/report.json")
		p12.output = append(p12.output, verify.output...)
		p12.err = verify.err
	}
	add("p13-p12-regression", p12.err, "P12 and its P05-P11 regression chain passed on the current head")
	evidence = append(evidence, report.RuntimeEvidence{Kind: "p12-regression", SHA256: report.Hash(p12.output), Bytes: int64(len(p12.output))})
	for _, phase := range []string{"P05", "P06", "P08", "P09", "P10", "P11"} {
		path := "build/reports/" + phase + "/report.json"
		verified := run(root, nil, "make", "verify-report", "REPORT="+path)
		add("p13-"+strings.ToLower(phase)+"-regression", verified.err, phase+" current report verified after P12")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-regression", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}
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
	add("p13-scratch-cleanup", err, "scratch removed")

	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P13", Suite: "AROP P13 asset broker", Class: "p13.assets",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 9, "database_engines": 2, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P13 binds its nine manifest-owned artifacts and an independently discovered Git/manifest transition closure. Asset grants are short-lived, operation/run/identity/content bound, revocable and persisted only by digest; mutations and durable audit share one UoW. SQLite and PostgreSQL 16 enforce equivalent 0020 contracts and run the same repository matrix. DNS and IP policy is freshly evaluated for every connect and redirect hop. P05-P12 regressions are rerun on the current Git head and enter only as digest-and-byte runtime evidence; runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P13/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P13 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P13 checks failed; see build/reports/P13/report.json"))
	}
	fmt.Printf("AROP asset broker passed: %d checks.\n", len(checks))
}

func declaredRuntimeInputs(root string) ([]string, error) {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	var found []string
	matches := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.ID != "phase-report-p13" {
			continue
		}
		matches++
		found = append([]string(nil), artifact.RuntimeInputs...)
	}
	if matches != 1 {
		return nil, fmt.Errorf("phase-report-p13 manifest entries=%d", matches)
	}
	return found, nil
}

func requireEmpty(items []string) error {
	if len(items) != 0 {
		return fmt.Errorf("runtime_inputs must be empty, got %q", items)
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
	intro := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	commits := lines(intro.output)
	if intro.err != nil || len(commits) != 1 {
		return errors.New("transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return errors.New("transition carrier introduction parent is not the P12 endpoint")
	}
	successor := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", p14Carrier)
	successorCommits := lines(successor.output)
	if successor.err != nil || len(successorCommits) != 1 {
		return errors.New("P14 transition carrier must have one introduction commit")
	}
	successorParent := run(root, nil, "git", "rev-parse", successorCommits[0]+"^")
	if successorParent.err != nil || strings.TrimSpace(string(successorParent.output)) != p13Endpoint {
		return errors.New("P14 transition carrier does not freeze the P13 endpoint")
	}
	sources, err := changedSources(root, p13Endpoint)
	if err != nil {
		return err
	}
	artifacts, err := affectedArtifacts(root, sources)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P13_TRANSITION") == "1" {
		fmt.Printf("sources=%q\nartifacts=%q\n", sources, artifacts)
	}
	wantFrom := []string{"P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12"}
	if value.SchemaVersion != 1 || value.WaiverID != "P13-ASSET-BROKER-TRANSITION-001" || value.Status != "validated" || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Transition.ToPhase != "P13" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
		return errors.New("transition identity, policy or phase chain is invalid")
	}
	if !reflect.DeepEqual(value.SourceClosure, sources) || !reflect.DeepEqual(value.AffectedArtifacts, artifacts) || !reflect.DeepEqual(value.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(value.Constraints, canonicalConstraints()) {
		return fmt.Errorf("transition exact closure mismatch: sources=%q artifacts=%q", sources, artifacts)
	}
	return validateTransitionNegatives(value, sources, artifacts)
}

func changedSources(root, endpoint string) ([]string, error) {
	result := run(root, nil, "git", "diff", "--no-renames", "--name-only", "--diff-filter=ACMRTD", baseline+".."+endpoint)
	if result.err != nil {
		return nil, result.err
	}
	set := map[string]bool{}
	for _, path := range lines(result.output) {
		if !strings.HasPrefix(path, "build/reports/") {
			set[path] = true
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func affectedArtifacts(root string, sources []string) ([]string, error) {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	found := map[string]bool{}
	ownerPhases := map[string]bool{"P13": true}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P13" || artifact.ProducerPhase == "P13" {
			found[artifact.ID] = true
		}
	}
	for _, source := range sources {
		if source == "Makefile" {
			matched := false
			for _, artifact := range manifest.Artifacts {
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-asset-broker" {
					found[artifact.ID] = true
					matched = true
				}
			}
			if !matched {
				return nil, errors.New("Makefile has no manifest-owned P13 acceptance")
			}
			continue
		}
		best := -1
		var matches []blueprint.Artifact
		for _, artifact := range manifest.Artifacts {
			if artifact.PathRole != "concrete" || artifact.Path == "" {
				continue
			}
			score := -1
			if artifact.Path == source {
				score = len(artifact.Path)*2 + 1
			} else if strings.HasPrefix(source, artifact.Path+"/") {
				score = len(artifact.Path) * 2
			} else if directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifact.Path))); directory != "." && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
				score = len(directory)
			}
			if score > best {
				best, matches = score, []blueprint.Artifact{artifact}
			} else if score >= 0 && score == best {
				matches = append(matches, artifact)
			}
		}
		if best < 0 {
			return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
			if artifact.OwnerPhase != "" {
				ownerPhases[artifact.OwnerPhase] = true
			}
		}
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.ProducerPhase != "" && ownerPhases[artifact.ProducerPhase] {
			found[artifact.ID] = true
		}
	}
	result := make([]string, 0, len(found))
	for id := range found {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
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
	}
}

func canonicalConstraints() []string {
	return []string{"asset-tokens-are-short-lived-operation-scoped-and-revocable", "every-connect-and-redirect-hop-rechecks-dns-and-ip-policy", "asset-mutations-and-durable-audit-share-one-uow", "run-grant-authorization-is-mandatory-and-fail-closed", "p09-p10-and-p12-catalog-snapshots-remain-immutable", "production-catalog-ends-at-0020", "owner-phase-accountability-does-not-transfer", "p13-report-runtime-inputs-remain-empty"}
}

func validateTransitionNegatives(valid transition, sources, artifacts []string) error {
	check := func(name string, mutate func(*transition)) error {
		copy := clone(valid)
		mutate(&copy)
		if reflect.DeepEqual(copy.SourceClosure, sources) && reflect.DeepEqual(copy.AffectedArtifacts, artifacts) && reflect.DeepEqual(copy.Acceptance, canonicalAcceptance()) && reflect.DeepEqual(copy.Constraints, canonicalConstraints()) {
			return fmt.Errorf("transition negative %s was accepted", name)
		}
		return nil
	}
	groups := []struct {
		name string
		len  int
		omit func(*transition, int)
	}{{"source", len(valid.SourceClosure), func(v *transition, i int) { v.SourceClosure = append(v.SourceClosure[:i:i], v.SourceClosure[i+1:]...) }}, {"artifact", len(valid.AffectedArtifacts), func(v *transition, i int) {
		v.AffectedArtifacts = append(v.AffectedArtifacts[:i:i], v.AffectedArtifacts[i+1:]...)
	}}, {"acceptance", len(valid.Acceptance), func(v *transition, i int) { v.Acceptance = append(v.Acceptance[:i:i], v.Acceptance[i+1:]...) }}, {"constraint", len(valid.Constraints), func(v *transition, i int) { v.Constraints = append(v.Constraints[:i:i], v.Constraints[i+1:]...) }}}
	for _, group := range groups {
		for index := 0; index < group.len; index++ {
			if err := check(group.name+"-omission", func(value *transition) { group.omit(value, index) }); err != nil {
				return err
			}
		}
	}
	return nil
}

func clone(value transition) transition {
	data, _ := json.Marshal(value)
	var result transition
	_ = json.Unmarshal(data, &result)
	return result
}

func validateCatalog(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"func P09ProductionCatalog()", "func P10ProductionCatalog()", "func P12ProductionCatalog()", "func P13ProductionCatalog()", "func CurrentProductionCatalog()", `ReportPhase: "P13"`, "0020_asset.sql"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("catalog missing %s", required)
		}
	}
	start, end := strings.Index(text, "func P13ProductionCatalog()"), strings.Index(text, "func P14ProductionCatalog()")
	if start < 0 || end <= start || strings.Count(text[start:end], "0020_asset.sql") != 2 || strings.Contains(text[start:end], "0030_registry.sql") {
		return errors.New("P13 catalog snapshot is not exactly complete through 0020")
	}
	return nil
}

func validateGeneratedContracts(root string) error {
	output, err := os.MkdirTemp(filepath.Join(root, "build", "codegen"), "p13-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(output)
	relative, _ := filepath.Rel(root, output)
	relative = filepath.ToSlash(relative)
	generate := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"}, "node", "--permission", "--allow-fs-read=.", "--allow-fs-write=build/codegen", "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", "examples/assets/pipeline.json", "--output", relative, "--result", relative+"/provenance.json")
	if generate.err != nil {
		return generate.err
	}
	for _, path := range []string{"sdk/go/generated/asset/asset.gen.go", "sdk/python/src/arop/generated/asset/asset_gen.py", "sdk/typescript/src/generated/asset/asset.gen.ts"} {
		actual, readErr := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		if readErr != nil {
			return readErr
		}
		tracked, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil || !bytes.Equal(actual, tracked) {
			return fmt.Errorf("generated contract drift: %s", path)
		}
	}
	actual, err := os.ReadFile(filepath.Join(output, "provenance.json"))
	if err != nil {
		return err
	}
	tracked, err := os.ReadFile(filepath.Join(root, "examples/assets/generated/provenance.json"))
	if err != nil || !bytes.Equal(actual, tracked) {
		return errors.New("asset provenance drift")
	}
	return nil
}

func runTests(root, scratch, dsn string) commandResult {
	realScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return commandResult{err: errors.New("resolve private scratch")}
	}
	work := filepath.Join(scratch, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return commandResult{err: err}
	}
	environment := map[string]string{"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": realScratch, "AROP_P13_SCRATCH": realScratch, "AROP_P13_MIGRATION_ROOT": filepath.Join(root, "reference/control-plane/migrations"), "AROP_P13_POSTGRES_URL": dsn}
	return run(filepath.Join(root, "reference/control-plane"), environment, "go", "test", "-count=1", "-race", "-json", "./cmd/aropd", "./internal/app/platform/httpadapter", "./internal/domain/assets", "./internal/domain/assets/storage/sqlite", "./internal/domain/assets/storage/postgres", "./internal/domain/assets/testdata/acceptance", "./internal/storage/migrate")
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
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if result := run(scratch, map[string]string{"HOME": "/nonexistent"}, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p13", "--no-instructions"); result.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	instance := &cluster{socket: socket}
	instance.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	instance.cmd.Dir = scratch
	instance.cmd.Env = cleanEnvironment(map[string]string{"HOME": "/nonexistent"})
	instance.cmd.Stdout, instance.cmd.Stderr = &instance.log, &instance.log
	if err := instance.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, nil, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p13", "-d", "postgres", "-q").err == nil {
			return instance, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = instance.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}

func (instance *cluster) dsn() string {
	return "postgresql://arop_p13@localhost/postgres?host=" + url.QueryEscape(instance.socket) + "&port=5432&sslmode=disable"
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
