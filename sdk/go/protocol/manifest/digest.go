// Package manifest implements language-independent Agent Manifest operations.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

const digestPrefix = "sha256:"

// Digest strictly parses and validates a self-contained JSON or YAML Manifest,
// removes top-level digest/signature material, canonicalizes the JSON value
// with RFC 8785 JCS, and returns its lowercase SHA-256 digest. Manifests that
// use package-relative schema references must use DigestFile or
// DigestPackageFile so the offline reference boundary is explicit.
func Digest(document []byte) (string, error) {
	value, err := decodeSingleDocument(document)
	if err != nil {
		return "", err
	}
	if err := validateManifest(value, nil); err != nil {
		return "", err
	}
	return digestValidatedManifest(value)
}

func digestValidatedManifest(value any) (string, error) {
	manifest := cloneObject(value.(map[string]any))
	delete(manifest, "manifest_digest")
	delete(manifest, "signature")
	delete(manifest, "signatures")

	return canonicalValueDigest(manifest)
}

func canonicalValueDigest(value any) (string, error) {
	jsonDocument, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode value as JSON: %w", err)
	}

	canonical, err := jsoncanonicalizer.Transform(jsonDocument)
	if err != nil {
		return "", fmt.Errorf("canonicalize value with RFC 8785: %w", err)
	}

	sum := sha256.Sum256(canonical)
	return digestPrefix + hex.EncodeToString(sum[:]), nil
}
