package exporta2a

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a"
)

func TestCommandExportsDeterministicCardAndSeparateLossReport(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "examples", "manifests", "publication-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.json"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	var output, report bytes.Buffer
	command := Command{Stdout: &output, Report: &report}
	options := Options{PackageRoot: root, ManifestPath: "agent.json", InterfaceURL: "https://agents.example.invalid/a2a/demo"}
	if err := command.Execute(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(output.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "Publication Example" || card.SupportedInterfaces[0].URL != options.InterfaceURL {
		t.Fatalf("card=%+v", card)
	}
	if strings.Contains(output.String(), "run_token") || !strings.Contains(report.String(), "unmapped_security_semantics") {
		t.Fatal("security loss handling missing")
	}
	var second bytes.Buffer
	command.Stdout = &second
	command.Report = nil
	if err := command.Execute(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), second.Bytes()) {
		t.Fatal("export is not deterministic")
	}
}

func TestCommandRejectsUnsafePathEndpointAndCancellation(t *testing.T) {
	t.Parallel()
	command := Command{Stdout: &bytes.Buffer{}}
	for name, options := range map[string]Options{"escape": {ManifestPath: "../agent.json", InterfaceURL: "https://example.invalid"}, "endpoint": {ManifestPath: "agent.json", InterfaceURL: "http://example.invalid"}} {
		t.Run(name, func(t *testing.T) {
			if err := command.Execute(context.Background(), options); err == nil {
				t.Fatal("invalid export accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := command.Execute(ctx, Options{}); err == nil {
		t.Fatal("cancelled export accepted")
	}
}
