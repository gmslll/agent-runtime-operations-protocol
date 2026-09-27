package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseOptions(t *testing.T) {
	arguments := []string{
		"-control-plane", "https://control.example.test",
		"-worker-id", "worker-a",
		"-token-file", "/private/token",
		"-agent-id", "image.generate",
		"-agent-version", "1.0.0",
		"-manifest-digest", "sha256:" + strings.Repeat("a", 64),
		"-concurrency", "4",
		"-drain-timeout", "45s",
	}
	config, err := parseOptions(arguments)
	if err != nil || config.concurrency != 4 || config.drainTimeout != 45*time.Second || config.waitSeconds != 20 {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, invalid := range [][]string{
		{},
		append(append([]string(nil), arguments...), "secret-positional-value"),
		{"-control-plane", "http://control.example.test"},
	} {
		if _, err := parseOptions(invalid); err == nil {
			t.Fatalf("invalid arguments accepted: %v", invalid)
		}
	}
}
