// Command harness runs the P10 identity, credential and SecretRef acceptance.
// Only this orchestrator writes the P10 JSON/JUnit report.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	expectedCommand = "make test-identity-secrets"
	checkerPath     = "reference/control-plane/internal/identity/testdata/harness/main.go"
	reportPath      = "build/reports/P10/report.json"
	nestedModule    = "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane"
	waiverPath      = "reference/control-plane/internal/identity/testdata/transition/p10-baseline-transition-waiver.json"
)

var testPackages = []string{
	"./cmd/aropd",
	"./internal/adapters/secrets",
	"./internal/app/platform/httpadapter",
	"./internal/identity",
	"./internal/identity/testdata/acceptance",
	"./internal/ports/secrets",
	"./internal/storage/migrate",
}

var requiredSubtests = []string{
	nestedModule + "/internal/adapters/secrets/TestResolverRechecksAuthorizationWindowBeforeExposure/audit_blocks_across_request_deadline",
	nestedModule + "/internal/adapters/secrets/TestResolverRechecksAuthorizationWindowBeforeExposure/binding_expires_while_audit_runs",
	nestedModule + "/internal/adapters/secrets/TestResolverRechecksAuthorizationWindowBeforeExposure/binding_expires_while_provider_runs",
	nestedModule + "/internal/adapters/secrets/TestResolverRechecksAuthorizationWindowBeforeExposure/caller_cancels_while_provider_runs",
	nestedModule + "/internal/app/platform/httpadapter/TestHandlerChainPropagatesCorrelatesAndRedacts/authenticated-chain-fails-closed-before-usecase",
	nestedModule + "/internal/app/platform/httpadapter/TestHandlerChainPropagatesCorrelatesAndRedacts/authenticated-chain-fails-closed-before-usecase/unavailable",
	nestedModule + "/internal/app/platform/httpadapter/TestHandlerChainPropagatesCorrelatesAndRedacts/authenticated-chain-fails-closed-before-usecase/valid",
	nestedModule + "/internal/identity/TestMutationCommitAndLifecycleAuditFailuresRollback/commit",
	nestedModule + "/internal/identity/TestMutationCommitAndLifecycleAuditFailuresRollback/revoke-audit",
	nestedModule + "/internal/identity/TestMutationCommitAndLifecycleAuditFailuresRollback/rotate-audit",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/postgres",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/postgres/one-to-five",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/postgres/one-to-five-recovery-apply",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/postgres/one-to-five-recovery-restore",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/postgres/one-to-five-recovery-verifier",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/sqlite",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/sqlite/one-to-five",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/sqlite/one-to-five-recovery-apply",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/sqlite/one-to-five-recovery-restore",
	nestedModule + "/internal/identity/testdata/acceptance/TestP10DurableIdentityConformance/sqlite/one-to-five-recovery-verifier",
}

var inputRoots = []string{
	"reference/control-plane/cmd/aropd",
	"reference/control-plane/internal/adapters/observability/durable",
	"reference/control-plane/internal/adapters/secrets",
	"reference/control-plane/internal/adapters/storage/postgres",
	"reference/control-plane/internal/adapters/storage/sqlite",
	"reference/control-plane/internal/app/platform",
	"reference/control-plane/internal/identity",
	"reference/control-plane/internal/ports/observability",
	"reference/control-plane/internal/ports/secrets",
	"reference/control-plane/internal/storage/migrate",
	"reference/control-plane/migrations/postgres/0001_base.sql",
	"reference/control-plane/migrations/postgres/0005_identity.sql",
	"reference/control-plane/migrations/sqlite/0001_base.sql",
	"reference/control-plane/migrations/sqlite/0005_identity.sql",
	"reference/control-plane/go.mod", "reference/control-plane/go.sum",
	"go.mod", "go.sum", "Makefile",
	"internal/tooling/report",
	"spec/artifact-manifest.yaml", "spec/schemas/artifact-manifest.schema.json",
	"spec/requirements.yaml", "spec/schemas/requirements.schema.json", "spec/schemas/check-report.schema.json",
	"docs/DECISIONS.md", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md",
	"docs/RELIABILITY_AND_OPERATIONS.md", "docs/SECURITY_AND_GOVERNANCE.md",
}

