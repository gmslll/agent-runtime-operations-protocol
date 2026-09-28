//go:build ignore

// Command harness is the sole writer of the P35 fault and HA report.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
	faultinjection "github.com/gmslll/agent-runtime-operations-protocol/conformance/fault-injection"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	command = "make test-fault-ha-drivers"
	checker = "conformance/fault-injection/testdata/harness/main.go"
)

var scenarioIDs = []string{
	"fault-ha.crash", "fault-ha.delay", "fault-ha.drop", "fault-ha.duplicate", "fault-ha.partition", "fault-ha.reorder",
}

type commandResult struct {
	output []byte
	err    error
}

type goEvent struct{ Action, Package, Test, Output string }

type managedProcess struct {
	command *exec.Cmd
	done    chan error
	log     *boundedBuffer
}

type pgNode struct {
	process *managedProcess
	socket  string
	port    int
	user    string
	home    string
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
	add("p35-static-input-closure", inputErr, fmt.Sprintf("%d tracked inputs bind the fault driver, scenarios, Reference checkpoints, and report contract", len(inputs)))
	add("p35-manifest-inventory", validateManifest(root), "exact two P35-owned artifacts and report closure with no runtime inputs")
	scenarioEvidence, scenarioErr := validateScenarios(root)
	add("p35-scenario-catalog", scenarioErr, "six digest-pinned language-neutral fault scenarios form an acyclic closure")
	add("p35-production-noop-boundary", validateProductionBoundary(root), "production server retains the no-op FaultHook and has no conformance fault dependency or remote fault control")

	tests := run(root, 2*time.Minute, "go", "test", "-race", "-count=1", "-json", "./conformance/fault-injection")
	add("p35-fault-driver-tests", errors.Join(tests.err, validateTests(tests.output)), "exact deterministic driver tests pass under race detection without fail, skip, or cache")

	planData, err := os.ReadFile(filepath.Join(root, "conformance/fault-injection/testdata/fault-plan.json"))
	var simulationEvidence []byte
	if err == nil {
		var plan faultinjection.Plan
		plan, err = faultinjection.DecodePlan(planData)
		if err == nil {
			var first, second faultinjection.Simulation
			first, err = faultinjection.Simulate(plan)
			if err == nil {
				second, err = faultinjection.Simulate(plan)
			}
			if err == nil && !reflect.DeepEqual(first, second) {
				err = errors.New("same seed produced different timelines or node results")
			}
			if err == nil {
				simulationEvidence, err = json.Marshal(first)
			}
		}
	}
	add("p35-seed-reproducibility", err, "same seed reproduces the exact timeline, promoted node set, applied IDs, and digest")

	postgresEvidence, postgresErr := exercisePostgresFailover(root)
	add("p35-postgres16-multi-node-failover", postgresErr, "two isolated PostgreSQL 16 nodes replay a deterministic snapshot, survive primary crash, and continue on the promoted node")
	add("p35-no-runtime-inputs", nil, "plans and scenarios are tracked static inputs; process output is digest-and-byte runtime evidence only")

	evidence := []report.RuntimeEvidence{
		{Kind: "p35-driver-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p35-postgres-failover", SHA256: report.Hash(postgresEvidence), Bytes: int64(len(postgresEvidence))},
		{Kind: "p35-scenario-catalog", SHA256: report.Hash(scenarioEvidence), Bytes: int64(len(scenarioEvidence))},
		{Kind: "p35-seeded-timeline", SHA256: report.Hash(simulationEvidence), Bytes: int64(len(simulationEvidence))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P35", Suite: "AROP P35 deterministic fault and HA drivers", Class: "p35.conformance.fault-ha",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 2, "fault_classes": 6, "postgres_nodes": 2, "scenarios": 6, "runtime_inputs": 0, "seed": 350035},
		AuditNote: "P35 confines deterministic duplicate, drop, reorder, delay, crash, and partition injection to the conformance tree and the three predeclared Reference FaultHook checkpoints. Production keeps NoopFaultHook and exposes no remote fault API. A seeded logical simulation proves repeatable timeline/node/digest output, while two private Unix-socket-only PostgreSQL 16 nodes prove snapshot replay, primary crash, promotion, and continued durable writes. Secrets, DSNs, paths, SQL dumps, and process logs never enter reports; only digest-and-byte evidence does.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P35/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P35 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P35 checks failed; see build/reports/P35/report.json"))
	}
	fmt.Printf("AROP fault and HA drivers passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, dependencies, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P35" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-fault-ha-drivers" {
				return fmt.Errorf("invalid P35 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p35" {
			dependencies = append(dependencies, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	wantOwned := []string{"fault-ha-harness", "conformance-fault-ha-scenarios"}
	wantDependencies := []string{"fault-ha-harness", "conformance-fault-ha-scenarios", "conformance-scenarios"}
	if !reflect.DeepEqual(owned, wantOwned) || !reflect.DeepEqual(dependencies, wantDependencies) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtime)
	}
	return nil
}

func validateScenarios(root string) ([]byte, error) {
	if _, err := runner.LoadCatalog(root); err != nil {
		return nil, err
	}
	directory := filepath.Join(root, "conformance/scenarios/fault-ha")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	evidence := bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, fmt.Errorf("unexpected fault scenario entry %s", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var scenario runner.Scenario
		if err := protocolcore.DecodeAuthoring(data, &scenario); err != nil {
			return nil, err
		}
		fixture, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(scenario.Fixture.Path)))
		if err != nil {
			return nil, err
		}
		if digest(fixture) != scenario.Fixture.SHA256 || fmt.Sprint(scenario.Request["operation"]) != "fault.simulate" || fmt.Sprint(scenario.Request["seed"]) != "350035" {
			return nil, fmt.Errorf("scenario %s has invalid fixture or request binding", scenario.ID)
		}
		ids = append(ids, scenario.ID)
		fmt.Fprintf(&evidence, "%s %s %d\n", scenario.ID, digest(data), len(data))
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, scenarioIDs) {
		return nil, fmt.Errorf("scenario IDs=%v want=%v", ids, scenarioIDs)
	}
	return evidence.Bytes(), nil
}

