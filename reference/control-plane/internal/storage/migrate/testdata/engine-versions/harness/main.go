// Command harness executes the P09 dual-database migration and durable-storage
// acceptance suite. It creates an isolated PostgreSQL 16 cluster and real
// SQLite files, runs the nested module from a commit-backed root-module proxy,
// rediscovers the exact Go package/test terminals, and emits standard reports.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	expectedCommand = "make test-storage-migrations"
	checkerPath     = "reference/control-plane/internal/storage/migrate/testdata/engine-versions/harness/main.go"
	waiverPath      = "reference/control-plane/internal/storage/migrate/testdata/engine-versions/baseline-transition-waiver.json"
	p10WaiverPath   = "reference/control-plane/internal/identity/testdata/transition/p10-baseline-transition-waiver.json"
	p14WaiverPath   = "reference/control-plane/internal/domain/registry/testdata/transition/p14-baseline-transition-waiver.json"
	reportDirectory = "build/reports/P09"
	compositionTest = "reference/control-plane/internal/storage/migrate/testdata/engine-versions/composition/p09_storage_acceptance_test.go"
	nestedModule    = "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane"
	rootModule      = "github.com/gmslll/agent-runtime-operations-protocol"
	postgresMajor   = 16
)

var rootRequirementPattern = regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/gmslll/agent-runtime-operations-protocol\s+(v0\.0\.0-([0-9]{14})-([0-9a-f]{12}))\s*$`)

var expectedTests = map[string][]string{
	nestedModule + "/internal/storage/migrate/testdata/engine-versions/acceptance": {
		"TestMigrationEngineConformance",
		"TestUnitOfWorkConformance",
		"TestDurableObservationStoreConformance",
		"TestDurableConfigurationAndReadiness",
		"TestFilesystemBoundaryConformance",
		"TestMigrationProcessWorker",
	},
}

var requiredAcceptanceSubtests = map[string][]string{
	"TestMigrationEngineConformance": {
		"sqlite/empty-to-current", "sqlite/n-minus-one-to-current", "sqlite/idempotent", "sqlite/dirty-refusal", "sqlite/checksum-drift-refusal", "sqlite/ahead-refusal", "sqlite/gap-refusal", "sqlite/schema-artifact-refusal/index", "sqlite/schema-artifact-refusal/check", "sqlite/schema-artifact-refusal/unique", "sqlite/unmanaged-database-refusal/user-object", "sqlite/counterfeit-schema-refusal/weak-columns", "sqlite/counterfeit-schema-refusal/history-constraint-body", "sqlite/read-only-refusal", "sqlite/concurrent-serialization", "sqlite/backup-restore/successful-upgrade", "sqlite/backup-restore/migration-failure-restores-v1", "sqlite/backup-restore/corrupt-snapshot-refusal", "sqlite/backup-restore/truncated-snapshot-refusal", "sqlite/backup-restore/wrong-snapshot-refusal", "sqlite/backup-restore/restore-failure-preserves-original/mid-copy-cancel", "sqlite/backup-restore/snapshot-metadata-survives-adapter-restart",
		"sqlite/sparse-catalog/empty-1-5-10-20", "sqlite/sparse-catalog/one-to-five", "sqlite/sparse-catalog/idempotent-at-five", "sqlite/sparse-catalog/dirty-five", "sqlite/sparse-catalog/checksum-five", "sqlite/sparse-catalog/history-unknown-seven", "sqlite/sparse-catalog/missing-five-before-ten", "sqlite/sparse-catalog/catalog-missing-applied-five", "sqlite/sparse-catalog/duplicate-version-refusal", "sqlite/sparse-catalog/unordered-version-refusal",
		"postgres/empty-to-current", "postgres/n-minus-one-to-current", "postgres/idempotent", "postgres/dirty-refusal", "postgres/checksum-drift-refusal", "postgres/ahead-refusal", "postgres/gap-refusal", "postgres/schema-artifact-refusal/index", "postgres/schema-artifact-refusal/check", "postgres/schema-artifact-refusal/unique", "postgres/unmanaged-database-refusal/user-object", "postgres/unmanaged-database-refusal/non-public-schema", "postgres/counterfeit-schema-refusal/weak-columns", "postgres/counterfeit-schema-refusal/history-constraint-body", "postgres/read-only-refusal", "postgres/concurrent-serialization", "postgres/backup-restore/successful-upgrade", "postgres/backup-restore/migration-failure-restores-v1", "postgres/backup-restore/corrupt-snapshot-refusal", "postgres/backup-restore/truncated-snapshot-refusal", "postgres/backup-restore/wrong-snapshot-refusal", "postgres/backup-restore/restore-failure-preserves-original/archive-rendering", "postgres/backup-restore/restore-failure-preserves-original/transaction-client", "postgres/backup-restore/snapshot-metadata-survives-adapter-restart",
		"postgres/sparse-catalog/empty-1-5-10-20", "postgres/sparse-catalog/one-to-five", "postgres/sparse-catalog/idempotent-at-five", "postgres/sparse-catalog/dirty-five", "postgres/sparse-catalog/checksum-five", "postgres/sparse-catalog/history-unknown-seven", "postgres/sparse-catalog/missing-five-before-ten", "postgres/sparse-catalog/catalog-missing-applied-five", "postgres/sparse-catalog/duplicate-version-refusal", "postgres/sparse-catalog/unordered-version-refusal",
	},
	"TestUnitOfWorkConformance": {
		"sqlite/success", "sqlite/error", "sqlite/panic", "sqlite/cancel", "sqlite/nested-refusal",
		"postgres/success", "postgres/error", "postgres/panic", "postgres/cancel", "postgres/nested-refusal",
	},
	"TestDurableObservationStoreConformance": {
		"sqlite/atomic-pair", "sqlite/query", "sqlite/truthful-readiness", "sqlite/transaction-rollback",
		"postgres/atomic-pair", "postgres/query", "postgres/truthful-readiness", "postgres/transaction-rollback",
	},
	"TestDurableConfigurationAndReadiness": {
		"sqlite/explicit-mode", "sqlite/no-fallback", "sqlite/migration-gated-ready", "sqlite/p08-regression",
		"postgres/explicit-mode", "postgres/no-fallback", "postgres/migration-gated-ready", "postgres/p08-regression",
	},
	"TestFilesystemBoundaryConformance": {
		"sqlite/parent-directory-symlink", "sqlite/database-symlink", "sqlite/lock-final-symlink", "sqlite/lock-ancestor-symlink", "migrations/sqlite-engine-directory-symlink", "migrations/postgres-engine-directory-symlink",
	},
}

var inputRoots = []string{
	"reference/control-plane/internal/storage/migrate",
	"reference/control-plane/internal/adapters/storage/sqlite",
	"reference/control-plane/internal/adapters/storage/postgres",
	"reference/control-plane/internal/adapters/observability/durable",
	"reference/control-plane/migrations/sqlite/0001_base.sql",
	"reference/control-plane/migrations/sqlite/0005_identity.sql",
	"reference/control-plane/migrations/postgres/0001_base.sql",
	"reference/control-plane/migrations/postgres/0005_identity.sql",
	"reference/control-plane/internal/identity",
	"reference/control-plane/internal/ports/secrets",
	"reference/control-plane/internal/adapters/secrets",
	"reference/control-plane/internal/app/platform",
	"reference/control-plane/internal/ports/observability",
	p10WaiverPath,
	"reference/control-plane/cmd/aropd",
	"reference/control-plane/go.mod",
	"reference/control-plane/go.sum",
	"go.mod", "go.sum",
	"spec/artifact-manifest.yaml",
	"spec/schemas/artifact-manifest.schema.json",
	"spec/schemas/check-report.schema.json",
	"spec/requirements.yaml",
	"spec/schemas/requirements.schema.json",
	"docs/DEVELOPMENT_PLAN.md",
	"docs/IMPLEMENTATION_BLUEPRINT.md",
	"docs/RELIABILITY_AND_OPERATIONS.md",
	"docs/DECISIONS.md",
	"docs/DIRECTORY_STRUCTURE.md",
	"internal/tooling/report",
	checkerPath,
	"Makefile",
}

type fileEntry struct {
	Path, SHA256 string
	Mode         os.FileMode
	Bytes        int64
}

type baselineTransitionWaiver struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics        string `json:"owner_phase_semantics"`
		ExclusiveMutationOwnership bool   `json:"exclusive_mutation_ownership"`
	} `json:"policy"`
	Transition struct {
		FromPhase string `json:"from_phase"`
		ToPhase   string `json:"to_phase"`
		Artifact  string `json:"artifact"`
		Reason    string `json:"reason"`
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

type artifactManifest struct {
	SchemaVersion  int                `json:"schema_version"`
	CatalogID      string             `json:"catalog_id"`
	Updated        string             `json:"updated"`
	Purpose        string             `json:"purpose"`
	AuthorityChain []map[string]any   `json:"authority_chain"`
	Statuses       map[string]string  `json:"statuses"`
	Artifacts      []manifestArtifact `json:"artifacts"`
}

type manifestArtifact struct {
	ID                    string   `json:"id"`
	Path                  string   `json:"path"`
	Status                string   `json:"status"`
	Kind                  string   `json:"kind"`
	Authority             string   `json:"authority"`
	Language              string   `json:"language"`
	Capabilities          []string `json:"capabilities"`
	Owner                 string   `json:"owner"`
	OwnerPhase            string   `json:"owner_phase"`
	CompletionPhase       string   `json:"completion_phase"`
	ProducerPhase         string   `json:"producer_phase"`
	AcceptanceTest        string   `json:"acceptance_test"`
	Exposure              string   `json:"exposure"`
	PathRole              string   `json:"path_role"`
	FutureAction          string   `json:"future_action"`
	FutureOwner           string   `json:"future_owner"`
	FutureOwnerPhase      string   `json:"future_owner_phase"`
	FutureAcceptanceTest  string   `json:"future_acceptance_test"`
	ImplementationRuntime string   `json:"implementation_runtime"`
	ToolScope             string   `json:"tool_scope"`
	DerivesFrom           []string `json:"derives_from"`
	RuntimeInputs         []string `json:"runtime_inputs"`
	FutureArtifacts       []string `json:"future_artifacts"`
}
type commandResult struct {
	Argv   []string
	Output []byte
	Err    error
}
type goTestEvent struct{ Action, Package, Test, Output string }
type treeEntry struct{ Mode, Type, Object, Path string }
type moduleDownloadDocument struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Info     string `json:"Info"`
	GoMod    string `json:"GoMod"`
	Zip      string `json:"Zip"`
	Dir      string `json:"Dir"`
	Sum      string `json:"Sum"`
	GoModSum string `json:"GoModSum"`
	Error    string `json:"Error"`
}
type moduleLock struct{ Path, Version, Sum, GoModSum string }
type standaloneResult struct {
	Test, Composition, List      commandResult
	BootstrapEvidence            []byte
	BootstrapErr, CleanupErr     error
	Version, Commit, ModuleCache string
}
type postgresTools struct {
	BinDir, InitDB, Postgres, PGIsReady, PSQL, PGDump, PGRestore string
	SQLite3                                                      string
	Evidence                                                     []byte
}
type postgresCluster struct {
	Command                 *exec.Cmd
	Log                     bytes.Buffer
	Root, Data, Socket, URL string
	Tools                   postgresTools
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != expectedCommand {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", expectedCommand))
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = safeError(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	restoreHostEnvironment := installHostilePostgresEnvironment()
	defer restoreHostEnvironment()
	add("p09-host-environment-isolation", verifyMinimalEnvironment(), "host PG options, search path, connection target and TLS variables are absent from every child environment")

	before, inputPaths, staticErr := staticManifest(root)
	add("p09-static-input-closure", staticErr, fmt.Sprintf("%d regular inputs with exact path/mode/digest and no symlink ancestry", len(before)))
	add("p09-baseline-transition-waiver-strict-json", verifyBaselineTransitionWaiverStrictJSON(), "duplicate JSON keys are rejected before typed waiver validation")
	add("p09-migration-inventory", verifyMigrationInventory(root), "fixture and production base migration inventories are exact")

	tools, toolsErr := discoverPostgresTools()
	add("p09-postgres16-toolchain", toolsErr, "one co-located PostgreSQL 16 toolchain resolved and digested")

	temporaryRoot, scratchErr := filepath.EvalSymlinks("/tmp")
	if scratchErr == nil && (!filepath.IsAbs(temporaryRoot) || filepath.Clean(temporaryRoot) != temporaryRoot) {
		scratchErr = errors.New("resolved private temporary root is not absolute and clean")
	}
	var scratch string
	if scratchErr == nil {
		scratch, scratchErr = os.MkdirTemp(temporaryRoot, "arop-p09-")
	}
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p09-private-scratch", scratchErr, "private 0700 acceptance scratch created")

	var cluster *postgresCluster
	if toolsErr == nil && scratchErr == nil {
		cluster, err = startPostgres(scratch, tools)
	} else {
		err = errors.New("PostgreSQL prerequisites unavailable")
	}
	add("p09-private-postgres", err, "isolated PostgreSQL 16 uses only a private Unix socket and harness-owned data directory")

	var standalone standaloneResult
	if staticErr == nil && cluster != nil {
		standalone = runStandaloneAcceptance(root, scratch, cluster)
	} else {
		standalone.BootstrapErr = errors.New("standalone acceptance prerequisites failed")
		standalone.Test.Err = standalone.BootstrapErr
		standalone.Composition.Err = standalone.BootstrapErr
		standalone.List.Err = standalone.BootstrapErr
	}
	add("p09-commit-proxy-offline", standalone.BootstrapErr, "root module reconstructed from its Git commit; all nested tests run GOWORK=off and GOPROXY=off")
	testChecks, testErr := evaluateGoTests(standalone.Test.Output, standalone.Test.Err)
	checks = append(checks, testChecks...)
	add("p09-exact-go-test-inventory", testErr, "exact package, top-level test, required subtest and terminal inventories passed uncached")
	compositionChecks, compositionErr := evaluateCompositionTests(standalone.Composition.Output, standalone.Composition.Err)
	checks = append(checks, compositionChecks...)
	add("p09-real-composition", compositionErr, "cmd/aropd compose runs real SQLite and PostgreSQL storage, reports ready durable health, persists observations, fails closed, and bounds advisory-lock startup")
	listErr := verifyProductionList(root, standalone.Version, standalone.ModuleCache, standalone.List)
	add("p09-production-go-list", listErr, "dependency closure is exact and resolves from isolated caches")
	waiverErr := verifyBaselineTransitionWaiver(root, inputPaths, standalone.List.Output)
	add("p09-baseline-transition-waiver", waiverErr, "manifest ownership, the P08 baseline diff, and the actual Go compilation closure independently match the waiver's exact affected artifact/source set")
	add("p09-baseline-transition-waiver-omission-negatives", verifyBaselineTransitionWaiverOmissionNegatives(root, inputPaths, standalone.List.Output), "omitting any independently discovered P08 artifact, changed source, or acceptance checker is rejected")
	_, p10WaiverErr := runP10TransitionChecker(root, "validate")
	add("p10-transition-waiver-governance", p10WaiverErr, "the P10-owned declaration strictly binds P08/P09 accountable owners, planned source changes, three phase acceptances, and non-transfer constraints")
	_, p10NegativeErr := runP10TransitionChecker(root, "negative")
	add("p10-transition-waiver-negatives", p10NegativeErr, "omission, addition, duplicate, wrong owner, wrong acceptance, wrong constraint, changed-to-reverted, and delete/add-normalized rename inputs fail through the production validator")

	p08 := runP08Regression(root, scratch)
	add("p09-p08-regression", commandFailure(p08), "P08 platform acceptance and report verification remain green")

	if cluster != nil {
		err = cluster.Stop()
	} else {
		err = nil
	}
	add("p09-postgres-clean-shutdown", err, "private PostgreSQL stopped and was waited")
	if scratchErr == nil {
		err = removeAllWritable(scratch)
	} else {
		err = nil
	}
	add("p09-private-scratch-cleanup", err, "all private databases, backups, caches and sockets were removed")

	after, _, afterErr := staticManifest(root)
	if afterErr == nil && !reflect.DeepEqual(before, after) {
		afterErr = errors.New("tracked static input manifest changed during acceptance")
	}
	add("p09-static-input-immutability", afterErr, "static input manifest is byte-identical before and after acceptance")

	runtimeEvidence := []report.RuntimeEvidence{
		{Kind: "commit-proxy-bootstrap", SHA256: report.Hash(standalone.BootstrapEvidence), Bytes: int64(len(standalone.BootstrapEvidence))},
		{Kind: "go-list", SHA256: report.Hash(standalone.List.Output), Bytes: int64(len(standalone.List.Output))},
		{Kind: "go-test", SHA256: report.Hash(standalone.Test.Output), Bytes: int64(len(standalone.Test.Output))},
		{Kind: "composition-test", SHA256: report.Hash(standalone.Composition.Output), Bytes: int64(len(standalone.Composition.Output))},
		{Kind: "p08-regression", SHA256: report.Hash(p08.Output), Bytes: int64(len(p08.Output))},
		{Kind: "postgres-toolchain", SHA256: report.Hash(tools.Evidence), Bytes: int64(len(tools.Evidence))},
	}
	_, writeErr := report.Write(report.WriteOptions{
		Root: root, Directory: reportDirectory, Suite: "AROP P09 storage migrations", Class: "p09.storage",
		Command: expectedCommand, CheckerPath: checkerPath, InputPaths: inputPaths, RuntimeInputPaths: []string{},
		RuntimeEvidence: runtimeEvidence, Checks: checks,
		Summary: map[string]any{
			"command_deadline_seconds": 120, "config_sources": []string{"flags", "sanitized-environment"},
			"storage_startup_timeout_cases_ms": []int{750, 2000, 5000}, "migration_timeout_cases_ms": []int{200, 1000, 5000},
			"baseline_transitions": 1, "database_engines": 2, "postgres_major": postgresMajor, "required_subtests": requiredSubtestCount(), "runtime_inputs": 0,
		},
		AuditNote: "P09 binds its full static implementation, requirements, architecture decisions, migration, fixture, harness, module and report-tool closure. owner_phase means first introduction and accountable ownership rather than a permanent exclusive mutation lock; the strict P08-to-P09 baseline-transition waiver binds the reason, affected artifacts, exact source closure, and both phase acceptances. It starts a private Unix-socket-only PostgreSQL 16 cluster and real file-backed SQLite databases. The nested module is assembled from a commit-backed root file proxy; exact checksum-locked dependencies are prefetched once, then all tests and dependency discovery execute with GOWORK=off and GOPROXY=off. Real cmd/aropd composition is injected as a tracked same-package overlay, exercises explicit startup and migration timeout flags, and may not fall back to memory. Absolute scratch paths and DSNs are never reported; toolchain, bootstrap, test, composition, list and P08 outputs are represented only by digest and byte count.",
	})
	if writeErr != nil {
		fatal(writeErr)
	}
	for _, check := range checks {
		if !check.Passed {
			fmt.Fprintln(os.Stderr, check.Name+": "+check.Detail)
			os.Exit(1)
		}
	}
	fmt.Printf("AROP storage migrations passed: %d checks.\n", len(checks)+1)
}

func installHostilePostgresEnvironment() func() {
	values := map[string]string{
		"PGOPTIONS":  "-c default_transaction_read_only=on -c search_path=attacker",
		"PGHOST":     "host-environment-must-not-be-used.invalid",
		"PGDATABASE": "host_environment_must_not_be_used",
		"PGUSER":     "host_environment_must_not_be_used",
		"PGSSLMODE":  "verify-full",
	}
	prior := map[string]*string{}
	for key, value := range values {
		if existing, ok := os.LookupEnv(key); ok {
			copy := existing
			prior[key] = &copy
		} else {
			prior[key] = nil
		}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key, value := range prior {
			if value == nil {
				_ = os.Unsetenv(key)
			} else {
				_ = os.Setenv(key, *value)
			}
		}
	}
}

func verifyMinimalEnvironment() error {
	environment := cleanEnvironment(os.Environ(), map[string]string{"AROP_P09_SENTINEL": "present"})
	foundSentinel := false
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "PG") || key == "DATABASE_URL" {
			return fmt.Errorf("database environment leaked into child: %s", key)
		}
		if key == "AROP_P09_SENTINEL" {
			foundSentinel = true
		}
	}
	if !foundSentinel {
		return errors.New("explicit child environment override was lost")
	}
	return nil
}

func runP10TransitionChecker(root, mode string) ([]byte, error) {
	path := filepath.Join(root, "reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go")
	cmd := exec.Command("go", "run", "-modfile="+filepath.Join(root, "go.mod"), path, mode)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("P10 transition checker %s: %w: %s", mode, err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func p10AllowsPackage(root, importPath string) bool {
	output, err := runP10TransitionChecker(root, "allowed-packages")
	if err != nil {
		return false
	}
	for _, pkg := range strings.Fields(string(output)) {
		if pkg == importPath {
			return true
		}
	}
	return false
}

func p13AllowsPackage(root, importPath string) bool {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/domain/assets/testdata/transition/p13-baseline-transition-waiver.json"))
	if err != nil || !bytes.Contains(data, []byte(`"status": "validated"`)) || !bytes.Contains(data, []byte(`"commit": "3db6ee93a693d62d47e2d2fd25c5de43749f2e7d"`)) {
		return false
	}
	return strings.HasPrefix(importPath, nestedModule+"/internal/domain/assets")
}

func p14AllowsPackage(root, importPath string) bool {
	data, err := os.ReadFile(filepath.Join(root, p14WaiverPath))
	if err != nil || !bytes.Contains(data, []byte(`"status": "validated"`)) || !bytes.Contains(data, []byte(`"commit": "958d1d42bb7b4a3f6c015ad97004b434ba07d2d3"`)) {
		return false
	}
	base := nestedModule + "/internal/domain/registry"
	return importPath == base || strings.HasPrefix(importPath, base+"/")
}

func verifyBaselineTransitionWaiver(root string, inputPaths []string, goListOutput []byte) error {
	data, err := readRegular(root, waiverPath)
	if err != nil {
		return fmt.Errorf("read baseline transition waiver: %w", err)
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(waiverPath)))
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o644 {
		return fmt.Errorf("baseline transition waiver mode=%#o want 0644", info.Mode().Perm())
	}
	waiver, err := decodeBaselineTransitionWaiver(data)
	if err != nil {
		return err
	}
	wantArtifacts, wantSources, discoveryErr := discoverBaselineTransitionClosure(root, goListOutput)
	if discoveryErr != nil {
		return discoveryErr
	}
	return validateBaselineTransitionWaiver(root, inputPaths, waiver, wantArtifacts, wantSources)
}

func validateBaselineTransitionWaiver(root string, inputPaths []string, waiver baselineTransitionWaiver, wantArtifacts, wantSources []string) error {
	wantConstraints := []string{
		"no-memory-fallback-in-durable-modes",
		"p08-regression-must-pass-under-p09",
		"p08-static-closure-binds-p09-composition-sources",
		"frozen-planning-inputs-remain-unchanged",
	}
	problems := []string{}
	if waiver.SchemaVersion != 1 || waiver.WaiverID != "P09-P08-SERVER-BASELINE-TRANSITION-001" || waiver.Status != "accepted" {
		problems = append(problems, "identity/version/status mismatch")
	}
	if waiver.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || waiver.Policy.ExclusiveMutationOwnership {
		problems = append(problems, "owner_phase policy mismatch")
	}
	if waiver.Transition.FromPhase != "P08" || waiver.Transition.ToPhase != "P09" || waiver.Transition.Artifact != "reference-control-plane-server" || waiver.Transition.Reason != "P09 adds explicit durable storage composition to the P08 Control Plane server without transferring its accountable owner or changing its public exposure." {
		problems = append(problems, "transition mismatch")
	}
	if !reflect.DeepEqual(waiver.AffectedArtifacts, wantArtifacts) {
		problems = append(problems, fmt.Sprintf("affected_artifacts=%v want=%v", waiver.AffectedArtifacts, wantArtifacts))
	}
	if !reflect.DeepEqual(waiver.SourceClosure, wantSources) {
		problems = append(problems, fmt.Sprintf("source_closure=%v want=%v", waiver.SourceClosure, wantSources))
	}
	if len(waiver.Acceptance) != 2 ||
		waiver.Acceptance[0].Phase != "P08" || waiver.Acceptance[0].Command != "make test-control-plane-platform" || waiver.Acceptance[0].Report != "build/reports/P08/report.json" ||
		waiver.Acceptance[1].Phase != "P09" || waiver.Acceptance[1].Command != expectedCommand || waiver.Acceptance[1].Report != reportDirectory+"/report.json" {
		problems = append(problems, "acceptance command/report binding mismatch")
	}
	if !reflect.DeepEqual(waiver.Constraints, wantConstraints) {
		problems = append(problems, fmt.Sprintf("constraints=%v want=%v", waiver.Constraints, wantConstraints))
	}
	controlled := make(map[string]bool, len(inputPaths))
	for _, path := range inputPaths {
		controlled[path] = true
	}
	if !controlled[waiverPath] {
		problems = append(problems, "waiver is absent from P09 controlled inputs")
	}
	for _, path := range waiver.SourceClosure {
		if !controlled[path] {
			problems = append(problems, "source is absent from P09 controlled inputs: "+path)
			continue
		}
		if _, err := readRegular(root, path); err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		entry, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || entry.Mode().Perm() != 0o644 {
			problems = append(problems, fmt.Sprintf("%s mode must be 0644", path))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func verifyBaselineTransitionWaiverOmissionNegatives(root string, inputPaths []string, goListOutput []byte) error {
	data, err := readRegular(root, waiverPath)
	if err != nil {
		return err
	}
	waiver, err := decodeBaselineTransitionWaiver(data)
	if err != nil {
		return err
	}
	wantArtifacts, wantSources, err := discoverBaselineTransitionClosure(root, goListOutput)
	if err != nil {
		return err
	}
	requiredArtifacts := []string{"reference-control-plane-server", "control-plane-platform-foundation", "phase-report-p08"}
	requiredSources := []string{
		"reference/control-plane/internal/app/platform/config.go",
		"reference/control-plane/internal/app/platform/platform.go",
		"reference/control-plane/internal/app/platform/httpadapter/http.go",
		"reference/control-plane/internal/app/platform/testdata/harness/main.go",
	}
	for _, artifact := range requiredArtifacts {
		if !containsString(wantArtifacts, artifact) {
			return fmt.Errorf("independent artifact discovery omitted required P08 artifact %s", artifact)
		}
	}
	for _, path := range requiredSources {
		if !containsString(wantSources, path) {
			return fmt.Errorf("independent source discovery omitted required P08 source/checker %s", path)
		}
	}
	for _, artifact := range wantArtifacts {
		candidate := waiver
		candidate.AffectedArtifacts = withoutString(candidate.AffectedArtifacts, artifact)
		if err := validateBaselineTransitionWaiver(root, inputPaths, candidate, wantArtifacts, wantSources); err == nil {
			return fmt.Errorf("waiver omission negative accepted missing artifact %s", artifact)
		}
	}
	for _, path := range wantSources {
		candidate := waiver
		candidate.SourceClosure = withoutString(candidate.SourceClosure, path)
		if err := validateBaselineTransitionWaiver(root, inputPaths, candidate, wantArtifacts, wantSources); err == nil {
			return fmt.Errorf("waiver omission negative accepted missing source/checker %s", path)
		}
	}
	return nil
}

func decodeBaselineTransitionWaiver(data []byte) (baselineTransitionWaiver, error) {
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return baselineTransitionWaiver{}, fmt.Errorf("strict parse baseline transition waiver: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var waiver baselineTransitionWaiver
	if err := decoder.Decode(&waiver); err != nil {
		return baselineTransitionWaiver{}, fmt.Errorf("strict decode baseline transition waiver: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return baselineTransitionWaiver{}, errors.New("baseline transition waiver contains a trailing JSON value")
		}
		return baselineTransitionWaiver{}, fmt.Errorf("baseline transition waiver trailing content: %w", err)
	}
	return waiver, nil
}

func verifyBaselineTransitionWaiverStrictJSON() error {
	_, err := decodeBaselineTransitionWaiver([]byte(`{"schema_version":1,"schema_version":1}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		return errors.New("duplicate-key baseline transition waiver was not rejected by the strict loader")
	}
	return nil
}

