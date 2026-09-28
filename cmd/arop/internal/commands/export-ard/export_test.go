package exportard

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard"
)

func TestCommandExportsDeterministicPublicARDEntryAndLossReport(t *testing.T) {
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
	options := Options{PackageRoot: root, ManifestPath: "agent.json", Publisher: "example.com", AgentCardURL: "https://agents.example.com/card.json"}
	var output, loss bytes.Buffer
	command := Command{Stdout: &output, Report: &loss}
	if err := command.Execute(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	entry, _, err := ard.DecodeEntry(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Identifier != "urn:air:example.com:arop:publication.example" || !strings.Contains(loss.String(), "runtime_instances") {
		t.Fatalf("entry=%+v loss=%s", entry, loss.String())
	}
	var second bytes.Buffer
	command.Stdout, command.Report = &second, nil
	if err := command.Execute(context.Background(), options); err != nil || !bytes.Equal(output.Bytes(), second.Bytes()) {
		t.Fatalf("deterministic=%v err=%v", bytes.Equal(output.Bytes(), second.Bytes()), err)
	}
	var value map[string]any
	if err := json.Unmarshal(output.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRejectsUnsafePathPublisherURLAndCancellation(t *testing.T) {
	command := Command{Stdout: &bytes.Buffer{}}
	for _, options := range []Options{{ManifestPath: "../agent.json", Publisher: "example.com", AgentCardURL: "https://example.com/card"}, {ManifestPath: "agent.json", Publisher: "bad publisher", AgentCardURL: "https://example.com/card"}, {ManifestPath: "agent.json", Publisher: "example.com", AgentCardURL: "http://127.0.0.1/card"}} {
		if err := command.Execute(context.Background(), options); err == nil {
			t.Fatalf("invalid options accepted: %+v", options)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := command.Execute(ctx, Options{}); err == nil {
		t.Fatal("cancelled export accepted")
	}
}
