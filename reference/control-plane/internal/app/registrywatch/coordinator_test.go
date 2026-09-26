package registrywatch

import (
	"context"
	"path/filepath"
	"testing"

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
}

func errorsIsDependency(err error) bool { return err == ErrDependencyUnavailable }