type commandResult struct {
	Output []byte
	Err    error
}

type goEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type postgresCluster struct {
	command      *exec.Cmd
	root, socket string
	log          bytes.Buffer
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = safeError(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	if os.Getenv("AROP_CHECK_COMMAND") != expectedCommand {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", expectedCommand))
	}

	inputs, before, inputErr := staticInputs(root)
	add("p10-static-input-closure", inputErr, fmt.Sprintf("%d regular symlink-free static inputs", len(inputs)))
	add("p10-runtime-inputs-empty", nil, "P10 has no runtime_inputs")
	add("p10-production-catalog", verifyCatalogSource(root), "Current catalog is P10 0001+0005 while the P09 snapshot remains 0001-only")
	add("p10-no-public-secret-api", verifyNoPublicSecretAPI(root), "SecretRef stays an internal port/adapter with no public route, schema, or SDK value API")

	temporaryRoot, scratchErr := filepath.EvalSymlinks("/tmp")
	if scratchErr == nil && (!filepath.IsAbs(temporaryRoot) || filepath.Clean(temporaryRoot) != temporaryRoot) {
		scratchErr = errors.New("resolved private temporary root is not absolute and clean")
	}
	var scratch string
	if scratchErr == nil {
		scratch, scratchErr = os.MkdirTemp(temporaryRoot, "arop-p10-")
	}
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p10-private-scratch", scratchErr, "private acceptance scratch created")
	var cluster *postgresCluster
	var postgresEvidence []byte
	if scratchErr == nil {
		cluster, postgresEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p10-private-postgres", err, "isolated PostgreSQL 16 uses a private Unix socket")

	var tests commandResult
	var inventoryEvidence []byte
	if inputErr == nil && cluster != nil {
		tests = runP10Tests(root, scratch, cluster)
	} else {
		tests.Err = errors.New("P10 test prerequisites failed")
	}
	inventoryChecks, discoveredInventory, inventoryErr := evaluateTests(root, tests)
	inventoryEvidence = discoveredInventory
	checks = append(checks, inventoryChecks...)
	add("p10-exact-test-inventory", inventoryErr, "all statically discovered packages and top-level tests ran once with no fail, skip, cache, or no-tests terminal")
	add("p10-exact-test-inventory-negatives", verifyInventoryNegatives(), "nested skip and duplicate top-level/package terminals fail closed")
	add("p10-secret-egress-scan", rejectSensitive(tests.Output), "test output contains no credential or SecretRef canary")

	transition := run(root, nil, "go", "run", "-modfile="+filepath.Join(root, "go.mod"), filepath.Join(root, "reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go"), "validate")
	add("p10-transition-waiver", transition.Err, "validated waiver equals independently discovered Git and manifest closure")
	transitionNegative := run(root, nil, "go", "run", "-modfile="+filepath.Join(root, "go.mod"), filepath.Join(root, "reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go"), "negative")
	add("p10-transition-waiver-negatives", transitionNegative.Err, "artifact, source, acceptance, constraint, owner, rename and changed-to-reverted negatives fail closed")

	p08 := run(root, nil, "make", "test-control-plane-platform")
	if p08.Err == nil {
		p08verify := run(root, nil, "make", "verify-report", "REPORT=build/reports/P08/report.json")
		p08.Output = append(p08.Output, p08verify.Output...)
		p08.Err = p08verify.Err
	}
	add("p10-p08-regression", p08.Err, "P08 acceptance and current report verification pass on the P10 head")
	p09 := run(root, nil, "make", "test-storage-migrations")
	if p09.Err == nil {
		p09verify := run(root, nil, "make", "verify-report", "REPORT=build/reports/P09/report.json")
		p09.Output = append(p09.Output, p09verify.Output...)
		p09.Err = p09verify.Err
	}
	add("p10-p09-regression", p09.Err, "P09 dual-database acceptance and current report verification pass on the P10 head")

	if cluster != nil {
		err = cluster.stop()
	} else {
		err = nil
	}
	add("p10-postgres-clean-shutdown", err, "private PostgreSQL stopped and was waited")
	if scratchErr == nil {
		err = os.RemoveAll(scratch)
	} else {
		err = nil
	}
	add("p10-private-scratch-cleanup", err, "acceptance scratch was removed")

	_, after, afterErr := staticInputs(root)
	if afterErr == nil && !reflect.DeepEqual(before, after) {
		afterErr = errors.New("static inputs changed during P10 acceptance")
	}
	add("p10-static-input-immutability", afterErr, "tracked static inputs remained byte-identical")

	runtimeEvidence := []report.RuntimeEvidence{
		{Kind: "p10-go-test", SHA256: report.Hash(tests.Output), Bytes: int64(len(tests.Output))},
		{Kind: "p10-test-inventory", SHA256: report.Hash(inventoryEvidence), Bytes: int64(len(inventoryEvidence))},
		{Kind: "p10-postgres-toolchain", SHA256: report.Hash(postgresEvidence), Bytes: int64(len(postgresEvidence))},
		{Kind: "p10-transition-validation", SHA256: report.Hash(transition.Output), Bytes: int64(len(transition.Output))},
		{Kind: "p10-transition-negatives", SHA256: report.Hash(transitionNegative.Output), Bytes: int64(len(transitionNegative.Output))},
		{Kind: "p08-regression", SHA256: report.Hash(p08.Output), Bytes: int64(len(p08.Output))},
		{Kind: "p09-regression", SHA256: report.Hash(p09.Output), Bytes: int64(len(p09.Output))},
	}
	written, writeErr := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P10", Suite: "AROP P10 identity and secrets", Class: "p10.identity-secrets",
		Command: expectedCommand, CheckerPath: checkerPath, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: runtimeEvidence,
		Checks:    checks,
		Summary:   map[string]any{"owned_artifacts": 7, "database_engines": 2, "runtime_inputs": 0, "test_packages": len(testPackages)},
		AuditNote: "P10 is reference-only. Its static closure binds all seven P10-owned artifacts, P08/P09 composition sources, paired 0001/0005 migrations, catalog declarations, transition waiver, report tooling and exact discovered test inventory. P08 and P09 are rerun on the same head. Runtime logs, PostgreSQL toolchain evidence and temporary database results appear only as digest and byte count. No public Secret value API, production 0010 migration, durable-to-memory fallback, raw credential, DSN or absolute scratch path enters the report.",
	})
	fatal(writeErr)
	for _, artifact := range []string{reportPath, "build/reports/P10/junit.xml"} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifact)))
		fatal(err)
		fatal(rejectSensitive(contents))
	}
	verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: reportPath})
	fatal(verifyErr)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(fmt.Errorf("P10 self-verification mode=%s success=%t", mode, verified.Success))
	}
	if !written.Success {
		fatal(errors.New("P10 identity/secrets checks failed; see " + reportPath))
	}
	fmt.Printf("AROP identity and secrets passed: %d checks.\n", len(written.Checks))
}

