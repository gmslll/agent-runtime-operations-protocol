package ard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

func TestExportPublishesOnlyPublicCatalogMetadata(t *testing.T) {
	manifest := fixtureManifest(t)
	entry, report, err := Export(manifest, "sha256:"+strings.Repeat("a", 64), "example.com", "https://agents.example.com/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Identifier != "urn:air:example.com:arop:publication.example" || entry.Type != AgentCardMediaType || len(entry.RepresentativeQueries) < 2 || entry.Version != "1.0.0" {
		t.Fatalf("entry=%+v", entry)
	}
	wire, err := EncodeEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"runtime_instances", "runtime_endpoint", "lease", "deployment_credentials", "authorization_snapshot"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("entry leaked %s", forbidden)
		}
	}
	if report.Overall != "unsupported" || len(report.NotExported) != 9 {
		t.Fatalf("report=%+v", report)
	}
}

func TestRoundTripProducesReviewOnlyCandidateNotRuntimeReadiness(t *testing.T) {
	entry, _, err := Export(fixtureManifest(t), "sha256:"+strings.Repeat("b", 64), "example.com", "https://agents.example.com/card.json")
	if err != nil {
		t.Fatal(err)
	}
	entry.Extensions = map[string]json.RawMessage{"example:ranking": json.RawMessage(`{"score":0.9}`)}
	wire, err := EncodeEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	candidate, disposition, err := Import(wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != CandidateNew || candidate.RuntimeReady || !candidate.RequiresReview || candidate.TrustVerified || candidate.AgentID != "publication.example" || candidate.Extensions["example:ranking"] == nil {
		t.Fatalf("candidate=%+v disposition=%s", candidate, disposition)
	}
	same, disposition, err := Import(wire, &candidate)
	if err != nil || disposition != CandidateUnchanged || same.SourceDigest != candidate.SourceDigest {
		t.Fatalf("same=%+v disposition=%s err=%v", same, disposition, err)
	}
	var reorderedObject map[string]any
	if err := json.Unmarshal(wire, &reorderedObject); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(reorderedObject)
	if err != nil {
		t.Fatal(err)
	}
	_, reorderedDigest, err := DecodeEntry(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedDigest != candidate.SourceDigest {
		t.Fatalf("semantic digest changed after JSON member reordering: %s != %s", reorderedDigest, candidate.SourceDigest)
	}
}

func TestImportRejectsImmutableVersionConflict(t *testing.T) {
	entry, _, err := Export(fixtureManifest(t), "sha256:"+strings.Repeat("c", 64), "example.com", "https://agents.example.com/card.json")
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := EncodeEntry(entry)
	candidate, _, err := Import(wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry.Description = "changed without version bump"
	changed, _ := EncodeEntry(entry)
	if _, _, err := Import(changed, &candidate); err == nil {
		t.Fatal("version conflict accepted")
	}
}

func TestStrictEntryPreservesNamespacedUnknownAndRejectsMalformed(t *testing.T) {
	valid := `{"@context":"` + BaseContext + `","identifier":"urn:air:example.com:arop:demo","displayName":"Demo","type":"application/a2a-agent-card+json","url":"https://example.com/card.json","version":"1.0.0","example:field":{"x":1}}`
	entry, _, err := DecodeEntry([]byte(valid))
	if err != nil || entry.Extensions["example:field"] == nil {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	for _, invalid := range []string{
		`{"identifier":"urn:air:example.com:arop:demo","identifier":"urn:air:evil.example:arop:demo","displayName":"Demo","type":"application/a2a-agent-card+json","url":"https://example.com/card.json"}`,
		valid + ` null`,
		`{"identifier":"urn:air:example.com:arop:demo","displayName":"Demo","type":"application/a2a-agent-card+json","url":"https://example.com/card.json","data":{}}`,
		`{"identifier":"urn:air:example.com:arop:demo","displayName":"Demo","type":"application/a2a-agent-card+json","url":"http://127.0.0.1/card.json"}`,
		`{"identifier":"urn:air:example.com:arop:demo","displayName":"Demo","type":"application/a2a-agent-card+json","url":"https://127.0.0.1/card.json"}`,
		`{"identifier":"urn:air:example.com:arop:demo","displayName":"Demo","displayNmae":"typo","type":"application/a2a-agent-card+json","url":"https://example.com/card.json"}`,
	} {
		if _, _, err := DecodeEntry([]byte(invalid)); err == nil {
			t.Fatalf("invalid entry accepted: %s", invalid)
		}
	}
}

func TestPinnedARDV091FixtureImportsWithoutTrustUpgrade(t *testing.T) {
	wire, err := os.ReadFile("testdata/upstream-v0.91-entry.json")
	if err != nil {
		t.Fatal(err)
	}
	candidate, disposition, err := Import(wire, nil)
	if err != nil || disposition != CandidateNew || candidate.TrustVerified || candidate.RuntimeReady {
		t.Fatalf("candidate=%+v disposition=%s err=%v", candidate, disposition, err)
	}
	compatibility, err := os.ReadFile("testdata/compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compatibility), SchemaCommit) || !strings.Contains(string(compatibility), SchemaSHA256) {
		t.Fatal("ARD schema pin mismatch")
	}
}

func fixtureManifest(t *testing.T) controlplane.AgentManifest {
	t.Helper()
	wire, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifests", "publication-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := controlplane.DecodeAgentManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