func discoverBaselineTransitionClosure(root string, goListOutput []byte) ([]string, []string, error) {
	manifest, err := loadArtifactManifest(root)
	if err != nil {
		return nil, nil, err
	}
	changed, err := changedPathsSinceP08(root)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := compiledRepositoryPaths(goListOutput)
	if err != nil {
		return nil, nil, err
	}

	affected := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.PathRole != "concrete" || artifact.Path == "" {
			continue
		}
		if artifact.OwnerPhase != "P08" && artifact.OwnerPhase != "P09" && artifact.ID != "nested-control-plane-go-module" {
			continue
		}
		if anyPathWithin(changed, artifact.Path) {
			affected[artifact.ID] = true
		}
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.PathRole != "concrete" || (artifact.ProducerPhase != "P08" && artifact.ProducerPhase != "P09") {
			continue
		}
		for _, dependency := range artifact.DerivesFrom {
			if affected[dependency] {
				affected[artifact.ID] = true
				break
			}
		}
	}

	sources := map[string]bool{}
	for path := range changed {
		if compiled[path] {
			sources[path] = true
		}
	}
	for _, artifact := range manifest.Artifacts {
		if !affected[artifact.ID] {
			continue
		}
		switch artifact.Kind {
		case "database-migration", "go-module-definition":
			if changed[artifact.Path] {
				sources[artifact.Path] = true
			}
		}
		if artifact.ID == "nested-control-plane-go-module" && changed["reference/control-plane/go.sum"] {
			sources["reference/control-plane/go.sum"] = true
		}
	}
	for path := range changed {
		if isP08AcceptanceChecker(root, path, manifest.Artifacts) {
			sources[path] = true
		}
	}

	artifacts := sortedBoolKeys(affected)
	paths := sortedBoolKeys(sources)
	if len(artifacts) == 0 || len(paths) == 0 {
		return nil, nil, errors.New("independent baseline transition discovery produced an empty closure")
	}
	for _, path := range paths {
		if _, err := readRegular(root, path); err != nil {
			return nil, nil, fmt.Errorf("independently discovered source %s: %w", path, err)
		}
	}
	return artifacts, paths, nil
}

