// Package sqlite provides the SQLite database and transaction adapters used by
// the reference Control Plane. SQLite is an explicit durable mode; it is never
// selected as an implicit fallback.
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
	"github.com/gofrs/flock"
	modernsqlite "modernc.org/sqlite"
)

func Open(filePath string) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return OpenContext(ctx, filePath)
}

func OpenContext(ctx context.Context, filePath string) (*sql.DB, error) {
	if filePath == "" || !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || filepath.Base(filePath) == string(filepath.Separator) || filepath.Base(filePath) == "." {
		return nil, errors.New("SQLite database path must be absolute and clean")
	}
	if err := validatePathComponents(filePath, true, false); err != nil {
		return nil, fmt.Errorf("validate SQLite database path: %w", err)
	}
	location := (&url.URL{Scheme: "file", Path: filePath}).String()
	dsn := location + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.New("open SQLite database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.New("connect SQLite database")
	}
	if err := secureRegularFile(filePath, 0o600); err != nil {
		_ = db.Close()
		return nil, errors.New("secure SQLite database permissions")
	}
	if err := validatePathComponents(filePath, false, true); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("validate created SQLite database: %w", err)
	}
	return db, nil
}

func validatePathComponents(target string, allowMissingLeaf, requireRegularLeaf bool) error {
	if target == "" || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("path must be absolute and clean")
	}
	volume := filepath.VolumeName(target)
	current := string(filepath.Separator)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(target, volume), string(filepath.Separator)), string(filepath.Separator))
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) && allowMissingLeaf && index == len(parts)-1 {
				return nil
			}
			return errors.New("path component does not exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path component must not be a symlink")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return errors.New("path ancestor must be a directory")
		}
		if index == len(parts)-1 && requireRegularLeaf && !info.Mode().IsRegular() {
			return errors.New("path leaf must be a regular file")
		}
	}
	return nil
}

func secureRegularFile(path string, permissions os.FileMode) error {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return errors.New("file must be regular and contain no symlink")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) || !os.SameFile(before, after) {
		return errors.New("file identity changed during validation")
	}
	return file.Chmod(permissions)
}

type transactionContextKey struct{}

type UnitOfWork struct {
	db              *sql.DB
	maintenanceLock *flock.Flock
}

func NewUnitOfWork(db *sql.DB) (*UnitOfWork, error) {
	if db == nil {
		return nil, errors.New("SQLite unit of work requires database")
	}
	return &UnitOfWork{db: db}, nil
}

func NewUnitOfWorkWithMigrationLock(db *sql.DB, lockPath string) (*UnitOfWork, error) {
	if db == nil {
		return nil, errors.New("SQLite unit of work requires database")
	}
	if err := validatePathComponents(lockPath, false, true); err != nil {
		return nil, errors.New("SQLite unit of work migration lock is unsafe")
	}
	return &UnitOfWork{db: db, maintenanceLock: flock.New(lockPath)}, nil
}

func (unit *UnitOfWork) Within(ctx context.Context, callback func(context.Context) error) (returnErr error) {
	if callback == nil {
		return errors.New("unit of work callback is required")
	}
	if _, nested := unit.Transaction(ctx); nested {
		return errors.New("nested SQLite unit of work is not supported")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if unit.maintenanceLock != nil {
		locked, err := unit.maintenanceLock.TryRLockContext(ctx, 25*time.Millisecond)
		if err != nil {
			return fmt.Errorf("acquire SQLite application maintenance guard: %w", err)
		}
		if !locked {
			return errors.New("SQLite application maintenance guard was not acquired")
		}
		defer func() { returnErr = errors.Join(returnErr, unit.maintenanceLock.Unlock()) }()
	}
	tx, err := unit.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin SQLite unit of work: %w", err)
	}
	committed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}
		if !committed {
			returnErr = errors.Join(returnErr, tx.Rollback())
		}
	}()
	transactionContext := context.WithValue(ctx, transactionContextKey{}, tx)
	if err := callback(transactionContext); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite unit of work: %w", err)
	}
	committed = true
	return nil
}

func (unit *UnitOfWork) Transaction(ctx context.Context) (*sql.Tx, bool) {
	if unit == nil || ctx == nil {
		return nil, false
	}
	tx, ok := ctx.Value(transactionContextKey{}).(*sql.Tx)
	return tx, ok && tx != nil
}

type MigrationLocker struct {
	lockPath     string
	databasePath string
	lock         *flock.Flock
}

