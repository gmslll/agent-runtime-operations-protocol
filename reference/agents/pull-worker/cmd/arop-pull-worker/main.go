package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pullworker "github.com/gmslll/agent-runtime-operations-protocol/reference/agents/pull-worker"
	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
	workersdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/worker"
)

type options struct {
	controlPlane, workerID, sessionID, tokenFile       string
	agentID, agentVersion, skillID, manifestDigest     string
	generation, concurrency, leaseSeconds, waitSeconds uint64
	drainTimeout                                       time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "arop pull worker stopped:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	config, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	if config.sessionID == "" {
		config.sessionID, err = workersdk.NewSessionID(time.Now().UTC())
		if err != nil {
			return err
		}
	}
	client, err := workersdk.NewClient(workersdk.ClientConfig{
		BaseURL:    config.controlPlane,
		HTTPClient: &http.Client{Transport: http.DefaultTransport},
		Credential: pullworker.FileCredentialSource{Path: config.tokenFile},
	})
	if err != nil {
		return err
	}
	runner, err := workersdk.NewRunner(workersdk.RunnerConfig{
		API:          client,
		Handler:      pullworker.EchoHandler{},
		WorkerID:     config.workerID,
		SessionID:    config.sessionID,
		Generation:   config.generation,
		Concurrency:  int(config.concurrency),
		LeaseSeconds: config.leaseSeconds,
		WaitSeconds:  config.waitSeconds,
		SupportedBindings: []workerwire.AgentBinding{{
			ID:             workerwire.AgentId(config.agentID),
			Version:        workerwire.SemanticVersion(config.agentVersion),
			SkillID:        workerwire.SkillId(config.skillID),
			ManifestDigest: workerwire.Sha256Digest(config.manifestDigest),
		}},
	})
	if err != nil {
		return err
	}
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(runContext) }()
	select {
	case err := <-runErr:
		return err
	case <-signalContext.Done():
	}
	drainContext, cancel := context.WithTimeout(context.Background(), config.drainTimeout)
	defer cancel()
	drainErr := runner.Drain(drainContext)
	if drainErr != nil {
		cancelRun()
		return errors.New("worker drain deadline exceeded")
	}
	workerErr := <-runErr
	if workerErr != nil && !errors.Is(workerErr, context.Canceled) {
		return workerErr
	}
	return nil
}

func parseOptions(arguments []string) (options, error) {
	var config options
	flags := flag.NewFlagSet("arop-pull-worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&config.controlPlane, "control-plane", "", "AROP Control Plane HTTPS base URL")
	flags.StringVar(&config.workerID, "worker-id", "", "stable worker id")
	flags.StringVar(&config.sessionID, "session-id", "", "optional UUIDv7 worker session id")
	flags.StringVar(&config.tokenFile, "token-file", "", "private worker bearer token file")
	flags.StringVar(&config.agentID, "agent-id", "", "supported Agent id")
	flags.StringVar(&config.agentVersion, "agent-version", "", "supported Agent version")
	flags.StringVar(&config.skillID, "skill-id", "default", "supported skill id")
	flags.StringVar(&config.manifestDigest, "manifest-digest", "", "published Agent Manifest digest")
	flags.Uint64Var(&config.generation, "generation", 1, "worker session generation")
	flags.Uint64Var(&config.concurrency, "concurrency", 1, "maximum concurrent claims")
	flags.Uint64Var(&config.leaseSeconds, "lease-seconds", 60, "claim lease seconds")
	flags.Uint64Var(&config.waitSeconds, "wait-seconds", 20, "long-poll wait seconds")
	flags.DurationVar(&config.drainTimeout, "drain-timeout", 30*time.Second, "graceful drain timeout")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return options{}, errors.New("invalid worker arguments")
	}
	if config.controlPlane == "" || config.workerID == "" || config.tokenFile == "" || config.agentID == "" || config.agentVersion == "" || config.manifestDigest == "" || config.generation == 0 || config.generation > 9007199254740991 || config.concurrency == 0 || config.concurrency > 1024 || config.leaseSeconds < 15 || config.leaseSeconds > 300 || config.waitSeconds > 30 || config.drainTimeout < time.Second || config.drainTimeout > 10*time.Minute {
		return options{}, errors.New("incomplete or invalid worker configuration")
	}
	return config, nil
}