func loadArtifactManifest(root string) (artifactManifest, error) {
	var manifest artifactManifest
	if err := structuredfile.Load(filepath.Join(root, "spec", "artifact-manifest.yaml"), &manifest); err != nil {
		return artifactManifest{}, fmt.Errorf("load artifact manifest for transition closure: %w", err)
	}
	if manifest.SchemaVersion != 1 || len(manifest.Artifacts) == 0 {
		return artifactManifest{}, errors.New("artifact manifest has an unsupported version or no artifacts")
	}
	seen := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "" || seen[artifact.ID] {
			return artifactManifest{}, fmt.Errorf("artifact manifest contains an empty or duplicate id %q", artifact.ID)
		}
		seen[artifact.ID] = true
	}
	return manifest, nil
}

func changedPathsSinceP08(root string) (map[string]bool, error) {
	output, err := runGit(root, "log", "--diff-filter=A", "--format=%H", "--", waiverPath)
	if err != nil {
		return nil, fmt.Errorf("locate transition introduction commit: %w", err)
	}
	commits := strings.Fields(string(output))
	if len(commits) != 1 {
		return nil, fmt.Errorf("transition waiver must have exactly one introduction commit, found %d", len(commits))
	}
	parentOutput, err := runGit(root, "rev-parse", commits[0]+"^")
	if err != nil {
		return nil, fmt.Errorf("resolve P08 baseline parent: %w", err)
	}
	baseline := strings.TrimSpace(string(parentOutput))
	if len(baseline) != 40 {
		return nil, errors.New("resolved P08 baseline is not a full commit id")
	}
	if _, err := runGit(root, "merge-base", "--is-ancestor", baseline, "HEAD"); err != nil {
		return nil, errors.New("resolved P08 baseline is not an ancestor of HEAD")
	}
	p10IntroOutput, err := runGit(root, "log", "--diff-filter=A", "--format=%H", "--", p10WaiverPath)
	if err != nil {
		return nil, fmt.Errorf("locate P10 waiver introduction: %w", err)
	}
	p10Introductions := strings.Fields(string(p10IntroOutput))
	if len(p10Introductions) != 1 {
		return nil, fmt.Errorf("P10 waiver introduction commits=%d want=1", len(p10Introductions))
	}
	p10ParentOutput, err := runGit(root, "rev-parse", p10Introductions[0]+"^")
	if err != nil {
		return nil, fmt.Errorf("resolve P10 introduction parent: %w", err)
	}
	p10Parent := strings.TrimSpace(string(p10ParentOutput))
	endpointOutput, err := runGit(root, "log", "-1", "--format=%H", p10Parent, "--", waiverPath)
	if err != nil {
		return nil, fmt.Errorf("resolve last P09 waiver touch before P10: %w", err)
	}
	endpoint := strings.TrimSpace(string(endpointOutput))
	if len(endpoint) != 40 {
		return nil, errors.New("accepted P09 transition endpoint is not a full commit id")
	}
	laterTouches, err := runGit(root, "log", "--format=%H", endpoint+"..HEAD", "--", waiverPath)
	if err != nil {
		return nil, fmt.Errorf("audit P09 waiver after endpoint: %w", err)
	}
	if strings.TrimSpace(string(laterTouches)) != "" {
		return nil, errors.New("old P09 waiver changed after its accepted endpoint")
	}
	if _, err := runGit(root, "merge-base", "--is-ancestor", baseline, endpoint); err != nil {
		return nil, errors.New("accepted P09 transition endpoint does not descend from the P08 baseline")
	}
	if _, err := runGit(root, "merge-base", "--is-ancestor", endpoint, "HEAD"); err != nil {
		return nil, errors.New("accepted P09 transition endpoint is not an ancestor of HEAD")
	}
	diff, err := runGit(root, "diff", "--no-renames", "--name-only", "--diff-filter=ACMRTD", "-z", baseline, endpoint, "--")
	if err != nil {
		return nil, fmt.Errorf("discover P09 paths in fixed accepted history %s..%s: %w", baseline, endpoint, err)
	}
	paths := map[string]bool{}
	for _, raw := range bytes.Split(diff, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := filepath.ToSlash(string(raw))
		if path == "" || filepath.IsAbs(filepath.FromSlash(path)) || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("git returned unsafe changed path %q", path)
		}
		paths[path] = true
	}
	if len(paths) == 0 {
		return nil, errors.New("P09 transition diff from the P08 baseline is empty")
	}
	return paths, nil
}

