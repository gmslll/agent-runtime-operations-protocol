package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pgstore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqlitestore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

const p09CompositionLockKey int64 = 0x41524f505f4350

var p09CompositionSequence atomic.Uint64

func TestP09StorageComposition(t *testing.T) {
	for _, mode := range []platform.Mode{platform.ModeSQLite, platform.ModePostgres} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Run("compose-ready-durable", func(t *testing.T) { p09TestComposeReadyDurable(t, mode) })
			t.Run("compose-failure-no-memory-fallback", func(t *testing.T) { p09TestComposeFailureNoFallback(t, mode) })
			if mode == platform.ModePostgres {
				t.Run("advisory-lock-timeout-no-memory-fallback", p09TestPostgresLockTimeout)
			}
		})
	}
}

func p09TestComposeReadyDurable(t *testing.T, mode platform.Mode) {
	dsn, cleanupDatabase := p09CompositionDatabase(t, mode)
	defer cleanupDatabase()
	args := p09CompositionArgs(t, mode, dsn, "5s", "5s")
	application, server, cleanup, err := composeWithCatalog(args, nil, migrate.P09ProductionCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if application.Durability() != "durable" {
		t.Fatalf("durability=%q want=durable", application.Durability())
	}
	snapshot := application.Readiness(context.Background())
	if !snapshot.Ready || snapshot.Durability != "durable" {
		t.Fatalf("readiness=%+v", snapshot)
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/health/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready status=%d", response.Code)
	}
	var body struct {
		Status     string `json:"status"`
		Durability string `json:"durability"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Status != "ready" || body.Durability != "durable" {
		t.Fatalf("ready body=%q err=%v", response.Body.String(), err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	p09RequirePersistedObservations(t, mode, dsn)

	// A second real composition over the same database must observe the durable
	// state and remain ready; an in-memory fallback would lose it.
	restarted, _, cleanupRestart, err := composeWithCatalog(args, nil, migrate.P09ProductionCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Durability() != "durable" || !restarted.Readiness(context.Background()).Ready {
		t.Fatal("restart did not preserve ready durable composition")
	}
	if err := cleanupRestart(); err != nil {
		t.Fatal(err)
	}

	// The real composition must not discover a migration that is absent from
	// the P09 report-bound production catalog, even when it appears in the
	// configured directory later.
	future := filepath.Join(p09MigrationRootArg(args), string(mode), "0002_future.sql")
	if err := os.WriteFile(future, []byte("SELECT 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, server, unexpectedCleanup, err := composeWithCatalog(args, nil, migrate.P09ProductionCatalog())
	if err == nil {
		if unexpectedCleanup != nil {
			_ = unexpectedCleanup()
		}
		t.Fatalf("real composition consumed undeclared future migration: application=%v server=%v", application != nil, server != nil)
	}
	if application != nil || server != nil || unexpectedCleanup != nil {
		t.Fatal("undeclared future migration failure returned live components")
	}
}

func p09TestComposeFailureNoFallback(t *testing.T, mode platform.Mode) {
	dsn, cleanupDatabase := p09CompositionDatabase(t, mode)
	defer cleanupDatabase()
	db := p09OpenCompositionDatabase(t, mode, dsn)
	_, err := db.Exec(`CREATE TABLE unmanaged_compose_probe(id BIGINT PRIMARY KEY, value TEXT NOT NULL)`)
	if err == nil {
		_, err = db.Exec(`INSERT INTO unmanaged_compose_probe(id,value) VALUES(1,'must-survive')`)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	application, server, cleanup, err := composeWithCatalog(p09CompositionArgs(t, mode, dsn, "2s", "1s"), nil, migrate.P09ProductionCatalog())
	if err == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("unmanaged database composed successfully with durability=%q server=%v", application.Durability(), server != nil)
	}
	if application != nil || server != nil || cleanup != nil {
		t.Fatal("failed durable composition returned live components")
	}
	db = p09OpenCompositionDatabase(t, mode, dsn)
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM unmanaged_compose_probe WHERE id=1`).Scan(&value); err != nil || value != "must-survive" {
		t.Fatalf("failed composition mutated unmanaged database: value=%q err=%v", value, err)
	}
}

func p09TestPostgresLockTimeout(t *testing.T) {
	dsn, cleanupDatabase := p09CompositionDatabase(t, platform.ModePostgres)
	defer cleanupDatabase()
	db := p09OpenCompositionDatabase(t, platform.ModePostgres, dsn)
	defer db.Close()
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err = connection.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, p09CompositionLockKey); err != nil {
		t.Fatal(err)
	}
	defer connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, p09CompositionLockKey)
	started := time.Now()
	application, server, cleanup, err := composeWithCatalog(p09CompositionArgs(t, platform.ModePostgres, dsn, "750ms", "200ms"), nil, migrate.P09ProductionCatalog())
	elapsed := time.Since(started)
	if err == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("composition bypassed held advisory lock with durability=%q", application.Durability())
	}
	if application != nil || server != nil || cleanup != nil {
		t.Fatal("timed-out composition returned live components")
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("migration timeout elapsed=%s", elapsed)
	}
}

