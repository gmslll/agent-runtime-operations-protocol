// Package migrate provides the deliberately small, fail-closed migration
// engine used by the reference Control Plane. It only migrates forward and it
// records the checksum of every applied migration so edited history cannot be
// mistaken for a healthy database.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"

	CheckpointAfterDirtyCommit = "after_dirty_commit"
)

var migrationFilePattern = regexp.MustCompile(`^([0-9]{4})_([a-z][a-z0-9_]*)\.sql$`)

type Queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Migration struct {
	Version  int64
	Name     string
	Checksum string
	SQL      []byte
}

func NewMigration(version int64, name string, contents []byte) (Migration, error) {
	if version < 1 || version > 9999 {
		return Migration{}, errors.New("migration version must be within 1..9999")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(name) {
		return Migration{}, errors.New("migration name is invalid")
	}
	if len(contents) == 0 || !utf8.Valid(contents) || strings.IndexByte(string(contents), 0) >= 0 || strings.HasPrefix(string(contents), "\ufeff") {
		return Migration{}, errors.New("migration SQL must be non-empty canonical UTF-8")
	}
	digest := sha256.Sum256(contents)
	return Migration{Version: version, Name: name, Checksum: hex.EncodeToString(digest[:]), SQL: append([]byte(nil), contents...)}, nil
}

type Catalog struct {
	dialect    Dialect
	migrations []Migration
}

func NewCatalog(dialect Dialect, migrations []Migration) (*Catalog, error) {
	if dialect != DialectSQLite && dialect != DialectPostgres {
		return nil, errors.New("unsupported migration dialect")
	}
	items := append([]Migration(nil), migrations...)
	if len(items) == 0 {
		return nil, errors.New("migration catalog must not be empty")
	}
	for index, item := range items {
		if index == 0 && item.Version != 1 {
			return nil, errors.New("migration catalog must start at version 1")
		}
		if index > 0 && item.Version <= items[index-1].Version {
			return nil, errors.New("migration catalog versions must be unique and strictly increasing")
		}
		canonical, err := NewMigration(item.Version, item.Name, item.SQL)
		if err != nil {
			return nil, fmt.Errorf("invalid migration %d: %w", item.Version, err)
		}
		if item.Checksum != "" && item.Checksum != canonical.Checksum {
			return nil, fmt.Errorf("migration %d checksum does not match its SQL", item.Version)
		}
		items[index] = canonical
	}
	return &Catalog{dialect: dialect, migrations: items}, nil
}

func LoadCatalog(filesystem fs.FS, directory string, dialect Dialect) (*Catalog, error) {
	if filesystem == nil || directory == "" || path.IsAbs(directory) || path.Clean(directory) != directory || strings.HasPrefix(directory, "../") {
		return nil, errors.New("migration catalog directory is invalid")
	}
	if err := validateCatalogDirectory(filesystem, directory); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(filesystem, directory)
	if err != nil {
		return nil, fmt.Errorf("read migration catalog: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("migration catalog contains non-regular entry %q", entry.Name())
		}
		match := migrationFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration catalog contains unexpected file %q", entry.Name())
		}
		var version int64
		if _, err := fmt.Sscanf(match[1], "%d", &version); err != nil {
			return nil, fmt.Errorf("parse migration version: %w", err)
		}
		contents, err := fs.ReadFile(filesystem, path.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		migration, err := NewMigration(version, match[2], contents)
		if err != nil {
			return nil, fmt.Errorf("load migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration)
	}
	return NewCatalog(dialect, migrations)
}

func validateCatalogDirectory(filesystem fs.FS, directory string) error {
	if directory == "." {
		return nil
	}
	parent := "."
	for _, component := range strings.Split(directory, "/") {
		entries, err := fs.ReadDir(filesystem, parent)
		if err != nil {
			return errors.New("inspect migration catalog directory")
		}
		found := false
		for _, entry := range entries {
			if entry.Name() != component {
				continue
			}
			found = true
			if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() {
				return errors.New("migration catalog directory contains symlink or non-directory component")
			}
			break
		}
		if !found {
			return errors.New("migration catalog directory does not exist")
		}
		parent = path.Join(parent, component)
	}
	return nil
}

func (catalog *Catalog) Dialect() Dialect { return catalog.dialect }
func (catalog *Catalog) TargetVersion() int64 {
	if catalog == nil || len(catalog.migrations) == 0 {
		return 0
	}
	return catalog.migrations[len(catalog.migrations)-1].Version
}

type Locker interface {
	Lock(context.Context) (func() error, error)
}

type Snapshot struct {
	ID     string
	Digest string
}

type BackupRestore interface {
	Create(context.Context, int64, int64) (Snapshot, error)
	Restore(context.Context, Snapshot) error
}

type SnapshotFinalizer interface {
	Finalize(context.Context, Snapshot) error
}

type Verifier func(context.Context, Queryer) error
type FaultHook func(context.Context, string, int64) error
type Clock func() time.Time

type Option func(*Runner) error

const defaultRecoveryTimeout = 30 * time.Second

func WithVerifier(verifier Verifier) Option {
	return func(runner *Runner) error {
		if verifier == nil {
			return errors.New("migration verifier is nil")
		}
		runner.verifier = verifier
		return nil
	}
}

func WithBackupRestore(backup BackupRestore) Option {
	return func(runner *Runner) error {
		if backup == nil {
			return errors.New("backup/restore adapter is nil")
		}
		runner.backup = backup
		return nil
	}
}

func WithFaultHook(hook FaultHook) Option {
	return func(runner *Runner) error {
		if hook == nil {
			return errors.New("migration fault hook is nil")
		}
		runner.fault = hook
		return nil
	}
}

func WithClock(clock Clock) Option {
	return func(runner *Runner) error {
		if clock == nil {
			return errors.New("migration clock is nil")
		}
		runner.clock = clock
		return nil
	}
}

// WithRecoveryTimeout bounds the out-of-band restore and verification path.
// Recovery deliberately does not inherit an already-cancelled migration
// context, otherwise a timeout at the unsafe point would make rollback
// impossible.
func WithRecoveryTimeout(timeout time.Duration) Option {
	return func(runner *Runner) error {
		if timeout <= 0 || timeout > time.Hour {
			return errors.New("migration recovery timeout must be within 1ns..1h")
		}
		runner.recoveryTimeout = timeout
		return nil
	}
}

type Runner struct {
	db              *sql.DB
	catalog         *Catalog
	locker          Locker
	verifier        Verifier
	backup          BackupRestore
	fault           FaultHook
	clock           Clock
	recoveryTimeout time.Duration
}

func NewRunner(db *sql.DB, catalog *Catalog, locker Locker, options ...Option) (*Runner, error) {
	if db == nil || catalog == nil || locker == nil {
		return nil, errors.New("database, catalog, and migration locker are required")
	}
	runner := &Runner{db: db, catalog: catalog, locker: locker, clock: func() time.Time { return time.Now().UTC() }, recoveryTimeout: defaultRecoveryTimeout}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil migration option")
		}
		if err := option(runner); err != nil {
			return nil, err
		}
	}
	return runner, nil
}

type AppliedMigration struct {
	Version  int64
	Name     string
	Checksum string
	Dirty    bool
}

type Status struct {
	CurrentVersion int64
	TargetVersion  int64
	Ready          bool
	Applied        []AppliedMigration
	Reason         string
}

type Result struct {
	FromVersion int64
	ToVersion   int64
	Applied     []int64
	Snapshot    *Snapshot
}

func (runner *Runner) Name() string { return "storage.migrations" }

func (runner *Runner) Check(ctx context.Context) error {
	status, err := runner.Status(ctx)
	if err != nil {
		return err
	}
	if !status.Ready {
		return fmt.Errorf("database migration readiness failed: %s", status.Reason)
	}
	return nil
}

func (runner *Runner) Status(ctx context.Context) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	exists, err := runner.historyExists(ctx)
	if err != nil {
		return Status{}, err
	}
	status := Status{TargetVersion: runner.catalog.TargetVersion()}
	if !exists {
		unmanaged, inspectErr := runner.hasUnmanagedObjects(ctx)
		if inspectErr != nil {
			return Status{}, inspectErr
		}
		if unmanaged {
			status.Reason = "unmanaged_database"
		} else {
			status.Reason = "migration_history_missing"
		}
		return status, nil
	}
	if err := runner.verifyHistorySchema(ctx); err != nil {
		status.Reason = "migration_history_schema_incompatible"
		return status, nil
	}
	applied, err := runner.readHistory(ctx)
	if err != nil {
		return Status{}, err
	}
	status.Applied = applied
	if len(applied) != 0 {
		status.CurrentVersion = applied[len(applied)-1].Version
	}
	if err := runner.validateHistory(applied); err != nil {
		status.Reason = "migration_history_incompatible"
		return status, nil
	}
	if status.CurrentVersion != status.TargetVersion {
		status.Reason = "migration_version_mismatch"
		return status, nil
	}
	if runner.verifier == nil {
		status.Reason = "schema_verifier_missing"
		return status, nil
	}
	if err := runner.verifier(ctx, runner.db); err != nil {
		status.Reason = "schema_verification_failed"
		return status, nil
	}
	if err := runner.verifyWritable(ctx); err != nil {
		status.Reason = "database_not_writable"
		return status, nil
	}
	status.Ready, status.Reason = true, "ok"
	return status, nil
}