func compiledRepositoryPaths(output []byte) (map[string]bool, error) {
	type listedPackage struct {
		ImportPath     string
		GoFiles        []string
		CgoFiles       []string
		TestGoFiles    []string
		XTestGoFiles   []string
		IgnoredGoFiles []string
		Error          *struct{ Err string }
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	paths := map[string]bool{}
	for {
		var item listedPackage
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode Go compilation closure: %w", err)
		}
		if item.Error != nil {
			return nil, fmt.Errorf("Go compilation closure contains package error for %s", item.ImportPath)
		}
		if item.ImportPath != nestedModule && !strings.HasPrefix(item.ImportPath, nestedModule+"/") {
			continue
		}
		relativePackage := strings.TrimPrefix(item.ImportPath, nestedModule)
		relativePackage = strings.TrimPrefix(relativePackage, "/")
		directory := "reference/control-plane"
		if relativePackage != "" {
			directory += "/" + relativePackage
		}
		files := append([]string{}, item.GoFiles...)
		files = append(files, item.CgoFiles...)
		files = append(files, item.TestGoFiles...)
		files = append(files, item.XTestGoFiles...)
		for _, name := range files {
			if name == "" || filepath.Base(name) != name {
				return nil, fmt.Errorf("Go compilation closure contains non-local filename %q", name)
			}
			paths[directory+"/"+name] = true
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("Go compilation closure contains no nested-module source files")
	}
	return paths, nil
}

func isP08AcceptanceChecker(root, path string, artifacts []manifestArtifact) bool {
	if !strings.HasSuffix(path, "/testdata/harness/main.go") {
		return false
	}
	owned := false
	for _, artifact := range artifacts {
		if artifact.OwnerPhase == "P08" && artifact.AcceptanceTest == "make-test-control-plane-platform" && pathWithinArtifact(path, artifact.Path) {
			owned = true
			break
		}
	}
	if !owned {
		return false
	}
	data, err := readRegular(root, path)
	return err == nil && bytes.Contains(data, []byte(`expectedCommand = "make test-control-plane-platform"`))
}

func anyPathWithin(paths map[string]bool, artifactPath string) bool {
	for path := range paths {
		if pathWithinArtifact(path, artifactPath) {
			return true
		}
	}
	return false
}

func pathWithinArtifact(path, artifactPath string) bool {
	return path == artifactPath || strings.HasPrefix(path, strings.TrimSuffix(artifactPath, "/")+"/")
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func withoutString(values []string, omitted string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != omitted {
			result = append(result, value)
		}
	}
	return result
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, safeError(err))
		os.Exit(1)
	}
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	return text
}

func runP08Regression(root, scratch string) commandResult {
	home := filepath.Join(scratch, "p08-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return commandResult{Argv: []string{"make", "test-control-plane-platform"}, Err: err}
	}
	result := runCommandWithTimeout(root, map[string]string{
		"AROP_CHECK_COMMAND": "", "HOME": home, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0",
	}, []string{"make", "test-control-plane-platform"}, 5*time.Minute)
	if result.Err != nil {
		return result
	}
	verify := runCommand(root, map[string]string{"HOME": home, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}, []string{"make", "verify-report", "REPORT=build/reports/P08/report.json"})
	result.Output = append(append(result.Output, '\n'), verify.Output...)
	result.Err = verify.Err
	return result
}

