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
		return fmt.Errorf("usage: arop manifest digest <manifest.yaml|manifest.json>")
	}

	digest, err := manifest.DigestFile(arguments[2])
	if err != nil {
		return fmt.Errorf("digest manifest: %w", err)
	}

	fmt.Println(digest)
	return nil
}
