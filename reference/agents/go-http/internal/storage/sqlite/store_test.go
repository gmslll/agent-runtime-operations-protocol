package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitemigration "github.com/gmslll/agent-runtime-operations-protocol/reference/agents/go-http/migrations/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/provider"
	_ "modernc.org/sqlite"
)

func TestMigrationEmptyIdempotentDirtyAndTamper(t *testing.T) {
	ctx := context.Background()
	path := testPath(t, "provider.db")
	store, err := Open(ctx, path, sqlitemigration.Initial)
	if err != nil {
		t.Fatalf("open empty provider database: %v", err)
	}
	if err = store.Ready(ctx); err != nil {
		t.Fatalf("new provider database is not ready: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path, sqlitemigration.Initial)
	if err != nil {
		t.Fatalf("idempotent reopen failed: %v", err)
	}
	if _, err = store.db.Exec(`CREATE TABLE injected(value TEXT) STRICT`); err != nil {
		t.Fatal(err)
	}
	if err = store.Ready(ctx); err == nil {
		t.Fatal("schema verifier accepted an extra table")
	}
	_ = store.Close()

	dirtyPath := testPath(t, "dirty.db")
	dirty, err := sql.Open("sqlite", (&urlBuilder{path: dirtyPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dirty.Exec(`CREATE TABLE unexpected(value TEXT) STRICT`); err != nil {
		t.Fatal(err)
	}
	_ = dirty.Close()
	if _, err = Open(ctx, dirtyPath, sqlitemigration.Initial); err == nil {
		t.Fatal("migration accepted a non-empty unversioned database")
	}

	wrong := append([]byte(nil), sqlitemigration.Initial...)
	wrong = append(wrong, '\n')
	if _, err = Open(ctx, path, wrong); err == nil {
		t.Fatal("migration accepted a changed historical checksum")
	}
}

func TestInboxEffectOutboxRollbackAndConcurrency(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testPath(t, "state.db"), sqlitemigration.Initial)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 27, 5, 0, 0, 0, time.UTC)
	inbox := validInbox(now)
	sentinel := errors.New("rollback")
	if err = store.Within(ctx, func(ctx context.Context, transaction provider.Transaction) error {
		if createErr := transaction.CreateInbox(ctx, inbox); createErr != nil {
			return createErr
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("rollback error mismatch: %v", err)
	}
	if _, err = store.GetInbox(ctx, inbox.RunID, inbox.AttemptID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("rolled back inbox is visible: %v", err)
	}
	if err = store.Within(ctx, func(ctx context.Context, transaction provider.Transaction) error {
		if createErr := transaction.CreateInbox(ctx, inbox); createErr != nil {
			return createErr
		}
		if createErr := transaction.CreateEffect(ctx, provider.EffectRecord{EffectID: "eff_payment_0001", RunID: inbox.RunID, AttemptID: inbox.AttemptID, RequestDigest: digest64("3"), State: provider.EffectStarted, StartedAt: now, UpdatedAt: now}); createErr != nil {
			return createErr
		}
		first, appendErr := transaction.AppendOutbox(ctx, provider.OutboxRecord{RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "evt_one", EventType: "arop.run.accepted", Envelope: json.RawMessage(`{"ok":true}`), CreatedAt: now})
		if appendErr != nil || first != 1 {
			return errors.New("first sequence mismatch")
		}
		second, appendErr := transaction.AppendOutbox(ctx, provider.OutboxRecord{RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "evt_two", EventType: "arop.run.running", Envelope: json.RawMessage(`{"ok":true}`), CreatedAt: now})
		if appendErr != nil || second != 2 {
			return errors.New("second sequence mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Within(ctx, func(ctx context.Context, transaction provider.Transaction) error {
		return transaction.CompleteEffect(ctx, "eff_payment_0001", digest64("3"), json.RawMessage(`{"receipt":"safe"}`), now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	values, err := store.ListOutbox(ctx, inbox.RunID, inbox.AttemptID, 0, 10)
	if err != nil || len(values) != 2 || values[0].Sequence != 1 || values[1].Sequence != 2 {
		t.Fatalf("unexpected outbox: %#v, %v", values, err)
	}

	const workers = 8
	var wait sync.WaitGroup
	results := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- store.Within(ctx, func(ctx context.Context, transaction provider.Transaction) error {
				_, appendErr := transaction.AppendOutbox(ctx, provider.OutboxRecord{RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "evt_concurrent_" + time.Now().Format("150405.000000000"), EventType: "arop.run.progress", Envelope: json.RawMessage(`{"ok":true}`), CreatedAt: time.Now().UTC()})
				return appendErr
			})
		}()
	}
	wait.Wait()
	close(results)
	for workerErr := range results {
		if workerErr != nil {
			t.Fatalf("concurrent outbox append: %v", workerErr)
		}
	}
	values, err = store.ListOutbox(ctx, inbox.RunID, inbox.AttemptID, 0, 32)
	if err != nil || len(values) != 2+workers {
		t.Fatalf("concurrent outbox lost data: %d, %v", len(values), err)
	}
	for index, value := range values {
		if value.Sequence != uint64(index+1) {
			t.Fatalf("sequence %d at index %d", value.Sequence, index)
		}
	}
}

func TestOpenRejectsSymlinkedPathComponents(t *testing.T) {
	realDirectory := testPath(t, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(realDirectory), "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), filepath.Join(link, "provider.db"), sqlitemigration.Initial); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked parent accepted: %v", err)
	}
}

func validInbox(now time.Time) provider.InboxRecord {
	return provider.InboxRecord{
		RunID: "run_01999999-9999-7999-8999-999999999999", AttemptID: "att_01999999-9999-7999-8999-999999999998",
		RequestDigest: digest64("1"), AuthorizationDigest: digest64("2"), RequestJSON: json.RawMessage(`{"schema_version":1}`),
		AgentID: "test.agent", AgentVersion: "1.0.0", SkillID: "default", DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a",
		Generation: 1, FencingToken: 1, StateVersion: 1, Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", State: provider.InboxAccepted,
		DeadlineAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

func digest64(character string) string { return "sha256:" + strings.Repeat(character, 64) }

func testPath(t *testing.T, name string) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, name)
}

type urlBuilder struct{ path string }

func (builder *urlBuilder) String() string {
	return "file:" + builder.path + "?_pragma=foreign_keys(1)&_txlock=immediate"
}