func staticManifest(root string) ([]fileEntry, []string, error) {
	paths := []string{}
	seen := map[string]bool{}
	for _, relative := range inputRoots {
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, nil, fmt.Errorf("static input %s: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("static input is symlink: %s", relative)
		}
		if info.IsDir() {
			err = filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				rel = filepath.ToSlash(rel)
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("static input contains symlink: %s", rel)
				}
				if entry.IsDir() {
					return nil
				}
				entryInfo, statErr := entry.Info()
				if statErr != nil {
					return statErr
				}
				if !entryInfo.Mode().IsRegular() {
					return fmt.Errorf("static input is non-regular: %s", rel)
				}
				if !seen[rel] {
					seen[rel] = true
					paths = append(paths, rel)
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		} else {
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("static input is non-regular: %s", relative)
			}
			if !seen[relative] {
				seen[relative] = true
				paths = append(paths, relative)
			}
		}
	}
	sort.Strings(paths)
	entries := make([]fileEntry, 0, len(paths))
	for _, relative := range paths {
		if err := rejectSymlinkAncestors(root, relative); err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, fileEntry{Path: relative, SHA256: report.Hash(data), Mode: info.Mode().Perm(), Bytes: int64(len(data))})
	}
	return entries, paths, nil
}

func rejectSymlinkAncestors(root, relative string) error {
	current := root
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("unsafe static path %q", relative)
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in static input: %s", relative)
		}
	}
	return nil
}

