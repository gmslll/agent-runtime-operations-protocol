package main

import (
	"fmt"
	"os"

	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) != 3 || arguments[0] != "manifest" || arguments[1] != "digest" {
		return fmt.Errorf("usage: arop manifest digest <manifest.yaml|manifest.json>")
	}

	document, err := os.ReadFile(arguments[2])
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	digest, err := manifest.Digest(document)
	if err != nil {
		return fmt.Errorf("digest manifest: %w", err)
	}

	fmt.Println(digest)
	return nil
}
