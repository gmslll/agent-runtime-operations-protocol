package registrywatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

func TestSQLiteCoordinatorFencesLeaderAndCompactsMonotonically(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "registry.db")
	db, err := sqliteadapter.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE arop_registry_meta(singleton INTEGER PRIMARY KEY,revision INTEGER NOT NULL,compaction_watermark INTEGER NOT NULL); INSERT INTO arop_registry_meta VALUES(1,9,2); CREATE TABLE arop_registry_events(revision INTEGER PRIMARY KEY); INSERT INTO arop_registry_events VALUES(1),(9)`); err != nil {
		t.Fatal(err)
	}
	first, err := NewSQLiteCoordinator(db, filepath.Join(root, "registry.lock"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSQLiteCoordinator(db, filepath.Join(root, "registry.lock"))
	if err != nil {
		t.Fatal(err)
	}
	leadership, err := first.Acquire(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := second.Acquire(blocked, "node-b"); !errorsIsDependency(err) {
		t.Fatalf("second leader acquired or wrong error: %v", err)
	}
	result, err := leadership.Compact(context.Background(), 5)
	if err != nil || result.Revision != 9 || result.PreviousWatermark != 2 || result.Watermark != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := leadership.Compact(context.Background(), 4); !registry.HasReason(err, registry.ReasonInvalidRequest) {
		t.Fatalf("watermark regression accepted: %v", err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM arop_registry_events`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("append-only events changed: count=%d err=%v", events, err)
	}
	if err := leadership.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := leadership.Compact(context.Background(), 6); !errorsIsDependency(err) {
		t.Fatalf("closed leader compacted: %v", err)
	}
	replacement, err := second.Acquire(context.Background(), "node-b")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := replacement.Compact(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	backupDirectory := filepath.Join(root, "backups")
	if err := os.Mkdir(backupDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	backup, err := sqliteadapter.NewBackupRestore(db, databasePath, backupDirectory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := backup.Create(context.Background(), 30, 31)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE arop_registry_meta SET revision=10,compaction_watermark=10 WHERE singleton=1; INSERT INTO arop_registry_events VALUES(10)`); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	var revision, watermark uint64
	if err := db.QueryRow(`SELECT revision,compaction_watermark FROM arop_registry_meta WHERE singleton=1`).Scan(&revision, &watermark); err != nil || revision != 9 || watermark != 9 {
		t.Fatalf("restored meta revision=%d watermark=%d err=%v", revision, watermark, err)
	}
	if err := second.Check(context.Background()); err != nil {
		t.Fatalf("restored SQLite readiness: %v", err)
	}
}

func TestPostgresCoordinatorFencesLeaderAndCompactsMonotonically(t *testing.T) {
	dsn := os.Getenv("AROP_P16_POSTGRES_URL")
	if dsn == "" {
		t.Skip("P16 harness supplies a private PostgreSQL 16 URL")
	}
	db, err := postgresadapter.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE IF EXISTS arop_registry_events; DROP TABLE IF EXISTS arop_registry_meta; CREATE TABLE arop_registry_meta(singleton SMALLINT PRIMARY KEY,revision BIGINT NOT NULL,compaction_watermark BIGINT NOT NULL); INSERT INTO arop_registry_meta VALUES(1,12,3); CREATE TABLE arop_registry_events(revision BIGINT PRIMARY KEY); INSERT INTO arop_registry_events VALUES(1),(12)`); err != nil {
		t.Fatal(err)
	}
	first, _ := NewPostgresCoordinator(db)
	second, _ := NewPostgresCoordinator(db)
	leadership, err := first.Acquire(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(context.Background(), "node-b"); !errorsIsDependency(err) {
		t.Fatalf("second node acquired leadership: %v", err)
	}
	result, err := leadership.Compact(context.Background(), 8)
	if err != nil || result.Revision != 12 || result.PreviousWatermark != 3 || result.Watermark != 8 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := leadership.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := leadership.Compact(context.Background(), 9); !errorsIsDependency(err) {
		t.Fatalf("closed PostgreSQL leader compacted: %v", err)
	}
	replacement, err := second.Acquire(context.Background(), "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Compact(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM arop_registry_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("append-only events changed: count=%d err=%v", count, err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	postgresBin := os.Getenv("AROP_P16_POSTGRES_BIN")
	backupRoot := os.Getenv("AROP_P16_SCRATCH")
	if postgresBin == "" || backupRoot == "" {
		t.Fatal("P16 harness must supply PostgreSQL backup tools and scratch")
	}
	backupDirectory := filepath.Join(backupRoot, "postgres-backups")
	if err := os.Mkdir(backupDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	backup, err := postgresadapter.NewBackupRestore(db, dsn, backupDirectory, filepath.Join(postgresBin, "pg_dump"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := backup.Create(context.Background(), 30, 31)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE arop_registry_meta SET revision=13,compaction_watermark=13 WHERE singleton=1; INSERT INTO arop_registry_events VALUES(13)`); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	var revision, watermark uint64
	if err := db.QueryRow(`SELECT revision,compaction_watermark FROM arop_registry_meta WHERE singleton=1`).Scan(&revision, &watermark); err != nil || revision != 12 || watermark != 12 {
		t.Fatalf("restored PostgreSQL meta revision=%d watermark=%d err=%v", revision, watermark, err)
	}
	if err := second.Check(context.Background()); err != nil {
		t.Fatalf("restored PostgreSQL readiness: %v", err)
	}
}

func errorsIsDependency(err error) bool { return err == ErrDependencyUnavailable }