func (runner *Runner) Migrate(ctx context.Context) (result Result, returnErr error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	unlock, err := runner.locker.Lock(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release migration lock: %w", unlockErr))
		}
	}()
	exists, err := runner.historyExists(ctx)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		unmanaged, inspectErr := runner.hasUnmanagedObjects(ctx)
		if inspectErr != nil {
			return Result{}, inspectErr
		}
		if unmanaged {
			return Result{}, errors.New("database contains unmanaged objects without migration history")
		}
	}
	if err := runner.ensureHistory(ctx); err != nil {
		return Result{}, err
	}
	if err := runner.verifyHistorySchema(ctx); err != nil {
		return Result{}, fmt.Errorf("verify migration history schema: %w", err)
	}
	applied, err := runner.readHistory(ctx)
	if err != nil {
		return Result{}, err
	}
	if err := runner.validateHistory(applied); err != nil {
		return Result{}, err
	}
	current := int64(0)
	if len(applied) != 0 {
		current = applied[len(applied)-1].Version
	}
	result = Result{FromVersion: current, ToVersion: current}
	if current == runner.catalog.TargetVersion() {
		if runner.verifier == nil {
			return Result{}, errors.New("migration schema verifier is required")
		}
		if err := runner.verifier(ctx, runner.db); err != nil {
			return Result{}, fmt.Errorf("verify migrated schema: %w", err)
		}
		return result, nil
	}
	if current > 0 {
		if runner.backup == nil {
			return Result{}, errors.New("non-empty database migration requires backup/restore adapter")
		}
		snapshot, err := runner.backup.Create(ctx, current, runner.catalog.TargetVersion())
		if err != nil {
			return Result{}, fmt.Errorf("create pre-migration backup: %w", err)
		}
		if snapshot.ID == "" || snapshot.Digest == "" {
			return Result{}, errors.New("backup adapter returned incomplete snapshot evidence")
		}
		result.Snapshot = &snapshot
		restoreFrom := current
		defer func() {
			if returnErr == nil {
				return
			}
			restoreContext, cancel := context.WithTimeout(context.Background(), runner.recoveryTimeout)
			defer cancel()
			if restoreErr := runner.backup.Restore(restoreContext, snapshot); restoreErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore pre-migration backup: %w", restoreErr))
				return
			}
			restored, statusErr := runner.Status(restoreContext)
			if statusErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("verify restored migration status: %w", statusErr))
				return
			}
			if restored.CurrentVersion != restoreFrom || restored.Reason == "migration_history_incompatible" {
				returnErr = errors.Join(returnErr, errors.New("restored migration status does not match the pre-migration version"))
				return
			}
			if finalizer, ok := runner.backup.(SnapshotFinalizer); ok {
				if finalizeErr := finalizer.Finalize(restoreContext, snapshot); finalizeErr != nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("finalize restored snapshot evidence: %w", finalizeErr))
				}
			}
		}()
	}
	for _, migration := range runner.catalog.migrations[len(applied):] {
		if err := runner.markDirty(ctx, migration); err != nil {
			return Result{}, err
		}
		if runner.fault != nil {
			if err := runner.fault(ctx, CheckpointAfterDirtyCommit, migration.Version); err != nil {
				return Result{}, fmt.Errorf("migration fault at %s: %w", CheckpointAfterDirtyCommit, err)
			}
		}
		if err := runner.apply(ctx, migration); err != nil {
			return Result{}, err
		}
		result.Applied = append(result.Applied, migration.Version)
		result.ToVersion = migration.Version
	}
	if runner.verifier == nil {
		return Result{}, errors.New("migration schema verifier is required")
	}
	if err := runner.verifier(ctx, runner.db); err != nil {
		return Result{}, fmt.Errorf("verify migrated schema: %w", err)
	}
	return result, nil
}

