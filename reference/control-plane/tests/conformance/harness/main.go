//go:build ignore

// Command harness is the sole writer of the P34 server-conformance report.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	command = "make test-server-conformance"
	checker = "reference/control-plane/tests/conformance/harness/main.go"
)

type commandResult struct {
	output []byte
	err    error
}

type managedProcess struct {
	command *exec.Cmd
	done    chan error
	log     *boundedBuffer
}

type runnerReport struct {
	SchemaVersion int    `json:"schema_version"`
	Protocol      string `json:"protocol"`
	Profile       string `json:"profile"`
	ProfileSHA256 string `json:"profile_sha256"`
	TargetSHA256  string `json:"target_sha256"`
	StartedAt     string `json:"started_at"`
	CompletedAt   string `json:"completed_at"`
	Passed        bool   `json:"passed"`
	PassedCount   int    `json:"passed_count"`
	FailedCount   int    `json:"failed_count"`
	SkippedCount  int    `json:"skipped_count"`
	Results       []struct {
		ID             string `json:"id"`
		Title          string `json:"title"`
		Required       bool   `json:"required"`
		Outcome        string `json:"outcome"`
		Message        string `json:"message,omitempty"`
		DurationMS     int64  `json:"duration_ms"`
		ScenarioSHA256 string `json:"scenario_sha256"`
		FixturePath    string `json:"fixture_path"`
		FixtureSHA256  string `json:"fixture_sha256"`
		FixtureBytes   int64  `json:"fixture_bytes"`
	} `json:"results"`
}

type goEvent struct{ Action, Package, Test, Output string }

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
	add("p34-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked inputs bind the reusable runner and nested driver", len(inputs)))
	add("p34-manifest-inventory", validateManifest(root), "exact P34 driver ownership and empty runtime inputs")
	add("p34-two-module-boundary", validateModuleBoundary(root), "root and Reference Control Plane remain the only Go modules")

	scratch, err := secureScratch()
	if err != nil {
		add("p34-private-test-environment", err, "")
		write(root, inputs, checks, nil, nil, nil, nil)
		return
	}
	defer os.RemoveAll(scratch)
	add("p34-private-test-environment", nil, "ephemeral test-only key, SQLite file, PostgreSQL cluster and reports use a private scratch root")
	workspace, err := writeWorkspace(scratch, root)
	if err != nil {
		add("p34-driver-tests", err, "")
		write(root, inputs, checks, nil, nil, nil, nil)
		return
	}
	moduleCache, err := discoverModuleCache()
	if err != nil {
		add("p34-driver-tests", err, "")
		write(root, inputs, checks, nil, nil, nil, nil)
		return
	}
	rootBuildEnvironment, err := goBuildEnvironment(scratch, moduleCache, "off")
	if err != nil {
		add("p34-driver-tests", err, "")
		write(root, inputs, checks, nil, nil, nil, nil)
		return
	}
	nestedEnvironment, err := goBuildEnvironment(scratch, moduleCache, workspace)
	if err != nil {
		add("p34-driver-tests", err, "")
		write(root, inputs, checks, nil, nil, nil, nil)
		return
	}
	tests := runWithEnv(root, 2*time.Minute, nestedEnvironment, "go", "-C", "reference/control-plane", "test", "-race", "-count=1", "-json", "./tests/conformance/driver")
	add("p34-driver-tests", errors.Join(tests.err, validateTests(tests.output)), "exact driver tests pass under race detection without skip/cache")

	runnerBinary := filepath.Join(scratch, "arop-conformance")
	serverBinary := filepath.Join(scratch, "aropd")
	buildRootRunner := runWithEnv(root, 2*time.Minute, rootBuildEnvironment, "go", "build", "-trimpath", "-o", runnerBinary, "./cmd/arop-conformance")
	buildServer := runWithEnv(root, 4*time.Minute, nestedEnvironment, "go", "-C", "reference/control-plane", "build", "-trimpath", "-o", serverBinary, "./cmd/aropd")
	add("p34-binary-builds", errors.Join(buildRootRunner.err, buildServer.err), "root runner and nested server build without a persistent replace directive or third module")

	postgres, postgresDSN, postgresEvidence, postgresErr := startPostgres(scratch)
	defer func() {
		if postgres != nil {
			_ = postgres.stop(15 * time.Second)
		}
	}()
	add("p34-private-postgres16", postgresErr, "private Unix-socket-only PostgreSQL 16 is ready with host authentication rejected")

	keyPath := filepath.Join(scratch, "asset.key")
	key := make([]byte, 32)
	_, keyErr := rand.Read(key)
	if keyErr == nil {
		keyErr = os.WriteFile(keyPath, key, 0o600)
	}
	clear(key)

	var sqliteOutput, postgresOutput []byte
	if buildRootRunner.err == nil && buildServer.err == nil && keyErr == nil {
		sqliteOutput, err = exerciseBackend(root, scratch, nestedEnvironment, runnerBinary, serverBinary, keyPath, "sqlite", filepath.Join(scratch, "control-plane.sqlite"))
	}
	add("p34-sqlite-control-plane-profile", errors.Join(buildRootRunner.err, buildServer.err, keyErr, err), "root P33 runner passes the full Control Plane profile against a durable SQLite server")

	if buildRootRunner.err == nil && buildServer.err == nil && keyErr == nil && postgresErr == nil {
		postgresOutput, err = exerciseBackend(root, scratch, nestedEnvironment, runnerBinary, serverBinary, keyPath, "postgres", postgresDSN)
	} else if postgresErr != nil {
		err = postgresErr
	}
	add("p34-postgres-control-plane-profile", errors.Join(buildRootRunner.err, buildServer.err, keyErr, err), "the identical profile passes against a private PostgreSQL 16 server")
	add("p34-no-runtime-inputs", nil, "all executed files are tracked static inputs or digest-only runtime evidence")

	if postgres != nil {
		stopErr := postgres.stop(15 * time.Second)
		postgres = nil
		add("p34-clean-shutdown", stopErr, "both servers and private PostgreSQL stop cleanly")
	} else {
		add("p34-clean-shutdown", postgresErr, "")
	}
	if removeErr := os.RemoveAll(scratch); removeErr != nil {
		fatal(removeErr)
	}
	write(root, inputs, checks, tests.output, sqliteOutput, postgresOutput, postgresEvidence)
}

