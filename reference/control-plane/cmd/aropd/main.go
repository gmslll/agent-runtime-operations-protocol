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
	"slices"
	"strings"
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
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registrywatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	assetpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets/storage/postgres"
	assetsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	dispatchpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch/storage/postgres"
	dispatchsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	eventpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event/storage/postgres"
	eventsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	publicationpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/postgres"
	publicationsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
	registrypostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/postgres"
	registrysqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/sqlite"
	domainrun "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	runpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run/storage/postgres"
	runsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run/storage/sqlite"
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
	if catalogOwns(catalogClosure, "P13") {
		config.MaxBodyBytes = assets.MaxAssetBytes
	} else if catalogOwns(catalogClosure, "P12") {
		config.MaxBodyBytes = publication.MaxBundleBytes
	}
	assetKey, err := assetTokenMaterial(config, catalogClosure)
	if err != nil {
		return nil, nil, nil, err
	}
	defer clear(assetKey)
	clock := platform.RealClock{}
	ids := platform.SystemIDSource{Clock: clock}
	faults := platform.NoopFaultHook{}
	uow, store, checks, cleanup, err := composeStorageContext(context.Background(), config, catalogClosure, clock, ids, faults, assetKey)
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
	if identityService := identityServiceFromChecks(checks); identityService != nil {
		if publicationService := publicationServiceFromChecks(checks); publicationService != nil {
			if assetService := assetServiceFromChecks(checks); assetService != nil {
				if registryService := registryAPIServiceFromChecks(checks); registryService != nil {
					if watchService := registryWatchServiceFromChecks(checks); watchService != nil {
						handler, err = httpadapter.NewRegistryRecoveryApplicationHandler(application, referenceAuthenticate(identityService, ids), publicationService, assetService, registryService, watchService)
					} else {
						handler, err = httpadapter.NewRegistryApplicationHandler(application, referenceAuthenticate(identityService, ids), publicationService, assetService, registryService)
					}
				} else {
					handler, err = httpadapter.NewApplicationHandler(application, referenceAuthenticate(identityService, ids), publicationService, assetService)
				}
			} else {
				handler, err = httpadapter.NewPublicationHandler(application, referenceAuthenticate(identityService, ids), publicationService)
			}
		} else {
			handler, err = httpadapter.NewAuthenticatedHandler(application, referenceAuthenticate(identityService, ids))
		}
	}
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, errors.New("assemble Control Plane HTTP handler")
	}
	if runService := runServiceFromChecks(checks); runService != nil {
		handler, err = httpadapter.NewRunApplicationHandler(application, referenceAuthenticate(identityServiceFromChecks(checks), ids), publicationServiceFromChecks(checks), assetServiceFromChecks(checks), registryAPIServiceFromChecks(checks), registryWatchServiceFromChecks(checks), runService)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, errors.New("assemble run lifecycle HTTP handler")
		}
	}
	if dispatchService := dispatchServiceFromChecks(checks); dispatchService != nil {
		handler, err = httpadapter.NewDispatchApplicationHandler(application, referenceAuthenticate(identityServiceFromChecks(checks), ids), publicationServiceFromChecks(checks), assetServiceFromChecks(checks), registryAPIServiceFromChecks(checks), registryWatchServiceFromChecks(checks), runServiceFromChecks(checks), dispatchService)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, errors.New("assemble dispatch HTTP handler")
		}
	}
	if eventService := eventServiceFromChecks(checks); eventService != nil {
		handler, err = httpadapter.NewEventApplicationHandler(application, referenceAuthenticate(identityServiceFromChecks(checks), ids), publicationServiceFromChecks(checks), assetServiceFromChecks(checks), registryAPIServiceFromChecks(checks), registryWatchServiceFromChecks(checks), runServiceFromChecks(checks), dispatchServiceFromChecks(checks), eventService)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, errors.New("assemble event ledger HTTP handler")
		}
	}
	httpServer := httpadapter.NewServer(application, handler)
	if registryWatchServiceFromChecks(checks) != nil {
		httpServer, err = httpadapter.NewRegistryRecoveryServer(application, handler)
		if err != nil {
			_ = cleanup()
			return nil, nil, nil, errors.New("assemble registry recovery HTTP server")
		}
	}
	return application, httpServer, cleanup, nil
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
	catalog := migrate.CurrentProductionCatalog()
	assetKey, err := assetTokenMaterial(config, catalog)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer clear(assetKey)
	return composeStorageContext(context.Background(), config, catalog, clock, platform.SystemIDSource{Clock: clock}, platform.NoopFaultHook{}, assetKey)
}

