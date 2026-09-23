package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestMatchesGoldenFixtures(t *testing.T) {
	t.Parallel()

	repositoryRoot := filepath.Join("..", "..", "..", "..")
	digestDocument, err := os.ReadFile(filepath.Join(
		repositoryRoot,
		"examples",
		"manifests",
		"digests.json",
	))
	if err != nil {
		t.Fatalf("read digest fixtures: %v", err)
	}

	var fixtures map[string]string
	if err := json.Unmarshal(digestDocument, &fixtures); err != nil {
		t.Fatalf("parse digest fixtures: %v", err)
	}

	for manifestPath, expectedDigest := range fixtures {
		manifestPath := manifestPath
		expectedDigest := expectedDigest
		t.Run(filepath.Base(manifestPath), func(t *testing.T) {
			t.Parallel()

			actualDigest, err := DigestFile(filepath.Join(repositoryRoot, manifestPath))
			if err != nil {
				t.Fatalf("digest manifest: %v", err)
			}
			if actualDigest != expectedDigest {
				t.Fatalf("digest = %q, want %q", actualDigest, expectedDigest)
			}
		})
	}
}

func TestDigestExcludesPublicationMetadata(t *testing.T) {
	t.Parallel()

	base := []byte(`protocol: arop/v1
kind: AgentManifest
identity:
  id: hello.agent
  version: 1.0.0
  name: Hello
  summary: Hello
  owner: {team: example-team}
skills:
  - id: default
    name: Greet
    invoke_modes: [params]
    input_schema: {type: object}
    output_schema: {type: object}
execution:
  default_timeout_seconds: 30
  max_timeout_seconds: 60
  effects: {level: none, idempotency: supported, human_confirmation: false}
  capabilities: {}
`)
	withMetadata := append(append([]byte(nil), base...), []byte("manifest_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsignature:\n  algorithm: example\n")...)

	baseDigest, err := Digest(base)
	if err != nil {
		t.Fatalf("digest base manifest: %v", err)
	}
	metadataDigest, err := Digest(withMetadata)
	if err != nil {
		t.Fatalf("digest manifest with metadata: %v", err)
	}
	if metadataDigest != baseDigest {
		t.Fatalf("metadata digest = %q, want %q", metadataDigest, baseDigest)
	}
}

func TestDigestRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	_, err := Digest([]byte("kind: AgentManifest\nkind: Other\n"))
	if err == nil {
		t.Fatal("Digest accepted duplicate YAML keys")
	}
}

func TestDigestRejectsMultipleDocuments(t *testing.T) {
	t.Parallel()

	_, err := Digest([]byte("kind: AgentManifest\n---\nkind: Other\n"))
	if err == nil {
		t.Fatal("Digest accepted multiple YAML documents")
	}
}

func TestJSONCompatibleYAMLProfile(t *testing.T) {
	t.Parallel()

	value, err := decodeStrictYAML([]byte("timestamp: 2026-09-23\nunderscored: 1_000\nscientific: 1e2\nzero_negative_exponent: 0e-400\nzero_positive_exponent: 0.000e999\n"))
	if err != nil {
		t.Fatalf("decode interoperable YAML scalars: %v", err)
	}
	object := value.(map[string]any)
	if object["timestamp"] != "2026-09-23" || object["underscored"] != "1_000" || object["scientific"] != float64(100) || object["zero_negative_exponent"] != float64(0) || object["zero_positive_exponent"] != float64(0) {
		t.Fatalf("unexpected scalar normalization: %#v", object)
	}

	invalidYAML := map[string][]byte{
		"anchor":         []byte("value: &x 1\ncopy: *x\n"),
		"explicit_tag":   []byte("value: !!timestamp 2026-09-23\n"),
		"non_string_key": []byte("1: value\n"),
		"overflow":       []byte("value: 1e400\n"),
		"underflow":      []byte("value: 1e-4000\n"),
		"unsafe_integer": []byte("value: 9007199254740992\n"),
		"invalid_utf8":   {'v', 'a', 'l', 'u', 'e', ':', ' ', 0xff},
	}
	for name, document := range invalidYAML {
		if _, err := decodeStrictYAML(document); err == nil {
			t.Errorf("%s YAML was accepted", name)
		}
	}

	for name, document := range map[string][]byte{
		"unpaired_high":  []byte(`{"value":"\ud800"}`),
		"unpaired_low":   []byte(`{"value":"\udc00"}`),
		"unsafe_integer": []byte(`{"value":9007199254740992}`),
	} {
		if _, err := decodeStrictJSON(document); err == nil {
			t.Errorf("%s JSON was accepted", name)
		}
	}
	if _, err := decodeStrictJSON([]byte(`{"value":"\ud83d\ude00"}`)); err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
}

