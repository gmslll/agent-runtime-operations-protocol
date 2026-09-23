package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/server"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		slog.Error("reference control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listenAddress := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	flag.Parse()

	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           server.NewHandler(version),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("reference control plane listening", "address", *listenAddress)
		serverErrors <- httpServer.ListenAndServe()
	}()

	shutdownContext, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-shutdownContext.Done():
	}

	gracefulContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(gracefulContext); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP during shutdown: %w", err)
	}
	return nil
}
