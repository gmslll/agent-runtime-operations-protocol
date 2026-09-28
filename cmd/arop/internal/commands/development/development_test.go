package development

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitIsDeterministicIdempotentAndSecretFree(t *testing.T) {
	root := t.TempDir()
	first := bytes.Buffer{}
	command := Command{Stdout: &first}
	if err := command.Execute(context.Background(), []string{"init", root}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, filepath.FromSlash(configRelative))
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Execute(context.Background(), []string{"init", root}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(configPath)
	if !bytes.Equal(before, after) || strings.Contains(strings.ToLower(string(before)), "secret") || strings.Contains(strings.ToLower(string(before)), "password") || strings.Contains(strings.ToLower(string(before)), "token") {
		t.Fatalf("init output is not deterministic and secret-free: %s", before)
	}
}

func TestDoctorJSONAndUnsafeConfigFailClosed(t *testing.T) {
	root := t.TempDir()
	if err := (Command{}).Execute(context.Background(), []string{"init", root}); err != nil {
		t.Fatal(err)
	}
	output := bytes.Buffer{}
	if err := (Command{Stdout: &output}).Execute(context.Background(), []string{"doctor", root}); err != nil {
		t.Fatal(err)
	}
	var report DoctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || !report.Healthy || len(report.Checks) != 5 {
		t.Fatalf("doctor report=%+v err=%v", report, err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(configRelative)), []byte(`{"schema_version":1,"listen":"0.0.0.0:8080","database":"../escape","backup_directory":".arop/backups","sample_manifest":"agent-manifest.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := (Command{Stdout: &output}).Execute(context.Background(), []string{"doctor", root}); err == nil || !strings.Contains(err.Error(), "failures") {
		t.Fatalf("unsafe doctor result err=%v output=%s", err, output.String())
	}
}

func TestDevRequiresHealthyWorkspaceAndExplicitBinaries(t *testing.T) {
	root := t.TempDir()
	command := Command{LookupEnv: func(string) string { return "" }}
	if err := command.Execute(context.Background(), []string{"dev", root}); err == nil || !strings.Contains(err.Error(), "doctor") {
		t.Fatalf("uninitialized dev error=%v", err)
	}
	if err := command.Execute(context.Background(), []string{"init", root}); err != nil {
		t.Fatal(err)
	}
	if err := command.Execute(context.Background(), []string{"dev", root}); err == nil || !strings.Contains(err.Error(), "AROP_CONTROL_PLANE_BINARY") {
		t.Fatalf("missing binary error=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := command.Execute(cancelled, []string{"dev", root}); err == nil {
		t.Fatal("cancelled dev succeeded")
	}
}