func runP10Tests(root, scratch string, cluster *postgresCluster) commandResult {
	work := filepath.Join(scratch, "go.work")
	contents := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace " + nestedRootModule() + " => " + root + "\n")
	if err := os.WriteFile(work, contents, 0o600); err != nil {
		return commandResult{Err: err}
	}
	env := map[string]string{
		"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0",
		"TMPDIR":                scratch,
		"AROP_P10_POSTGRES_URL": cluster.url(), "AROP_P10_MIGRATION_ROOT": filepath.Join(root, "reference/control-plane/migrations"),
	}
	args := append([]string{"test", "-count=1", "-json"}, testPackages...)
	return run(filepath.Join(root, "reference/control-plane"), env, "go", args...)
}

func nestedRootModule() string { return "github.com/gmslll/agent-runtime-operations-protocol" }

func evaluateTests(root string, result commandResult) ([]report.Check, []byte, error) {
	expected, err := discoverTests(root)
	if err != nil {
		return nil, nil, err
	}
	evidence := inventoryEvidence(expected, requiredSubtests)
	checks, err := evaluateTestEvents(expected, requiredSubtests, result)
	return checks, evidence, err
}

func evaluateTestEvents(expected, required []string, result commandResult) ([]report.Check, error) {
	expectedSet := map[string]bool{}
	for _, key := range expected {
		expectedSet[key] = true
	}
	expectedPackageList := expectedPackages(expected)
	expectedPackageSet := map[string]bool{}
	for _, pkg := range expectedPackageList {
		expectedPackageSet[pkg] = true
	}
	topLevelPasses := map[string]int{}
	subtestPasses := map[string]int{}
	packagePasses := map[string]int{}
	packageTerminals := map[string]int{}
	unknownTests := map[string]bool{}
	unknownPackages := map[string]bool{}
	checks := []report.Check{}
	problems := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(result.Output))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4<<20)
	for scanner.Scan() {
		var event goEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Test != "" {
			topLevel := strings.SplitN(event.Test, "/", 2)[0]
			key := event.Package + "/" + topLevel
			if !expectedSet[key] && !unknownTests[key] {
				unknownTests[key] = true
				problems = append(problems, "unknown top-level test "+key)
			}
			if event.Action == "skip" || event.Action == "fail" {
				problems = append(problems, event.Package+"/"+event.Test+" terminal="+event.Action)
			}
			if !strings.Contains(event.Test, "/") && event.Action == "pass" {
				topLevelPasses[key]++
			}
			if strings.Contains(event.Test, "/") && event.Action == "pass" {
				subtestPasses[event.Package+"/"+event.Test]++
			}
		}
		if event.Test == "" && (event.Action == "pass" || event.Action == "skip" || event.Action == "fail") {
			packageTerminals[event.Package]++
			if event.Action == "pass" {
				packagePasses[event.Package]++
			} else {
				problems = append(problems, "package "+event.Package+" terminal="+event.Action)
			}
			if !expectedPackageSet[event.Package] && !unknownPackages[event.Package] {
				unknownPackages[event.Package] = true
				problems = append(problems, "unknown package terminal "+event.Package)
			}
		}
		if strings.Contains(event.Output, "[no test files]") || strings.Contains(event.Output, "(cached)") {
			problems = append(problems, "no-tests or cached output")
		}
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, err.Error())
	}
	for _, key := range expected {
		count := topLevelPasses[key]
		ok := count == 1
		detail := fmt.Sprintf("passing terminals=%d want=1", count)
		if ok {
			detail = "passed exactly once"
		}
		checks = append(checks, report.Check{Name: "p10-test/" + strings.TrimPrefix(key, nestedModule+"/"), Passed: ok, Detail: detail})
		if !ok {
			problems = append(problems, fmt.Sprintf("top-level passing terminals %s=%d want=1", key, count))
		}
	}
	for _, pkg := range expectedPackageList {
		if packageTerminals[pkg] != 1 || packagePasses[pkg] != 1 {
			problems = append(problems, fmt.Sprintf("package terminals %s=%d pass=%d want=1/1", pkg, packageTerminals[pkg], packagePasses[pkg]))
		}
	}
	for _, key := range required {
		if subtestPasses[key] != 1 {
			problems = append(problems, fmt.Sprintf("required subtest passing terminals %s=%d want=1", key, subtestPasses[key]))
		}
	}
	if result.Err != nil {
		problems = append(problems, result.Err.Error())
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return checks, errors.New(strings.Join(problems, "; "))
	}
	return checks, nil
}