func verifyMigrationInventory(root string) error {
	want := map[string][]string{
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/sqlite":          {"0001_fixture.sql", "0002_fixture.sql"},
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/postgres":        {"0001_fixture.sql", "0002_fixture.sql"},
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/sparse/sqlite":   {"0001_sparse_base.sql", "0005_sparse_generation.sql", "0010_sparse_timestamp.sql", "0020_sparse_index.sql"},
		"reference/control-plane/internal/storage/migrate/testdata/engine-versions/sparse/postgres": {"0001_sparse_base.sql", "0005_sparse_generation.sql", "0010_sparse_timestamp.sql", "0020_sparse_index.sql"},
	}
	problems := []string{}
	for directory, expected := range want {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil {
			problems = append(problems, directory+": "+err.Error())
			continue
		}
		actual := []string{}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				problems = append(problems, directory+" contains non-regular "+entry.Name())
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				problems = append(problems, directory+" contains non-regular "+entry.Name())
				continue
			}
			actual = append(actual, entry.Name())
			data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(directory), entry.Name()))
			if readErr != nil || len(data) == 0 || bytes.Contains(data, []byte{'\r'}) || !bytes.HasSuffix(data, []byte{'\n'}) {
				problems = append(problems, directory+"/"+entry.Name()+" is not canonical non-empty LF SQL")
			}
		}
		sort.Strings(actual)
		sort.Strings(expected)
		if !reflect.DeepEqual(actual, expected) {
			problems = append(problems, fmt.Sprintf("%s inventory=%v want=%v", directory, actual, expected))
		}
	}
	// P09 owns and binds only the two base migrations. Later phases own the
	// reserved sparse versions declared in the frozen artifact manifest, so
	// their future files must not retroactively enter P09's input closure.
	for _, base := range []string{
		"reference/control-plane/migrations/sqlite/0001_base.sql",
		"reference/control-plane/migrations/postgres/0001_base.sql",
	} {
		data, err := readRegular(root, base)
		if err != nil || len(data) == 0 || bytes.Contains(data, []byte{'\r'}) || !bytes.HasSuffix(data, []byte{'\n'}) {
			problems = append(problems, base+" is not canonical non-empty LF SQL")
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func discoverPostgresTools() (postgresTools, error) {
	tool := postgresTools{}
	bindings := []struct {
		name   string
		target *string
	}{
		{"initdb", &tool.InitDB}, {"postgres", &tool.Postgres}, {"pg_isready", &tool.PGIsReady},
		{"psql", &tool.PSQL}, {"pg_dump", &tool.PGDump}, {"pg_restore", &tool.PGRestore},
	}
	var binDir string
	evidence := bytes.Buffer{}
	for _, binding := range bindings {
		found, err := exec.LookPath(binding.name)
		if err != nil {
			return postgresTools{}, fmt.Errorf("PostgreSQL tool %s unavailable", binding.name)
		}
		real, err := filepath.EvalSymlinks(found)
		if err != nil {
			return postgresTools{}, fmt.Errorf("resolve PostgreSQL tool %s: %w", binding.name, err)
		}
		info, err := os.Lstat(real)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return postgresTools{}, fmt.Errorf("PostgreSQL tool %s is not a regular resolved file", binding.name)
		}
		if binDir == "" {
			binDir = filepath.Dir(real)
		} else if filepath.Dir(real) != binDir {
			return postgresTools{}, errors.New("PostgreSQL tools are not co-located")
		}
		version := runCommand("/", map[string]string{"PATH": binDir + ":/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "TZ": "UTC"}, []string{real, "--version"})
		if version.Err != nil || !regexp.MustCompile(`\b16\.`).Match(version.Output) {
			return postgresTools{}, fmt.Errorf("%s is not PostgreSQL 16", binding.name)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			return postgresTools{}, err
		}
		fmt.Fprintf(&evidence, "%s\x00%s\x00%d\x00%s\n", binding.name, report.Hash(data), len(data), strings.TrimSpace(string(version.Output)))
		*binding.target = real
	}
	tool.BinDir, tool.Evidence = binDir, evidence.Bytes()
	sqlitePath, err := exec.LookPath("sqlite3")
	if err != nil {
		return postgresTools{}, errors.New("SQLite CLI unavailable")
	}
	sqliteReal, err := filepath.EvalSymlinks(sqlitePath)
	if err != nil {
		return postgresTools{}, err
	}
	sqliteInfo, err := os.Lstat(sqliteReal)
	if err != nil || !sqliteInfo.Mode().IsRegular() || sqliteInfo.Mode()&os.ModeSymlink != 0 {
		return postgresTools{}, errors.New("SQLite CLI is not a resolved regular file")
	}
	sqliteVersion := runCommand("/", map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "TZ": "UTC"}, []string{sqliteReal, "--version"})
	if sqliteVersion.Err != nil {
		return postgresTools{}, errors.New("SQLite CLI version probe failed")
	}
	sqliteData, err := os.ReadFile(sqliteReal)
	if err != nil {
		return postgresTools{}, err
	}
	fmt.Fprintf(&evidence, "sqlite3\x00%s\x00%d\x00%s\n", report.Hash(sqliteData), len(sqliteData), strings.TrimSpace(string(sqliteVersion.Output)))
	tool.SQLite3, tool.Evidence = sqliteReal, evidence.Bytes()
	return tool, nil
}

func startPostgres(scratch string, tools postgresTools) (*postgresCluster, error) {
	root := filepath.Join(scratch, "postgres")
	data, socket := filepath.Join(root, "data"), filepath.Join(root, "socket")
	for _, directory := range []string{root, socket} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	environment := postgresEnvironment(tools.BinDir)
	init := runCommand(root, environment, []string{tools.InitDB, "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p09", "--no-instructions"})
	if init.Err != nil {
		return nil, errors.New("private PostgreSQL initdb failed (output digest " + report.Hash(init.Output) + ")")
	}
	cluster := &postgresCluster{Root: root, Data: data, Socket: socket, Tools: tools}
	cluster.URL = "postgresql://arop_p09@localhost/postgres?host=" + url.QueryEscape(socket) + "&port=5432&sslmode=disable"
	cluster.Command = exec.Command(tools.Postgres, "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "fsync=on", "-c", "synchronous_commit=on", "-c", "logging_collector=off", "-c", "log_statement=none")
	cluster.Command.Dir, cluster.Command.Env = root, cleanEnvironment(os.Environ(), environment)
	cluster.Command.Stdout, cluster.Command.Stderr = &cluster.Log, &cluster.Log
	if err := cluster.Command.Start(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ready := runCommand(root, environment, []string{tools.PGIsReady, "-h", socket, "-p", "5432", "-U", "arop_p09", "-d", "postgres", "-q"})
		if ready.Err == nil {
			break
		}
		if cluster.Command.ProcessState != nil {
			_ = cluster.Stop()
			return nil, errors.New("private PostgreSQL exited during startup")
		}
		time.Sleep(50 * time.Millisecond)
	}
	probe := runCommand(root, environment, []string{tools.PSQL, "-X", "-v", "ON_ERROR_STOP=1", "-h", socket, "-p", "5432", "-U", "arop_p09", "-d", "postgres", "-Atqc", "SELECT current_setting('server_version'),current_setting('data_directory'),current_setting('listen_addresses'),current_setting('unix_socket_directories'),current_setting('TimeZone')"})
	if probe.Err != nil {
		_ = cluster.Stop()
		return nil, errors.New("private PostgreSQL probe failed")
	}
	fields := strings.Split(strings.TrimSpace(string(probe.Output)), "|")
	if len(fields) != 5 || !strings.HasPrefix(fields[0], "16.") || filepath.Clean(fields[1]) != filepath.Clean(data) || fields[2] != "" || filepath.Clean(fields[3]) != filepath.Clean(socket) || fields[4] != "UTC" {
		_ = cluster.Stop()
		return nil, errors.New("private PostgreSQL identity/configuration mismatch")
	}
	return cluster, nil
}

func postgresEnvironment(binDir string) map[string]string {
	return map[string]string{"PATH": binDir + ":/usr/bin:/bin", "HOME": "/nonexistent", "LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PGCONNECT_TIMEOUT": "2"}
}

func (cluster *postgresCluster) Stop() error {
	if cluster == nil || cluster.Command == nil || cluster.Command.Process == nil {
		return nil
	}
	if cluster.Command.ProcessState != nil {
		return cluster.Command.Wait()
	}
	if err := cluster.Command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cluster.Command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || !exitErr.Success() {
				return fmt.Errorf("PostgreSQL wait: %w", err)
			}
		}
		return nil
	case <-time.After(10 * time.Second):
		_ = cluster.Command.Process.Kill()
		<-done
		return errors.New("PostgreSQL shutdown timed out")
	}
}

func runStandaloneAcceptance(root, scratch string, cluster *postgresCluster) (result standaloneResult) {
	packages := sortedPackagePaths()
	testArgv := append([]string{"go", "test", "-count=1", "-run=.", "-json"}, packages...)
	compositionOverlay := filepath.Join(scratch, "composition-overlay.json")
	compositionArgv := []string{"go", "test", "-count=1", "-run=^TestP09StorageComposition$", "-json", "-overlay=" + compositionOverlay, "./cmd/aropd"}
	listPackages := append([]string{}, packages...)
	listPackages = append(listPackages,
		"./cmd/aropd",
		"./internal/storage/migrate",
		"./internal/adapters/storage/sqlite",
		"./internal/adapters/storage/postgres",
		"./internal/adapters/observability/durable",
		"./internal/adapters/observability/memory",
		"./internal/app/platform",
		"./internal/app/platform/httpadapter",
		"./internal/app/platform/ports",
		"./internal/ports/observability",
	)
	sort.Strings(listPackages)
	listArgv := append([]string{"go", "list", "-deps", "-json"}, listPackages...)
	result.Test.Argv, result.Composition.Argv, result.List.Argv = testArgv, compositionArgv, listArgv
	proxyRoot := filepath.Join(scratch, "proxy")
	cache, goPath := filepath.Join(scratch, "gocache"), filepath.Join(scratch, "gopath")
	moduleCache, moduleCopy, temporary := filepath.Join(scratch, "modcache"), filepath.Join(scratch, "module"), filepath.Join(scratch, "gotmp")
	result.ModuleCache = moduleCache
	for _, directory := range []string{proxyRoot, cache, goPath, moduleCache, moduleCopy, temporary, filepath.Join(scratch, "sqlite"), filepath.Join(scratch, "backups")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			result.BootstrapErr = err
			result.Test.Err = err
			result.Composition.Err = err
			result.List.Err = err
			return result
		}
	}
	version, commit, commitTime, rootGoMod, err := resolveRootRequirement(root)
	result.Version, result.Commit = version, commit
	if err == nil {
		err = writeCommitProxy(root, proxyRoot, version, commit, commitTime, rootGoMod)
	}
	if err == nil {
		err = copyNestedModuleFiles(root, moduleCopy)
	}
	nestedRoot := filepath.Join(root, "reference", "control-plane")
	if err == nil {
		var overlay []byte
		overlay, err = json.Marshal(struct {
			Replace map[string]string `json:"Replace"`
		}{Replace: map[string]string{
			filepath.Join(nestedRoot, "cmd", "aropd", "p09_storage_acceptance_test.go"): filepath.Join(root, filepath.FromSlash(compositionTest)),
		}})
		if err == nil {
			err = os.WriteFile(compositionOverlay, append(overlay, '\n'), 0o600)
		}
	}
	baseEnvironment := map[string]string{
		"CGO_ENABLED": "0", "GOCACHE": cache, "GODEBUG": "", "GOENV": "off", "GOFLAGS": "-mod=readonly",
		"GOMODCACHE": moduleCache, "GONOSUMDB": "*", "GOPATH": goPath, "GOPROXY": "off", "GOSUMDB": "off",
		"GOTOOLCHAIN": "local", "GOTMPDIR": temporary, "GOWORK": "off", "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	}
	var download, verify commandResult
	if err == nil {
		proxyEnvironment := cloneStrings(baseEnvironment)
		proxyEnvironment["GOPROXY"] = (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",off"
		download = runCommand(moduleCopy, proxyEnvironment, []string{"go", "mod", "download", "-json", rootModule + "@" + version})
		err = commandFailure(download)
	}
	if err == nil {
		err = validateDownloadedRoot(root, scratch, version, rootGoMod, download.Output)
	}
	var dependencyDownload commandResult
	if err == nil {
		online := cloneStrings(baseEnvironment)
		online["GOPROXY"] = (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",https://proxy.golang.org"
		dependencyDownload = runCommand(nestedRoot, online, []string{"go", "mod", "download", "-json", "all"})
		err = commandFailure(dependencyDownload)
	}
	if err == nil {
		err = validateAllDownloads(root, scratch, version, dependencyDownload.Output)
	}
	if err == nil {
		verify = runCommand(nestedRoot, baseEnvironment, []string{"go", "mod", "verify"})
		err = commandFailure(verify)
	}
	result.BootstrapEvidence = bytes.Join([][]byte{[]byte("version=" + version + "\ncommit=" + commit + "\n"), download.Output, dependencyDownload.Output, verify.Output}, []byte("\n"))
	result.BootstrapErr = err
	if err != nil {
		wrapped := fmt.Errorf("standalone commit-proxy bootstrap: %w", err)
		result.Test.Err = wrapped
		result.Composition.Err = wrapped
		result.List.Err = wrapped
		return result
	}
	testEnvironment := cloneStrings(baseEnvironment)
	testEnvironment["PATH"] = cluster.Tools.BinDir + ":/usr/bin:/bin"
	testEnvironment["AROP_P09_POSTGRES_URL"] = cluster.URL
	testEnvironment["AROP_P09_POSTGRES_BIN"] = cluster.Tools.BinDir
	testEnvironment["AROP_P09_SQLITE_BIN"] = cluster.Tools.SQLite3
	testEnvironment["AROP_P09_SQLITE_ROOT"] = filepath.Join(scratch, "sqlite")
	testEnvironment["AROP_P09_BACKUP_ROOT"] = filepath.Join(scratch, "backups")
	testEnvironment["AROP_P09_MIGRATION_ROOT"] = filepath.Join(root, "reference", "control-plane", "migrations")
	testEnvironment["AROP_P09_ENGINE_FIXTURES"] = filepath.Join(root, "reference", "control-plane", "internal", "storage", "migrate", "testdata", "engine-versions")
	result.Test = runCommand(nestedRoot, testEnvironment, testArgv)
	result.Composition = runCommand(nestedRoot, testEnvironment, compositionArgv)
	result.List = runCommand(nestedRoot, baseEnvironment, listArgv)
	return result
}

func resolveRootRequirement(root string) (string, string, time.Time, []byte, error) {
	nestedGoMod, err := readRegular(root, "reference/control-plane/go.mod")
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	matches := rootRequirementPattern.FindAllSubmatch(nestedGoMod, -1)
	if len(matches) != 1 {
		return "", "", time.Time{}, nil, fmt.Errorf("nested go.mod must contain exactly one root pseudo-version; found %d", len(matches))
	}
	version, timestampText, shortCommit := string(matches[0][1]), string(matches[0][2]), string(matches[0][3])
	wantTime, err := time.Parse("20060102150405", timestampText)
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	commitBytes, err := runGit(root, "rev-parse", shortCommit+"^{commit}")
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if len(commit) != 40 || !strings.HasPrefix(commit, shortCommit) {
		return "", "", time.Time{}, nil, errors.New("root pseudo-version commit suffix mismatch")
	}
	if _, err = runGit(root, "merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return "", "", time.Time{}, nil, errors.New("root pseudo-version is not an ancestor")
	}
	timeBytes, err := runGit(root, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	commitTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(timeBytes)))
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	if !commitTime.UTC().Equal(wantTime.UTC()) {
		return "", "", time.Time{}, nil, errors.New("root pseudo-version timestamp mismatch")
	}
	rootGoMod, err := runGit(root, "show", commit+":go.mod")
	if err != nil {
		return "", "", time.Time{}, nil, err
	}
	currentGoMod, err := readRegular(root, "go.mod")
	if err != nil || !bytes.Equal(rootGoMod, currentGoMod) {
		return "", "", time.Time{}, nil, errors.New("root go.mod differs from pinned commit")
	}
	return version, commit, commitTime.UTC(), rootGoMod, nil
}

func writeCommitProxy(root, proxyRoot, version, commit string, commitTime time.Time, rootGoMod []byte) error {
	directory := filepath.Join(proxyRoot, filepath.FromSlash(rootModule), "@v")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := json.Marshal(struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{version, commitTime})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, version+".info"), append(info, '\n'), 0o600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, version+".mod"), rootGoMod, 0o600); err != nil {
		return err
	}
	return writeModuleZip(root, filepath.Join(directory, version+".zip"), version, commit)
}

func writeModuleZip(root, destination, version, commit string) error {
	entries, err := gitTree(root, commit)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	closeWith := func(base error) error {
		if closeErr := archive.Close(); base == nil {
			base = closeErr
		}
		if closeErr := file.Close(); base == nil {
			base = closeErr
		}
		return base
	}
	prefix := rootModule + "@" + version + "/"
	var total int64
	folded := map[string]string{}
	for _, entry := range entries {
		if excludedFromModuleZip(entry.Path) {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
			return closeWith(fmt.Errorf("unsupported Git entry %s at %s", entry.Mode, entry.Path))
		}
		if err := validateArchivePath(entry.Path); err != nil {
			return closeWith(err)
		}
		if err := recordArchivePath(entry.Path, folded); err != nil {
			return closeWith(err)
		}
		data, err := runGit(root, "cat-file", "blob", entry.Object)
		if err != nil {
			return closeWith(err)
		}
		total += int64(len(data))
		if total > 500<<20 {
			return closeWith(errors.New("module zip exceeds 500 MiB"))
		}
		header := &zip.FileHeader{Name: prefix + entry.Path, Method: zip.Deflate}
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		if entry.Mode == "100755" {
			header.SetMode(0o755)
		} else {
			header.SetMode(0o644)
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return closeWith(err)
		}
		if _, err = writer.Write(data); err != nil {
			return closeWith(err)
		}
	}
	return closeWith(nil)
}

