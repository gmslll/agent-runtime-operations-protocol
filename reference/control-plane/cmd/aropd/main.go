package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/httpadapter"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:], os.Environ()); err != nil {
		slog.Error("reference control plane stopped", "error", err)
		os.Exit(1)
	}
}

func compose(args, environment []string) (*platform.Platform, *http.Server, error) {
	config, err := platform.ParseConfig(args, environment)
	if err != nil {
		return nil, nil, err
	}
	store, err := memory.New(config.AuditCapacity, config.TraceCapacity)
	if err != nil {
		return nil, nil, errors.New("initialize ephemeral observability store")
	}
	clock := platform.RealClock{}
	application, err := platform.New(config, platform.Dependencies{
		Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{},
		UoW: &platform.SerialUnitOfWork{}, Observability: store,
	}, "arop-reference-control-plane", version)
	if err != nil {
		return nil, nil, errors.New("initialize Control Plane platform")
	}
	handler, err := httpadapter.NewHandler(application)
	if err != nil {
		return nil, nil, errors.New("assemble Control Plane HTTP handler")
	}
	return application, httpadapter.NewServer(application, handler), nil
}

func run(args, environment []string) error {
	application, httpServer, err := compose(args, environment)
	if err != nil {
		return err
	}
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("reference control plane listening", "address", httpServer.Addr, "durability", platform.DurabilityMode)
	return serve(shutdownContext, application, httpServer, application.Config().ShutdownTimeout)
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