func TestValidationBeforeDigestAndOfflineReferenceBoundary(t *testing.T) {
	t.Parallel()

	repositoryRoot := filepath.Join("..", "..", "..", "..")
	invalidFiles, err := filepath.Glob(filepath.Join(repositoryRoot, "examples", "manifests", "invalid", "*"))
	if err != nil {
		t.Fatalf("glob invalid fixtures: %v", err)
	}
	for _, fixturePath := range invalidFiles {
		fixturePath := fixturePath
		t.Run("fixture/"+filepath.Base(fixturePath), func(t *testing.T) {
			t.Parallel()
			if digest, err := DigestFile(fixturePath); err == nil {
				t.Fatalf("invalid fixture produced digest %q", digest)
			}
		})
	}

	complete, err := os.ReadFile(filepath.Join(repositoryRoot, "examples", "manifests", "valid", "complete.yaml"))
	if err != nil {
		t.Fatalf("read complete fixture: %v", err)
	}
	if digest, err := Digest(complete); err == nil {
		t.Fatalf("byte-only Digest accepted unresolved package refs and produced %q", digest)
	}

	packageRoot := canonicalTestDirectory(t, t.TempDir())
	writePackageFile(t, packageRoot, "schemas/value.schema", `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"value": {"$ref": "leaf.schema#/$defs/value"}},
  "additionalProperties": false
}`)
	writePackageFile(t, packageRoot, "schemas/leaf.schema", `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$defs": {"value": {"type": "string", "minLength": 1}}
}`)
	writePackageFile(t, packageRoot, "manifest.yaml", validManifestWithInputRef("./schemas/value.schema"))
	if _, err := DigestPackageFile(packageRoot, "manifest.yaml"); err != nil {
		t.Fatalf("valid package-local schema closure failed: %v", err)
	}

	badReferences := map[string]string{
		"remote":         "https://example.invalid/schema.json",
		"file":           "file:///tmp/schema.json",
		"absolute":       "/tmp/schema.json",
		"windows":        `C:\\temp\\schema.json`,
		"backslash":      `schemas\\value.schema`,
		"traversal":      "../schema.json",
		"encoded_escape": "%2e%2e/schema.json",
		"missing":        "./schemas/missing.schema",
	}
	for name, reference := range badReferences {
		name, reference := name, reference
		t.Run("reference/"+name, func(t *testing.T) {
			manifestName := "invalid-" + name + ".yaml"
			writePackageFile(t, packageRoot, manifestName, validManifestWithInputRef(reference))
			if digest, err := DigestPackageFile(packageRoot, manifestName); err == nil {
				t.Fatalf("unsafe reference %q produced digest %q", reference, digest)
			}
		})
	}

	outside := canonicalTestDirectory(t, t.TempDir())
	writePackageFile(t, outside, "outside.schema", `{"type":"object"}`)
	if err := os.Symlink(filepath.Join(outside, "outside.schema"), filepath.Join(packageRoot, "schemas", "linked.schema")); err != nil {
		t.Fatalf("create schema symlink: %v", err)
	}
	writePackageFile(t, packageRoot, "symlink.yaml", validManifestWithInputRef("./schemas/linked.schema"))
	if digest, err := DigestPackageFile(packageRoot, "symlink.yaml"); err == nil {
		t.Fatalf("symlink schema produced digest %q", digest)
	}

	if err := os.Mkdir(filepath.Join(packageRoot, "linked-dir-target"), 0o755); err != nil {
		t.Fatalf("create symlink target directory: %v", err)
	}
	writePackageFile(t, packageRoot, "linked-dir-target/value.schema", `{"type":"object"}`)
	if err := os.Symlink(filepath.Join(packageRoot, "linked-dir-target"), filepath.Join(packageRoot, "linked-dir")); err != nil {
		t.Fatalf("create directory symlink: %v", err)
	}
	writePackageFile(t, packageRoot, "symlink-parent.yaml", validManifestWithInputRef("./linked-dir/value.schema"))
	if digest, err := DigestPackageFile(packageRoot, "symlink-parent.yaml"); err == nil {
		t.Fatalf("symlink parent produced digest %q", digest)
	}
}