func (runner *Runner) ensureHistory(ctx context.Context) error {
	statement := sqliteHistoryDDL
	if runner.catalog.dialect == DialectPostgres {
		statement = postgresHistoryDDL
	}
	if _, err := runner.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create migration history: %w", err)
	}
	return nil
}

const sqliteHistoryDDL = `CREATE TABLE IF NOT EXISTS arop_schema_migrations (
version INTEGER NOT NULL,
name TEXT NOT NULL,
checksum TEXT NOT NULL,
dirty INTEGER NOT NULL,
started_at_ns INTEGER NOT NULL,
applied_at_ns INTEGER,
CONSTRAINT arop_schema_migrations_pkey PRIMARY KEY(version),
CONSTRAINT arop_schema_migrations_version_check CHECK(version BETWEEN 1 AND 9999),
CONSTRAINT arop_schema_migrations_name_check CHECK(length(name) BETWEEN 1 AND 128),
CONSTRAINT arop_schema_migrations_checksum_check CHECK(length(checksum) = 64 AND checksum NOT GLOB '*[^0-9a-f]*'),
CONSTRAINT arop_schema_migrations_dirty_check CHECK(dirty IN (0,1)),
CONSTRAINT arop_schema_migrations_started_check CHECK(started_at_ns > 0),
CONSTRAINT arop_schema_migrations_applied_check CHECK(applied_at_ns IS NULL OR applied_at_ns >= started_at_ns)
)`

