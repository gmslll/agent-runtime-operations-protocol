package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	secretadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/secrets"
	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/httpadapter"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/identity"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:], os.Environ()); err != nil {
		slog.Error("reference control plane stopped", "error", err)
		os.Exit(1)
	}
}

func compose(args, environment []string) (*platform.Platform, *http.Server, func() error, error) {
	return composeWithCatalog(args, environment, migrate.CurrentProductionCatalog())
}

func composeWithCatalog(args, environment []string, catalogClosure migrate.CatalogClosure) (*platform.Platform, *http.Server, func() error, error) {
	config, err := platform.ParseConfig(args, environment)
	if err != nil {
		return nil, nil, nil, err
	}
	clock := platform.RealClock{}
	ids := platform.SystemIDSource{Clock: clock}
	faults := platform.NoopFaultHook{}
	uow, store, checks, cleanup, err := composeStorageContext(context.Background(), config, catalogClosure, clock, ids, faults)
	if err != nil {
		return nil, nil, nil, err
	}
	application, err := platform.New(config, platform.Dependencies{
		Clock: clock, IDs: ids, Faults: faults,
		UoW: uow, Observability: store, Checks: checks,
	}, "arop-reference-control-plane", version)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, errors.New("initialize Control Plane platform")
	}
	handler, err := httpadapter.NewHandler(application)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, errors.New("assemble Control Plane HTTP handler")
	}
	return application, httpadapter.NewServer(application, handler), cleanup, nil
}

func run(args, environment []string) error {
	application, httpServer, cleanup, err := compose(args, environment)
	if err != nil {
		return err
	}
	defer cleanup()
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("reference control plane listening", "address", httpServer.Addr, "durability", application.Durability())
	return serve(shutdownContext, application, httpServer, application.Config().ShutdownTimeout)
}

func composeStorage(config platform.Config) (platformports.UnitOfWork, observability.Store, []platformports.ReadinessCheck, func() error, error) {
	clock := platform.RealClock{}
	return composeStorageContext(context.Background(), config, migrate.CurrentProductionCatalog(), clock, platform.SystemIDSource{Clock: clock}, platform.NoopFaultHook{})
}

func composeStorageContext(ctx context.Context, config platform.Config, catalogClosure migrate.CatalogClosure, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook) (platformports.UnitOfWork, observability.Store, []platformports.ReadinessCheck, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	startupContext, cancelStartup := context.WithTimeout(ctx, config.StorageStartupTimeout)
	defer cancelStartup()
	switch config.Mode {
	case platform.ModeDevelopmentMemory:
		store, err := memory.New(config.AuditCapacity, config.TraceCapacity)
		if err != nil {
			return nil, nil, nil, nil, errors.New("initialize ephemeral observability store")
		}
		return &platform.SerialUnitOfWork{}, store, nil, func() error { return nil }, nil
	case platform.ModeSQLite:
		root, err := canonicalDirectory(config.MigrationRoot)
		if err != nil {
			return nil, nil, nil, nil, errors.New("validate migration root")
		}
		db, err := sqliteadapter.OpenContext(startupContext, config.DatabaseDSN)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		cleanup := db.Close
		catalog, err := migrate.LoadCatalogClosure(os.DirFS(root), catalogClosure, migrate.DialectSQLite)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("load SQLite migration catalog")
		}
		locker, err := sqliteadapter.NewMigrationLocker(config.DatabaseDSN + ".migrate.lock")
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize SQLite migration lock")
		}
		backup, err := sqliteadapter.NewBackupRestore(db, config.DatabaseDSN, config.BackupDirectory)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize SQLite backup/restore")
		}
		runner, err := migrate.NewRunner(db, catalog, locker,
			migrate.WithVerifier(schemaVerifier(catalogClosure, migrate.DialectSQLite)),
			migrate.WithBackupRestore(backup), migrate.WithRecoveryTimeout(config.MigrationTimeout))
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize SQLite migration runner")
		}
		cancelStartup()
		migrationContext, cancelMigration := context.WithTimeout(ctx, config.MigrationTimeout)
		_, err = runner.Migrate(migrationContext)
		cancelMigration()
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("migrate SQLite database")
		}
		uow, err := sqliteadapter.NewUnitOfWorkWithMigrationLock(db, config.DatabaseDSN+".migrate.lock")
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, err
		}
		store, err := durable.New(db, migrate.DialectSQLite, uow.Transaction, runner.Check)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, err
		}
		checks := []platformports.ReadinessCheck{runner}
		if catalogClosure.ReportPhase == "P10" {
			identityService, err := composeIdentity(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, identityService)
		}
		return uow, store, checks, cleanup, nil
	case platform.ModePostgres:
		root, err := canonicalDirectory(config.MigrationRoot)
		if err != nil {
			return nil, nil, nil, nil, errors.New("validate migration root")
		}
		db, err := postgresadapter.Open(startupContext, config.DatabaseDSN)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		cleanup := db.Close
		catalog, err := migrate.LoadCatalogClosure(os.DirFS(root), catalogClosure, migrate.DialectPostgres)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("load PostgreSQL migration catalog")
		}
		unlockTimeout := 5 * time.Second
		if config.MigrationTimeout < unlockTimeout {
			unlockTimeout = config.MigrationTimeout
		}
		locker, err := postgresadapter.NewMigrationLockerWithTimeout(db, postgresadapter.MaintenanceLockKey, unlockTimeout)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize PostgreSQL migration lock")
		}
		pgDumpPath, err := exec.LookPath("pg_dump")
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("locate pg_dump")
		}
		pgDumpPath, err = filepath.EvalSymlinks(pgDumpPath)
		if err != nil || !filepath.IsAbs(pgDumpPath) {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("resolve pg_dump")
		}
		backup, err := postgresadapter.NewBackupRestore(db, config.DatabaseDSN, config.BackupDirectory, pgDumpPath)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize PostgreSQL backup/restore")
		}
		runner, err := migrate.NewRunner(db, catalog, locker,
			migrate.WithVerifier(schemaVerifier(catalogClosure, migrate.DialectPostgres)),
			migrate.WithBackupRestore(backup), migrate.WithRecoveryTimeout(config.MigrationTimeout))
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("initialize PostgreSQL migration runner")
		}
		cancelStartup()
		migrationContext, cancelMigration := context.WithTimeout(ctx, config.MigrationTimeout)
		_, err = runner.Migrate(migrationContext)
		cancelMigration()
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, errors.New("migrate PostgreSQL database")
		}
		uow, err := postgresadapter.NewUnitOfWorkWithMaintenanceLock(db, postgresadapter.MaintenanceLockKey)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, err
		}
		store, err := durable.New(db, migrate.DialectPostgres, uow.Transaction, runner.Check)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, nil, err
		}
		checks := []platformports.ReadinessCheck{runner}
		if catalogClosure.ReportPhase == "P10" {
			identityService, err := composeIdentity(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, identityService)
		}
		return uow, store, checks, cleanup, nil
	default:
		return nil, nil, nil, nil, errors.New("unsupported storage mode")
	}
}