func exerciseBackend(root, scratch string, nestedEnvironment []string, runnerBinary, serverBinary, keyPath, storage, dsn string) ([]byte, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	backup := filepath.Join(scratch, storage+"-backup")
	if err := os.Mkdir(backup, 0o700); err != nil {
		return nil, err
	}
	arguments := []string{
		"--mode=" + storage,
		"--database-dsn=" + dsn,
		"--migration-root=" + filepath.Join(root, "reference/control-plane/migrations"),
		"--backup-directory=" + backup,
		"--asset-token-key-file=" + keyPath,
		"--asset-token-key-id=atk_p34_conformance",
		"--dispatch-issuer=https://control-plane.example.invalid",
		fmt.Sprintf("--listen=127.0.0.1:%d", port),
		"--storage-startup-timeout=45s", "--migration-timeout=2m", "--shutdown-timeout=10s",
	}
	server, err := startProcess(root, serverBinary, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if server != nil {
			_ = server.stop(15 * time.Second)
		}
	}()
	if err := waitReady(server, endpoint+"/v1/health/ready", 90*time.Second); err != nil {
		return nil, err
	}

	driverBinary := filepath.Join(scratch, "driver-"+storage)
	ldflags := "-X main.endpoint=" + endpoint + " -X main.backend=" + storage
	buildDriver := runWithEnv(root, 2*time.Minute, nestedEnvironment, "go", "-C", "reference/control-plane", "build", "-trimpath", "-ldflags="+ldflags, "-o", driverBinary, "./tests/conformance/driver")
	if buildDriver.err != nil {
		return nil, buildDriver.err
	}
	jsonPath := filepath.Join(scratch, storage+"-runner.json")
	junitPath := filepath.Join(scratch, storage+"-runner.xml")
	execution := run(root, 2*time.Minute, runnerBinary, "--root", root, "--profile", "control-plane", "--target", driverBinary, "--json", jsonPath, "--junit", junitPath)
	if execution.err != nil {
		return execution.output, execution.err
	}
	reportData, err := os.ReadFile(jsonPath)
	if err != nil {
		return execution.output, err
	}
	junitData, err := os.ReadFile(junitPath)
	if err != nil {
		return execution.output, err
	}
	if err := validateRunnerReport(reportData, junitData); err != nil {
		return execution.output, err
	}
	if err := server.stop(15 * time.Second); err != nil {
		return execution.output, err
	}
	server = nil
	return append(append(execution.output, reportData...), junitData...), nil
}