func inventoryEvidence(expected, required []string) []byte {
	lines := make([]string, 0, len(expectedPackages(expected))+len(expected)+len(required))
	for _, pkg := range expectedPackages(expected) {
		lines = append(lines, "package\t"+pkg)
	}
	for _, test := range expected {
		lines = append(lines, "top-level\t"+test)
	}
	for _, subtest := range required {
		lines = append(lines, "required-subtest\t"+subtest)
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func verifyInventoryNegatives() error {
	pkg := nestedModule + "/internal/identity"
	test := pkg + "/TestInventoryProbe"
	event := func(action, testName string) []byte {
		encoded, err := json.Marshal(goEvent{Action: action, Package: pkg, Test: testName})
		if err != nil {
			panic(err)
		}
		return append(encoded, '\n')
	}
	packagePass := event("pass", "")
	valid := bytes.Join([][]byte{event("pass", "TestInventoryProbe/nested"), event("pass", "TestInventoryProbe"), packagePass}, nil)
	if _, err := evaluateTestEvents([]string{test}, []string{test + "/nested"}, commandResult{Output: valid}); err != nil {
		return fmt.Errorf("valid inventory probe failed: %w", err)
	}
	cases := []struct {
		name     string
		output   []byte
		required []string
	}{
		{"nested-skip", bytes.Join([][]byte{event("pass", "TestInventoryProbe"), event("skip", "TestInventoryProbe/nested"), packagePass}, nil), []string{test + "/nested"}},
		{"duplicate-pass-terminal", bytes.Join([][]byte{event("pass", "TestInventoryProbe"), event("pass", "TestInventoryProbe"), packagePass}, nil), nil},
		{"duplicate-package-terminal", bytes.Join([][]byte{event("pass", "TestInventoryProbe"), packagePass, packagePass}, nil), nil},
	}
	for _, testCase := range cases {
		if _, err := evaluateTestEvents([]string{test}, testCase.required, commandResult{Output: testCase.output}); err == nil {
			return errors.New(testCase.name + " inventory negative was accepted")
		}
	}
	return nil
}

func discoverTests(root string) ([]string, error) {
	moduleRoot := filepath.Join(root, "reference/control-plane")
	result := []string{}
	for _, relative := range testPackages {
		directory := filepath.Join(moduleRoot, strings.TrimPrefix(relative, "./"))
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		packageName := nestedModule + "/" + strings.TrimPrefix(relative, "./")
		count := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, 0)
			if err != nil {
				return nil, err
			}
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") && function.Type.Params != nil && len(function.Type.Params.List) == 1 {
					result = append(result, packageName+"/"+function.Name.Name)
					count++
				}
			}
		}
		if count == 0 {
			return nil, errors.New("no tests in " + relative)
		}
	}
	sort.Strings(result)
	return result, nil
}

