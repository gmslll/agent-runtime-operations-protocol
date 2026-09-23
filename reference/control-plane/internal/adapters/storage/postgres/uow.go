// Package postgres provides PostgreSQL database, transaction, and advisory
// lock adapters for the reference Control Plane.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	parsedURL, err := url.Parse(dsn)
	if err != nil || (parsedURL.Scheme != "postgres" && parsedURL.Scheme != "postgresql") || parsedURL.Host == "" || parsedURL.Path == "" || parsedURL.Path == "/" {
		return nil, errors.New("PostgreSQL DSN must be an explicit postgres URL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse PostgreSQL DSN")
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	connectContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(connectContext); err != nil {
		_ = db.Close()
		return nil, errors.New("connect PostgreSQL database")
	}
	return db, nil
}

const MaintenanceLockKey int64 = 0x41524f505f4350

type transactionContextKey struct{}

type UnitOfWork struct {
	db             *sql.DB
	maintenanceKey int64
}

func NewUnitOfWork(db *sql.DB) (*UnitOfWork, error) {
	if db == nil {
		return nil, errors.New("PostgreSQL unit of work requires database")
	}
	return &UnitOfWork{db: db}, nil
}

func NewUnitOfWorkWithMaintenanceLock(db *sql.DB, key int64) (*UnitOfWork, error) {
	if db == nil || key == 0 {
		return nil, errors.New("PostgreSQL unit of work requires database and maintenance lock key")
	}
	return &UnitOfWork{db: db, maintenanceKey: key}, nil
}

func (unit *UnitOfWork) Within(ctx context.Context, callback func(context.Context) error) (returnErr error) {
	if callback == nil {
		return errors.New("unit of work callback is required")
	}
	if _, nested := unit.Transaction(ctx); nested {
		return errors.New("nested PostgreSQL unit of work is not supported")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := unit.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL unit of work: %w", err)
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
	if unit.maintenanceKey != 0 {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, unit.maintenanceKey); err != nil {
			return fmt.Errorf("acquire PostgreSQL application maintenance guard: %w", err)
		}
	}
	transactionContext := context.WithValue(ctx, transactionContextKey{}, tx)
	if err := callback(transactionContext); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit PostgreSQL unit of work: %w", err)
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
	db            *sql.DB
	key           int64
	unlockTimeout time.Duration
}

func NewMigrationLocker(db *sql.DB, key int64) (*MigrationLocker, error) {
	return NewMigrationLockerWithTimeout(db, key, 5*time.Second)
}

func NewMigrationLockerWithTimeout(db *sql.DB, key int64, unlockTimeout time.Duration) (*MigrationLocker, error) {
	if db == nil || key == 0 {
		return nil, errors.New("PostgreSQL migration locker requires database and non-zero key")
	}
	if unlockTimeout <= 0 || unlockTimeout > time.Minute {
		return nil, errors.New("PostgreSQL migration unlock timeout must be within 1ns..1m")
	}
	return &MigrationLocker{db: db, key: key, unlockTimeout: unlockTimeout}, nil
}