func composeStorageContext(ctx context.Context, config platform.Config, catalogClosure migrate.CatalogClosure, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook, assetKey []byte) (platformports.UnitOfWork, observability.Store, []platformports.ReadinessCheck, func() error, error) {
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
		if catalogClosure.ReportPhase != "P09" {
			identityService, err := composeIdentity(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, identityService)
		}
		if catalogOwns(catalogClosure, "P12") {
			publicationService, err := composePublication(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, publicationService)
		}
		if catalogOwns(catalogClosure, "P13") {
			assetService, err := composeAssets(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, faults, config.AssetTokenKeyID, assetKey)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, assetService)
		}
		if catalogOwns(catalogClosure, "P14") {
			registryService, watchService, err := composeRegistry(db, migrate.DialectSQLite, uow.Transaction, uow, clock, ids, config.DatabaseDSN+".registry-recovery.lock")
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, registryService, watchService)
		}
		if catalogOwns(catalogClosure, "P18") {
			runService, err := composeRun(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, catalogOwns(catalogClosure, "P20"))
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, runService)
		}
		if catalogOwns(catalogClosure, "P19") {
			dispatchService, err := composeDispatch(ctx, db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, config.DispatchIssuer, catalogOwns(catalogClosure, "P20"))
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, dispatchService)
		}
		if catalogOwns(catalogClosure, "P20") {
			eventService, err := composeEvent(db, migrate.DialectSQLite, uow.Transaction, uow, store, clock, ids, config.DispatchIssuer)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, eventService)
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
		if catalogClosure.ReportPhase != "P09" {
			identityService, err := composeIdentity(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, identityService)
		}
		if catalogOwns(catalogClosure, "P12") {
			publicationService, err := composePublication(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, faults)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, publicationService)
		}
		if catalogOwns(catalogClosure, "P13") {
			assetService, err := composeAssets(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, faults, config.AssetTokenKeyID, assetKey)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, assetService)
		}
		if catalogOwns(catalogClosure, "P14") {
			registryService, watchService, err := composeRegistry(db, migrate.DialectPostgres, uow.Transaction, uow, clock, ids)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, registryService, watchService)
		}
		if catalogOwns(catalogClosure, "P18") {
			runService, err := composeRun(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, catalogOwns(catalogClosure, "P20"))
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, runService)
		}
		if catalogOwns(catalogClosure, "P19") {
			dispatchService, err := composeDispatch(ctx, db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, config.DispatchIssuer, catalogOwns(catalogClosure, "P20"))
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, dispatchService)
		}
		if catalogOwns(catalogClosure, "P20") {
			eventService, err := composeEvent(db, migrate.DialectPostgres, uow.Transaction, uow, store, clock, ids, config.DispatchIssuer)
			if err != nil {
				_ = cleanup()
				return nil, nil, nil, nil, err
			}
			checks = append(checks, eventService)
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
		if err := identityVerifier(ctx, query); err != nil {
			return err
		}
		if !catalogOwns(catalog, "P12") {
			return nil
		}
		var err error
		if dialect == migrate.DialectSQLite {
			err = publicationsqlite.VerifySchema()(ctx, query)
		} else {
			err = publicationpostgres.VerifySchema()(ctx, query)
		}
		if err != nil || !catalogOwns(catalog, "P13") {
			return err
		}
		if dialect == migrate.DialectSQLite {
			err = assetsqlite.VerifySchema()(ctx, query)
		} else {
			err = assetpostgres.VerifySchema()(ctx, query)
		}
		if err != nil || !catalogOwns(catalog, "P14") {
			return err
		}
		if dialect == migrate.DialectSQLite {
			err = registrysqlite.VerifySchema()(ctx, query)
		} else {
			err = registrypostgres.VerifySchema()(ctx, query)
		}
		if err != nil || !catalogOwns(catalog, "P18") {
			return err
		}
		if dialect == migrate.DialectSQLite {
			err = runsqlite.VerifySchema()(ctx, query)
		} else {
			err = runpostgres.VerifySchema()(ctx, query)
		}
		if err != nil || !catalogOwns(catalog, "P19") {
			return err
		}
		if dialect == migrate.DialectSQLite {
			err = dispatchsqlite.VerifySchema()(ctx, query)
		} else {
			err = dispatchpostgres.VerifySchema()(ctx, query)
		}
		if err != nil || !catalogOwns(catalog, "P20") {
			return err
		}
		if dialect == migrate.DialectSQLite {
			return eventsqlite.VerifySchema()(ctx, query)
		}
		return eventpostgres.VerifySchema()(ctx, query)
	}
}