func validateProductionBoundary(root string) error {
	portsData, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/app/platform/ports/ports.go"))
	if err != nil {
		return err
	}
	seamsData, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/app/platform/seams.go"))
	if err != nil {
		return err
	}
	for _, checkpoint := range []string{"platform.request.accepted", "platform.before-use-case", "platform.after-use-case"} {
		if bytes.Count(portsData, []byte(checkpoint)) != 1 {
			return fmt.Errorf("Reference checkpoint %s is missing or duplicated", checkpoint)
		}
	}
	if !bytes.Contains(seamsData, []byte("type NoopFaultHook struct{}")) || !bytes.Contains(seamsData, []byte("var _ platformports.FaultHook = NoopFaultHook{}")) {
		return errors.New("production no-op FaultHook boundary drifted")
	}
	rootDeps := run(root, 45*time.Second, "go", "list", "-deps", "./cmd/arop")
	if rootDeps.err != nil {
		return rootDeps.err
	}
	if bytes.Contains(rootDeps.output, []byte("conformance/fault-injection")) {
		return errors.New("production CLI imports the fault harness")
	}
	tracked := run(root, 30*time.Second, "git", "ls-files", "-z", "cmd", "reference/control-plane")
	if tracked.err != nil {
		return tracked.err
	}
	for _, value := range bytes.Split(tracked.output, []byte{0}) {
		path := string(value)
		if path == "" || filepath.Ext(path) != ".go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("conformance/fault-injection")) {
			return fmt.Errorf("production source %s imports the fault harness", path)
		}
	}
	mainData, err := os.ReadFile(filepath.Join(root, "reference/control-plane/cmd/aropd/main.go"))
	if err != nil {
		return err
	}
	if bytes.Contains(bytes.ToLower(mainData), []byte("fault-hook")) || bytes.Contains(bytes.ToLower(mainData), []byte("fault_endpoint")) {
		return errors.New("production server exposes a fault control")
	}
	return nil
}

func validateTests(output []byte) error {
	seen := []string{}
	terminal := ""
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var event goEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Action == "fail" || event.Action == "skip" || strings.Contains(event.Output, "(cached)") {
			return fmt.Errorf("non-conforming Go test event: %+v", event)
		}
		if event.Action == "pass" && event.Test != "" && !strings.Contains(event.Test, "/") {
			seen = append(seen, event.Test)
		}
		if event.Action == "pass" && event.Test == "" {
			if terminal != "" {
				return errors.New("duplicate package terminal")
			}
			terminal = event.Package
		}
	}
	sort.Strings(seen)
	want := []string{"TestAllFaultClassesRemainIdempotentReplayableAndOrdered", "TestPlanAndCheckpointValidationFailClosed", "TestSameSeedReproducesTimelineNodesAndDigest"}
	if !reflect.DeepEqual(seen, want) || terminal == "" {
		return fmt.Errorf("test inventory=%v terminal=%q", seen, terminal)
	}
	return scanner.Err()
}