func p09CompositionArgs(t *testing.T, mode platform.Mode, dsn, startupTimeout, migrationTimeout string) []string {
	t.Helper()
	backupDirectory := filepath.Join(p09RequiredAbsoluteEnv("AROP_P09_BACKUP_ROOT"), fmt.Sprintf("compose-%03d", p09CompositionSequence.Add(1)))
	if err := os.Mkdir(backupDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	migrationRoot := filepath.Join(backupDirectory, "p09-catalog")
	p09CopyIsolatedCatalog(t, migrationRoot)
	return []string{
		"--listen=127.0.0.1:0",
		"--mode=" + string(mode),
		"--database-dsn=" + dsn,
		"--migration-root=" + migrationRoot,
		"--backup-directory=" + backupDirectory,
		"--storage-startup-timeout=" + startupTimeout,
		"--migration-timeout=" + migrationTimeout,
	}
}

func p09CopyIsolatedCatalog(t *testing.T, destination string) {
	t.Helper()
	source := p09RequiredAbsoluteEnv("AROP_P09_MIGRATION_ROOT")
	for _, declared := range migrate.P09ProductionCatalog().Migrations {
		relative := declared.Path
		contents, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Dir(filepath.Join(destination, filepath.FromSlash(relative)))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, filepath.FromSlash(relative)), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func p09MigrationRootArg(args []string) string {
	for _, argument := range args {
		if value, found := strings.CutPrefix(argument, "--migration-root="); found {
			return value
		}
	}
	panic("missing controlled P09 migration root argument")
}

func p09CompositionDatabase(t *testing.T, mode platform.Mode) (string, func()) {
	t.Helper()
	sequence := p09CompositionSequence.Add(1)
	if mode == platform.ModeSQLite {
		path := filepath.Join(p09RequiredAbsoluteEnv("AROP_P09_SQLITE_ROOT"), fmt.Sprintf("compose-%03d.db", sequence))
		return path, func() {}
	}
	base := p09RequiredEnv("AROP_P09_POSTGRES_URL")
	admin, err := pgstore.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("arop_p09_compose_%03d", sequence)
	if _, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String(), func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	}
}

func p09OpenCompositionDatabase(t *testing.T, mode platform.Mode, dsn string) *sql.DB {
	t.Helper()
	var db *sql.DB
	var err error
	if mode == platform.ModeSQLite {
		db, err = sqlitestore.Open(dsn)
	} else {
		db, err = pgstore.Open(context.Background(), dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func p09RequirePersistedObservations(t *testing.T, mode platform.Mode, dsn string) {
	t.Helper()
	db := p09OpenCompositionDatabase(t, mode, dsn)
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM arop_observations`).Scan(&count); err != nil || count < 1 {
		t.Fatalf("durable health observation count=%d err=%v", count, err)
	}
}

func p09RequiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" || strings.ContainsRune(value, 0) {
		panic("missing controlled P09 environment " + key)
	}
	return value
}

func p09RequiredAbsoluteEnv(key string) string {
	value := p09RequiredEnv(key)
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		panic("unclean controlled P09 path " + key)
	}
	return value
}
