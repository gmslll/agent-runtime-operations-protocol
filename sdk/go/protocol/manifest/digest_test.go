package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
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

			document, err := os.ReadFile(filepath.Join(repositoryRoot, manifestPath))
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			actualDigest, err := Digest(document)
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

	base := []byte("protocol: arop/v1\nkind: AgentManifest\n")
	withMetadata := []byte(
		"protocol: arop/v1\n" +
			"kind: AgentManifest\n" +
			"manifest_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
			"signature:\n" +
			"  algorithm: example\n",
	)

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
