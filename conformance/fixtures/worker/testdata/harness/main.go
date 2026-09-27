// Command harness is the sole writer of the P24 Worker Pull report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	command  = "make test-worker-service"
	checker  = "conformance/fixtures/worker/testdata/harness/main.go"
	waiver   = "conformance/fixtures/worker/testdata/transition/p24-baseline-transition-waiver.json"
	baseline = "5bada508fa5e7035abd1d33817b4de7abf3ee49d"
	carrier  = "9486c398cc66e239e3141780614ff99d0be05935"
)

var requiredOwned = []string{
	"generated-worker-go", "generated-worker-python", "generated-worker-typescript", "openapi-worker-runtime",
	"postgres-migration-worker", "schema-worker-claim", "schema-worker-complete", "sqlite-migration-worker",
	"worker-fixtures", "worker-pull-service",
}

var requiredConstraints = []string{
	"claim-and-attempt-lease-are-atomic",
	"completion-and-terminal-event-outbox-are-atomic",
	"old-worker-session-cannot-overwrite-new-attempt",
	"plaintext-lease-token-is-never-persisted-or-logged",
	"runtime-inputs-remain-empty",
}

type transition struct {
	SchemaVersion     int      `json:"schema_version"`
	TransitionID      string   `json:"transition_id"`
	Status            string   `json:"status"`
	BaselineCommit    string   `json:"baseline_commit"`
	CarrierCommit     string   `json:"carrier_commit"`
	AffectedArtifacts []string `json:"affected_artifacts"`
	SourceClosure     []string `json:"source_closure"`
	AcceptanceClosure []string `json:"acceptance_closure"`
	Constraints       []string `json:"constraints"`
}

type discovered struct {
	Artifacts  []string
	Sources    []string
	Acceptance []string
}

type commandResult struct {
	output []byte
	err    error
}

type cluster struct {
	cmd    *exec.Cmd
	log    bytes.Buffer
	socket string
	port   int
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
	add("p24-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	add("p24-manifest-inventory", validateManifest(root), "exact ten owned artifacts, report dependencies and empty runtime inputs")
	discovered, discoverErr := discoverTransition(root)
	add("p24-transition-discovery", discoverErr, "Git and manifest independently discover the complete source, artifact and acceptance closure")
	if os.Getenv("AROP_PRINT_P24_TRANSITION") == "1" {
		encoded, marshalErr := json.MarshalIndent(discovered, "", "  ")
		fatal(errors.Join(discoverErr, marshalErr))
		fmt.Println(string(encoded))
		return
	}
	transitionErr := validateTransition(root, discovered)
	add("p24-transition-waiver", transitionErr, "validated waiver exactly equals independently discovered closure")
	add("p24-carrier-first", validateCarrier(root), "P24 carrier is the first tree change after the tree-identical P23 endpoint")
	add("p24-openapi", validateOpenAPI(root), "Worker OpenAPI binds claim, renew, release and complete routes with exact scopes and errors")
	codegenEvidence, codegenErr := validateCodegen(root)
	add("p24-codegen", codegenErr, "signed generator reproduces Go, Python and TypeScript Worker models and provenance byte-for-byte")

	rootTests := run(root, nil, "go", "test", "-race", "-count=1", "./sdk/go/generated/worker")
	add("p24-root-go-tests", rootTests.err, "generated Go Worker model passes under race detection")
	rootVet := run(root, nil, "go", "vet", "./sdk/go/generated/worker")
	add("p24-root-go-vet", rootVet.err, "generated Go Worker model passes go vet")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p24-")
	var pgEvidence []byte
	var nestedTests, nestedVet commandResult
	if scratchErr == nil {
		defer os.RemoveAll(scratch)
		_ = os.Chmod(scratch, 0o700)
		instance, evidence, pgErr := startPostgres(scratch)
		pgEvidence = evidence
		if pgErr == nil {
			defer instance.stop()
			nestedTests, nestedVet = runNested(root, scratch, instance.dsn())
		} else {
			nestedTests.err, nestedVet.err = pgErr, pgErr
		}
	} else {
		nestedTests.err, nestedVet.err = scratchErr, scratchErr
	}
	add("p24-dual-store-worker-tests", nestedTests.err, "Worker service, HTTP, SQLite and live PostgreSQL 16 schema tests pass under race detection")
	add("p24-worker-test-terminals", rejectIncompleteTests(nestedTests.output), "Worker test stream has no skip, cache, no-tests or failure terminal")
	add("p24-control-plane-vet", nestedVet.err, "Worker service, adapters, composition and migration packages pass go vet")

	p05 := run(root, nil, "make", "test-go-workspace")
	add("p24-p05-regression", p05.err, "nested-module proxy archive, repin and historical transition replay pass")
	p23 := run(root, nil, "make", "verify-run-delivery")
	add("p24-p23-regression", p23.err, "P23 replays from its immutable carrier while P24 evolves the repository")

	evidence := []report.RuntimeEvidence{
		{Kind: "p24-codegen", SHA256: report.Hash(codegenEvidence), Bytes: int64(len(codegenEvidence))},
		{Kind: "p24-control-plane-tests", SHA256: report.Hash(nestedTests.output), Bytes: int64(len(nestedTests.output))},
		{Kind: "p24-control-plane-vet", SHA256: report.Hash(nestedVet.output), Bytes: int64(len(nestedVet.output))},
		{Kind: "p24-postgres-16", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))},
		{Kind: "p24-root-tests", SHA256: report.Hash(rootTests.output), Bytes: int64(len(rootTests.output))},
		{Kind: "p24-root-vet", SHA256: report.Hash(rootVet.output), Bytes: int64(len(rootVet.output))},
		{Kind: "p05-regression", SHA256: report.Hash(p05.output), Bytes: int64(len(p05.output))},
		{Kind: "p23-regression", SHA256: report.Hash(p23.output), Bytes: int64(len(p23.output))},
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P24", Suite: "AROP P24 durable Worker Pull", Class: "p24.worker.pull",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 10, "language_models": 3, "storage_engines": 2, "runtime_inputs": 0},
		AuditNote: "P24 atomically claims an Attempt and lease, fences every mutation by tenant, Worker session, generation, fencing token and secret-token digest, and commits terminal result, Event Ledger projection, capacity release and durable outbox in one transaction. Replays are request-digest stable, expired or restarted Worker sessions cannot overwrite a newer Attempt, lease plaintext never reaches persistence, SQLite and PostgreSQL 16 schemas are exact, all generated models are reproducible, and predecessor evidence enters only as digest-and-byte runtime evidence while runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P24/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P24 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P24 checks failed; see build/reports/P24/report.json"))
	}
	fmt.Printf("AROP durable Worker Pull passed: %d checks.\n", len(checks))
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	values := []string{}
	for _, value := range bytes.Split(result.output, []byte{0}) {
		if len(value) != 0 {
			values = append(values, string(value))
		}
	}
	sort.Strings(values)
	return values, nil
}

