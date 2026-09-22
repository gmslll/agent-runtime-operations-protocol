// Package manifest implements language-independent Agent Manifest operations.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"go.yaml.in/yaml/v3"
)

const digestPrefix = "sha256:"

// Digest parses a JSON or YAML Manifest, removes top-level digest/signature
// material, canonicalizes the JSON value with RFC 8785 JCS, and returns its
// lowercase SHA-256 digest.
func Digest(document []byte) (string, error) {
	value, err := decodeSingleDocument(document)
	if err != nil {
		return "", err
	}

	manifest, ok := value.(map[string]any)
	if !ok {
		return "", errors.New("manifest root must be an object")
	}

	delete(manifest, "manifest_digest")
	delete(manifest, "signature")
	delete(manifest, "signatures")

	jsonDocument, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode manifest as JSON: %w", err)
	}

	canonical, err := jsoncanonicalizer.Transform(jsonDocument)
	if err != nil {
		return "", fmt.Errorf("canonicalize manifest with RFC 8785: %w", err)
	}

	sum := sha256.Sum256(canonical)
	return digestPrefix + hex.EncodeToString(sum[:]), nil
}

func decodeSingleDocument(document []byte) (any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(document))

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if value == nil {
		return nil, errors.New("manifest is empty")
	}

	var trailing any
	err := decoder.Decode(&trailing)
	if err == nil {
		return nil, errors.New("manifest must contain exactly one YAML/JSON document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse trailing manifest content: %w", err)
	}

	return value, nil
}
