package publication

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineBundleValidatorAcceptsContractFixture(t *testing.T) {
	t.Parallel()
	archive, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "conformance", "fixtures", "contracts", "control-plane-publication", "fixtures", "bundles", "valid-agent-version.zip"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readSecureArchive(archive)
	if err != nil {
		t.Fatalf("read secure archive: %v", err)
	}
	canonical, _, err := canonicalManifest(entries[publicationManifestEntryName])
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	if got := digestBytes(canonical); got == "" {
		t.Fatal("empty canonical digest")
	}
	rootDigest, err := (RFC8785ManifestDigester{}).DigestManifest(context.Background(), canonical)
	if err != nil {
		t.Fatalf("digest canonical manifest: %v", err)
	}
	if rootDigest != digestBytes(canonical) {
		t.Fatalf("canonical digest mismatch got=%s want=%s document=%s", digestBytes(canonical), rootDigest, canonical)
	}
	bundle, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive)
	if err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	if bundle.AgentID == "" || bundle.Version == "" || !digestPattern.MatchString(bundle.ManifestDigest) || !digestPattern.MatchString(bundle.BundleSemanticDigest) {
		t.Fatalf("incomplete validated bundle: %+v", bundle)
	}
	if bytes.Contains(bundle.CanonicalManifest, []byte("\n")) {
		t.Fatal("canonical manifest retained whitespace")
	}
}

func TestOfflineBundleValidatorRejectsEndpointNetworkRefsAndUnsafeArchives(t *testing.T) {
	t.Parallel()
	manifest := minimalManifest(`,"endpoint":"https://runtime.example.invalid"`)
	for name, archive := range map[string][]byte{
		"endpoint":          zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(manifest)}),
		"network-reference": zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(minimalManifestWithRef("https://example.invalid/schema.json"))}),
		"parent-entry":      zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(minimalManifest("")), "../secret": []byte("sentinel")}),
		"missing-manifest":  zipBundle(t, map[string][]byte{"schema.json": []byte(`{}`)}),
	} {
		name, archive := name, archive
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
}

func TestBundleSemanticDigestIgnoresZipOrderAndMetadata(t *testing.T) {
	t.Parallel()
	entries := map[string][]byte{"agent-manifest.json": []byte(minimalManifestWithRef("./schemas/input.json")), "schemas/input.json": []byte(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"boolean"}}}`)}
	one := zipBundleInOrder(t, entries, []string{"agent-manifest.json", "schemas/input.json"}, zip.Store)
	formatted := map[string][]byte{"agent-manifest.json": entries["agent-manifest.json"], "schemas/input.json": []byte("{\n  \"properties\": {\"a\": {\"type\": \"boolean\"}, \"b\": {\"type\": \"string\"}},\n  \"type\": \"object\"\n}")}
	two := zipBundleInOrder(t, formatted, []string{"schemas/input.json", "agent-manifest.json"}, zip.Deflate)
	validator := OfflineBundleValidator{}
	first, err := validator.ValidateBundle(context.Background(), one)
	if err != nil {
		t.Fatal(err)
	}
	second, err := validator.ValidateBundle(context.Background(), two)
	if err != nil {
		t.Fatal(err)
	}
	if first.BundleSemanticDigest != second.BundleSemanticDigest {
		t.Fatalf("semantic digest changed across packaging: %s != %s", first.BundleSemanticDigest, second.BundleSemanticDigest)
	}
}

func TestCanonicalManifestUsesRFC8785OrderingNumbersAndStrings(t *testing.T) {
	t.Parallel()
	document := []byte(`{"z":1.0,"a":"<\u2028","signature":"excluded"}`)
	canonical, _, err := canonicalManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(canonical), `{"a":"< ","z":1}`; got != want {
		t.Fatalf("canonical=%q want=%q", got, want)
	}
	digest, err := (RFC8785ManifestDigester{}).DigestManifest(context.Background(), document)
	if err != nil || digest != digestBytes(canonical) {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
}

func zipBundle(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	return zipBundleInOrder(t, entries, names, zip.Deflate)
}

func zipBundleInOrder(t *testing.T, entries map[string][]byte, names []string, method uint16) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: method}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func minimalManifest(extra string) string {
	return `{"protocol":"arop/v1","kind":"AgentManifest","identity":{"id":"example.agent","version":"1.0.0","name":"Example","summary":"Example","owner":{"team":"example-team"}},"skills":[{"id":"default","name":"Default","invoke_modes":["params"],"input_schema":{"type":"object"},"output_schema":{"type":"object"}}],"execution":{"default_timeout_seconds":1,"max_timeout_seconds":2,"effects":{"level":"none","idempotency":"supported","human_confirmation":false},"capabilities":{}}` + extra + `}`
}

func minimalManifestWithRef(reference string) string {
	return strings.Replace(minimalManifest(""), `"input_schema":{"type":"object"}`, `"input_schema":{"$ref":"`+reference+`"}`, 1)
}
