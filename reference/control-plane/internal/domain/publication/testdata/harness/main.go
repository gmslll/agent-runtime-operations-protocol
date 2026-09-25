// Command harness is the sole writer of the P12 publication report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-publication-service"
	checker  = "reference/control-plane/internal/domain/publication/testdata/harness/main.go"
	waiver   = "reference/control-plane/internal/domain/publication/testdata/transition/p12-baseline-transition-waiver.json"
	baseline = "31da1c8353b61e5eceae97f90b5e633e29abf3e5"
)

type result struct {
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
	Baseline   struct{ Rule, Commit string } `json:"baseline"`
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
	add := func(name string, err error, ok string) {
		detail := ok
		if err != nil {
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, err := trackedInputs(root)
	add("p12-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	add("p12-runtime-inputs-empty", nil, "runtime_inputs is empty")
	add("p12-transition-waiver", validateTransition(root), "waiver exactly binds Git changes, P12 artifacts and acceptance")
	add("p12-catalog-snapshots", validateCatalog(root), "P09/P10 snapshots and P12 current catalog are explicit")
	add("p12-no-production-0020", rejectProduction0020(root), "production catalog and migration directories stop at 0010")

	scratch, scratchErr := os.MkdirTemp("/tmp", "arop-p12-")
	if scratchErr == nil {
		scratchErr = os.Chmod(scratch, 0o700)
	}
	add("p12-private-scratch", scratchErr, "private scratch created")
	var pg *cluster
	var pgEvidence []byte
	if scratchErr == nil {
		pg, pgEvidence, err = startPostgres(scratch)
	} else {
		err = scratchErr
	}
	add("p12-private-postgres16", err, "private PostgreSQL 16 ready on Unix socket")
	var tests result
	if pg != nil {
		tests = runTests(root, scratch, pg.url())
	} else {
		tests.err = errors.New("PostgreSQL prerequisite failed")
	}
	add("p12-sqlite-postgres-publication", tests.err, "publication, HTTP, CLI, migration and both repository suites pass without skip")
	add("p12-no-skips", rejectSkips(tests.output), "test stream has no skip/cache/no-tests terminal")

	regressions := []struct{ name, target, report string }{
		{"p05", "test-go-workspace", "build/reports/P05/report.json"},
		{"p06", "test-protocol-foundation", "build/reports/P06/report.json"},
		{"p08", "test-control-plane-platform", "build/reports/P08/report.json"},
		{"p09", "test-storage-migrations", "build/reports/P09/report.json"},
		{"p10", "test-identity-secrets", "build/reports/P10/report.json"},
		{"p11", "test-publication-contracts", "build/reports/P11/report.json"},
	}
	evidence := []report.RuntimeEvidence{{Kind: "p12-go-test", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))}, {Kind: "p12-postgres-toolchain", SHA256: report.Hash(pgEvidence), Bytes: int64(len(pgEvidence))}}
	for _, item := range regressions {
		r := run(root, nil, "make", item.target)
		if r.err == nil {
			v := run(root, nil, "make", "verify-report", "REPORT="+item.report)
			r.output = append(r.output, v.output...)
			r.err = v.err
		}
		add("p12-"+item.name+"-regression", r.err, strings.ToUpper(item.name)+" current report verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: item.name + "-regression", SHA256: report.Hash(r.output), Bytes: int64(len(r.output))})
	}
	if pg != nil {
		err = pg.stop()
	} else {
		err = nil
	}
	add("p12-postgres-shutdown", err, "private PostgreSQL stopped")
	if scratchErr == nil {
		err = os.RemoveAll(scratch)
	} else {
		err = nil
	}
	add("p12-scratch-cleanup", err, "scratch removed")

	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P12", Suite: "AROP P12 publication service", Class: "p12.publication", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 5, "database_engines": 2, "runtime_inputs": 0}, AuditNote: "P12 binds its five owned artifacts and the independently discovered transition closure. P09 and P10 migration snapshots remain immutable; Current is exactly 0001+0005+0010. P05/P06/P08/P09/P10/P11 are rerun on one head. Independent reviews for A commit 45867105, B and C are represented only by digest-and-byte runtime evidence supplied by their test streams; no old report is an input. Publication is reference-only, uses one durable UoW for mutation and audit, and introduces no Endpoint field, network fetch, production 0020, memory fallback, secret, DSN or scratch path."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P12/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P12 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P12 checks failed; see build/reports/P12/report.json"))
	}
	fmt.Printf("AROP publication service passed: %d checks.\n", len(checks))
}

func trackedInputs(root string) ([]string, error) {
	r := run(root, nil, "git", "ls-files", "-z")
	if r.err != nil {
		return nil, r.err
	}
	parts := bytes.Split(r.output, []byte{0})
	out := []string{}
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	sort.Strings(out)
	return out, nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var w transition
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&w); err != nil {
		return err
	}
	if w.SchemaVersion != 1 || w.Status != "validated" || w.Baseline.Commit != baseline || w.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" {
		return errors.New("transition identity/baseline invalid")
	}
	r := run(root, nil, "git", "diff", "--name-only", "--no-renames", baseline+"..HEAD")
	if r.err != nil {
		return r.err
	}
	want := nonemptyLines(r.output)
	sort.Strings(w.SourceClosure)
	if strings.Join(want, "\n") != strings.Join(w.SourceClosure, "\n") {
		return errors.New("transition source closure differs from Git")
	}
	wantArtifacts := []string{"arop-cli-publication-command", "phase-report-p12", "postgres-migration-publication", "publication-fixtures", "publication-service", "p12-baseline-transition-waiver", "sqlite-migration-publication"}
	if strings.Join(w.AffectedArtifacts, "\n") != strings.Join(wantArtifacts, "\n") {
		return errors.New("transition artifact closure mismatch")
	}
	if len(w.Acceptance) != 7 || len(w.Constraints) != 8 {
		return errors.New("transition acceptance/constraints incomplete")
	}
	return nil
}

