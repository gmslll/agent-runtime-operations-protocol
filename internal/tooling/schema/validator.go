// Package schema performs offline Draft 2020-12 validation for governance
// artifacts. It intentionally refuses network schema resolution.
package schema

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateFile validates value against a repository-local JSON Schema.
func ValidateFile(root, schemaPath string, value any) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	abs, err := filepath.Abs(filepath.Join(absRoot, filepath.FromSlash(schemaPath)))
	if err != nil {
		return fmt.Errorf("resolve schema %s: %w", schemaPath, err)
	}
	document, _, err := structuredfile.LoadAny(abs)
	if err != nil {
		return fmt.Errorf("load schema %s: %w", schemaPath, err)
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.AssertVocabs()
	compiler.UseRegexpEngine(compileECMAScript)
	loader := strictRepositoryLoader{root: absRoot}
	compiler.UseLoader(jsonschema.SchemeURLLoader{"file": loader, "https": loader})
	if err := compiler.AddResource(location, document); err != nil {
		return fmt.Errorf("register schema %s: %w", schemaPath, err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return fmt.Errorf("compile schema %s: %w", schemaPath, err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("validate against %s: %w", schemaPath, err)
	}
	return nil
}

func compileECMAScript(value string) (jsonschema.Regexp, error) {
	translated, err := translateNonCapturingGroups(value)
	if err != nil {
		return nil, err
	}
	compiled, err := regexp.Compile(translated)
	if err != nil {
		return nil, fmt.Errorf("pattern is outside the AROP linear-time regexp profile: %w", err)
	}
	return compiled, nil
}

func translateNonCapturingGroups(value string) (string, error) {
	var result strings.Builder
	result.Grow(len(value))
	inClass := false
	escaped := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			if !strings.ContainsRune(`.\\+*nr`, rune(character)) {
				return "", fmt.Errorf("pattern escape \\%c is outside the AROP linear-time regexp profile", character)
			}
			result.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			result.WriteByte(character)
			escaped = true
			continue
		}
		if inClass {
			if character == '[' && index+1 < len(value) && value[index+1] == ':' {
				return "", fmt.Errorf("POSIX character classes are outside the AROP linear-time regexp profile")
			}
			result.WriteByte(character)
			if character == ']' {
				inClass = false
			}
			continue
		}
		if character == '[' {
			inClass = true
			result.WriteByte(character)
			continue
		}
		if character == '.' {
			return "", fmt.Errorf("unescaped dot is outside the AROP linear-time regexp profile")
		}
		if character == '(' && index+1 < len(value) && value[index+1] == '?' {
			if index+2 < len(value) && value[index+2] == ':' {
				result.WriteByte('(')
				index += 2
				continue
			}
			return "", fmt.Errorf("pattern construct beginning at byte %d is outside the AROP linear-time regexp profile", index)
		}
		result.WriteByte(character)
	}
	return result.String(), nil
}

// ValidatePath applies the strict document parser before schema validation.
func ValidatePath(root, schemaPath, documentPath string) (any, []byte, error) {
	value, data, err := structuredfile.LoadAny(filepath.Join(root, filepath.FromSlash(documentPath)))
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateFile(root, schemaPath, value); err != nil {
		return nil, nil, err
	}
	return value, data, nil
}

type strictRepositoryLoader struct {
	root string
}

func (loader strictRepositoryLoader) Load(rawURL string) (any, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
	}
	var target string
	switch u.Scheme {
	case "file":
		if u.Host != "" || u.User != nil || u.RawQuery != "" {
			return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
		}
		target = filepath.Clean(filepath.FromSlash(u.Path))
	case "https":
		const prefix = "/schemas/v1/"
		if u.Host != "arop.invalid" || u.User != nil || u.RawQuery != "" || !strings.HasPrefix(u.Path, prefix) {
			return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
		}
		relative := strings.TrimPrefix(u.Path, prefix)
		if relative == "" || relative != pathpkg.Clean(relative) || strings.Contains(relative, `\`) {
			return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
		}
		target = filepath.Join(loader.root, "schemas", filepath.FromSlash(relative))
	default:
		return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
	}
	relative, err := filepath.Rel(loader.root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
	}
	value, _, err := structuredfile.LoadAny(target)
	return value, err
}