func loadManifest(root string) (blueprint.Manifest, error) {
	var manifest blueprint.Manifest
	err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest)
	return manifest, err
}

func validateManifest(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	owned := []string{}
	var dependencies, runtimeInputs []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P24" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-worker-service" {
				return fmt.Errorf("invalid P24 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p24" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
			runtimeInputs = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	sort.Strings(dependencies)
	if !reflect.DeepEqual(owned, requiredOwned) || !reflect.DeepEqual(dependencies, requiredOwned) || len(runtimeInputs) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func discoverTransition(root string) (discovered, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return discovered{}, err
	}
	result := run(root, nil, "git", "diff", "--name-status", "--no-renames", "-z", baseline+"..HEAD", "--")
	if result.err != nil {
		return discovered{}, result.err
	}
	fields := bytes.Split(result.output, []byte{0})
	sources := []string{}
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := string(fields[i]), filepath.ToSlash(string(fields[i+1]))
		if status == "" || path == "" {
			continue
		}
		if !strings.Contains("ACMDT", status[:1]) {
			return discovered{}, fmt.Errorf("unsupported Git status %q for %s", status, path)
		}
		sources = append(sources, path)
	}
	sort.Strings(sources)
	artifacts := map[string]bool{}
	acceptance := map[string]bool{}
	for _, source := range sources {
		best := -1
		matches := []blueprint.Artifact{}
		for _, artifact := range manifest.Artifacts {
			if artifact.PathRole != "concrete" || artifact.Path == "" || artifact.Path == "build/reports/P24" {
				continue
			}
			path := strings.TrimSuffix(filepath.ToSlash(artifact.Path), "/")
			if source != path && !strings.HasPrefix(source, path+"/") {
				continue
			}
			if len(path) > best {
				best, matches = len(path), []blueprint.Artifact{artifact}
			} else if len(path) == best {
				matches = append(matches, artifact)
			}
		}
		if len(matches) == 0 {
			if source == "Makefile" {
				contents, readErr := os.ReadFile(filepath.Join(root, source))
				if readErr != nil || !bytes.Contains(contents, []byte("test-worker-service:")) || !bytes.Contains(contents, []byte(checker)) {
					return discovered{}, errors.New("Makefile does not bind the exact P24 checker")
				}
				acceptance[command] = true
				continue
			}
			return discovered{}, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			artifacts[artifact.ID] = true
			if artifact.AcceptanceTest != "" {
				acceptance[strings.Replace(artifact.AcceptanceTest, "make-", "make ", 1)] = true
			}
		}
	}
	artifactList, acceptanceList := mapKeys(artifacts), mapKeys(acceptance)
	return discovered{Artifacts: artifactList, Sources: sources, Acceptance: acceptanceList}, nil
}

func mapKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateTransition(root string, want discovered) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var value transition
	if err = strictJSON(data, &value); err != nil {
		return err
	}
	return compareTransition(value, want)
}