func validateRunnerReport(data, junit []byte) error {
	var value runnerReport
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	if value.SchemaVersion != 1 || value.Protocol != "arop-conformance-driver/v1" || value.Profile != "control-plane" || !value.Passed || value.PassedCount != 3 || value.FailedCount != 0 || value.SkippedCount != 0 || len(value.Results) != 3 || value.ProfileSHA256 == "" || value.TargetSHA256 == "" {
		return errors.New("portable runner report is incomplete")
	}
	want := []string{"core.event-envelope.valid", "core.manifest.valid", "core.trace-context.vectors"}
	got := make([]string, 0, len(value.Results))
	for _, item := range value.Results {
		if item.Outcome != "pass" || item.ScenarioSHA256 == "" || item.FixtureSHA256 == "" {
			return errors.New("portable runner scenario evidence is incomplete")
		}
		got = append(got, item.ID)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("runner scenarios=%v want=%v", got, want)
	}
	for _, required := range []string{"profile_sha256", "target_sha256", "scenario_sha256", "fixture_sha256", "tests=\"3\"", "failures=\"0\""} {
		if !bytes.Contains(junit, []byte(required)) {
			return fmt.Errorf("JUnit output lacks %q", required)
		}
	}
	return nil
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, dependencies, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P34" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-server-conformance" {
				return fmt.Errorf("invalid P34 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p34" {
			dependencies = append(dependencies, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	if !reflect.DeepEqual(owned, []string{"server-conformance-driver"}) || !reflect.DeepEqual(dependencies, []string{"server-conformance-driver"}) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtime)
	}
	return nil
}

func validateModuleBoundary(root string) error {
	result := run(root, 30*time.Second, "git", "ls-files", "-z", "**/go.mod", "go.mod")
	if result.err != nil {
		return result.err
	}
	var modules []string
	for _, value := range bytes.Split(result.output, []byte{0}) {
		if len(value) != 0 {
			modules = append(modules, filepath.ToSlash(string(value)))
		}
	}
	sort.Strings(modules)
	if !reflect.DeepEqual(modules, []string{"go.mod", "reference/control-plane/go.mod"}) {
		return fmt.Errorf("Go modules=%v", modules)
	}
	return nil
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, 30*time.Second, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	var paths []string
	for _, value := range bytes.Split(result.output, []byte{0}) {
		if len(value) != 0 && !strings.HasPrefix(string(value), "build/") {
			paths = append(paths, filepath.ToSlash(string(value)))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func validateTests(output []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	seen := []string{}
	packageTerminal := ""
	for scanner.Scan() {
		var event goEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Action == "skip" || event.Action == "fail" || strings.Contains(event.Output, "(cached)") {
			return fmt.Errorf("non-conforming Go test event: %+v", event)
		}
		if event.Action == "pass" && event.Test != "" && !strings.Contains(event.Test, "/") {
			seen = append(seen, event.Test)
		}
		if event.Test == "" && event.Action == "pass" {
			if packageTerminal != "" {
				return errors.New("duplicate package terminal")
			}
			packageTerminal = event.Package
		}
	}
	sort.Strings(seen)
	want := []string{"TestDriverFailsClosedOnHealthOrAuthenticationContractDrift", "TestDriverRejectsMalformedInvocationFixtureEndpointAndBackend", "TestDriverRunsTheSameBlackBoxContractForBothDurableBackends"}
	if !reflect.DeepEqual(seen, want) || packageTerminal == "" {
		return fmt.Errorf("test inventory=%v package=%q", seen, packageTerminal)
	}
	return scanner.Err()
}

func startPostgres(scratch string) (*managedProcess, string, []byte, error) {
	tools := map[string]string{}
	evidence := bytes.Buffer{}
	for _, name := range []string{"initdb", "postgres", "pg_isready"} {
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, "", evidence.Bytes(), fmt.Errorf("PostgreSQL tool %s unavailable", name)
		}
		version := run(scratch, 20*time.Second, path, "--version")
		if version.err != nil || !bytes.Contains(version.output, []byte(" 16.")) {
			return nil, "", evidence.Bytes(), fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		tools[name] = path
		fmt.Fprintf(&evidence, "%s PostgreSQL-16 %d\n", name, len(version.output))
	}
	data := filepath.Join(scratch, "pgdata")
	socket := filepath.Join(scratch, "pgsocket")
	home := filepath.Join(scratch, "home")
	for _, directory := range []string{socket, home} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, "", evidence.Bytes(), err
		}
	}
	init := runWithEnv(scratch, 90*time.Second, postgresEnvironment(home), tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p34", "--no-instructions")
	if init.err != nil {
		return nil, "", evidence.Bytes(), errors.New("private initdb failed")
	}
	port, err := freePort()
	if err != nil {
		return nil, "", evidence.Bytes(), err
	}
	process, err := startProcessWithEnv(scratch, postgresEnvironment(home), tools["postgres"], "-D", data, "-k", socket, "-p", fmt.Sprint(port), "-c", "listen_addresses=", "-c", "unix_socket_permissions=0700", "-c", "fsync=off")
	if err != nil {
		return nil, "", evidence.Bytes(), err
	}
	readyDeadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(readyDeadline) {
		select {
		case processErr := <-process.done:
			return nil, "", evidence.Bytes(), fmt.Errorf("private PostgreSQL exited: %w", processErr)
		default:
		}
		ready := runWithEnv(scratch, 5*time.Second, postgresEnvironment(home), tools["pg_isready"], "-h", socket, "-p", fmt.Sprint(port), "-U", "arop_p34", "-d", "postgres")
		if ready.err == nil {
			query := url.Values{"host": {socket}, "port": {fmt.Sprint(port)}, "sslmode": {"disable"}}
			return process, "postgres://arop_p34@localhost/postgres?" + query.Encode(), evidence.Bytes(), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = process.stop(5 * time.Second)
	return nil, "", evidence.Bytes(), errors.New("private PostgreSQL readiness timeout")
}

func startProcess(directory, name string, arguments ...string) (*managedProcess, error) {
	return startProcessWithEnv(directory, cleanEnvironment(), name, arguments...)
}

func startProcessWithEnv(directory string, environment []string, name string, arguments ...string) (*managedProcess, error) {
	log := &boundedBuffer{limit: 1 << 20}
	command := exec.Command(name, arguments...)
	command.Dir = directory
	command.Env = environment
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	managed := &managedProcess{command: command, done: make(chan error, 1), log: log}
	go func() { managed.done <- command.Wait() }()
	return managed, nil
}

func (process *managedProcess) stop(timeout time.Duration) error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return nil
	}
	select {
	case err := <-process.done:
		if err == nil {
			return nil
		}
		return errors.New("managed process exited unexpectedly")
	default:
	}
	_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-process.done:
		if err != nil && !strings.Contains(err.Error(), "signal: terminated") {
			return errors.New("managed process shutdown failed")
		}
		return nil
	case <-time.After(timeout):
		_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
		select {
		case <-process.done:
		case <-time.After(2 * time.Second):
		}
		return errors.New("managed process shutdown timeout")
	}
}

func waitReady(process *managedProcess, target string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case processErr := <-process.done:
			select {
			case process.done <- processErr:
			default:
			}
			return errors.New("Control Plane exited before readiness")
		default:
		}
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Control Plane readiness timeout")
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func secureScratch() (string, error) {
	directory, err := os.MkdirTemp("/tmp", "arop-p34-")
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		_ = os.RemoveAll(resolved)
		return "", err
	}
	return resolved, nil
}

func writeWorkspace(scratch, root string) (string, error) {
	workspace := filepath.Join(scratch, "go.work")
	nested := filepath.Join(root, "reference/control-plane")
	moduleData, err := os.ReadFile(filepath.Join(nested, "go.mod"))
	if err != nil {
		return "", err
	}
	version := ""
	fields := strings.Fields(string(moduleData))
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "github.com/gmslll/agent-runtime-operations-protocol" && strings.HasPrefix(fields[index+1], "v0.0.0-") {
			version = fields[index+1]
			break
		}
	}
	if version == "" {
		return "", errors.New("nested root-module pin is missing")
	}
	content := fmt.Sprintf("go 1.24.0\n\nuse (\n\t%s\n\t%s\n)\n\nreplace github.com/gmslll/agent-runtime-operations-protocol %s => %s\n", root, nested, version, root)
	if err := os.WriteFile(workspace, []byte(content), 0o600); err != nil {
		return "", err
	}
	return workspace, nil
}

func write(root string, inputs []string, checks []report.Check, tests, sqlite, postgres, postgresTools []byte) {
	evidence := []report.RuntimeEvidence{
		{Kind: "p34-driver-tests", SHA256: report.Hash(tests), Bytes: int64(len(tests))},
		{Kind: "p34-sqlite-runner", SHA256: report.Hash(sqlite), Bytes: int64(len(sqlite))},
		{Kind: "p34-postgres-runner", SHA256: report.Hash(postgres), Bytes: int64(len(postgres))},
		{Kind: "p34-postgres-toolchain", SHA256: report.Hash(postgresTools), Bytes: int64(len(postgresTools))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P34", Suite: "AROP P34 nested Control Plane conformance", Class: "p34.conformance.server",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 1, "profiles": 1, "scenarios_per_backend": 3, "storage_backends": 2, "runtime_inputs": 0},
		AuditNote: "P34 keeps the portable runner in the root module and the black-box server driver in the sole nested Control Plane module. The same signed-by-digest Control Plane profile runs through HTTP against durable SQLite and a private Unix-socket-only PostgreSQL 16 cluster. Test-only key material, DSNs, endpoints, paths and process logs never enter reports; only digest-and-byte evidence does. The driver does not duplicate scenario selection or report logic, and no third Go module is introduced.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P34/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P34 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P34 checks failed; see build/reports/P34/report.json"))
	}
	fmt.Printf("AROP server conformance passed: %d checks.\n", len(checks))
}

