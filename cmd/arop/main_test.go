package main

import (
	"strings"
	"testing"
)

func TestRunRequiresManifestDigestCommand(t *testing.T) {
	t.Parallel()

	err := run([]string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "usage: arop manifest digest") {
		t.Fatalf("error = %v, want usage error", err)
	}
}