func expectedPackages(tests []string) []string {
	set := map[string]bool{}
	for _, test := range tests {
		index := strings.LastIndex(test, "/Test")
		if index > 0 {
			set[test[:index]] = true
		}
	}
	result := make([]string, 0, len(set))
	for pkg := range set {
		result = append(result, pkg)
	}
	sort.Strings(result)
	return result
}

func startPostgres(scratch string) (*postgresCluster, []byte, error) {
	tools := map[string]string{}
	evidence := bytes.Buffer{}
	for _, name := range []string{"initdb", "postgres", "pg_isready"} {
		found, err := exec.LookPath(name)
		if err != nil {
			return nil, nil, fmt.Errorf("PostgreSQL tool %s unavailable", name)
		}
		real, err := filepath.EvalSymlinks(found)
		if err != nil {
			return nil, nil, err
		}
		version := run("/", nil, real, "--version")
		if version.Err != nil || !strings.Contains(string(version.Output), "16.") {
			return nil, nil, fmt.Errorf("%s is not PostgreSQL 16", name)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&evidence, "%s\x00%s\x00%d\n", name, report.Hash(data), len(data))
		tools[name] = real
	}
	root := filepath.Join(scratch, "postgres")
	data, socket := filepath.Join(root, "data"), filepath.Join(root, "socket")
	if err := os.MkdirAll(socket, 0o700); err != nil {
		return nil, nil, err
	}
	init := run(root, map[string]string{"HOME": "/nonexistent"}, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p10", "--no-instructions")
	if init.Err != nil {
		return nil, evidence.Bytes(), errors.New("private initdb failed")
	}
	cluster := &postgresCluster{root: root, socket: socket}
	cluster.command = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	cluster.command.Dir = root
	cluster.command.Env = cleanEnvironment(map[string]string{"HOME": "/nonexistent"})
	cluster.command.Stdout, cluster.command.Stderr = &cluster.log, &cluster.log
	if err := cluster.command.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ready := run(root, nil, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p10", "-d", "postgres", "-q")
		if ready.Err == nil {
			return cluster, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cluster.stop()
	return nil, evidence.Bytes(), errors.New("private PostgreSQL did not become ready")
}

func (cluster *postgresCluster) url() string {
	return "postgresql://arop_p10@localhost/postgres?host=" + url.QueryEscape(cluster.socket) + "&port=5432&sslmode=disable"
}

func (cluster *postgresCluster) stop() error {
	if cluster == nil || cluster.command == nil || cluster.command.Process == nil {
		return nil
	}
	if err := cluster.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cluster.command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = cluster.command.Process.Kill()
		<-done
		return errors.New("PostgreSQL shutdown timeout")
	}
}

func verifyCatalogSource(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"func P09ProductionCatalog()", `ReportPhase: "P09"`, "func CurrentProductionCatalog()", `ReportPhase: "P10"`, "0005_identity.sql"} {
		if !strings.Contains(text, required) {
			return errors.New("catalog source misses " + required)
		}
	}
	if strings.Contains(text, "0010_publication.sql") {
		return errors.New("production catalog contains future 0010")
	}
	return nil
}

func verifyNoPublicSecretAPI(root string) error {
	for _, relative := range []string{"openapi", "schemas", "sdk", "reference/control-plane/internal/app/platform/httpadapter"} {
		base := filepath.Join(root, relative)
		if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lower := strings.ToLower(string(data))
			for _, forbidden := range []string{"/v1/secrets", "/v1/secret-values", "secret_value"} {
				if strings.Contains(lower, forbidden) {
					return fmt.Errorf("public Secret value surface in %s", path)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func staticInputs(root string) ([]string, map[string]string, error) {
	paths := []string{}
	seen := map[string]bool{}
	for _, relative := range inputRoots {
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, nil, fmt.Errorf("static input %s: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("static input symlink: " + relative)
		}
		if info.IsDir() {
			err = filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return errors.New("static input contains symlink")
				}
				if entry.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				if !seen[rel] {
					seen[rel], paths = true, append(paths, rel)
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		} else if !seen[relative] {
			seen[relative], paths = true, append(paths, relative)
		}
	}
	sort.Strings(paths)
	manifest := map[string]string{}
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, nil, err
		}
		manifest[relative] = report.Hash(data)
	}
	return paths, manifest, nil
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = directory
	cmd.Env = cleanEnvironment(overrides)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return commandResult{Output: output, Err: err}
}

func cleanEnvironment(overrides map[string]string) []string {
	allowed := map[string]bool{"PATH": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "TZ": true, "HOME": true}
	values := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C", "LC_ALL": "C", "TZ": "UTC", "HOME": "/nonexistent"}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found && allowed[key] {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func rejectSensitive(data []byte) error {
	text := strings.ToLower(string(data))
	for _, sentinel := range []string{"p10-secret-sentinel", "arop_dev_cred_", "bearer ", "cookie="} {
		if strings.Contains(text, sentinel) {
			return errors.New("sensitive sentinel in runtime output")
		}
	}
	return nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return strings.NewReplacer("/tmp/", "<scratch>/", "postgresql://", "<postgres-url>").Replace(err.Error())
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, safeError(err))
		os.Exit(1)
	}
}