func compareTransition(value transition, want discovered) error {
	if value.SchemaVersion != 1 || value.TransitionID != "P24-BASELINE-TRANSITION-001" || value.Status != "validated" || value.BaselineCommit != baseline || value.CarrierCommit != carrier {
		return errors.New("transition identity or lifecycle is not exact")
	}
	if !reflect.DeepEqual(value.SourceClosure, want.Sources) || !reflect.DeepEqual(value.AffectedArtifacts, want.Artifacts) || !reflect.DeepEqual(value.AcceptanceClosure, want.Acceptance) || !reflect.DeepEqual(value.Constraints, requiredConstraints) {
		return fmt.Errorf("transition mismatch sources=%v/%v artifacts=%v/%v acceptance=%v/%v constraints=%v/%v", value.SourceClosure, want.Sources, value.AffectedArtifacts, want.Artifacts, value.AcceptanceClosure, want.Acceptance, value.Constraints, requiredConstraints)
	}
	return nil
}

func validateCarrier(root string) error {
	if result := run(root, nil, "git", "merge-base", "--is-ancestor", carrier, "HEAD"); result.err != nil {
		return errors.New("P24 carrier is not an ancestor of HEAD")
	}
	parent := run(root, nil, "git", "rev-parse", carrier+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return errors.New("P24 carrier parent is not the signed P23 endpoint")
	}
	changed := run(root, nil, "git", "diff-tree", "--no-commit-id", "--name-only", "-r", carrier)
	if changed.err != nil || strings.TrimSpace(string(changed.output)) != waiver {
		return fmt.Errorf("P24 carrier changed unexpected paths: %s", changed.output)
	}
	return nil
}

func validateOpenAPI(root string) error {
	var document map[string]any
	if err := structuredfile.Load(filepath.Join(root, "openapi/worker-runtime-v1.yaml"), &document); err != nil {
		return err
	}
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"/v1/workers/{worker_id}/claims:next", "/v1/workers/{worker_id}/claims/{claim_id}:renew", "/v1/workers/{worker_id}/claims/{claim_id}:release", "/v1/workers/{worker_id}/claims/{claim_id}:complete", "worker:claim", "worker:complete", "worker-claim-v1.schema.json", "worker-complete-v1.schema.json", "AROPError"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("Worker OpenAPI missing %q", required)
		}
	}
	return nil
}