const postgresHistoryDDL = `CREATE TABLE IF NOT EXISTS arop_schema_migrations (
version BIGINT NOT NULL,
name TEXT NOT NULL,
checksum TEXT NOT NULL,
dirty BOOLEAN NOT NULL,
started_at_ns BIGINT NOT NULL,
applied_at_ns BIGINT,
CONSTRAINT arop_schema_migrations_pkey PRIMARY KEY(version),
CONSTRAINT arop_schema_migrations_version_check CHECK(version BETWEEN 1 AND 9999),
CONSTRAINT arop_schema_migrations_name_check CHECK(length(name) BETWEEN 1 AND 128),
CONSTRAINT arop_schema_migrations_checksum_check CHECK(checksum ~ '^[0-9a-f]{64}$'),
CONSTRAINT arop_schema_migrations_started_check CHECK(started_at_ns > 0),
CONSTRAINT arop_schema_migrations_applied_check CHECK(applied_at_ns IS NULL OR applied_at_ns >= started_at_ns)
)`

type schemaColumn struct {
	name       string
	dataType   string
	notNull    bool
	primaryKey int
	defaultSQL string
}

func (runner *Runner) verifyHistorySchema(ctx context.Context) error {
	switch runner.catalog.dialect {
	case DialectSQLite:
		rows, err := runner.db.QueryContext(ctx, `PRAGMA table_info('arop_schema_migrations')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		expected := []schemaColumn{
			{"version", "INTEGER", true, 1, ""}, {"name", "TEXT", true, 0, ""},
			{"checksum", "TEXT", true, 0, ""}, {"dirty", "INTEGER", true, 0, ""},
			{"started_at_ns", "INTEGER", true, 0, ""}, {"applied_at_ns", "INTEGER", false, 0, ""},
		}
		index := 0
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, dataType string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				return err
			}
			if index >= len(expected) || cid != index || name != expected[index].name || strings.ToUpper(dataType) != expected[index].dataType || (notNull != 0) != expected[index].notNull || primaryKey != expected[index].primaryKey || defaultValue.Valid {
				return errors.New("SQLite migration history columns are not exact")
			}
			index++
		}
		if err := rows.Err(); err != nil || index != len(expected) {
			return errors.New("SQLite migration history columns are not exact")
		}
		var ddl string
		if err := runner.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='arop_schema_migrations'`).Scan(&ddl); err != nil {
			return err
		}
		if normalizeSchemaSQL(ddl) != normalizeSchemaSQL(strings.Replace(sqliteHistoryDDL, " IF NOT EXISTS", "", 1)) {
			return errors.New("SQLite migration history definition is not exact")
		}
		return nil
	case DialectPostgres:
		rows, err := runner.db.QueryContext(ctx, `SELECT a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),'') FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid='public.arop_schema_migrations'::regclass AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`)
		if err != nil {
			return err
		}
		expected := []schemaColumn{
			{"version", "bigint", true, 0, ""}, {"name", "text", true, 0, ""}, {"checksum", "text", true, 0, ""},
			{"dirty", "boolean", true, 0, ""}, {"started_at_ns", "bigint", true, 0, ""}, {"applied_at_ns", "bigint", false, 0, ""},
		}
		index := 0
		for rows.Next() {
			var name, dataType, defaultSQL string
			var notNull bool
			if err := rows.Scan(&name, &dataType, &notNull, &defaultSQL); err != nil {
				rows.Close()
				return err
			}
			if index >= len(expected) || name != expected[index].name || dataType != expected[index].dataType || notNull != expected[index].notNull || defaultSQL != "" {
				rows.Close()
				return errors.New("PostgreSQL migration history columns are not exact")
			}
			index++
		}
		if err := rows.Close(); err != nil || index != len(expected) {
			return errors.New("PostgreSQL migration history columns are not exact")
		}
		constraints, err := runner.db.QueryContext(ctx, `SELECT conname,contype,pg_get_constraintdef(oid,true) FROM pg_constraint WHERE conrelid='public.arop_schema_migrations'::regclass ORDER BY conname`)
		if err != nil {
			return err
		}
		defer constraints.Close()
		expectedConstraints := map[string]string{
			"arop_schema_migrations_pkey": "p", "arop_schema_migrations_version_check": "c", "arop_schema_migrations_name_check": "c",
			"arop_schema_migrations_checksum_check": "c", "arop_schema_migrations_started_check": "c", "arop_schema_migrations_applied_check": "c",
		}
		seen := map[string]bool{}
		for constraints.Next() {
			var name, kind, definition string
			if err := constraints.Scan(&name, &kind, &definition); err != nil {
				return err
			}
			if expectedConstraints[name] != kind || seen[name] || !historyConstraintLooksExact(name, definition) {
				return errors.New("PostgreSQL migration history constraints are not exact")
			}
			seen[name] = true
		}
		if len(seen) != len(expectedConstraints) {
			return errors.New("PostgreSQL migration history constraints are not exact")
		}
		return constraints.Err()
	default:
		return errors.New("unsupported migration dialect")
	}
}