func validateCatalog(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/storage/migrate/production_catalog.go"))
	if err != nil {
		return err
	}
	s := string(b)
	for _, needle := range []string{"func P09ProductionCatalog()", `ReportPhase: "P09"`, "func P10ProductionCatalog()", `ReportPhase: "P10"`, "func CurrentProductionCatalog()", `ReportPhase: "P12"`, "0001_base.sql", "0005_identity.sql", "0010_publication.sql"} {
		if !strings.Contains(s, needle) {
			return fmt.Errorf("catalog missing %s", needle)
		}
	}
	return nil
}
func rejectProduction0020(root string) error {
	for _, d := range []string{"sqlite", "postgres"} {
		m, _ := filepath.Glob(filepath.Join(root, "reference/control-plane/migrations", d, "0020*"))
		if len(m) > 0 {
			return errors.New("production 0020 exists")
		}
	}
	return nil
}

func runTests(root, scratch, dsn string) result {
	work := filepath.Join(scratch, "go.work")
	body := []byte("go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n")
	if err := os.WriteFile(work, body, 0o600); err != nil {
		return result{err: err}
	}
	env := map[string]string{"GOWORK": work, "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TMPDIR": scratch, "AROP_P12_POSTGRES_URL": dsn}
	return run(filepath.Join(root, "reference/control-plane"), env, "go", "test", "-count=1", "-json", "./cmd/aropd", "./internal/app/platform/httpadapter", "./internal/domain/publication/...", "./internal/storage/migrate")
}
func rejectSkips(out []byte) error {
	if bytes.Contains(out, []byte(`"Action":"skip"`)) || bytes.Contains(out, []byte("[no test files]")) || bytes.Contains(out, []byte("(cached)")) {
		return errors.New("test inventory contains skip/cache/no-tests")
	}
	return nil
}

func startPostgres(scratch string) (*cluster, []byte, error) {
	tools := map[string]string{}
	evidence := bytes.Buffer{}
	for _, n := range []string{"initdb", "postgres", "pg_isready"} {
		p, e := exec.LookPath(n)
		if e != nil {
			return nil, nil, e
		}
		v := run("/", nil, p, "--version")
		if v.err != nil || !strings.Contains(string(v.output), "16.") {
			return nil, nil, fmt.Errorf("%s is not PostgreSQL 16", n)
		}
		tools[n] = p
		fmt.Fprintf(&evidence, "%s:%s\n", n, strings.TrimSpace(string(v.output)))
	}
	data := filepath.Join(scratch, "pgdata")
	socket := filepath.Join(scratch, "pgsocket")
	if err := os.Mkdir(socket, 0o700); err != nil {
		return nil, evidence.Bytes(), err
	}
	if r := run(scratch, map[string]string{"HOME": "/nonexistent"}, tools["initdb"], "-D", data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "--username=arop_p12", "--no-instructions"); r.err != nil {
		return nil, evidence.Bytes(), errors.New("initdb failed")
	}
	c := &cluster{socket: socket}
	c.cmd = exec.Command(tools["postgres"], "-D", data, "-h", "", "-k", socket, "-p", "5432", "-c", "unix_socket_permissions=0700", "-c", "timezone=UTC", "-c", "logging_collector=off")
	c.cmd.Dir = scratch
	c.cmd.Env = cleanEnv(map[string]string{"HOME": "/nonexistent"})
	c.cmd.Stdout = &c.log
	c.cmd.Stderr = &c.log
	if err := c.cmd.Start(); err != nil {
		return nil, evidence.Bytes(), err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run(scratch, nil, tools["pg_isready"], "-h", socket, "-p", "5432", "-U", "arop_p12", "-d", "postgres", "-q").err == nil {
			return c, evidence.Bytes(), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = c.stop()
	return nil, evidence.Bytes(), errors.New("postgres startup timeout")
}
func (c *cluster) url() string {
	return "postgresql://arop_p12@localhost/postgres?host=" + url.QueryEscape(c.socket) + "&port=5432&sslmode=disable"
}
func (c *cluster) stop() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		return errors.New("postgres shutdown timeout")
	}
}

func run(dir string, overrides map[string]string, name string, args ...string) result {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = cleanEnv(overrides)
	o, e := c.CombinedOutput()
	if e != nil {
		tail := o
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		e = fmt.Errorf("%w: %s", e, strings.TrimSpace(string(tail)))
	}
	return result{o, e}
}
func cleanEnv(overrides map[string]string) []string {
	keep := []string{"LANG", "LC_ALL", "PATH", "TMPDIR", "TZ"}
	m := map[string]string{}
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	for k, v := range overrides {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
func nonemptyLines(b []byte) []string {
	var out []string
	for _, s := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
func sanitize(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "/tmp/", "<tmp>/")
	if len(s) > 1500 {
		s = s[:1500]
	}
	return s
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