func NewMigrationLocker(lockPath string) (*MigrationLocker, error) {
	if lockPath == "" || !filepath.IsAbs(lockPath) || filepath.Clean(lockPath) != lockPath {
		return nil, errors.New("SQLite migration lock path must be absolute and clean")
	}
	databasePath := ""
	for _, suffix := range []string{".migrate.lock", ".migration.lock"} {
		if strings.HasSuffix(lockPath, suffix) {
			databasePath = strings.TrimSuffix(lockPath, suffix)
			break
		}
	}
	if databasePath == "" {
		return nil, errors.New("SQLite migration lock must be bound to a database path")
	}
	if err := validatePathComponents(databasePath, false, true); err != nil {
		return nil, errors.New("SQLite migration lock database path is unsafe")
	}
	if err := validatePathComponents(lockPath, true, true); err != nil {
		return nil, errors.New("SQLite migration lock path is unsafe")
	}
	return &MigrationLocker{lockPath: lockPath, databasePath: databasePath, lock: flock.New(lockPath)}, nil
}

func (locker *MigrationLocker) Lock(ctx context.Context) (func() error, error) {
	if locker == nil || locker.lock == nil {
		return nil, errors.New("SQLite migration locker is not initialized")
	}
	locked, err := locker.lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("SQLite migration lock was not acquired")
	}
	if err := secureRegularFile(locker.lockPath, 0o600); err != nil {
		_ = locker.lock.Unlock()
		return nil, errors.New("secure SQLite migration lock permissions")
	}
	if err := validatePathComponents(locker.lockPath, false, true); err != nil {
		_ = locker.lock.Unlock()
		return nil, errors.New("SQLite migration lock file is unsafe")
	}
	return locker.lock.Unlock, nil
}

type sqliteBackupConnection interface {
	NewBackup(string) (*modernsqlite.Backup, error)
	NewRestore(string) (*modernsqlite.Backup, error)
}

type snapshotRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	Engine           string `json:"engine"`
	DatabaseIdentity string `json:"database_identity"`
	ID               string `json:"id"`
	Digest           string `json:"digest"`
	FromVersion      int64  `json:"from_version"`
	ToVersion        int64  `json:"to_version"`
	State            string `json:"state"`
}

type SnapshotMetadata = snapshotRecord

// BackupRestore uses SQLite's online backup API, so both snapshot and restore
// are safe while the production database handle remains open.
type BackupRestore struct {
	db               *sql.DB
	database         string
	directory        string
	databaseIdentity string
	mu               sync.Mutex
}

func NewBackupRestore(db *sql.DB, databasePath, backupDirectory string) (*BackupRestore, error) {
	if db == nil {
		return nil, errors.New("SQLite backup requires database")
	}
	if err := validatePathComponents(databasePath, false, true); err != nil {
		return nil, errors.New("SQLite backup database path is unsafe")
	}
	if err := validatePathComponents(backupDirectory, false, false); err != nil {
		return nil, errors.New("SQLite backup directory is unsafe")
	}
	info, err := os.Lstat(backupDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("SQLite backup directory must be a private directory")
	}
	identityDigest := sha256.Sum256([]byte(databasePath))
	return &BackupRestore{db: db, database: databasePath, directory: backupDirectory, databaseIdentity: hex.EncodeToString(identityDigest[:])}, nil
}

func (backup *BackupRestore) Create(ctx context.Context, fromVersion, toVersion int64) (migrate.Snapshot, error) {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return migrate.Snapshot{}, err
	}
	file, err := os.CreateTemp(backup.directory, fmt.Sprintf("sqlite-v%d-to-v%d-*.db", fromVersion, toVersion))
	if err != nil {
		return migrate.Snapshot{}, errors.New("create SQLite backup file")
	}
	name := file.Name()
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(name)
		}
	}()
	if err := file.Chmod(0o600); err != nil || file.Close() != nil {
		return migrate.Snapshot{}, errors.New("secure SQLite backup file")
	}
	if err := backup.copyOnline(ctx, name, false); err != nil {
		return migrate.Snapshot{}, fmt.Errorf("create SQLite online backup: %w", err)
	}
	if err := verifySQLiteSnapshot(ctx, name); err != nil {
		return migrate.Snapshot{}, err
	}
	digest, err := digestRegularFile(name)
	if err != nil {
		return migrate.Snapshot{}, err
	}
	id := filepath.Base(name)
	record := snapshotRecord{SchemaVersion: 1, Engine: "sqlite", DatabaseIdentity: backup.databaseIdentity, ID: id, Digest: digest, FromVersion: fromVersion, ToVersion: toVersion, State: "available"}
	if err := backup.writeSnapshotMetadata(record); err != nil {
		return migrate.Snapshot{}, err
	}
	cleanup = false
	return migrate.Snapshot{ID: id, Digest: digest}, nil
}

