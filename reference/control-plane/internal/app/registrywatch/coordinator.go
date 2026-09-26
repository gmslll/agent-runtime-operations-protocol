package registrywatch

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
	"github.com/gofrs/flock"
)

const postgresLeaderLockKey int64 = 0x41524f505f5231

type PostgresCoordinator struct{ db *sql.DB }

func NewPostgresCoordinator(db *sql.DB) (*PostgresCoordinator, error) {
	if db == nil {
		return nil, ErrDependencyUnavailable
	}
	return &PostgresCoordinator{db: db}, nil
}

func (coordinator *PostgresCoordinator) Check(ctx context.Context) error {
	if err := coordinator.db.PingContext(ctx); err != nil {
		return ErrDependencyUnavailable
	}
	return nil
}

func (coordinator *PostgresCoordinator) Acquire(ctx context.Context, nodeID string) (Leadership, error) {
	if nodeID == "" || len(nodeID) > 128 {
		return nil, registry.NewError(registry.ReasonInvalidRequest)
	}
	connection, err := coordinator.db.Conn(ctx)
	if err != nil {
		return nil, ErrDependencyUnavailable
	}
	var acquired bool
	if err := connection.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, postgresLeaderLockKey).Scan(&acquired); err != nil || !acquired {
		_ = connection.Close()
		return nil, ErrDependencyUnavailable
	}
	return &postgresLeadership{connection: connection}, nil
}

type postgresLeadership struct {
	mu         sync.Mutex
	connection *sql.Conn
	closed     bool
}

func (leadership *postgresLeadership) Compact(ctx context.Context, target uint64) (CompactionResult, error) {
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.closed || target > registry.MaxSafeInteger {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	tx, err := leadership.connection.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	defer tx.Rollback()
	result, err := compactMeta(ctx, tx, target, true)
	if err != nil {
		return CompactionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	return result, nil
}

func (leadership *postgresLeadership) Close() error {
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.closed {
		return nil
	}
	leadership.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var released bool
	err := leadership.connection.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, postgresLeaderLockKey).Scan(&released)
	return errors.Join(err, leadership.connection.Close())
}

type SQLiteCoordinator struct {
	db   *sql.DB
	lock *flock.Flock
}

func NewSQLiteCoordinator(db *sql.DB, lockPath string) (*SQLiteCoordinator, error) {
	if db == nil || lockPath == "" || !filepath.IsAbs(lockPath) || filepath.Clean(lockPath) != lockPath {
		return nil, ErrDependencyUnavailable
	}
	return &SQLiteCoordinator{db: db, lock: flock.New(lockPath)}, nil
}

func (coordinator *SQLiteCoordinator) Check(ctx context.Context) error {
	if err := coordinator.db.PingContext(ctx); err != nil {
		return ErrDependencyUnavailable
	}
	return nil
}

func (coordinator *SQLiteCoordinator) Acquire(ctx context.Context, nodeID string) (Leadership, error) {
	if nodeID == "" || len(nodeID) > 128 {
		return nil, registry.NewError(registry.ReasonInvalidRequest)
	}
	locked, err := coordinator.lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !locked {
		return nil, ErrDependencyUnavailable
	}
	return &sqliteLeadership{db: coordinator.db, lock: coordinator.lock}, nil
}

type sqliteLeadership struct {
	mu     sync.Mutex
	db     *sql.DB
	lock   *flock.Flock
	closed bool
}

func (leadership *sqliteLeadership) Compact(ctx context.Context, target uint64) (CompactionResult, error) {
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.closed || target > registry.MaxSafeInteger {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	tx, err := leadership.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	defer tx.Rollback()
	result, err := compactMeta(ctx, tx, target, false)
	if err != nil {
		return CompactionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	return result, nil
}

func (leadership *sqliteLeadership) Close() error {
	leadership.mu.Lock()
	defer leadership.mu.Unlock()
	if leadership.closed {
		return nil
	}
	leadership.closed = true
	return leadership.lock.Unlock()
}

type metaTransaction interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func compactMeta(ctx context.Context, tx metaTransaction, target uint64, postgres bool) (CompactionResult, error) {
	query := `SELECT revision,compaction_watermark FROM arop_registry_meta WHERE singleton=1`
	if postgres {
		query += ` FOR UPDATE`
	}
	var revision, previous uint64
	if err := tx.QueryRowContext(ctx, query).Scan(&revision, &previous); err != nil || revision > registry.MaxSafeInteger || previous > revision {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	if target < previous || target > revision {
		return CompactionResult{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	placeholder := `?`
	if postgres {
		placeholder = `$1`
	}
	result, err := tx.ExecContext(ctx, `UPDATE arop_registry_meta SET compaction_watermark=`+placeholder+` WHERE singleton=1`, target)
	if err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	return CompactionResult{Revision: revision, PreviousWatermark: previous, Watermark: target}, nil
}