func (locker *MigrationLocker) Lock(ctx context.Context) (func() error, error) {
	if locker == nil || locker.db == nil {
		return nil, errors.New("PostgreSQL migration locker is not initialized")
	}
	connection, err := locker.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	for {
		var acquired bool
		if err := connection.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, locker.key).Scan(&acquired); err != nil {
			_ = connection.Close()
			return nil, err
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			_ = connection.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() error {
		unlockContext, cancel := context.WithTimeout(context.Background(), locker.unlockTimeout)
		defer cancel()
		var unlocked bool
		err := connection.QueryRowContext(unlockContext, `SELECT pg_advisory_unlock($1)`, locker.key).Scan(&unlocked)
		if err != nil {
			_ = connection.Raw(func(any) error { return driver.ErrBadConn })
		}
		closeErr := connection.Close()
		if err == nil && !unlocked {
			err = errors.New("PostgreSQL advisory lock was not owned")
		}
		return errors.Join(err, closeErr)
	}, nil
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

type connectionEnvironment struct {
	host, port, user, database, password string
	parameters                           map[string]string
}

// BackupRestore invokes PostgreSQL client tools without placing credentials
// or the DSN in argv. pg_restore's single transaction guarantees that corrupt
// input or a restore error leaves the original database unchanged.
type BackupRestore struct {
	db               *sql.DB
	connection       connectionEnvironment
	directory        string
	pgDumpPath       string
	pgRestorePath    string
	psqlPath         string
	databaseIdentity string
	mu               sync.Mutex
}

func NewBackupRestore(db *sql.DB, dsn, backupDirectory, pgDumpPath string) (*BackupRestore, error) {
	if db == nil {
		return nil, errors.New("PostgreSQL backup requires database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" {
		return nil, errors.New("PostgreSQL backup requires an explicit DSN")
	}
	if err := validatePrivateDirectory(backupDirectory); err != nil {
		return nil, err
	}
	dumpPath, err := validateExecutable(pgDumpPath, "pg_dump")
	if err != nil {
		return nil, err
	}
	restorePath, err := validateExecutable(filepath.Join(filepath.Dir(dumpPath), "pg_restore"), "pg_restore")
	if err != nil {
		return nil, err
	}
	psqlPath, err := validateExecutable(filepath.Join(filepath.Dir(dumpPath), "psql"), "psql")
	if err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse PostgreSQL backup connection")
	}
	parameters := map[string]string{}
	for query, environment := range map[string]string{"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "sslcrl": "PGSSLCRL"} {
		if value := parsed.Query().Get(query); value != "" {
			parameters[environment] = value
		}
	}
	connection := connectionEnvironment{host: config.Host, port: strconv.Itoa(int(config.Port)), user: config.User, database: config.Database, password: config.Password, parameters: parameters}
	if strings.ContainsAny(connection.password, "\r\n\x00") {
		return nil, errors.New("PostgreSQL password contains unsupported control character")
	}
	identityDigest := sha256.Sum256([]byte(config.Host + "\x00" + strconv.Itoa(int(config.Port)) + "\x00" + config.User + "\x00" + config.Database))
	return &BackupRestore{db: db, connection: connection, directory: backupDirectory, pgDumpPath: dumpPath, pgRestorePath: restorePath, psqlPath: psqlPath, databaseIdentity: hex.EncodeToString(identityDigest[:])}, nil
}

func (backup *BackupRestore) Create(ctx context.Context, fromVersion, toVersion int64) (migrate.Snapshot, error) {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	file, err := os.CreateTemp(backup.directory, fmt.Sprintf("postgres-v%d-to-v%d-*.dump", fromVersion, toVersion))
	if err != nil {
		return migrate.Snapshot{}, errors.New("create PostgreSQL backup file")
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
		return migrate.Snapshot{}, errors.New("secure PostgreSQL backup file")
	}
	err = backup.withConnectionEnvironment(func(environment []string, extraFiles []*os.File) error {
		command := exec.CommandContext(ctx, backup.pgDumpPath, "--format=custom", "--no-owner", "--no-privileges", "--file", name)
		command.Env = environment
		command.ExtraFiles = extraFiles
		return command.Run()
	})
	if err != nil {
		return migrate.Snapshot{}, errors.New("pg_dump failed")
	}
	if err := backup.validateArchive(ctx, name); err != nil {
		return migrate.Snapshot{}, err
	}
	digest, err := digestRegularFile(name)
	if err != nil {
		return migrate.Snapshot{}, err
	}
	id := filepath.Base(name)
	record := snapshotRecord{SchemaVersion: 1, Engine: "postgres", DatabaseIdentity: backup.databaseIdentity, ID: id, Digest: digest, FromVersion: fromVersion, ToVersion: toVersion, State: "available"}
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
	if err != nil || record.Digest != snapshot.Digest || snapshot.Digest == "" || record.State != "available" {
		return errors.New("PostgreSQL snapshot identity is not owned by this adapter")
	}
	path := filepath.Join(backup.directory, record.ID)
	digest, err := digestRegularFile(path)
	if err != nil || digest != record.Digest {
		return errors.New("PostgreSQL snapshot digest verification failed")
	}
	if err := backup.validateArchive(ctx, path); err != nil {
		return err
	}
	if err := backup.restoreArchive(ctx, path); err != nil {
		return err
	}
	var restoredVersion int64
	var dirty bool
	if err := backup.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0),COALESCE(bool_or(dirty),false) FROM arop_schema_migrations`).Scan(&restoredVersion, &dirty); err != nil || dirty || restoredVersion != record.FromVersion {
		return errors.New("restored PostgreSQL migration history is incompatible")
	}
	if err := backup.db.PingContext(ctx); err != nil {
		return errors.New("restored PostgreSQL database is unavailable")
	}
	return nil
}

func (backup *BackupRestore) Finalize(_ context.Context, snapshot migrate.Snapshot) error {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	record, err := backup.readSnapshotMetadata(snapshot.ID)
	if err != nil || record.Digest != snapshot.Digest || record.State != "available" {
		return errors.New("PostgreSQL snapshot cannot be finalized")
	}
	record.State = "restored_verified"
	return backup.writeSnapshotMetadata(record)
}

func (backup *BackupRestore) ListSnapshots() ([]SnapshotMetadata, error) {
	entries, err := os.ReadDir(backup.directory)
	if err != nil {
		return nil, errors.New("read PostgreSQL snapshot directory")
	}
	var result []SnapshotMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dump.metadata.json") {
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
		return snapshotRecord{}, errors.New("PostgreSQL snapshot ID is invalid")
	}
	data, err := os.ReadFile(filepath.Join(backup.directory, id+".metadata.json"))
	if err != nil || len(data) > 4096 {
		return snapshotRecord{}, errors.New("read PostgreSQL snapshot metadata")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record snapshotRecord
	if err := decoder.Decode(&record); err != nil {
		return snapshotRecord{}, errors.New("decode PostgreSQL snapshot metadata")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return snapshotRecord{}, errors.New("PostgreSQL snapshot metadata has trailing content")
	}
	if record.SchemaVersion != 1 || record.Engine != "postgres" || record.DatabaseIdentity != backup.databaseIdentity || record.ID != id || record.Digest == "" || record.FromVersion < 1 || record.ToVersion <= record.FromVersion || record.State != "available" && record.State != "restored_verified" {
		return snapshotRecord{}, errors.New("PostgreSQL snapshot metadata is incompatible")
	}
	return record, nil
}

func (backup *BackupRestore) writeSnapshotMetadata(record snapshotRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("encode PostgreSQL snapshot metadata")
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(backup.directory, ".arop-postgres-metadata-")
	if err != nil {
		return errors.New("create PostgreSQL snapshot metadata")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("secure PostgreSQL snapshot metadata")
	}
	if _, err := temporary.Write(data); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return errors.New("write PostgreSQL snapshot metadata")
	}
	if err := os.Rename(temporaryName, filepath.Join(backup.directory, record.ID+".metadata.json")); err != nil {
		return errors.New("publish PostgreSQL snapshot metadata")
	}
	return nil
}

func (backup *BackupRestore) validateArchive(ctx context.Context, path string) error {
	_, err := backup.archiveList(ctx, path)
	return err
}

func (backup *BackupRestore) archiveList(ctx context.Context, path string) ([]byte, error) {
	command := exec.CommandContext(ctx, backup.pgRestorePath, "--list", path)
	command.Env = safeProcessEnvironment()
	output, err := command.Output()
	if err != nil || len(output) == 0 || len(output) > 16<<20 {
		return nil, errors.New("PostgreSQL snapshot archive validation failed")
	}
	return output, nil
}

func (backup *BackupRestore) restoreArchive(ctx context.Context, path string) error {
	list, err := backup.archiveList(ctx, path)
	if err != nil {
		return err
	}
	hasPublicSchema := bytes.Contains(list, []byte(" SCHEMA - public "))
	script, err := anonymousWritableFile(backup.directory, ".arop-restore-sql-")
	if err != nil {
		return errors.New("prepare isolated PostgreSQL restore script")
	}
	defer script.Close()
	restore := exec.CommandContext(ctx, backup.pgRestorePath, "--file=-", "--no-owner", "--no-privileges", "--exit-on-error", path)
	restore.Env = safeProcessEnvironment()
	restore.Stdout = &hardLimitWriter{writer: script, remaining: 1 << 30}
	if err := restore.Run(); err != nil {
		return fmt.Errorf("PostgreSQL archive rendering failed: %w", err)
	}
	if err := script.Sync(); err != nil {
		return errors.New("sync PostgreSQL restore script")
	}
	info, err := script.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 1<<30 {
		return errors.New("PostgreSQL restore script size is invalid")
	}
	if _, err := script.Seek(0, io.SeekStart); err != nil {
		return errors.New("rewind PostgreSQL restore script")
	}
	prefixSQL := "DROP SCHEMA public CASCADE;\n"
	if !hasPublicSchema {
		prefixSQL += "CREATE SCHEMA public;\n"
	}
	prefix := strings.NewReader(prefixSQL)
	psqlErr := backup.withConnectionEnvironment(func(environment []string, extraFiles []*os.File) error {
		psql := exec.CommandContext(ctx, backup.psqlPath, "--no-psqlrc", "--no-password", "--set=ON_ERROR_STOP=1", "--single-transaction")
		psql.Env = environment
		psql.ExtraFiles = extraFiles
		psql.Stdin = io.MultiReader(prefix, script)
		return psql.Run()
	})
	if psqlErr != nil {
		return fmt.Errorf("transactional PostgreSQL restore client failed: %w", psqlErr)
	}
	return nil
}

type hardLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *hardLimitWriter) Write(contents []byte) (int, error) {
	if writer.remaining <= 0 {
		return 0, errors.New("PostgreSQL restore script exceeds hard limit")
	}
	if int64(len(contents)) <= writer.remaining {
		written, err := writer.writer.Write(contents)
		writer.remaining -= int64(written)
		return written, err
	}
	written, err := writer.writer.Write(contents[:writer.remaining])
	writer.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	return written, errors.New("PostgreSQL restore script exceeds hard limit")
}

func anonymousWritableFile(directory, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := os.Remove(name); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func anonymousReadFile(directory, pattern string, contents []byte) (*os.File, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	cleanup := func() {
		file.Close()
		_ = os.Remove(name)
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Write(contents); err != nil || file.Sync() != nil {
		cleanup()
		return nil, errors.New("write anonymous file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil || os.Remove(name) != nil {
		cleanup()
		return nil, errors.New("isolate anonymous file")
	}
	return file, nil
}

func (backup *BackupRestore) withConnectionEnvironment(callback func([]string, []*os.File) error) error {
	environment := append(safeProcessEnvironment(),
		"PGHOST="+backup.connection.host,
		"PGPORT="+backup.connection.port,
		"PGUSER="+backup.connection.user,
		"PGDATABASE="+backup.connection.database,
	)
	for key, value := range backup.connection.parameters {
		environment = append(environment, key+"="+value)
	}
	if backup.connection.password == "" {
		return callback(environment, nil)
	}
	escaped := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(backup.connection.password)
	file, err := anonymousReadFile(backup.directory, ".arop-pgpass-", []byte("*:*:*:*:"+escaped+"\n"))
	if err != nil {
		return errors.New("prepare PostgreSQL credential file")
	}
	defer file.Close()
	return callback(append(environment, "PGPASSFILE=/dev/fd/3"), []*os.File{file})
}

func validatePrivateDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("PostgreSQL backup directory must be absolute and clean")
	}
	if err := validateNoSymlinkComponents(path); err != nil {
		return errors.New("PostgreSQL backup directory path is unsafe")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("PostgreSQL backup directory must be private and contain no symlink")
	}
	return nil
}

func validateExecutable(path, expectedBase string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != expectedBase {
		return "", fmt.Errorf("%s path must be absolute and exact", expectedBase)
	}
	if err := validateNoSymlinkComponents(path); err != nil {
		return "", fmt.Errorf("%s path contains a symlink", expectedBase)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s must be a regular executable without symlink", expectedBase)
	}
	return path, nil
}

func validateNoSymlinkComponents(target string) error {
	if target == "" || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("path must be absolute and clean")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(target, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path contains missing or symlink component")
		}
	}
	return nil
}

func safeProcessEnvironment() []string {
	allowed := []string{"HOME", "LANG", "LC_ALL", "PATH", "TMPDIR", "TZ"}
	result := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func digestRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 1<<30 {
		return "", errors.New("PostgreSQL snapshot must be a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open PostgreSQL snapshot")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.New("hash PostgreSQL snapshot")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
