package testcommand

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommandDelegatesToPortableRunnerCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (Command{Stdout: &stdout, Stderr: &stderr}).Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "--profile <id> --target <executable>") || stdout.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
}
