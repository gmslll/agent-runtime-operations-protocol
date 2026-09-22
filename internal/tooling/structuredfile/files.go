// Package structuredfile contains repository-local JSON/YAML loading helpers for
// Go governance and release tooling. It deliberately has no Node dependency.
package structuredfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Load(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parsed, err := Parse(data, filepath.Ext(path))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("%s: normalize: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(normalized))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadAny reads exactly one strict JSON or YAML document. JSON object keys must
// be unique at every depth and trailing values are rejected. YAML aliases and
// duplicate mapping keys are rejected and a second document is never ignored.
func LoadAny(path string) (any, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	value, err := Parse(data, filepath.Ext(path))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return value, data, nil
}

// Parse parses one strict document. format may be a filename extension or one
// of json/yaml/yml.
func Parse(data []byte, format string) (any, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if !strings.HasPrefix(format, ".") {
		format = "." + format
	}
	if format == ".yaml" || format == ".yml" {
		return parseYAML(data)
	}
	return parseJSON(data)
}

func parseJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if err := requireJSONEOF(dec); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		value := map[string]any{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key is not a string")
			}
			if _, exists := value[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			child, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			value[key] = child
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("invalid JSON object terminator")
		}
		return value, nil
	case '[':
		value := []any{}
		for dec.More() {
			child, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			value = append(value, child)
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("invalid JSON array terminator")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func requireJSONEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil

}

func parseYAML(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := dec.Decode(&document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("trailing YAML document: %w", err)
	}
	if err := rejectDuplicateYAML(&document); err != nil {
		return nil, err
	}
	return yamlJSONValue(document.Content[0])
}

func yamlJSONValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		value := map[string]any{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			child, err := yamlJSONValue(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			value[node.Content[i].Value] = child
		}
		return value, nil
	case yaml.SequenceNode:
		value := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			child, err := yamlJSONValue(item)
			if err != nil {
				return nil, err
			}
			value = append(value, child)
		}
		return value, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			value, err := strconv.ParseBool(strings.ToLower(node.Value))
			if err != nil {
				return nil, err
			}
			return value, nil
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		default:
			return node.Value, nil
		}
	default:
		return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
	}
}

func rejectDuplicateYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Kind != yaml.ScalarNode || node.Content[i].Tag != "!!str" {
				return fmt.Errorf("YAML mapping keys must be strings")
			}
			key := node.Content[i].Value
			if seen[key] {
				return fmt.Errorf("duplicate YAML key %q", key)
			}
			seen[key] = true
		}
	}
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML aliases are not allowed")
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
	if err != nil || pathOutside(rel) {
		return "", fmt.Errorf("path escapes repository: %q", candidate)
	}
	return filepath.ToSlash(rel), nil
}

// RequireInsideFile resolves both the lexical path and every symlink before
// accepting a repository file. A path must be inside the repository in both
// views, so an in-repository symlink cannot escape the trust boundary.
func RequireInsideFile(root, candidate, label string) (string, error) {
	resolved, lexicalInside, canonicalInside, err := classifyExistingPath(root, candidate)
	if err != nil {
		return "", fmt.Errorf("%s path: %w", label, err)
	}
	if !lexicalInside || !canonicalInside {
		return "", fmt.Errorf("%s must remain inside the repository", label)
	}
	return resolved, nil
}

// RequireOutsideFile accepts only a real file whose lexical path and resolved
// symlink target are both outside the repository. This rejects misleading
// names such as "..inside" under the root and outside symlinks back into it.
func RequireOutsideFile(root, candidate, label string) (string, error) {
	resolved, lexicalInside, canonicalInside, err := classifyExistingPath(root, candidate)
	if err != nil {
		return "", fmt.Errorf("%s path: %w", label, err)
	}
	if lexicalInside || canonicalInside {
		return "", fmt.Errorf("%s must remain outside the repository", label)
	}
	return resolved, nil
}

func classifyExistingPath(root, candidate string) (resolved string, lexicalInside, canonicalInside bool, err error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve repository root: %w", err)
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve repository root symlinks: %w", err)
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve candidate: %w", err)
	}
	lexicalRel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return "", false, false, fmt.Errorf("compare lexical path: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(candidateAbs)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve candidate symlinks: %w", err)
	}
	canonicalRel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return "", false, false, fmt.Errorf("compare canonical path: %w", err)
	}
	return resolved, !pathOutside(lexicalRel), !pathOutside(canonicalRel), nil
}

func pathOutside(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