func composeIdentity(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook) (*identity.Service, error) {
	repository, err := identity.NewCredentialStore(db, dialect, lookup)
	if err != nil {
		return nil, errors.New("initialize credential store")
	}
	service, err := identity.New(identity.Dependencies{
		Clock: clock, IDs: ids, Faults: faults, UoW: uow, Observability: observations, Repository: repository,
		AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"agent:publish", "agent:read", "asset:exchange", "event:session", "registry:discover", "registry:operate", "registry:register", "registry:write", "run:command", "run:create", "run:dispatch", "run:read", "secret.read"}, AllowedTenants: []string{"reference-dev"},
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

type referencePublicationAuthorizer struct{}

func (referencePublicationAuthorizer) Authorize(_ context.Context, request publication.AuthorizationRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	want := "agent:read"
	if request.Operation == publication.OperationPublish {
		want = "agent:publish"
	}
	if !slices.Contains(request.Caller.Scopes, want) {
		return publication.NewError(publication.CategoryAuthorization, publication.ReasonPublicationForbidden)
	}
	return nil
}

func composePublication(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook) (*publication.Service, error) {
	digester := publication.RFC8785ManifestDigester{}
	var repository publication.Repository
	var err error
	if dialect == migrate.DialectSQLite {
		repository, err = publicationsqlite.New(db, lookup, digester)
	} else {
		repository, err = publicationpostgres.New(db, lookup, digester)
	}
	if err != nil {
		return nil, errors.New("initialize publication repository")
	}
	service, err := publication.New(publication.Dependencies{Clock: clock, IDs: ids, Faults: faults, UoW: uow, Observability: observations, Authorizer: referencePublicationAuthorizer{}, Repository: repository, Validator: publication.OfflineBundleValidator{}, ManifestDigester: digester, Fingerprinter: publication.SHA256RequestFingerprinter{}})
	if err != nil {
		return nil, errors.New("initialize publication service")
	}
	return service, nil
}

type denyRunGrantAuthorizer struct{}

func (denyRunGrantAuthorizer) Authorize(_ context.Context, request assets.RunGrantRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	// P13 owns the mandatory authorization seam but not the later run-grant
	// ledger. Production therefore denies until a durable adapter is composed.
	return assets.NewError(assets.CategoryAuthorization, assets.ReasonGrantForbidden)
}

func composeAssets(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, faults platformports.FaultHook, keyID string, key []byte) (*assets.Service, error) {
	var storage assets.StorageRepository
	var err error
	if dialect == migrate.DialectSQLite {
		storage, err = assetsqlite.New(db, lookup)
	} else {
		storage, err = assetpostgres.New(db, lookup)
	}
	if err != nil {
		return nil, errors.New("initialize asset repository")
	}
	repository, err := assets.NewPersistenceAdapter(storage)
	if err != nil {
		return nil, errors.New("initialize asset persistence adapter")
	}
	tokens, err := assets.NewHMACTokenIssuer(keyID, key)
	if err != nil {
		return nil, errors.New("initialize asset token issuer")
	}
	service, err := assets.New(assets.Dependencies{
		Clock: clock, IDs: ids, Faults: faults, UoW: uow, Audit: observations,
		Authorizer: denyRunGrantAuthorizer{}, Assets: repository, Grants: repository,
		Resolver: assets.SystemResolver{}, Tokens: tokens,
	})
	if err != nil {
		return nil, errors.New("initialize asset broker service")
	}
	return service, nil
}

type referenceRegistryAuthorizer struct{}

func (referenceRegistryAuthorizer) Authorize(_ context.Context, request registryapi.AuthorizationRequest) error {
	want := "registry:write"
	switch request.Operation {
	case registryapi.OperationRegister:
		want = "registry:register"
	case registryapi.OperationOperate:
		want = "registry:operate"
	case registryapi.OperationDiscover:
		want = "registry:discover"
	case registryapi.OperationWatch:
		want = "registry:discover"
	case registryapi.OperationKeepalive, registryapi.OperationDrain, registryapi.OperationDeregister:
	default:
		return registryapi.ErrForbidden
	}
	if !request.Caller.HasScope(want) {
		return registryapi.ErrForbidden
	}
	return nil
}

type registryIDSource struct{ source platformports.IDSource }

func (source registryIDSource) NewLeaseID(ctx context.Context) (string, error) {
	return source.source.NewID(ctx, platformports.IDLease)
}

func (source registryIDSource) NewEventID(ctx context.Context) (string, error) {
	return source.source.NewID(ctx, platformports.IDEvent)
}

func composeRegistry(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, clock platformports.Clock, ids platformports.IDSource, sqliteLockPath ...string) (*registryapi.Service, *registrywatch.Service, error) {
	type recoveryRepository interface {
		registry.Repository
		registrywatch.Reader
	}
	var repository recoveryRepository
	var coordinator registrywatch.Coordinator
	var err error
	if dialect == migrate.DialectSQLite {
		repository, err = registrysqlite.New(db, lookup)
		if err != nil {
			return nil, nil, errors.New("initialize registry repository")
		}
		if len(sqliteLockPath) != 1 {
			return nil, nil, errors.New("initialize SQLite registry recovery lock")
		}
		coordinator, err = registrywatch.NewSQLiteCoordinator(db, sqliteLockPath[0])
	} else {
		repository, err = registrypostgres.New(db, lookup)
		if err != nil {
			return nil, nil, errors.New("initialize registry repository")
		}
		coordinator, err = registrywatch.NewPostgresCoordinator(db)
	}
	if err != nil {
		return nil, nil, errors.New("initialize registry recovery coordinator")
	}
	core, err := registry.New(registry.Dependencies{Clock: clock, IDs: registryIDSource{source: ids}, Repository: repository, LeaseTTL: registry.DefaultLeaseTTL, KeepaliveInterval: registry.DefaultKeepaliveInterval})
	if err != nil {
		return nil, nil, errors.New("initialize registry core service")
	}
	hub := registrywatch.NewHub()
	service, err := registryapi.New(registryapi.Dependencies{Core: core, UoW: uow, Clock: clock, Authorizer: referenceRegistryAuthorizer{}, LeaseTTL: registry.DefaultLeaseTTL, KeepaliveInterval: registry.DefaultKeepaliveInterval, RevisionNotifier: hub})
	if err != nil {
		return nil, nil, errors.New("initialize registry API service")
	}
	watchService, err := registrywatch.New(registrywatch.Dependencies{Reader: repository, Authorizer: referenceRegistryAuthorizer{}, Notifier: hub, Coordinator: coordinator})
	if err != nil {
		return nil, nil, errors.New("initialize registry recovery service")
	}
	return service, watchService, nil
}

type referenceRunAuthorizer struct{}

func (referenceRunAuthorizer) Authorize(_ context.Context, caller domainrun.Caller, operation domainrun.Operation, agent domainrun.AgentBinding) (domainrun.AuthorizationSnapshot, error) {
	want := "run:command"
	switch operation {
	case domainrun.OperationCreate:
		want = "run:create"
	case domainrun.OperationRead:
		want = "run:read"
	case domainrun.OperationCommand, domainrun.OperationExpire, domainrun.OperationEffect:
	default:
		return domainrun.AuthorizationSnapshot{}, domainrun.NewError(domainrun.CategoryAuthorization, domainrun.ReasonRunForbidden)
	}
	if !slices.Contains(caller.Scopes, want) {
		return domainrun.AuthorizationSnapshot{}, domainrun.NewError(domainrun.CategoryAuthorization, domainrun.ReasonRunForbidden)
	}
	snapshot := domainrun.AuthorizationSnapshot{TenantID: caller.TenantID, PrincipalID: caller.PrincipalID, CredentialID: caller.CredentialID, Operation: operation, Agent: agent, Scopes: slices.Clone(caller.Scopes), BudgetClass: "standard", RiskClass: "controlled"}
	if _, _, _, err := snapshot.Canonical(); err != nil {
		return domainrun.AuthorizationSnapshot{}, domainrun.NewError(domainrun.CategoryValidation, domainrun.ReasonInvalidRequest)
	}
	return snapshot, nil
}

type runIDSource struct{ source platformports.IDSource }

func (source runIDSource) next(ctx context.Context, prefix string) (string, error) {
	identifier, err := source.source.NewID(ctx, platformports.IDEvent)
	if err != nil || !strings.HasPrefix(identifier, "evt_") {
		return "", errors.New("generate run lifecycle identifier")
	}
	return prefix + strings.TrimPrefix(identifier, "evt_"), nil
}

func (source runIDSource) NewRunID(ctx context.Context) (string, error) {
	return source.next(ctx, "run_")
}

func (source runIDSource) NewOutboxID(ctx context.Context) (string, error) {
	return source.next(ctx, "out_")
}

func composeRun(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, eventResults bool) (*domainrun.Service, error) {
	var repository domainrun.Repository
	var err error
	if dialect == migrate.DialectSQLite {
		repository, err = runsqlite.New(db, lookup)
	} else {
		repository, err = runpostgres.New(db, lookup)
	}
	if err != nil {
		return nil, errors.New("initialize run repository")
	}
	if eventResults {
		enabler, ok := repository.(interface{ EnableEventResults() })
		if !ok {
			return nil, errors.New("run repository cannot expose event results")
		}
		enabler.EnableEventResults()
	}
	service, err := domainrun.New(domainrun.Dependencies{Clock: clock, IDs: runIDSource{source: ids}, UoW: uow, Observability: observations, Authorizer: referenceRunAuthorizer{}, Repository: repository})
	if err != nil {
		return nil, errors.New("initialize run lifecycle service")
	}
	return service, nil
}

type referenceDispatchAuthorizer struct{}

func (referenceDispatchAuthorizer) Authorize(_ context.Context, caller dispatch.Caller, operation dispatch.Operation, agent domainrun.AgentBinding) error {
	if caller.Validate() != nil || agent.Validate() != nil || operation != dispatch.OperationIssue || !slices.Contains(caller.Scopes, "run:dispatch") {
		return dispatch.NewError(dispatch.CategoryAuthorization, dispatch.ReasonDispatchForbidden)
	}
	return nil
}

type dispatchIDSource struct{ source platformports.IDSource }

func (source dispatchIDSource) event(ctx context.Context, prefix string) (string, error) {
	identifier, err := source.source.NewID(ctx, platformports.IDEvent)
	if err != nil || !strings.HasPrefix(identifier, "evt_") {
		return "", errors.New("generate dispatch identifier")
	}
	return prefix + strings.TrimPrefix(identifier, "evt_"), nil
}

func (source dispatchIDSource) NewAttemptID(ctx context.Context) (string, error) {
	return source.event(ctx, "att_")
}

func (source dispatchIDSource) NewDeploymentID(ctx context.Context) (string, error) {
	return source.event(ctx, "dep_")
}

func (source dispatchIDSource) NewTokenID(ctx context.Context) (string, error) {
	return source.event(ctx, "tok_")
}

func (source dispatchIDSource) NewAuditID(ctx context.Context) (string, error) {
	return source.source.NewID(ctx, platformports.IDAudit)
}

func composeDispatch(ctx context.Context, db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, issuer string, eventCapacityReleases bool) (*dispatch.Service, error) {
	var repository dispatch.Repository
	var runs dispatch.RunReader
	var registryRepository registry.Repository
	var err error
	if dialect == migrate.DialectSQLite {
		store, storeErr := dispatchsqlite.New(db, lookup, issuer)
		if storeErr != nil {
			return nil, errors.New("initialize SQLite dispatch repository")
		}
		if eventCapacityReleases {
			store.EnableEventCapacityReleases()
		}
		repository, runs = store, store
		registryRepository, err = registrysqlite.New(db, lookup)
	} else {
		store, storeErr := dispatchpostgres.New(db, lookup, issuer)
		if storeErr != nil {
			return nil, errors.New("initialize PostgreSQL dispatch repository")
		}
		if eventCapacityReleases {
			store.EnableEventCapacityReleases()
		}
		repository, runs = store, store
		registryRepository, err = registrypostgres.New(db, lookup)
	}
	if err != nil {
		return nil, errors.New("initialize dispatch registry reader")
	}
	registryCore, err := registry.New(registry.Dependencies{Clock: clock, IDs: registryIDSource{source: ids}, Repository: registryRepository, LeaseTTL: registry.DefaultLeaseTTL, KeepaliveInterval: registry.DefaultKeepaliveInterval})
	if err != nil {
		return nil, errors.New("initialize dispatch candidate source")
	}
	rawKeyID, err := ids.NewID(ctx, platformports.IDEvent)
	if err != nil || !strings.HasPrefix(rawKeyID, "evt_") {
		return nil, errors.New("generate dispatch signing key identifier")
	}
	const maxTokenTTL = 5 * time.Minute
	signer, err := dispatch.NewProcessSigner(clock.Now(), "key_"+strings.TrimPrefix(rawKeyID, "evt_"), 24*time.Hour, maxTokenTTL)
	if err != nil {
		return nil, errors.New("initialize dispatch ticket signer")
	}
	service, err := dispatch.New(dispatch.Dependencies{
		Clock: clock, IDs: dispatchIDSource{source: ids}, UoW: uow, Observability: observations,
		Runs: runs, Candidates: dispatch.RegistryCandidateSource{Registry: registryCore}, Authorizer: referenceDispatchAuthorizer{}, Repository: repository, Signer: signer,
		Issuer: issuer, TicketTTL: 2 * time.Minute, AttemptLease: 5 * time.Minute, MaxTokenTTL: maxTokenTTL,
	})
	if err != nil {
		return nil, errors.New("initialize dispatch service")
	}
	return service, nil
}

type eventIDSource struct{ source platformports.IDSource }

func (source eventIDSource) NewAuditID(ctx context.Context) (string, error) {
	return source.source.NewID(ctx, platformports.IDAudit)
}

func composeEvent(db *sql.DB, dialect migrate.Dialect, lookup durable.TransactionLookup, uow platformports.UnitOfWork, observations observability.Store, clock platformports.Clock, ids platformports.IDSource, issuer string) (*event.Service, error) {
	var repository event.Repository
	var err error
	if dialect == migrate.DialectSQLite {
		repository, err = eventsqlite.New(db, lookup)
	} else {
		repository, err = eventpostgres.New(db, lookup)
	}
	if err != nil {
		return nil, errors.New("initialize event ledger repository")
	}
	service, err := event.New(event.Dependencies{Clock: clock, IDs: eventIDSource{source: ids}, Tokens: event.SecureTokenSource{}, UoW: uow, Observability: observations, Repository: repository, ControlPlaneBaseURL: issuer, SessionTTL: 2 * time.Minute, AttemptLeaseTTL: 5 * time.Minute})
	if err != nil {
		return nil, errors.New("initialize event ledger service")
	}
	return service, nil
}

func identityServiceFromChecks(checks []platformports.ReadinessCheck) *identity.Service {
	for _, check := range checks {
		if service, ok := check.(*identity.Service); ok {
			return service
		}
	}
	return nil
}

func publicationServiceFromChecks(checks []platformports.ReadinessCheck) *publication.Service {
	for _, check := range checks {
		if service, ok := check.(*publication.Service); ok {
			return service
		}
	}
	return nil
}

func assetServiceFromChecks(checks []platformports.ReadinessCheck) *assets.Service {
	for _, check := range checks {
		if service, ok := check.(*assets.Service); ok {
			return service
		}
	}
	return nil
}

func registryAPIServiceFromChecks(checks []platformports.ReadinessCheck) *registryapi.Service {
	for _, check := range checks {
		if service, ok := check.(*registryapi.Service); ok {
			return service
		}
	}
	return nil
}

func registryWatchServiceFromChecks(checks []platformports.ReadinessCheck) *registrywatch.Service {
	for _, check := range checks {
		if service, ok := check.(*registrywatch.Service); ok {
			return service
		}
	}
	return nil
}

func runServiceFromChecks(checks []platformports.ReadinessCheck) *domainrun.Service {
	for _, check := range checks {
		if service, ok := check.(*domainrun.Service); ok {
			return service
		}
	}
	return nil
}

func dispatchServiceFromChecks(checks []platformports.ReadinessCheck) *dispatch.Service {
	for _, check := range checks {
		if service, ok := check.(*dispatch.Service); ok {
			return service
		}
	}
	return nil
}

func eventServiceFromChecks(checks []platformports.ReadinessCheck) *event.Service {
	for _, check := range checks {
		if service, ok := check.(*event.Service); ok {
			return service
		}
	}
	return nil
}

func catalogOwns(catalog migrate.CatalogClosure, phase string) bool {
	return slices.Contains(catalog.OwnerPhases, phase)
}

func assetTokenMaterial(config platform.Config, catalog migrate.CatalogClosure) ([]byte, error) {
	if !catalogOwns(catalog, "P13") || config.Mode == platform.ModeDevelopmentMemory {
		return nil, nil
	}
	if config.AssetTokenKeyFile == "" || config.AssetTokenKeyID == "" {
		return nil, errors.New("asset token key configuration is required")
	}
	key, err := readPrivateAssetTokenKey(config.AssetTokenKeyFile)
	if err != nil {
		return nil, errors.New("load asset token key")
	}
	return key, nil
}

func readPrivateAssetTokenKey(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("asset token key path is invalid")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		if component == "" {
			return nil, errors.New("asset token key path is invalid")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("asset token key path is not private")
		}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 32 {
		return nil, errors.New("asset token key file is not private")
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, errors.New("asset token key file is invalid")
	}
	return key, nil
}

func referenceAuthenticate(service *identity.Service, ids platformports.IDSource) httpadapter.AuthenticateFunc {
	return func(ctx context.Context, credential string, metadata platform.RequestMetadata) (httpadapter.AuthenticatedPrincipal, error) {
		spanID, err := ids.NewID(ctx, platformports.IDSpan)
		if err != nil {
			return httpadapter.AuthenticatedPrincipal{}, httpadapter.ErrAuthenticationUnavailable
		}
		metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, spanID
		requiredScopes := httpadapter.RequiredScopesFromContext(ctx)
		if len(requiredScopes) == 0 {
			requiredScopes = []string{"secret.read"}
		}
		principal, err := service.Authenticate(ctx, identity.AuthenticateRequest{
			Credential: credential,
			TenantID:   "reference-dev",
			Audience:   "reference-control-plane",
			Scopes:     requiredScopes,
			Metadata:   metadata,
		})
		if err != nil {
			if errors.Is(err, identity.ErrUnavailable) {
				return httpadapter.AuthenticatedPrincipal{}, httpadapter.ErrAuthenticationUnavailable
			}
			return httpadapter.AuthenticatedPrincipal{}, errors.New("credential authentication failed")
		}
		return httpadapter.AuthenticatedPrincipal{
			TenantID: principal.TenantID, PrincipalID: principal.PrincipalID,
			SubjectID: principal.SubjectID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes),
		}, nil
	}
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