func historyConstraintLooksExact(name, definition string) bool {
	normalized := normalizeConstraintDefinition(definition)
	expected := map[string]string{
		"arop_schema_migrations_pkey":           "primarykeyversion",
		"arop_schema_migrations_version_check":  "checkversion>=1andversion<=9999",
		"arop_schema_migrations_name_check":     "checklengthname>=1andlengthname<=128",
		"arop_schema_migrations_checksum_check": "checkchecksum~'^[0-9a-f]{64}$'",
		"arop_schema_migrations_started_check":  "checkstarted_at_ns>0",
		"arop_schema_migrations_applied_check":  "checkapplied_at_nsisnullorapplied_at_ns>=started_at_ns",
	}
	return expected[name] != "" && normalized == expected[name]
}

func normalizeConstraintDefinition(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "::text", "")
	return strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "(", "", ")", "").Replace(value)
}

func normalizeSchemaSQL(value string) string {
	return strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(strings.ToLower(value))
}

func (runner *Runner) hasUnmanagedObjects(ctx context.Context) (bool, error) {
	var count int
	var err error
	switch runner.catalog.dialect {
	case DialectSQLite:
		err = runner.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND name <> 'arop_schema_migrations' AND type IN ('table','view','trigger','index')`).Scan(&count)
	case DialectPostgres:
		err = runner.db.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM pg_namespace n WHERE n.nspname NOT IN ('pg_catalog','information_schema','public') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%') +
(SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname <> 'arop_schema_migrations' AND c.relkind IN ('r','p','v','m','S','f')) +
(SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public') +
(SELECT COUNT(*) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public' AND t.typrelid=0 AND t.typtype IN ('d','e','c','r','m'))`).Scan(&count)
	default:
		return false, errors.New("unsupported migration dialect")
	}
	if err != nil {
		return false, fmt.Errorf("inspect unmanaged database objects: %w", err)
	}
	return count != 0, nil
}

