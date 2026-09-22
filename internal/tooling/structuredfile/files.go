// Package structuredfile contains repository-local JSON/YAML loading helpers for
// Go governance and release tooling. It deliberately has no Node dependency.
package structuredfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Load(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" {
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			return err
		}
		if err := rejectDuplicateYAML(&node); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return node.Decode(value)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func rejectDuplicateYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if seen[key] {
				return fmt.Errorf("duplicate YAML key %q", key)
			}
			seen[key] = true
		}
	}
	for _, child := range node.Content {
		if err := rejectDuplicateYAML(child); err != nil {
			return err
		}
	}
	return nil
}

func FindRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for current := abs; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "spec", "artifact-manifest.yaml")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return "", fmt.Errorf("repository root not found from %s", start)
}

func SafeRelative(root, candidate string) (string, error) {
	if filepath.IsAbs(candidate) || strings.Contains(candidate, ":") {
		return "", fmt.Errorf("unsafe repository path %q", candidate)
	}
	clean := filepath.Clean(candidate)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe repository path %q", candidate)
	}
	abs := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes repository: %q", candidate)
	}
	return filepath.ToSlash(rel), nil
}
