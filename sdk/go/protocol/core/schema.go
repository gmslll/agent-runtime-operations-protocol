package core

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaSet is a closed, in-memory Draft 2020-12 schema bundle. Resolution is
// intentionally limited to resources supplied at construction time; it never
// reads the filesystem or network.
type SchemaSet struct {
	compiled map[string]*jsonschema.Schema
}

const draft202012SchemaURI = "https://json-schema.org/draft/2020-12/schema"

// NewSchemaSet strictly parses and compiles every resource in an offline
// closure. Map keys are absolute schema resource URIs.
func NewSchemaSet(resources map[string][]byte) (*SchemaSet, error) {
	if len(resources) == 0 {
		return nil, fmt.Errorf("schema set is empty")
	}
	documents := make(map[string]any, len(resources))
	for location, data := range resources {
		u, err := url.Parse(location)
		if err != nil || !u.IsAbs() || u.Fragment != "" {
			return nil, fmt.Errorf("schema location must be an absolute resource URI without fragment: %q", location)
		}
		document, err := ParseJSON(data)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", location, err)
		}
		object, ok := document.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema %s root must be an object", location)
		}
		if dialect, present := object["$schema"]; present && dialect != draft202012SchemaURI {
			return nil, fmt.Errorf("schema %s declares dialect %v, want exact %q", location, dialect, draft202012SchemaURI)
		}
		if err := validateSchemaDialectProfile(object); err != nil {
			return nil, fmt.Errorf("schema %s: %w", location, err)
		}
		if identifier, present := object["$id"]; present {
			if identifier != location {
				return nil, fmt.Errorf("schema %s declares mismatched $id %v", location, identifier)
			}
		}
		documents[location] = document
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.AssertVocabs()
	compiler.UseRegexpEngine(compileLinearECMA)
	compiler.UseLoader(memorySchemaLoader{documents: documents})
	for location, document := range documents {
		if err := compiler.AddResource(location, document); err != nil {
			return nil, fmt.Errorf("register schema %s: %w", location, err)
		}
	}
	set := &SchemaSet{compiled: map[string]*jsonschema.Schema{}}
	for location := range documents {
		compiled, err := compiler.Compile(location)
		if err != nil {
			return nil, fmt.Errorf("compile closed schema %s: %w", location, err)
		}
		set.compiled[location] = compiled
	}
	return set, nil
}

func validateSchemaDialectProfile(schema any) error {
	switch typed := schema.(type) {
	case map[string]any:
		if dialect, present := typed["$schema"]; present && dialect != draft202012SchemaURI {
			return fmt.Errorf("nested schema declares dialect %v, want exact %q", dialect, draft202012SchemaURI)
		}
		// The P06 single-dialect profile reserves the property name $schema
		// throughout a schema document, including locations reachable by a
		// JSON Pointer $ref or an extension keyword. This deliberately stricter
		// rule prevents hidden dialect switches without needing to predict every
		// vocabulary's schema-bearing locations.
		for key, child := range typed {
			if err := validateSchemaDialectProfile(child); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case []any:
		for index, child := range typed {
			if err := validateSchemaDialectProfile(child); err != nil {
				return fmt.Errorf("%d: %w", index, err)
			}
		}
	}
	return nil
}

// Validate parses one strict JSON document before validating it.
func (set *SchemaSet) Validate(schemaLocation string, document []byte) error {
	if set == nil {
		return fmt.Errorf("schema set is nil")
	}
	compiled, ok := set.compiled[schemaLocation]
	if !ok {
		return fmt.Errorf("schema %q is not in the offline set", schemaLocation)
	}
	value, err := ParseJSON(document)
	if err != nil {
		return err
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("validate against %s: %w", schemaLocation, err)
	}
	return nil
}

type memorySchemaLoader struct{ documents map[string]any }

func (loader memorySchemaLoader) Load(location string) (any, error) {
	document, ok := loader.documents[location]
	if !ok {
		return nil, fmt.Errorf("offline schema set rejected unresolved resource %q", location)
	}
	return document, nil
}

// compileLinearECMA accepts the deterministic, linear-time intersection used
// by AROP schemas. Go's RE2 engine prevents timeout/error ambiguity under
// `not`, `if`, and other boolean JSON Schema keywords. Non-capturing groups are
// semantics-preservingly rewritten because captures are not observable here;
// lookaround, backreferences, and other backtracking-only constructs fail
// closed during SchemaSet construction.
func compileLinearECMA(value string) (jsonschema.Regexp, error) {
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
