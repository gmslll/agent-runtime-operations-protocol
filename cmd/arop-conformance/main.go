package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
)

func main() {
	if err := runner.ExecuteCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
