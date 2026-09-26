package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/publish"
	registercommand "github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/register"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
	registrysdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/registry"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
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
		return fmt.Errorf("usage: arop manifest digest <manifest.yaml|manifest.json> | arop publish <agent-id> <bundle.zip> | arop register <instance-id> <config.json>")
	}

	digest, err := manifest.DigestFile(arguments[2])
	if err != nil {
		return fmt.Errorf("digest manifest: %w", err)
	}

	fmt.Println(digest)
	return nil
}