func gitTree(root, commit string) ([]treeEntry, error) {
	output, err := runGit(root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	entries := []treeEntry{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		parts := bytes.SplitN(raw, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, errors.New("malformed git tree record")
		}
		metadata := strings.Fields(string(parts[0]))
		if len(metadata) != 3 {
			return nil, errors.New("malformed git tree metadata")
		}
		entries = append(entries, treeEntry{metadata[0], metadata[1], metadata[2], string(parts[1])})
	}
	return entries, nil
}

func excludedFromModuleZip(path string) bool {
	if path == "reference/control-plane" || strings.HasPrefix(path, "reference/control-plane/") {
		return true
	}
	for _, element := range strings.Split(path, "/") {
		if element == "vendor" {
			return true
		}
	}
	return false
}

func validateArchivePath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || len(path) > 1024 || strings.Contains(path, `\`) {
		return fmt.Errorf("invalid module archive path %q", path)
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." || strings.HasSuffix(element, ".") || strings.HasSuffix(element, " ") {
			return fmt.Errorf("invalid module archive element %q", element)
		}
		stem := strings.ToUpper(strings.SplitN(element, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return fmt.Errorf("reserved module archive element %q", element)
		}
		for _, character := range element {
			if character < 0x20 || strings.ContainsRune(`:*?"<>|`, character) {
				return fmt.Errorf("invalid module archive character in %q", path)
			}
		}
	}
	return nil
}

func recordArchivePath(path string, seen map[string]string) error {
	elements := strings.Split(path, "/")
	for index := range elements {
		prefix := strings.Join(elements[:index+1], "/")
		folded := foldPath(prefix)
		if prior := seen[folded]; prior != "" && prior != prefix {
			return fmt.Errorf("case-fold collision: %s and %s", prior, prefix)
		}
		seen[folded] = prefix
	}
	return nil
}

func foldPath(path string) string {
	var value strings.Builder
	for _, character := range path {
		minimum := character
		for next := unicode.SimpleFold(character); next != character; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		value.WriteRune(minimum)
	}
	return value.String()
}

func validateDownloadedRoot(root, scratch, version string, rootGoMod, output []byte) error {
	var document moduleDownloadDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode root download: %w", err)
	}
	if document.Error != "" || document.Path != rootModule || document.Version != version || document.Sum == "" || document.GoModSum == "" {
		return errors.New("unexpected commit-backed root download identity")
	}
	for _, path := range []string{document.Info, document.GoMod, document.Zip, document.Dir} {
		if path == "" || !pathWithin(scratch, path) || pathWithin(root, path) {
			return errors.New("root download escaped isolated cache")
		}
	}
	data, err := os.ReadFile(document.GoMod)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, rootGoMod) {
		return errors.New("downloaded root go.mod differs from pinned commit")
	}
	sums, err := loadGoSums(root)
	if err != nil {
		return err
	}
	if sums[rootModule+" "+version] != document.Sum || sums[rootModule+" "+version+"/go.mod"] != document.GoModSum {
		return errors.New("nested go.sum does not bind commit-backed root checksums")
	}
	return nil
}

func validateAllDownloads(root, scratch, rootVersion string, output []byte) error {
	sums, err := loadGoSums(root)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	seen := map[string]bool{}
	count := 0
	for {
		var document moduleDownloadDocument
		err = decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode module download stream: %w", err)
		}
		if document.Error != "" || document.Path == "" || document.Version == "" || document.Sum == "" || document.GoModSum == "" {
			return fmt.Errorf("incomplete module download for %s", document.Path)
		}
		key := document.Path + " " + document.Version
		if seen[key] {
			return fmt.Errorf("duplicate module download %s", key)
		}
		seen[key] = true
		count++
		if document.Path == rootModule && document.Version != rootVersion {
			return errors.New("download stream uses wrong root version")
		}
		if sums[key] != document.Sum || sums[key+"/go.mod"] != document.GoModSum {
			return fmt.Errorf("download checksum mismatch for %s", key)
		}
		for _, path := range []string{document.Info, document.GoMod, document.Zip, document.Dir} {
			if path == "" || !pathWithin(scratch, path) || pathWithin(root, path) {
				return fmt.Errorf("download %s escaped isolated cache", key)
			}
		}
	}
	if count < 1 || !seen[rootModule+" "+rootVersion] {
		return errors.New("module download stream omitted root module")
	}
	return nil
}

func loadGoSums(root string) (map[string]string, error) {
	data, err := readRegular(root, "reference/control-plane/go.sum")
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed nested go.sum line")
		}
		key := fields[0] + " " + fields[1]
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate nested go.sum entry %s", key)
		}
		values[key] = fields[2]
	}
	return values, nil
}

func copyNestedModuleFiles(root, destination string) error {
	for _, relative := range []string{"reference/control-plane/go.mod", "reference/control-plane/go.sum"} {
		data, err := readRegular(root, relative)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(destination, filepath.Base(relative)), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func runGit(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-c", "core.autocrlf=false", "-c", "core.safecrlf=true"}, arguments...)...)
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ(), map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_OPTIONAL_LOCKS": "0", "LANG": "C", "LC_ALL": "C", "TZ": "UTC"})
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git command failed (sha256=%s bytes=%d)", report.Hash(output), len(output))
	}
	return output, nil
}

func runCommand(directory string, environment map[string]string, argv []string) commandResult {
	return runCommandWithTimeout(directory, environment, argv, 2*time.Minute)
}

func runCommandWithTimeout(directory string, environment map[string]string, argv []string, timeout time.Duration) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = cleanEnvironment(os.Environ(), environment)
	output, err := command.CombinedOutput()
	return commandResult{append([]string(nil), argv...), output, err}
}

func commandFailure(result commandResult) error {
	if result.Err == nil {
		return nil
	}
	return fmt.Errorf("command failed: %s (output sha256=%s bytes=%d)", strings.Join(result.Argv, " "), report.Hash(result.Output), len(result.Output))
}