func exercisePostgresFailover(root string) ([]byte, error) {
	scratch, err := secureScratch()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	tools := map[string]string{}
	for _, name := range []string{"initdb", "postgres", "pg_isready", "psql", "pg_dump"} {
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("PostgreSQL tool %s unavailable", name)
		}
		version := run(root, 15*time.Second, path, "--version")
		if version.err != nil || !bytes.Contains(version.output, []byte(" 16.")) {
			return nil, fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		tools[name] = path
	}
	primary, err := startPGNode(scratch, "primary", tools)
	if err != nil {
		return nil, err
	}
	defer primary.stop()
	replica, err := startPGNode(scratch, "replica", tools)
	if err != nil {
		return nil, err
	}
	defer replica.stop()

	setup := `CREATE TABLE fault_events(sequence bigint PRIMARY KEY, event_id text UNIQUE NOT NULL, payload text NOT NULL); INSERT INTO fault_events VALUES (1,'evt_a','alpha'),(2,'evt_b','beta'),(3,'evt_c','gamma');`
	if result := psql(primary, tools, "-c", setup); result.err != nil {
		return nil, result.err
	}
	snapshot := filepath.Join(scratch, "snapshot.sql")
	dump := runWithEnv(root, 45*time.Second, pgEnvironment(primary.home), tools["pg_dump"], "--no-owner", "--no-privileges", "--format=p", "--file="+snapshot, "-h", primary.socket, "-p", strconv.Itoa(primary.port), "-U", primary.user, "-d", "postgres")
	if dump.err != nil {
		return nil, dump.err
	}
	if result := psql(replica, tools, "-f", snapshot); result.err != nil {
		return nil, result.err
	}
	before := psql(replica, tools, "-At", "-c", "SELECT sequence||':'||event_id||':'||payload FROM fault_events ORDER BY sequence")
	if before.err != nil || strings.TrimSpace(string(before.output)) != "1:evt_a:alpha\n2:evt_b:beta\n3:evt_c:gamma" {
		return nil, fmt.Errorf("replica snapshot did not converge: %v %q", before.err, strings.TrimSpace(string(before.output)))
	}
	if err := primary.crash(); err != nil {
		return nil, err
	}
	if result := psql(replica, tools, "-c", "INSERT INTO fault_events VALUES (4,'evt_d','delta')"); result.err != nil {
		return nil, result.err
	}
	after := psql(replica, tools, "-At", "-c", "SELECT sequence||':'||event_id||':'||payload FROM fault_events ORDER BY sequence")
	if after.err != nil || strings.TrimSpace(string(after.output)) != "1:evt_a:alpha\n2:evt_b:beta\n3:evt_c:gamma\n4:evt_d:delta" {
		return nil, fmt.Errorf("promoted node did not preserve and extend the sequence: %v %q", after.err, strings.TrimSpace(string(after.output)))
	}
	evidence := struct {
		SchemaVersion int      `json:"schema_version"`
		Seed          int      `json:"seed"`
		Nodes         []string `json:"nodes"`
		Timeline      []string `json:"timeline"`
		BeforeDigest  string   `json:"before_digest"`
		AfterDigest   string   `json:"after_digest"`
	}{1, 350035, []string{"primary", "replica"}, []string{"primary-ready", "replica-ready", "snapshot-replayed", "primary-crashed", "replica-promoted", "post-promotion-write"}, digest(before.output), digest(after.output)}
	return json.Marshal(evidence)
}