func TestOfflineSchemaIDBaseAndLexicalAncestorBoundary(t *testing.T) {
	t.Parallel()

	packageRoot := canonicalTestDirectory(t, t.TempDir())
	writePackageFile(t, packageRoot, "schemas/root.schema", `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "nested/base.schema",
  "type": "object",
  "properties": {"value": {"$ref": "leaf.schema"}}
}`)
	writePackageFile(t, packageRoot, "schemas/nested/leaf.schema", `{"type":"string"}`)
	writePackageFile(t, packageRoot, "valid-id-base.yaml", validManifestWithInputRef("./schemas/root.schema"))
	if _, err := DigestPackageFile(packageRoot, "valid-id-base.yaml"); err != nil {
		t.Fatalf("relative $id base closure failed: %v", err)
	}

	writePackageFile(t, packageRoot, "schemas/duplicate-id.schema", `{
  "allOf": [
    {"$id": "same.schema", "type": "string"},
    {"$id": "same.schema", "type": "string"}
  ]
}`)
	writePackageFile(t, packageRoot, "duplicate-id.yaml", validManifestWithInputRef("./schemas/duplicate-id.schema"))
	if digest, err := DigestPackageFile(packageRoot, "duplicate-id.yaml"); err == nil {
		t.Fatalf("duplicate resolved $id produced digest %q", digest)
	}

	writePackageFile(t, packageRoot, "schemas/unknown-keyword.schema", `{"type":"object","typoKeyword":true}`)
	writePackageFile(t, packageRoot, "unknown-keyword.yaml", validManifestWithInputRef("./schemas/unknown-keyword.schema"))
	if digest, err := DigestPackageFile(packageRoot, "unknown-keyword.yaml"); err == nil {
		t.Fatalf("unknown Schema keyword produced digest %q", digest)
	}

	parent := canonicalTestDirectory(t, t.TempDir())
	realRoot := filepath.Join(parent, "real-package")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatalf("create real package root: %v", err)
	}
	writePackageFile(t, realRoot, "manifest.yaml", validManifestWithInputRef("./schema.json"))
	writePackageFile(t, realRoot, "schema.json", `{"type":"object"}`)
	aliasRoot := filepath.Join(parent, "alias-package")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatalf("create package ancestor symlink: %v", err)
	}
	if digest, err := DigestPackageFile(aliasRoot, "manifest.yaml"); err == nil {
		t.Fatalf("symlinked lexical package root produced digest %q", digest)
	}
}

func TestEmbeddedManifestSchemasMatchAuthority(t *testing.T) {
	t.Parallel()

	repositoryRoot := filepath.Join("..", "..", "..", "..")
	checks := []struct {
		path    string
		encoded string
	}{
		{"schemas/manifest/agent-manifest-v1.schema.json", currentManifestSchemaGZIPBase64},
		{"schemas/common/identifiers.schema.json", identifiersSchemaGZIPBase64},
		{"schemas/resources/secret-ref-v1.schema.json", secretRefSchemaGZIPBase64},
	}
	for _, check := range checks {
		expected, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(check.path)))
		if err != nil {
			t.Fatalf("read authoritative schema %s: %v", check.path, err)
		}
		actual, err := bundledSchema(check.encoded)
		if err != nil {
			t.Fatalf("decode embedded schema %s: %v", check.path, err)
		}
		if string(actual) != string(expected) {
			t.Fatalf("embedded schema drifted from %s", check.path)
		}
	}
}

func writePackageFile(t *testing.T, root, relative, content string) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("create package directory: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write package file %s: %v", relative, err)
	}
}

func canonicalTestDirectory(t *testing.T, directory string) string {
	t.Helper()
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve test directory: %v", err)
	}
	return realDirectory
}

func validManifestWithInputRef(reference string) string {
	return strings.ReplaceAll(`protocol: arop/v1
kind: AgentManifest
identity:
  id: test.agent
  version: 1.0.0
  name: Test
  summary: Test
  owner: {team: test-team}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: {$ref: __REFERENCE__}
    output_schema: {type: object}
execution:
  default_timeout_seconds: 30
  max_timeout_seconds: 60
  effects: {level: none, idempotency: supported, human_confirmation: false}
  capabilities: {}
`, "__REFERENCE__", reference)
}