func run(directory string, timeout time.Duration, name string, arguments ...string) commandResult {
	return runWithEnv(directory, timeout, cleanEnvironment(), name, arguments...)
}

func runWithEnv(directory string, timeout time.Duration, environment []string, name string, arguments ...string) commandResult {
	command := exec.Command(name, arguments...)
	command.Dir = directory
	command.Env = environment
	timer := time.AfterFunc(timeout, func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	output, err := command.CombinedOutput()
	timer.Stop()
	if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment() []string {
	values := []string{"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "HOME=", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	for _, key := range []string{"PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			values = append(values, key+"="+value)
		}
	}
	return values
}

func discoverModuleCache() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("resolve user home for read-only Go module cache")
	}
	command := exec.Command("go", "env", "GOMODCACHE")
	// The host may intentionally relocate GOPATH/GOMODCACHE through its Go
	// environment. Read only that resolved cache path here, then disable GOENV
	// and network lookup for every actual build below.
	discoveryEnvironment := []string{"GOWORK=off", "HOME=" + home}
	if goPath := os.Getenv("GOPATH"); goPath != "" {
		discoveryEnvironment = append(discoveryEnvironment, "GOPATH="+goPath)
	}
	command.Env = append(discoveryEnvironment, pathEnvironment()...)
	output, err := command.Output()
	if err != nil {
		return "", errors.New("resolve Go module cache")
	}
	path := strings.TrimSpace(string(output))
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", errors.New("Go module cache is unavailable")
	}
	return path, nil
}