func (backup *BackupRestore) Restore(ctx context.Context, snapshot migrate.Snapshot) error {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	record, err := backup.readSnapshotMetadata(snapshot.ID)
	if err != nil || snapshot.Digest == "" || record.Digest != snapshot.Digest || record.State != "available" {
		return errors.New("SQLite snapshot identity is not owned by this adapter")
	}
	path := filepath.Join(backup.directory, record.ID)
	digest, err := digestRegularFile(path)
	if err != nil || digest != record.Digest {
		return errors.New("SQLite snapshot digest verification failed")
	}
	if err := verifySQLiteSnapshot(ctx, path); err != nil {
		return err
	}
	if err := backup.copyOnline(ctx, path, true); err != nil {
		return fmt.Errorf("restore SQLite online backup: %w", err)
	}
	if err := sqliteIntegrity(ctx, backup.db); err != nil {
		return errors.New("restored SQLite database failed integrity verification")
	}
	return nil
}

func (backup *BackupRestore) Finalize(_ context.Context, snapshot migrate.Snapshot) error {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	record, err := backup.readSnapshotMetadata(snapshot.ID)
	if err != nil || record.Digest != snapshot.Digest || record.State != "available" {
		return errors.New("SQLite snapshot cannot be finalized")
	}
	record.State = "restored_verified"
	return backup.writeSnapshotMetadata(record)
}

func (backup *BackupRestore) ListSnapshots() ([]SnapshotMetadata, error) {
	entries, err := os.ReadDir(backup.directory)
	if err != nil {
		return nil, errors.New("read SQLite snapshot directory")
	}
	var result []SnapshotMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db.metadata.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".metadata.json")
		record, err := backup.readSnapshotMetadata(id)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (backup *BackupRestore) readSnapshotMetadata(id string) (snapshotRecord, error) {
	if id == "" || filepath.Base(id) != id {
		return snapshotRecord{}, errors.New("SQLite snapshot ID is invalid")
	}
	data, err := os.ReadFile(filepath.Join(backup.directory, id+".metadata.json"))
	if err != nil || len(data) > 4096 {
		return snapshotRecord{}, errors.New("read SQLite snapshot metadata")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record snapshotRecord
	if err := decoder.Decode(&record); err != nil {
		return snapshotRecord{}, errors.New("decode SQLite snapshot metadata")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return snapshotRecord{}, errors.New("SQLite snapshot metadata has trailing content")
	}
	if record.SchemaVersion != 1 || record.Engine != "sqlite" || record.DatabaseIdentity != backup.databaseIdentity || record.ID != id || record.Digest == "" || record.FromVersion < 1 || record.ToVersion <= record.FromVersion || record.State != "available" && record.State != "restored_verified" {
		return snapshotRecord{}, errors.New("SQLite snapshot metadata is incompatible")
	}
	return record, nil
}

func (backup *BackupRestore) writeSnapshotMetadata(record snapshotRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("encode SQLite snapshot metadata")
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(backup.directory, ".arop-sqlite-metadata-")
	if err != nil {
		return errors.New("create SQLite snapshot metadata")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("secure SQLite snapshot metadata")
	}
	if _, err := temporary.Write(data); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return errors.New("write SQLite snapshot metadata")
	}
	if err := os.Rename(temporaryName, filepath.Join(backup.directory, record.ID+".metadata.json")); err != nil {
		return errors.New("publish SQLite snapshot metadata")
	}
	return nil
}

func (backup *BackupRestore) copyOnline(ctx context.Context, remotePath string, restore bool) error {
	connection, err := backup.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	remoteURI := (&url.URL{Scheme: "file", Path: remotePath}).String()
	return connection.Raw(func(raw any) error {
		provider, ok := raw.(sqliteBackupConnection)
		if !ok {
			return errors.New("SQLite driver does not expose online backup")
		}
		var operation *modernsqlite.Backup
		if restore {
			operation, err = provider.NewRestore(remoteURI)
		} else {
			operation, err = provider.NewBackup(remoteURI)
		}
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = operation.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, stepErr := operation.Step(128)
			if stepErr != nil {
				return stepErr
			}
			if !more {
				if err := operation.Finish(); err != nil {
					return err
				}
				finished = true
				return nil
			}
		}
	})
}

func digestRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("snapshot must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open snapshot")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 1<<30+1)); err != nil {
		return "", errors.New("hash snapshot")
	}
	if info.Size() <= 0 || info.Size() > 1<<30 {
		return "", errors.New("snapshot size is invalid")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifySQLiteSnapshot(ctx context.Context, path string) error {
	location := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", location)
	if err != nil {
		return errors.New("open SQLite snapshot")
	}
	defer db.Close()
	if err := sqliteIntegrity(ctx, db); err != nil {
		return errors.New("SQLite snapshot integrity verification failed")
	}
	return nil
}

func sqliteIntegrity(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	return nil
}