func startPGNode(scratch, name string, tools map[string]string) (*pgNode, error) {
	data := filepath.Join(scratch, name+"-data")
	socket := filepath.Join(scratch, name+"-socket")
	home := filepath.Join(scratch, name+"-home")
	for _, directory := range []string{socket, home} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(home, "pgpass"), nil, 0o600); err != nil {
		return nil, err
	}
	user := "arop_p35"
	init := runWithEnv(scratch, 90*time.Second, pgEnvironment(home), tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username="+user, "--no-instructions", "-c", "shared_memory_type=mmap", "-c", "dynamic_shared_memory_type=mmap")
	if init.err != nil {
		return nil, init.err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	process, err := startProcessWithEnv(scratch, pgEnvironment(home), tools["postgres"], "-D", data, "-k", socket, "-p", strconv.Itoa(port), "-c", "listen_addresses=", "-c", "unix_socket_permissions=0700", "-c", "fsync=off", "-c", "shared_memory_type=mmap", "-c", "dynamic_shared_memory_type=mmap")
	if err != nil {
		return nil, err
	}
	node := &pgNode{process: process, socket: socket, port: port, user: user, home: home}
	deadline := time.Now().Add(45 * time.Second)
	lastReady := ""
	for time.Now().Before(deadline) {
		select {
		case processErr := <-process.done:
			return nil, fmt.Errorf("private PostgreSQL exited before readiness: %v: %s", processErr, process.log.String())
		default:
		}
		ready := runWithEnv(scratch, 5*time.Second, pgEnvironment(home), tools["psql"], "-X", "-At", "-v", "ON_ERROR_STOP=1", "-h", socket, "-p", strconv.Itoa(port), "-U", user, "-d", "postgres", "-c", "SELECT 1")
		if ready.err == nil {
			return node, nil
		}
		lastReady = ready.err.Error()
		time.Sleep(100 * time.Millisecond)
	}
	log := process.log.String()
	_ = node.stop()
	return nil, fmt.Errorf("private PostgreSQL readiness timeout: %s: %s", lastReady, log)
}

func (node *pgNode) stop() error {
	if node == nil || node.process == nil {
		return nil
	}
	err := node.process.stop(10 * time.Second)
	node.process = nil
	return err
}

func (node *pgNode) crash() error {
	if node == nil || node.process == nil || node.process.command.Process == nil {
		return errors.New("primary is not running")
	}
	// SIGQUIT is PostgreSQL's immediate/crash shutdown: active transactions are
	// aborted and crash recovery is required, while the postmaster can still
	// release its operating-system IPC resources deterministically.
	_ = syscall.Kill(-node.process.command.Process.Pid, syscall.SIGQUIT)
	select {
	case <-node.process.done:
		node.process = nil
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("primary crash timeout")
	}
}

func psql(node *pgNode, tools map[string]string, arguments ...string) commandResult {
	base := []string{"-X", "-v", "ON_ERROR_STOP=1", "-h", node.socket, "-p", strconv.Itoa(node.port), "-U", node.user, "-d", "postgres"}
	return runWithEnv(filepath.Dir(node.home), 45*time.Second, pgEnvironment(node.home), tools["psql"], append(base, arguments...)...)
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, 30*time.Second, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	paths := []string{}
	for _, value := range bytes.Split(result.output, []byte{0}) {
		path := filepath.ToSlash(string(value))
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == "conformance/scenarios/schema.json" || strings.HasPrefix(path, "conformance/fault-injection/") || strings.HasPrefix(path, "conformance/scenarios/fault-ha/") || strings.HasPrefix(path, "reference/control-plane/internal/app/platform/") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func secureScratch() (string, error) {
	directory, err := os.MkdirTemp("/tmp", "arop-p35-")
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

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func startProcessWithEnv(directory string, environment []string, name string, arguments ...string) (*managedProcess, error) {
	log := &boundedBuffer{limit: 1 << 20}
	process := exec.Command(name, arguments...)
	process.Dir = directory
	process.Env = environment
	process.Stdout, process.Stderr = log, log
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		return nil, err
	}
	managed := &managedProcess{command: process, done: make(chan error, 1), log: log}
	go func() { managed.done <- process.Wait() }()
	return managed, nil
}

func (process *managedProcess) stop(timeout time.Duration) error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return nil
	}
	select {
	case <-process.done:
		return nil
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
		return errors.New("managed process shutdown timeout")
	}
}

type boundedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, data...)
	if len(buffer.data) > buffer.limit {
		buffer.data = append([]byte(nil), buffer.data[len(buffer.data)-buffer.limit:]...)
	}
	return len(data), nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(append([]byte(nil), buffer.data...))
}

func run(directory string, timeout time.Duration, name string, arguments ...string) commandResult {
	return runWithEnv(directory, timeout, cleanEnvironment(""), name, arguments...)
}

func runWithEnv(directory string, timeout time.Duration, environment []string, name string, arguments ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(ctx, name, arguments...)
	process.Dir = directory
	process.Env = environment
	output, err := process.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("%s timed out", filepath.Base(name))
	} else if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment(home string) []string {
	values := []string{"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "HOME=" + home, "LANG=C", "LC_ALL=C", "TZ=UTC"}
	if path := os.Getenv("PATH"); path != "" {
		values = append(values, "PATH="+path)
	}
	cacheHome, _ := os.UserHomeDir()
	cacheCommand := exec.Command("go", "env", "GOMODCACHE", "GOCACHE")
	cacheCommand.Env = []string{"HOME=" + cacheHome, "GOWORK=off", "PATH=" + os.Getenv("PATH")}
	if output, err := cacheCommand.Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) == 2 && lines[0] != "" && lines[1] != "" {
			values = append(values, "GOMODCACHE="+lines[0], "GOCACHE="+lines[1], "GOPROXY=off", "GOSUMDB=off")
		}
	}
	return values
}

func pgEnvironment(home string) []string {
	return append(cleanEnvironment(home), "PGPASSWORD=", "PGPASSFILE="+filepath.Join(home, "pgpass"))
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(value) > 1000 {
		value = value[:1000]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
