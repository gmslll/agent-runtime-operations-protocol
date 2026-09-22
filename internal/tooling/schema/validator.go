// Package schema performs offline Draft 2020-12 validation for governance
// artifacts. It intentionally refuses network schema resolution.
package schema

import (
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/dlclark/regexp2"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateFile validates value against a repository-local JSON Schema.
func ValidateFile(root, schemaPath string, value any) error {
	abs, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(schemaPath)))
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
	compiler.UseLoader(jsonschema.SchemeURLLoader{"file": strictFileLoader{}})
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

type ecmaRegexp regexp2.Regexp

func (r *ecmaRegexp) MatchString(value string) bool {
	matched, err := (*regexp2.Regexp)(r).MatchString(value)
	return err == nil && matched
}
func (r *ecmaRegexp) String() string { return (*regexp2.Regexp)(r).String() }
func compileECMAScript(value string) (jsonschema.Regexp, error) {
	compiled, err := regexp2.Compile(value, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmaRegexp)(compiled), nil
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

type strictFileLoader struct{}

func (strictFileLoader) Load(rawURL string) (any, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "file" {
		return nil, fmt.Errorf("offline schema loader rejected %q", rawURL)
	}
	value, _, err := structuredfile.LoadAny(filepath.FromSlash(u.Path))
	return value, err
}