func cleanEnvironment(base []string, overrides map[string]string) []string {
	// Host state is not an input. In particular, no PG*, database, Go, Node,
	// loader, credential, or proxy variable is inherited. PATH is the only
	// allowlisted host value and is required by the nested Make regression.
	values := map[string]string{"HOME": "/nonexistent", "LANG": "C", "LC_ALL": "C", "TZ": "UTC"}
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if ok && key == "PATH" {
			values[key] = value
		}
	}
	for key, value := range overrides {
		if value == "" {
			delete(values, key)
		} else {
			values[key] = value
		}
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

func cloneStrings(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func readRegular(root, relative string) ([]byte, error) {
	if err := rejectSymlinkAncestors(root, relative); err != nil {
		return nil, err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a regular file: %s", relative)
	}
	return os.ReadFile(path)
}

func pathWithin(parent, child string) bool {
	parentAbsolute, parentErr := filepath.Abs(parent)
	childAbsolute, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	relative, err := filepath.Rel(parentAbsolute, childAbsolute)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func removeAllWritable(path string) error {
	if path == "" || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("refusing unsafe cleanup")
	}
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return os.Chmod(current, 0o600)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(path)
}

func sortedPackagePaths() []string {
	paths := make([]string, 0, len(expectedTests))
	for name := range expectedTests {
		paths = append(paths, "./"+strings.TrimPrefix(name, nestedModule+"/"))
	}
	sort.Strings(paths)
	return paths
}

func requiredSubtestCount() int {
	count := 0
	for _, subtests := range requiredAcceptanceSubtests {
		count += len(subtests)
	}
	return count
}

func evaluateGoTests(data []byte, commandErr error) ([]report.Check, error) {
	testTerminals := map[string][]string{}
	packageTerminals := map[string][]string{}
	cacheMarkers, noTestMarkers := 0, 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse go test JSON: %w", err)
		}
		if strings.Contains(event.Output, "(cached)") {
			cacheMarkers++
		}
		if strings.Contains(event.Output, "[no test files]") || strings.Contains(event.Output, "no tests to run") {
			noTestMarkers++
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		if event.Test == "" {
			packageTerminals[event.Package] = append(packageTerminals[event.Package], event.Action)
		} else {
			testTerminals[event.Package+"/"+event.Test] = append(testTerminals[event.Package+"/"+event.Test], event.Action)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	checks := []report.Check{}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{Name: name, Passed: passed, Detail: detail})
	}
	wantPackages := make([]string, 0, len(expectedTests))
	for name := range expectedTests {
		wantPackages = append(wantPackages, name)
	}
	sort.Strings(wantPackages)
	actualPackages := sortedKeys(packageTerminals)
	add("p09-package-terminal-set", reflect.DeepEqual(wantPackages, actualPackages), setDetail(wantPackages, actualPackages))
	for _, pkg := range wantPackages {
		actions := packageTerminals[pkg]
		add("p09-package:"+pkg, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	wantTests := map[string]bool{}
	for pkg, tests := range expectedTests {
		for _, test := range tests {
			wantTests[pkg+"/"+test] = true
		}
	}
	acceptancePackage := nestedModule + "/internal/storage/migrate/testdata/engine-versions/acceptance"
	for top, subtests := range requiredAcceptanceSubtests {
		for _, subtest := range subtests {
			parts := strings.Split(subtest, "/")
			for index := 1; index <= len(parts); index++ {
				wantTests[acceptancePackage+"/"+top+"/"+strings.Join(parts[:index], "/")] = true
			}
		}
	}
	wantNames := sortedKeys(wantTests)
	actualNames := sortedKeys(testTerminals)
	add("p09-exact-test-terminal-set", reflect.DeepEqual(wantNames, actualNames), setDetail(wantNames, actualNames))
	for _, name := range wantNames {
		actions := testTerminals[name]
		add("p09-test:"+name, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	add("p09-no-cache", cacheMarkers == 0, fmt.Sprintf("cache_markers=%d", cacheMarkers))
	add("p09-no-empty-package", noTestMarkers == 0, fmt.Sprintf("no_test_markers=%d", noTestMarkers))
	add("p09-go-test-command", commandErr == nil, fmt.Sprintf("output_sha256=%s bytes=%d", report.Hash(data), len(data)))
	problems := []string{}
	if commandErr != nil {
		problems = append(problems, "go test exited non-zero")
	}
	if !reflect.DeepEqual(wantPackages, actualPackages) {
		problems = append(problems, "package terminal set mismatch")
	}
	if !reflect.DeepEqual(wantNames, actualNames) {
		problems = append(problems, "test terminal set mismatch")
	}
	if cacheMarkers != 0 || noTestMarkers != 0 {
		problems = append(problems, "cached or empty execution")
	}
	for name, actions := range packageTerminals {
		if len(actions) != 1 || actions[0] != "pass" {
			problems = append(problems, "package failed: "+name)
		}
	}
	for name, actions := range testTerminals {
		if len(actions) != 1 || actions[0] != "pass" {
			problems = append(problems, "test failed: "+name)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return checks, errors.New(strings.Join(problems, "; "))
	}
	return checks, nil
}

func evaluateCompositionTests(data []byte, commandErr error) ([]report.Check, error) {
	packageName := nestedModule + "/cmd/aropd"
	top := "TestP09StorageComposition"
	wantTests := []string{
		packageName + "/" + top,
		packageName + "/" + top + "/sqlite",
		packageName + "/" + top + "/sqlite/compose-ready-durable",
		packageName + "/" + top + "/sqlite/compose-failure-no-memory-fallback",
		packageName + "/" + top + "/postgres",
		packageName + "/" + top + "/postgres/compose-ready-durable",
		packageName + "/" + top + "/postgres/compose-failure-no-memory-fallback",
		packageName + "/" + top + "/postgres/advisory-lock-timeout-no-memory-fallback",
	}
	sort.Strings(wantTests)
	testTerminals := map[string][]string{}
	packageTerminals := map[string][]string{}
	cacheMarkers, noTestMarkers := 0, 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse composition go test JSON: %w", err)
		}
		if strings.Contains(event.Output, "(cached)") {
			cacheMarkers++
		}
		if strings.Contains(event.Output, "[no test files]") || strings.Contains(event.Output, "no tests to run") {
			noTestMarkers++
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		if event.Test == "" {
			packageTerminals[event.Package] = append(packageTerminals[event.Package], event.Action)
		} else {
			testTerminals[event.Package+"/"+event.Test] = append(testTerminals[event.Package+"/"+event.Test], event.Action)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	checks := []report.Check{}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{Name: name, Passed: passed, Detail: detail})
	}
	wantPackages := []string{packageName}
	actualPackages := sortedKeys(packageTerminals)
	add("p09-composition-package-terminal-set", reflect.DeepEqual(wantPackages, actualPackages), setDetail(wantPackages, actualPackages))
	actions := packageTerminals[packageName]
	add("p09-composition-package", len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	actualTests := sortedKeys(testTerminals)
	add("p09-composition-test-terminal-set", reflect.DeepEqual(wantTests, actualTests), setDetail(wantTests, actualTests))
	for _, name := range wantTests {
		actions = testTerminals[name]
		add("p09-composition-test:"+name, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	add("p09-composition-no-cache", cacheMarkers == 0, fmt.Sprintf("cache_markers=%d", cacheMarkers))
	add("p09-composition-no-empty-package", noTestMarkers == 0, fmt.Sprintf("no_test_markers=%d", noTestMarkers))
	add("p09-composition-go-test-command", commandErr == nil, fmt.Sprintf("output_sha256=%s bytes=%d", report.Hash(data), len(data)))
	problems := []string{}
	if commandErr != nil {
		problems = append(problems, "composition go test exited non-zero")
	}
	if !reflect.DeepEqual(wantPackages, actualPackages) || !reflect.DeepEqual(wantTests, actualTests) {
		problems = append(problems, "composition terminal inventory mismatch")
	}
	if cacheMarkers != 0 || noTestMarkers != 0 {
		problems = append(problems, "composition execution was cached or empty")
	}
	for name, terminals := range packageTerminals {
		if len(terminals) != 1 || terminals[0] != "pass" {
			problems = append(problems, "composition package failed: "+name)
		}
	}
	for name, terminals := range testTerminals {
		if len(terminals) != 1 || terminals[0] != "pass" {
			problems = append(problems, "composition test failed: "+name)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return checks, errors.New(strings.Join(problems, "; "))
	}
	return checks, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func setDetail(want, actual []string) string {
	if reflect.DeepEqual(want, actual) {
		return fmt.Sprintf("exact=%d", len(want))
	}
	return fmt.Sprintf("want=%v actual=%v", want, actual)
}

func verifyProductionList(root, rootVersion, moduleCache string, result commandResult) error {
	if result.Err != nil {
		return commandFailure(result)
	}
	type listedPackage struct {
		ImportPath, Dir string
		Standard        bool
		Module          *struct{ Path, Version string }
	}
	allowedNested := map[string]bool{
		nestedModule + "/cmd/aropd":                                                    true,
		nestedModule + "/internal/storage/migrate":                                     true,
		nestedModule + "/internal/adapters/storage/sqlite":                             true,
		nestedModule + "/internal/adapters/storage/postgres":                           true,
		nestedModule + "/internal/adapters/observability/durable":                      true,
		nestedModule + "/internal/adapters/observability/memory":                       true,
		nestedModule + "/internal/app/platform":                                        true,
		nestedModule + "/internal/app/platform/httpadapter":                            true,
		nestedModule + "/internal/app/platform/ports":                                  true,
		nestedModule + "/internal/ports/observability":                                 true,
		nestedModule + "/internal/storage/migrate/testdata/engine-versions/acceptance": true,
		nestedModule + "/internal/domain/publication":                                  true,
		nestedModule + "/internal/domain/publication/storage/postgres":                 true,
		nestedModule + "/internal/domain/publication/storage/sqlite":                   true,
	}
	locks, err := nestedModuleLocks(root)
	if err != nil {
		return err
	}
	seenNested := map[string]bool{}
	seenExternal := map[string]bool{}
	seenCore := false
	problems := []string{}
	decoder := json.NewDecoder(bytes.NewReader(result.Output))
	for {
		var item listedPackage
		err = decoder.Decode(&item)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode go list: %w", err)
		}
		if item.Standard {
			continue
		}
		if allowedNested[item.ImportPath] {
			if item.Module == nil || item.Module.Path != nestedModule || item.Module.Version != "" || !pathWithin(filepath.Join(root, "reference/control-plane"), item.Dir) {
				problems = append(problems, "nested package resolved outside main module: "+item.ImportPath)
			}
			seenNested[item.ImportPath] = true
			continue
		}
		if p10AllowsPackage(root, item.ImportPath) && item.Module != nil && item.Module.Path == nestedModule && item.Module.Version == "" && pathWithin(filepath.Join(root, "reference/control-plane"), item.Dir) {
			seenNested[item.ImportPath] = true
			continue
		}
		if p13AllowsPackage(root, item.ImportPath) && item.Module != nil && item.Module.Path == nestedModule && item.Module.Version == "" && pathWithin(filepath.Join(root, "reference/control-plane"), item.Dir) {
			seenNested[item.ImportPath] = true
			continue
		}
		if p14AllowsPackage(root, item.ImportPath) && item.Module != nil && item.Module.Path == nestedModule && item.Module.Version == "" && pathWithin(filepath.Join(root, "reference/control-plane"), item.Dir) {
			seenNested[item.ImportPath] = true
			continue
		}
		if item.ImportPath == rootModule+"/sdk/go/protocol/core" {
			if item.Module == nil || item.Module.Path != rootModule || item.Module.Version != rootVersion || !pathWithin(moduleCache, item.Dir) || pathWithin(root, item.Dir) {
				problems = append(problems, "root core did not resolve from commit-backed cache")
			}
			seenCore = true
			continue
		}
		if item.Module != nil {
			if version, ok := locks[item.Module.Path]; ok && version == item.Module.Version && pathWithin(moduleCache, item.Dir) && !pathWithin(root, item.Dir) {
				seenExternal[item.Module.Path] = true
				continue
			}
		}
		problems = append(problems, "dependency outside exact P09 closure: "+item.ImportPath)
	}
	for pkg := range allowedNested {
		if !seenNested[pkg] {
			problems = append(problems, "missing nested package "+pkg)
		}
	}
	if !seenCore {
		problems = append(problems, "missing root protocol core")
	}
	for module := range locks {
		if module == rootModule {
			continue
		}
		if !seenExternal[module] {
			problems = append(problems, "locked module not exercised "+module)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func nestedModuleLocks(root string) (map[string]string, error) {
	data, err := readRegular(root, "reference/control-plane/go.mod")
	if err != nil {
		return nil, err
	}
	locks := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		if line == "require (" {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) == 2 && (inBlock || strings.Contains(fields[0], ".")) {
			if prior := locks[fields[0]]; prior != "" && prior != fields[1] {
				return nil, fmt.Errorf("duplicate module lock %s", fields[0])
			}
			locks[fields[0]] = fields[1]
		}
	}
	if locks[rootModule] == "" {
		return nil, errors.New("nested go.mod missing root lock")
	}
	return locks, nil
}