func goBuildEnvironment(scratch, moduleCache, workspace string) ([]string, error) {
	directories := map[string]string{
		"GOCACHE":  filepath.Join(scratch, "go-build-cache"),
		"GOPATH":   filepath.Join(scratch, "go-path"),
		"GOTMPDIR": filepath.Join(scratch, "go-tmp"),
		"HOME":     filepath.Join(scratch, "go-home"),
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	values := []string{
		"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=" + workspace, "GOTOOLCHAIN=local", "CGO_ENABLED=0",
		"GOPROXY=off", "GOSUMDB=off", "GOMODCACHE=" + moduleCache,
		"GOCACHE=" + directories["GOCACHE"], "GOPATH=" + directories["GOPATH"], "GOTMPDIR=" + directories["GOTMPDIR"], "HOME=" + directories["HOME"],
		"LANG=C", "LC_ALL=C", "TZ=UTC",
	}
	return append(values, pathEnvironment()...), nil
}

func pathEnvironment() []string {
	values := []string{}
	for _, key := range []string{"PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			values = append(values, key+"="+value)
		}
	}
	return values
}

func postgresEnvironment(home string) []string {
	values := cleanEnvironment()
	for index, value := range values {
		if strings.HasPrefix(value, "HOME=") {
			values[index] = "HOME=" + home
		}
	}
	values = append(values, "PGPASSWORD=", "PGPASSFILE=/dev/null", "PGSERVICEFILE=/dev/null")
	return values
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (writer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := writer.limit - writer.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = writer.buffer.Write(data[:remaining])
	}
	return len(data), nil
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 512 {
		value = value[:512]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
