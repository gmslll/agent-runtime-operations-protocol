package testcommand

import (
	"context"
	"io"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
)

type Command struct {
	Stdout io.Writer
	Stderr io.Writer
}

func (command Command) Execute(ctx context.Context, arguments []string) error {
	return runner.ExecuteCLI(ctx, arguments, command.Stdout, command.Stderr)
}
