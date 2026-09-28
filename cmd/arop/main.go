package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	development "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/development"
	exporta2a "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-a2a"
	exportard "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/export-ard"
	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/publish"
	registercommand "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/register"
	testcommand "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/test"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
	registrysdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	return runContext(context.Background(), arguments)
}

func runContext(ctx context.Context, arguments []string) error {
	if len(arguments) >= 1 && (arguments[0] == "init" || arguments[0] == "dev" || arguments[0] == "doctor") {
		return (development.Command{Stdout: os.Stdout, Stderr: os.Stderr}).Execute(ctx, arguments)
	}
	if len(arguments) >= 1 && arguments[0] == "test" {
		return (testcommand.Command{Stdout: os.Stdout, Stderr: os.Stderr}).Execute(context.Background(), arguments[1:])
	}
	if len(arguments) == 5 && arguments[0] == "export" && arguments[1] == "ard" {
		return (exportard.Command{Stdout: os.Stdout}).Execute(context.Background(), exportard.Options{PackageRoot: ".", ManifestPath: arguments[2], Publisher: arguments[3], AgentCardURL: arguments[4]})
	}
	if len(arguments) == 4 && arguments[0] == "export" && arguments[1] == "a2a" {
		return (exporta2a.Command{Stdout: os.Stdout}).Execute(context.Background(), exporta2a.Options{PackageRoot: ".", ManifestPath: arguments[2], InterfaceURL: arguments[3]})
	}
	if len(arguments) == 3 && arguments[0] == "register" {
		baseURL, err := url.Parse(os.Getenv("AROP_CONTROL_PLANE_URL"))
		if err != nil {
			return fmt.Errorf("invalid AROP_CONTROL_PLANE_URL")
		}
		credential := os.Getenv("AROP_CONTROL_PLANE_TOKEN")
		client := registrysdk.Client{BaseURL: baseURL, HTTPClient: &http.Client{Timeout: 30 * time.Second}, Credential: registrysdk.CredentialSourceFunc(func(context.Context) (string, error) {
			if credential == "" {
				return "", fmt.Errorf("registry credential unavailable")
			}
			return credential, nil
		})}
		return (registercommand.Command{Client: client, Stdout: os.Stdout}).Execute(context.Background(), registercommand.Options{InstanceID: arguments[1], ConfigPath: arguments[2], IdempotencyKey: os.Getenv("AROP_IDEMPOTENCY_KEY")})
	}
	if len(arguments) == 3 && arguments[0] == "publish" {
		baseURL, err := url.Parse(os.Getenv("AROP_CONTROL_PLANE_URL"))
		if err != nil {
			return fmt.Errorf("invalid AROP_CONTROL_PLANE_URL")
		}
		credential := os.Getenv("AROP_CONTROL_PLANE_TOKEN")
		command := publish.Command{BaseURL: baseURL, Client: &http.Client{Timeout: 30 * time.Second}, Credential: publish.CredentialSourceFunc(func(context.Context) (string, error) {
			if credential == "" {
				return "", fmt.Errorf("publication credential unavailable")
			}
			return credential, nil
		}), Stdout: os.Stdout}
		return command.Execute(context.Background(), publish.Options{AgentID: arguments[1], BundlePath: arguments[2], IdempotencyKey: os.Getenv("AROP_IDEMPOTENCY_KEY")})
	}
	if len(arguments) == 2 && arguments[0] == "manifest" && arguments[1] == "digest-env" {
		manifestPath := os.Getenv("AROP_MANIFEST_FILE")
		if manifestPath == "" {
			return fmt.Errorf("AROP_MANIFEST_FILE must name a package-relative Manifest")
		}
		digest, err := manifest.DigestPackageFile(".", manifestPath)
		if err != nil {
			return fmt.Errorf("digest manifest: %w", err)
		}
		fmt.Println(digest)
		return nil
	}
	if len(arguments) != 3 || arguments[0] != "manifest" || arguments[1] != "digest" {
		return fmt.Errorf("usage: arop manifest digest <manifest.yaml|manifest.json> | arop init|dev|doctor [workspace] | arop publish <agent-id> <bundle.zip> | arop register <instance-id> <config.json> | arop export a2a <manifest> <https-interface-url> | arop export ard <manifest> <publisher-domain> <https-agent-card-url> | arop test --profile <id> --target <executable> [options]")
	}

	digest, err := manifest.DigestFile(arguments[2])
	if err != nil {
		return fmt.Errorf("digest manifest: %w", err)
	}

	fmt.Println(digest)
	return nil
}