func schemaVerifier(catalog migrate.CatalogClosure, dialect migrate.Dialect) migrate.Verifier {
	durableVerifier := durable.VerifySchema(dialect)
	if catalog.ReportPhase == "P09" {
		return durableVerifier
	}
	identityVerifier := identity.VerifySchema(dialect)
	return func(ctx context.Context, query migrate.Queryer) error {
		if err := durableVerifier(ctx, query); err != nil {
			return err
		}
		return identityVerifier(ctx, query)
	}
}

func composeIdentity(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook) (*identity.Service, error) {
	repository, err := identity.NewCredentialStore(db, dialect, lookup)
	if err != nil {
		return nil, errors.New("initialize credential store")
	}
	service, err := identity.New(identity.Dependencies{
		Clock: clock, IDs: ids, Faults: faults, UoW: uow, Observability: observations, Repository: repository,
		AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"secret.read"},
	})
	if err != nil {
		return nil, errors.New("initialize identity service")
	}
	// An empty resolver is deliberately composed: without an explicit deployment
	// binding every SecretRef remains denied, and no public handler is registered.
	if _, err := secretadapter.New(secretadapter.Config{Clock: clock, IDs: ids, Observability: observations}); err != nil {
		return nil, errors.New("initialize deny-by-default secret resolver")
	}
	return service, nil
}

func canonicalDirectory(directory string) (string, error) {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("directory must be absolute and clean")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return "", errors.New("directory must exist and contain no symlink")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", errors.New("directory must exist")
	}
	return directory, nil
}

type drainingController interface {
	SetDraining(bool)
}

type serverLifecycle interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

func serve(shutdownContext context.Context, application drainingController, httpServer serverLifecycle, shutdownTimeout time.Duration) error {
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- httpServer.ListenAndServe()
	}()
	select {
	case serveErr := <-serverErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", serveErr)
	case <-shutdownContext.Done():
	}
	application.SetDraining(true)
	gracefulContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(gracefulContext)
	var closeErr error
	if shutdownErr != nil {
		closeErr = httpServer.Close()
	}
	serveErr := <-serverErrors
	var lifecycleErrors []error
	if shutdownErr != nil {
		lifecycleErrors = append(lifecycleErrors, fmt.Errorf("graceful shutdown: %w", shutdownErr))
	}
	if closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
		lifecycleErrors = append(lifecycleErrors, fmt.Errorf("force close HTTP: %w", closeErr))
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		lifecycleErrors = append(lifecycleErrors, fmt.Errorf("serve HTTP during shutdown: %w", serveErr))
	}
	return errors.Join(lifecycleErrors...)
}