func validateCodegen(root string) ([]byte, error) {
	base := filepath.Join(root, "build", "codegen")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	output, err := os.MkdirTemp(base, "p24-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(output)
	relative, _ := filepath.Rel(root, output)
	relative = filepath.ToSlash(relative)
	nodeModules, err := filepath.EvalSymlinks(filepath.Join(root, "node_modules"))
	if err != nil {
		return nil, errors.New("resolve locked node_modules")
	}
	generated := run(root, map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"}, "node", "--permission", "--allow-fs-read=.", "--allow-fs-read="+nodeModules, "--allow-fs-write=build/codegen", "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", "conformance/fixtures/worker/pipeline.json", "--output", relative, "--result", relative+"/provenance.json")
	if generated.err != nil {
		return generated.output, generated.err
	}
	paths := []string{"sdk/go/generated/worker/worker.gen.go", "sdk/python/src/arop/generated/worker/worker_gen.py", "sdk/typescript/src/generated/worker/worker.gen.ts"}
	for _, path := range paths {
		actual, readErr := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
		tracked, trackedErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil || trackedErr != nil || !bytes.Equal(actual, tracked) {
			return generated.output, fmt.Errorf("generated Worker model drift: %s", path)
		}
	}
	actual, err := os.ReadFile(filepath.Join(output, "provenance.json"))
	if err != nil {
		return generated.output, err
	}
	tracked, err := os.ReadFile(filepath.Join(root, "conformance/fixtures/worker/generated/provenance.json"))
	if err != nil || !bytes.Equal(actual, tracked) {
		return generated.output, errors.New("Worker provenance drift")
	}
	return append(generated.output, actual...), nil
}

func runNested(root, scratch, dsn string) (commandResult, commandResult) {
	work := filepath.Join(scratch, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n"
	if err := os.WriteFile(work, []byte(body), 0o600); err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	overrides := map[string]string{"GOWORK": work, "TMPDIR": scratch, "AROP_TEST_POSTGRES_DSN": dsn}
	directory := filepath.Join(root, "reference/control-plane")
	packages := []string{"./internal/domain/worker/...", "./internal/app/platform/httpadapter", "./internal/storage/migrate", "./cmd/aropd"}
	tests := run(directory, overrides, "go", append([]string{"test", "-json", "-p=1", "-race", "-count=1"}, packages...)...)
	vet := run(directory, overrides, "go", append([]string{"vet"}, packages...)...)
	return tests, vet
}

func rejectIncompleteTests(output []byte) error {
	for _, marker := range []string{`"Action":"skip"`, `"Action":"fail"`, "[no test files]", "(cached)"} {
		if bytes.Contains(output, []byte(marker)) {
			return fmt.Errorf("test output contains forbidden terminal %q", marker)
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
			candidate := filepath.Join("/opt/homebrew/opt/postgresql@16/bin", name)
			if _, statErr := os.Stat(candidate); statErr != nil {
				return nil, nil, err
			}
			path = candidate
		}
		version := run("/", nil, path, "--version")
		if version.err != nil || !strings.Contains(string(version.output), "16.") {
			return nil, nil, fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		tools[name] = path
		fmt.Fprintf(&evidence, "%s:%s\n", name, strings.TrimSpace(string(version.output)))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, evidence.Bytes(), err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	data, socket, emptyPass := filepath.Join(scratch, "pgdata"), filepath.Join(scratch, "pgsocket"), filepath.Join(scratch, "pgpass")
	if err = os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if err = os.WriteFile(emptyPass, nil, 0o600); err != nil {
		return nil, evidence.Bytes(), err
	}
	pgEnv := map[string]string{"HOME": "/nonexistent", "PGPASSFILE": emptyPass}
	if result := run(scratch, pgEnv, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p24", "--no-instructions"); result.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	instance := &cluster{socket: socket, port: port}
	instance.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", strconv.Itoa(port), "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	instance.cmd.Dir = scratch
	instance.cmd.Env = cleanEnvironment(pgEnv)
	instance.cmd.Stdout, instance.cmd.Stderr = &instance.log, &instance.log
	if err = instance.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, pgEnv, tools["pg_isready"], "-h", socket, "-p", strconv.Itoa(port), "-U", "arop_p24", "-d", "postgres", "-q").err == nil {
			return instance, append(evidence.Bytes(), instance.log.Bytes()...), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = instance.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}

func (instance *cluster) dsn() string {
	return "postgresql://arop_p24@localhost/postgres?host=" + url.QueryEscape(instance.socket) + "&port=" + strconv.Itoa(instance.port) + "&sslmode=disable"
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

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	process := exec.Command(name, args...)
	process.Dir = directory
	process.Env = cleanEnvironment(overrides)
	output, err := process.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment(overrides map[string]string) []string {
	banned := []string{"GOFLAGS=", "GOENV=", "GOWORK=", "GOCACHE=", "GOCACHEPROG=", "GOMODCACHE=", "GOTMPDIR=", "GOROOT=", "GOTOOLCHAIN=", "GOEXPERIMENT=", "CGO_ENABLED=", "NODE_OPTIONS=", "NODE_PATH=", "NPM_CONFIG_NODE_OPTIONS=", "PYTHONHOME=", "PYTHONPATH=", "PYTHONSTARTUP=", "PYTHONINSPECT=", "PYTHONWARNINGS=", "PYTHONUSERBASE=", "PGHOST=", "PGHOSTADDR=", "PGPORT=", "PGDATABASE=", "PGUSER=", "PGPASSWORD=", "PGSERVICE=", "PGSERVICEFILE=", "PGPASSFILE=", "PGOPTIONS="}
	values := []string{}
	for _, entry := range os.Environ() {
		reject := false
		for _, prefix := range banned {
			if strings.HasPrefix(entry, prefix) {
				reject = true
				break
			}
		}
		if !reject {
			values = append(values, entry)
		}
	}
	defaults := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC"}
	for key, value := range overrides {
		defaults[key] = value
	}
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values = append(values, key+"="+defaults[key])
	}
	return values
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), filepath.Clean(os.TempDir()), "<tmp>")
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 6000 {
		text = text[len(text)-6000:]
	}
	return text
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