func (runner *Runner) verifyWritable(ctx context.Context) error {
	tx, err := runner.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE arop_schema_migrations SET name=name WHERE version=-1`
	if runner.catalog.dialect == DialectPostgres {
		query = `UPDATE arop_schema_migrations SET name=name WHERE version=$1`
		_, err = tx.ExecContext(ctx, query, int64(-1))
	} else {
		_, err = tx.ExecContext(ctx, query)
	}
	return err
}

func (runner *Runner) historyExists(ctx context.Context) (bool, error) {
	var exists bool
	var err error
	switch runner.catalog.dialect {
	case DialectSQLite:
		var count int
		err = runner.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='arop_schema_migrations'`).Scan(&count)
		exists = count == 1
	case DialectPostgres:
		err = runner.db.QueryRowContext(ctx, `SELECT to_regclass('public.arop_schema_migrations') IS NOT NULL`).Scan(&exists)
	}
	if err != nil {
		return false, fmt.Errorf("inspect migration history: %w", err)
	}
	return exists, nil
}

func (runner *Runner) readHistory(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := runner.db.QueryContext(ctx, `SELECT version,name,checksum,dirty FROM arop_schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	var result []AppliedMigration
	for rows.Next() {
		var item AppliedMigration
		if err := rows.Scan(&item.Version, &item.Name, &item.Checksum, &item.Dirty); err != nil {
			return nil, fmt.Errorf("scan migration history: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration history: %w", err)
	}
	return result, nil
}

func (runner *Runner) validateHistory(applied []AppliedMigration) error {
	if len(applied) > len(runner.catalog.migrations) {
		return errors.New("database migration version is ahead of this binary")
	}
	for index, record := range applied {
		expected := runner.catalog.migrations[index]
		if record.Version != expected.Version {
			return errors.New("database migration history contains a gap")
		}
		if record.Dirty {
			return fmt.Errorf("database migration %d is dirty", record.Version)
		}
		if record.Name != expected.Name || record.Checksum != expected.Checksum {
			return fmt.Errorf("database migration %d checksum or name drift", record.Version)
		}
	}
	return nil
}

func (runner *Runner) markDirty(ctx context.Context, migration Migration) error {
	now := runner.clock().UTC()
	if now.IsZero() {
		return errors.New("migration clock returned zero time")
	}
	query := `INSERT INTO arop_schema_migrations(version,name,checksum,dirty,started_at_ns,applied_at_ns) VALUES(?,?,?,?,?,NULL)`
	if runner.catalog.dialect == DialectPostgres {
		query = `INSERT INTO arop_schema_migrations(version,name,checksum,dirty,started_at_ns,applied_at_ns) VALUES($1,$2,$3,$4,$5,NULL)`
	}
	if _, err := runner.db.ExecContext(ctx, query, migration.Version, migration.Name, migration.Checksum, true, now.UnixNano()); err != nil {
		return fmt.Errorf("mark migration %d dirty: %w", migration.Version, err)
	}
	return nil
}

func (runner *Runner) apply(ctx context.Context, migration Migration) error {
	tx, err := runner.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.Version, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	statements, err := splitStatements(migration.SQL)
	if err != nil {
		return fmt.Errorf("parse migration %d: %w", migration.Version, err)
	}
	for index, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("execute migration %d statement %d: %w", migration.Version, index+1, err)
		}
	}
	query := `UPDATE arop_schema_migrations SET dirty=?, applied_at_ns=? WHERE version=? AND dirty=?`
	arguments := []any{false, runner.clock().UTC().UnixNano(), migration.Version, true}
	if runner.catalog.dialect == DialectPostgres {
		query = `UPDATE arop_schema_migrations SET dirty=$1, applied_at_ns=$2 WHERE version=$3 AND dirty=$4`
	}
	result, err := tx.ExecContext(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("mark migration %d clean: %w", migration.Version, err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("migration %d dirty marker was not updated exactly once", migration.Version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.Version, err)
	}
	committed = true
	return nil
}

func splitStatements(contents []byte) ([]string, error) {
	parts := strings.Split(string(contents), "\n-- arop:statement\n")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement == "" {
			return nil, errors.New("migration contains an empty statement")
		}
		statements = append(statements, statement)
	}
	return statements, nil
}
