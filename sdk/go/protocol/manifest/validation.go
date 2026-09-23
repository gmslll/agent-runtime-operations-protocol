package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const (
	manifestSchemaID    = "https://arop.invalid/schemas/v1/manifest/agent-manifest-v1.schema.json"
	identifiersSchemaID = "https://arop.invalid/schemas/v1/common/identifiers.schema.json"
	secretRefSchemaID   = "https://arop.invalid/schemas/v1/resources/secret-ref-v1.schema.json"
	packageSchemaBase   = "https://arop.package.invalid/"
	draft202012Schema   = "https://json-schema.org/draft/2020-12/schema"
)

var (
	manifestSchemaOnce sync.Once
	manifestSchema     *jsonschema.Schema
	manifestSchemaErr  error
	jsonNumberPattern  = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	portablePathPart   = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	portableAnchor     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._:-]*$`)
	portablePointer    = regexp.MustCompile(`^[A-Za-z0-9._~$/-]*$`)
	portableDateTime   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|[+-](\d{2}):(\d{2}))$`)
)

// Validate strictly parses and validates a self-contained Manifest. Relative
// package schema references are rejected because byte slices do not carry a
// trustworthy package boundary; use ValidateFile or ValidatePackageFile.
func Validate(document []byte) error {
	value, err := decodeSingleDocument(document)
	if err != nil {
		return err
	}
	return validateManifest(value, nil)
}

// ValidateFile validates a Manifest and treats its containing directory as the
// package root for offline schema references.
func ValidateFile(filePath string) error {
	_, _, err := loadManifestFile(filepath.Dir(filePath), filepath.Base(filePath), false)
	return err
}

// DigestFile validates and digests a Manifest while treating its containing
// directory as the package root.
func DigestFile(filePath string) (string, error) {
	value, _, err := loadManifestFile(filepath.Dir(filePath), filepath.Base(filePath), false)
	if err != nil {
		return "", err
	}
	return digestValidatedManifest(value)
}

// ValidatePackageFile validates packageRelativePath below packageRoot. Both
// the Manifest and every referenced schema are read without following symlink
// components and may not escape packageRoot.
func ValidatePackageFile(packageRoot, packageRelativePath string) error {
	_, _, err := loadManifestFile(packageRoot, packageRelativePath, true)
	return err
}

// DigestPackageFile validates and digests a package-contained Manifest.
func DigestPackageFile(packageRoot, packageRelativePath string) (string, error) {
	value, _, err := loadManifestFile(packageRoot, packageRelativePath, true)
	if err != nil {
		return "", err
	}
	return digestValidatedManifest(value)
}

func loadManifestFile(packageRoot, manifestPath string, requireRelative bool) (any, []byte, error) {
	root, err := cleanPackageRoot(packageRoot)
	if err != nil {
		return nil, nil, err
	}
	if requireRelative {
		if err := validateRelativeReference(manifestPath); err != nil {
			return nil, nil, fmt.Errorf("manifest path: %w", err)
		}
	}
	data, err := readPackageFile(root, filepath.ToSlash(manifestPath))
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	value, err := decodeDocument(data, manifestPath)
	if err != nil {
		return nil, nil, err
	}
	context := &packageContext{root: root, manifestPath: filepath.ToSlash(manifestPath)}
	if err := validateManifest(value, context); err != nil {
		return nil, nil, err
	}
	return value, data, nil
}

func validateManifest(value any, context *packageContext) error {
	if err := rejectDangerousObjectKeys(value); err != nil {
		return err
	}
	manifest, ok := value.(map[string]any)
	if !ok {
		return errors.New("manifest root must be an object")
	}

	validationValue := cloneObject(manifest)
	delete(validationValue, "manifest_digest")
	delete(validationValue, "signature")
	delete(validationValue, "signatures")

	compiled, err := compileBundledManifestSchema()
	if err != nil {
		return err
	}
	if err := compiled.Validate(validationValue); err != nil {
		return fmt.Errorf("manifest schema validation failed: %w", err)
	}
	if err := validateManifestSemantics(validationValue); err != nil {
		return err
	}
	return validateDeclaredSchemas(validationValue, context)
}

func compileBundledManifestSchema() (*jsonschema.Schema, error) {
	manifestSchemaOnce.Do(func() {
		manifestSchema, manifestSchemaErr = buildBundledManifestSchema()
	})
	return manifestSchema, manifestSchemaErr
}

func buildBundledManifestSchema() (*jsonschema.Schema, error) {
	manifestDocument, err := bundledSchema(currentManifestSchemaGZIPBase64)
	if err != nil {
		return nil, err
	}
	identifiersDocument, err := bundledSchema(identifiersSchemaGZIPBase64)
	if err != nil {
		return nil, err
	}
	secretDocument, err := bundledSchema(secretRefSchemaGZIPBase64)
	if err != nil {
		return nil, err
	}

	compiler := newSchemaCompiler()
	for schemaID, document := range map[string][]byte{
		manifestSchemaID:    manifestDocument,
		identifiersSchemaID: identifiersDocument,
		secretRefSchemaID:   secretDocument,
	} {
		value, err := decodeStrictJSON(document)
		if err != nil {
			return nil, fmt.Errorf("parse embedded schema %s: %w", schemaID, err)
		}
		if err := compiler.AddResource(schemaID, value); err != nil {
			return nil, fmt.Errorf("register embedded schema %s: %w", schemaID, err)
		}
	}
	compiled, err := compiler.Compile(manifestSchemaID)
	if err != nil {
		return nil, fmt.Errorf("compile manifest schema: %w", err)
	}
	return compiled, nil
}

func validateManifestSemantics(manifest map[string]any) error {
	skills, _ := manifest["skills"].([]any)
	seenSkills := make(map[string]struct{}, len(skills))
	for _, rawSkill := range skills {
		skill, _ := rawSkill.(map[string]any)
		id, _ := skill["id"].(string)
		if _, exists := seenSkills[id]; exists {
			return fmt.Errorf("manifest semantic validation failed: duplicate skill id %q", id)
		}
		seenSkills[id] = struct{}{}
	}

	execution, _ := manifest["execution"].(map[string]any)
	defaultTimeout, _ := integerValue(execution["default_timeout_seconds"])
	maxTimeout, _ := integerValue(execution["max_timeout_seconds"])
	if defaultTimeout > maxTimeout {
		return errors.New("manifest semantic validation failed: default_timeout_seconds exceeds max_timeout_seconds")
	}
	capabilities, _ := execution["capabilities"].(map[string]any)
	requiredProfiles := stringSet(capabilities["required_profiles"])
	for profile := range stringSet(capabilities["optional_profiles"]) {
		if _, exists := requiredProfiles[profile]; exists {
			return fmt.Errorf("manifest semantic validation failed: capability %q cannot be both required and optional", profile)
		}
	}

	extensions, _ := manifest["extensions"].(map[string]any)
	for extension := range stringSet(manifest["required_extensions"]) {
		if _, exists := extensions[extension]; !exists {
			return fmt.Errorf("manifest semantic validation failed: required extension %q has no payload", extension)
		}
	}
	return nil
}

func validateDeclaredSchemas(manifest map[string]any, context *packageContext) error {
	skills, _ := manifest["skills"].([]any)
	for index, rawSkill := range skills {
		skill, _ := rawSkill.(map[string]any)
		for _, field := range []string{"input_schema", "output_schema"} {
			declaration, _ := skill[field].(map[string]any)
			if _, _, err := compilePackageSchema(declaration, context); err != nil {
				return fmt.Errorf("skills[%d].%s: %w", index, field, err)
			}
		}
	}
	extensions, _ := manifest["extensions"].(map[string]any)
	extensionIDs := make([]string, 0, len(extensions))
	for extensionID := range extensions {
		extensionIDs = append(extensionIDs, extensionID)
	}
	sort.Strings(extensionIDs)
	for _, extensionID := range extensionIDs {
		envelope, _ := extensions[extensionID].(map[string]any)
		schemaReference, _ := envelope["schema_ref"].(string)
		declaration := map[string]any{"$ref": schemaReference}
		compiled, closure, err := compilePackageSchema(declaration, context)
		if err != nil {
			return fmt.Errorf("extensions[%q].schema_ref: %w", extensionID, err)
		}
		if len(closure.documents) != 2 {
			return fmt.Errorf("extensions[%q].schema_ref: extension schemas may only use document-local fragment references", extensionID)
		}
		targetLocation, fragmentOnly, err := resolveSchemaURI(packageSchemaBase+contextPath(context), schemaReference, false)
		if err != nil || fragmentOnly {
			return fmt.Errorf("extensions[%q].schema_ref must name a package schema file", extensionID)
		}
		schemaDocument, exists := closure.documents[targetLocation]
		if !exists {
			return fmt.Errorf("extensions[%q].schema_ref did not resolve to a package schema", extensionID)
		}
		if err := validateDocumentLocalReferences(schemaDocument); err != nil {
			return fmt.Errorf("extensions[%q].schema_ref: %w", extensionID, err)
		}
		actualDigest, err := canonicalValueDigest(schemaDocument)
		if err != nil {
			return fmt.Errorf("extensions[%q].schema_digest: %w", extensionID, err)
		}
		declaredDigest, _ := envelope["schema_digest"].(string)
		if declaredDigest != actualDigest {
			return fmt.Errorf("extensions[%q].schema_digest is %q, want %q", extensionID, declaredDigest, actualDigest)
		}
		if err := compiled.Validate(envelope["data"]); err != nil {
			return fmt.Errorf("extensions[%q].data: %w", extensionID, err)
		}
	}
	return nil
}

func validateDocumentLocalReferences(value any) error {
	if _, ok := value.(bool); ok {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return errors.New("JSON Schema must be an object or boolean")
	}
	for _, keyword := range []string{"$ref", "$dynamicRef"} {
		if raw, exists := schema[keyword]; exists {
			reference, ok := raw.(string)
			if !ok || !strings.HasPrefix(reference, "#") {
				return fmt.Errorf("Extension Schema %s must be a document-local fragment reference", keyword)
			}
		}
	}
	for keyword, child := range schema {
		switch {
		case schemaObjectKeywords[keyword]:
			if err := validateDocumentLocalReferences(child); err != nil {
				return err
			}
		case schemaArrayKeywords[keyword]:
			for _, item := range child.([]any) {
				if err := validateDocumentLocalReferences(item); err != nil {
					return err
				}
			}
		case schemaMapKeywords[keyword]:
			for _, item := range child.(map[string]any) {
				if err := validateDocumentLocalReferences(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type packageContext struct {
	root         string
	manifestPath string
}

type schemaClosure struct {
	documents map[string]any
	owners    map[string]string
	context   *packageContext
}

// indexSchemaIDs registers every embedded Schema Resource in one physical
// document before any reference is resolved. JSON Schema resource lookup is
// independent of object member order: a sibling $ref may target a later $id.
func indexSchemaIDs(value any, baseLocation, owner string, closure *schemaClosure) error {
	if _, ok := value.(bool); ok {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return errors.New("JSON Schema must be an object or boolean")
	}
	currentBase := baseLocation
	if rawID, exists := schema["$id"]; exists {
		identifier, ok := rawID.(string)
		if !ok {
			return errors.New("$id must be a string")
		}
		resolved, _, err := resolveSchemaURI(baseLocation, identifier, true)
		if err != nil {
			return err
		}
		if previous, exists := closure.owners[resolved]; exists && previous != owner {
			return fmt.Errorf("duplicate resolved $id %q in %s and %s", resolved, previous, owner)
		}
		closure.owners[resolved] = owner
		currentBase = resolved
	}
	for keyword, child := range schema {
		switch {
		case schemaObjectKeywords[keyword]:
			if err := indexSchemaIDs(child, currentBase, owner+"/"+keyword, closure); err != nil {
				return err
			}
		case schemaArrayKeywords[keyword]:
			items, ok := child.([]any)
			if !ok {
				return fmt.Errorf("JSON Schema keyword %q must be an array", keyword)
			}
			for index, item := range items {
				if err := indexSchemaIDs(item, currentBase, fmt.Sprintf("%s/%s/%d", owner, keyword, index), closure); err != nil {
					return err
				}
			}
		case schemaMapKeywords[keyword]:
			entries, ok := child.(map[string]any)
			if !ok {
				return fmt.Errorf("JSON Schema keyword %q must be an object", keyword)
			}
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := indexSchemaIDs(entries[name], currentBase, owner+"/"+keyword+"/"+name, closure); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func compilePackageSchema(declaration map[string]any, context *packageContext) (*jsonschema.Schema, *schemaClosure, error) {
	if err := rejectDangerousObjectKeys(declaration); err != nil {
		return nil, nil, err
	}
	compiler := newSchemaCompiler()
	rootLocation := packageSchemaBase + contextPath(context)
	closure := &schemaClosure{
		documents: map[string]any{rootLocation: declaration},
		owners:    map[string]string{rootLocation: contextPath(context) + "#"},
		context:   context,
	}
	if err := indexSchemaIDs(declaration, rootLocation, contextPath(context)+"#", closure); err != nil {
		return nil, nil, err
	}
	if err := walkSchema(declaration, rootLocation, contextPath(context), contextPath(context)+"#", closure); err != nil {
		return nil, nil, err
	}
	locations := make([]string, 0, len(closure.documents))
	for location := range closure.documents {
		locations = append(locations, location)
	}
	sort.Strings(locations)
	for _, location := range locations {
		document := closure.documents[location]
		if err := compiler.AddResource(location, document); err != nil {
			return nil, nil, fmt.Errorf("register offline schema %s: %w", location, err)
		}
	}
	compiled, err := compiler.Compile(rootLocation)
	if err != nil {
		return nil, nil, fmt.Errorf("compile offline schema closure: %w", err)
	}
	return compiled, closure, nil
}

func contextPath(context *packageContext) string {
	if context == nil {
		return "manifest.schema.json"
	}
	return context.manifestPath
}

func walkSchema(value any, baseLocation, physicalPath, owner string, closure *schemaClosure) error {
	if boolean, ok := value.(bool); ok {
		_ = boolean
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return errors.New("JSON Schema must be an object or boolean")
	}
	for keyword := range schema {
		if !allowedSchemaKeywords[keyword] {
			return fmt.Errorf("unknown JSON Schema keyword %q", keyword)
		}
	}
	if rawDialect, exists := schema["$schema"]; exists {
		dialect, ok := rawDialect.(string)
		if !ok || dialect != draft202012Schema {
			return fmt.Errorf("$schema must be exactly %q", draft202012Schema)
		}
	}
	if rawFormat, exists := schema["format"]; exists {
		format, ok := rawFormat.(string)
		if !ok || format != "date-time" {
			return errors.New("Publisher Schema format must be exactly date-time")
		}
	}
	if rawPattern, exists := schema["pattern"]; exists {
		pattern, ok := rawPattern.(string)
		if !ok {
			return errors.New("pattern must be a string")
		}
		if err := validatePortablePattern(pattern); err != nil {
			return err
		}
	}
	if err := rejectDangerousSchemaPropertyNames(schema); err != nil {
		return err
	}
	currentBase := baseLocation
	if rawID, exists := schema["$id"]; exists {
		identifier, ok := rawID.(string)
		if !ok {
			return errors.New("$id must be a string")
		}
		resolved, _, err := resolveSchemaURI(baseLocation, identifier, true)
		if err != nil {
			return err
		}
		if previous, exists := closure.owners[resolved]; exists && previous != owner {
			return fmt.Errorf("duplicate resolved $id %q in %s and %s", resolved, previous, owner)
		}
		closure.owners[resolved] = owner
		currentBase = resolved
	}
	for _, keyword := range []string{"$ref", "$dynamicRef"} {
		rawReference, exists := schema[keyword]
		if !exists {
			continue
		}
		reference, ok := rawReference.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", keyword)
		}
		targetLocation, fragmentOnly, err := resolveSchemaURI(currentBase, reference, false)
		if err != nil {
			return err
		}
		if fragmentOnly {
			continue
		}
		if closure.context == nil {
			return fmt.Errorf("relative schema reference %q requires a package file API", reference)
		}
		targetURL, _ := url.Parse(targetLocation)
		targetPath := strings.TrimPrefix(targetURL.Path, "/")
		if _, exists := closure.owners[targetLocation]; exists {
			// The target may be an embedded Schema Resource identified by a
			// nested $id. Its containing physical document is already in the
			// compiler closure and must not be mistaken for a package filename.
			continue
		}
		data, err := readPackageFile(closure.context.root, targetPath)
		if err != nil {
			return fmt.Errorf("load offline schema reference %q: %w", reference, err)
		}
		document, err := decodeDocument(data, targetPath)
		if err != nil {
			return fmt.Errorf("parse offline schema reference %q: %w", reference, err)
		}
		if err := rejectDangerousObjectKeys(document); err != nil {
			return fmt.Errorf("parse offline schema reference %q: %w", reference, err)
		}
		closure.documents[targetLocation] = document
		closure.owners[targetLocation] = targetPath + "#"
		if err := indexSchemaIDs(document, targetLocation, targetPath+"#", closure); err != nil {
			return err
		}
		if err := walkSchema(document, targetLocation, targetPath, targetPath+"#", closure); err != nil {
			return err
		}
	}
	for keyword, child := range schema {
		switch {
		case schemaObjectKeywords[keyword]:
			if err := walkSchema(child, currentBase, physicalPath, owner+"/"+keyword, closure); err != nil {
				return err
			}
		case schemaArrayKeywords[keyword]:
			items, ok := child.([]any)
			if !ok {
				return fmt.Errorf("JSON Schema keyword %q must be an array", keyword)
			}
			for index, item := range items {
				if err := walkSchema(item, currentBase, physicalPath, fmt.Sprintf("%s/%s/%d", owner, keyword, index), closure); err != nil {
					return err
				}
			}
		case schemaMapKeywords[keyword]:
			entries, ok := child.(map[string]any)
			if !ok {
				return fmt.Errorf("JSON Schema keyword %q must be an object", keyword)
			}
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := walkSchema(entries[name], currentBase, physicalPath, owner+"/"+keyword+"/"+name, closure); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

var allowedSchemaKeywords = map[string]bool{
	"$schema": true, "$id": true, "$ref": true, "$anchor": true, "$dynamicRef": true, "$dynamicAnchor": true,
	"$vocabulary": true, "$comment": true, "$defs": true, "type": true, "enum": true, "const": true,
	"maximum": true, "exclusiveMaximum": true, "minimum": true, "exclusiveMinimum": true,
	"maxLength": true, "minLength": true, "pattern": true, "maxItems": true, "minItems": true, "uniqueItems": true,
	"maxContains": true, "minContains": true, "items": true, "prefixItems": true, "contains": true, "unevaluatedItems": true,
	"maxProperties": true, "minProperties": true, "required": true, "dependentRequired": true, "dependentSchemas": true,
	"properties": true, "additionalProperties": true, "unevaluatedProperties": true, "propertyNames": true,
	"allOf": true, "anyOf": true, "oneOf": true, "not": true, "if": true, "then": true, "else": true,
	"format": true, "contentEncoding": true, "contentMediaType": true, "contentSchema": true,
	"title": true, "description": true, "default": true, "deprecated": true, "readOnly": true, "writeOnly": true, "examples": true,
}

var schemaObjectKeywords = map[string]bool{
	"items": true, "contains": true, "unevaluatedItems": true, "additionalProperties": true, "unevaluatedProperties": true,
	"propertyNames": true, "not": true, "if": true, "then": true, "else": true, "contentSchema": true,
}
var schemaArrayKeywords = map[string]bool{"prefixItems": true, "allOf": true, "anyOf": true, "oneOf": true}
var schemaMapKeywords = map[string]bool{"$defs": true, "properties": true, "dependentSchemas": true}

func resolveSchemaURI(baseLocation, reference string, identifier bool) (string, bool, error) {
	label := "schema reference"
	if identifier {
		label = "schema $id"
	}
	if reference == "" && identifier {
		return "", false, errors.New("schema $id must not be empty")
	}
	if strings.IndexFunc(reference, func(character rune) bool {
		return character < 0x21 || character > 0x7e
	}) >= 0 {
		return "", false, fmt.Errorf("%s %q contains characters outside visible ASCII", label, reference)
	}
	if strings.Contains(reference, "\\") || strings.Contains(reference, "%") {
		return "", false, fmt.Errorf("%s %q uses encoded or backslash path syntax", label, reference)
	}
	relative, err := url.Parse(reference)
	if err != nil {
		return "", false, fmt.Errorf("parse %s %q: %w", label, reference, err)
	}
	if relative.Scheme != "" || relative.Host != "" || relative.User != nil || relative.Opaque != "" || strings.HasPrefix(relative.Path, "/") {
		return "", false, fmt.Errorf("%s %q is not package-relative", label, reference)
	}
	if relative.RawQuery != "" || (identifier && relative.Fragment != "") {
		return "", false, fmt.Errorf("%s %q contains a forbidden query or fragment", label, reference)
	}
	if err := validateSchemaFragment(relative.Fragment); err != nil {
		return "", false, fmt.Errorf("%s %q: %w", label, reference, err)
	}
	if relative.Path != "" {
		if err := validateRelativeReference(relative.Path); err != nil {
			return "", false, fmt.Errorf("%s %q: %w", label, reference, err)
		}
	}
	base, err := url.Parse(baseLocation)
	if err != nil {
		return "", false, fmt.Errorf("parse schema base: %w", err)
	}
	resolved := base.ResolveReference(relative)
	if resolved.Scheme != "https" || resolved.Host != "arop.package.invalid" || resolved.User != nil || resolved.RawQuery != "" {
		return "", false, fmt.Errorf("%s %q escapes the package", label, reference)
	}
	resolved.Fragment = ""
	resolvedPath := strings.TrimPrefix(resolved.Path, "/")
	if err := validateRelativeReference(resolvedPath); err != nil {
		return "", false, fmt.Errorf("%s %q: %w", label, reference, err)
	}
	return resolved.String(), relative.Path == "", nil
}

func cleanPackageRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve package root: %w", err)
	}
	volume := filepath.VolumeName(abs)
	relative := strings.TrimPrefix(abs, volume+string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		if err := requireExactDirectoryEntry(current, component); err != nil {
			return "", fmt.Errorf("inspect package root ancestor: %w", err)
		}
		current = filepath.Join(current, component)
		info, inspectErr := os.Lstat(current)
		if inspectErr != nil {
			return "", fmt.Errorf("inspect package root ancestor %s: %w", current, inspectErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("package root lexical ancestor must not be a symlink: %s", current)
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect package root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("package root must be a real directory, not a symlink")
	}
	return filepath.Clean(abs), nil
}

func validateRelativeReference(relative string) error {
	if relative == "" {
		return errors.New("path is empty")
	}
	if strings.Contains(relative, "\\") {
		return errors.New("backslashes are forbidden")
	}
	if path.IsAbs(relative) || filepath.IsAbs(relative) || regexp.MustCompile(`^[A-Za-z]:`).MatchString(relative) {
		return errors.New("absolute paths are forbidden")
	}
	for index, component := range strings.Split(relative, "/") {
		if component == ".." {
			return errors.New("path traversal is forbidden")
		}
		if component == "" && relative != "." {
			return errors.New("empty path components are forbidden")
		}
		if component != "." && !portablePathPart.MatchString(component) {
			return fmt.Errorf("path component %q is outside the portable grammar [A-Za-z0-9._~-]+", component)
		}
		if component == "." && index != 0 {
			return errors.New("dot path component is only allowed as a leading ./ prefix")
		}
	}
	cleaned := path.Clean(relative)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return errors.New("path must name a package file")
	}
	return nil
}

func validateSchemaFragment(fragment string) error {
	if fragment == "" {
		return nil
	}
	if strings.HasPrefix(fragment, "/") {
		if !portablePointer.MatchString(fragment) {
			return errors.New("JSON Pointer fragment contains non-portable characters")
		}
		for index := 0; index < len(fragment); index++ {
			if fragment[index] == '~' && (index+1 >= len(fragment) || (fragment[index+1] != '0' && fragment[index+1] != '1')) {
				return errors.New("JSON Pointer fragment contains an invalid ~ escape")
			}
		}
		return nil
	}
	if !portableAnchor.MatchString(fragment) {
		return errors.New("anchor fragment is outside the portable grammar")
	}
	return nil
}

func validatePortablePattern(pattern string) error {
	if len(pattern) > 512 || len(pattern) < 2 || pattern[0] != '^' || pattern[len(pattern)-1] != '$' {
		return errors.New("pattern must be a full-match ASCII expression (^...$) of at most 512 bytes")
	}
	for _, character := range pattern {
		if character < 0x20 || character > 0x7e {
			return errors.New("pattern must contain visible ASCII only")
		}
	}
	body := pattern[1 : len(pattern)-1]
	for index := 0; index < len(body); {
		if body[index] == '[' {
			end := strings.IndexByte(body[index+1:], ']')
			if end < 0 {
				return errors.New("pattern contains an unterminated character class")
			}
			end += index + 1
			if err := validatePortableCharacterClass(body[index+1 : end]); err != nil {
				return err
			}
			index = end + 1
		} else {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 _:/@,-", rune(body[index])) {
				return fmt.Errorf("pattern contains forbidden token %q", body[index])
			}
			index++
		}
		if index < len(body) && body[index] == '{' {
			end := strings.IndexByte(body[index+1:], '}')
			if end < 0 {
				return errors.New("pattern contains an unterminated fixed repetition")
			}
			end += index + 1
			digits := body[index+1 : end]
			count, err := strconv.Atoi(digits)
			if err != nil || digits == "" || (len(digits) > 1 && digits[0] == '0') || count < 1 || count > 256 {
				return errors.New("pattern repetition must be canonical {n} with 1 <= n <= 256")
			}
			index = end + 1
		}
	}
	return nil
}

func validatePortableCharacterClass(class string) error {
	if class == "" || class[0] == '^' {
		return errors.New("pattern character class must be non-empty and non-negated")
	}
	for index := 0; index < len(class); index++ {
		character := class[index]
		if isASCIIAlphaNumeric(character) || strings.ContainsRune(" _:/@,", rune(character)) {
			continue
		}
		if character == '-' {
			if index == 0 || index == len(class)-1 {
				continue
			}
			left, right := class[index-1], class[index+1]
			if samePortableRangeClass(left, right) && left <= right {
				continue
			}
		}
		return fmt.Errorf("pattern character class contains forbidden token %q", character)
	}
	return nil
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func samePortableRangeClass(left, right byte) bool {
	return left >= '0' && left <= '9' && right >= '0' && right <= '9' ||
		left >= 'A' && left <= 'Z' && right >= 'A' && right <= 'Z' ||
		left >= 'a' && left <= 'z' && right >= 'a' && right <= 'z'
}

func validatePortableDateTime(value any) error {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	matches := portableDateTime.FindStringSubmatch(text)
	if matches == nil {
		return errors.New("must use strict RFC 3339 date-time syntax")
	}
	numbers := make([]int, 8)
	for index := range numbers {
		if matches[index+1] == "" {
			continue
		}
		numbers[index], _ = strconv.Atoi(matches[index+1])
	}
	year, month, day := numbers[0], numbers[1], numbers[2]
	if month < 1 || month > 12 || day < 1 || day > daysInGregorianMonth(year, month) || numbers[3] > 23 || numbers[4] > 59 || numbers[5] > 59 || numbers[6] > 23 || numbers[7] > 59 {
		return errors.New("contains an invalid calendar date, time, or UTC offset")
	}
	return nil
}

func daysInGregorianMonth(year, month int) int {
	days := [...]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if month == 2 && (year%400 == 0 || year%4 == 0 && year%100 != 0) {
		return 29
	}
	if month < 1 || month > 12 {
		return 0
	}
	return days[month]
}

func rejectDangerousObjectKeys(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isDangerousObjectKey(key) {
				return fmt.Errorf("dangerous object key %q is forbidden", key)
			}
			if err := rejectDangerousObjectKeys(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectDangerousObjectKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func isDangerousObjectKey(key string) bool {
	return key == "__proto__" || key == "prototype" || key == "constructor"
}

func rejectDangerousSchemaPropertyNames(schema map[string]any) error {
	for _, keyword := range []string{"required"} {
		values, exists := schema[keyword].([]any)
		if !exists {
			continue
		}
		for _, value := range values {
			if name, ok := value.(string); ok && isDangerousObjectKey(name) {
				return fmt.Errorf("dangerous schema property name %q is forbidden in %s", name, keyword)
			}
		}
	}
	if dependencies, exists := schema["dependentRequired"].(map[string]any); exists {
		for _, value := range dependencies {
			values, _ := value.([]any)
			for _, item := range values {
				if name, ok := item.(string); ok && isDangerousObjectKey(name) {
					return fmt.Errorf("dangerous schema property name %q is forbidden in dependentRequired", name)
				}
			}
		}
	}
	return nil
}

func readPackageFile(root, relative string) ([]byte, error) {
	if err := validateRelativeReference(relative); err != nil {
		return nil, err
	}
	current := root
	components := strings.Split(path.Clean(relative), "/")
	for index, component := range components {
		if err := requireExactDirectoryEntry(current, component); err != nil {
			return nil, err
		}
		current = filepath.Join(current, filepath.FromSlash(component))
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink path component is forbidden: %s", strings.Join(components[:index+1], "/"))
		}
		if index < len(components)-1 && !info.IsDir() {
			return nil, fmt.Errorf("non-directory path component: %s", strings.Join(components[:index+1], "/"))
		}
		if index == len(components)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("package reference is not a regular file: %s", relative)
		}
	}
	return os.ReadFile(current)
}

func requireExactDirectoryEntry(parent, component string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", parent, err)
	}
	for _, entry := range entries {
		if entry.Name() == component {
			return nil
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), component) {
			return fmt.Errorf("path component %q does not exactly match on-disk name %q", component, entry.Name())
		}
	}
	return fmt.Errorf("path component %q does not exist below %s", component, parent)
}

func decodeSingleDocument(document []byte) (any, error) {
	return decodeStrictYAML(document)
}

func decodeDocument(document []byte, name string) (any, error) {
	if strings.EqualFold(filepath.Ext(name), ".json") {
		return decodeStrictJSON(document)
	}
	return decodeStrictYAML(document)
}

func decodeStrictYAML(document []byte) (any, error) {
	if !utf8.Valid(document) {
		return nil, errors.New("manifest is not valid UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(document))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if len(node.Content) == 0 {
		return nil, errors.New("manifest is empty")
	}
	var trailing yaml.Node
	err := decoder.Decode(&trailing)
	if err == nil {
		return nil, errors.New("manifest must contain exactly one YAML/JSON document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse trailing manifest content: %w", err)
	}
	return yamlJSONValue(node.Content[0])
}

func yamlJSONValue(node *yaml.Node) (any, error) {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return nil, errors.New("YAML aliases and anchors are forbidden")
	}
	if node.Style&yaml.TaggedStyle != 0 && !jsonCompatibleYAMLTag(node.Tag) {
		return nil, fmt.Errorf("explicit non-JSON YAML tag %q is forbidden", node.Tag)
	}
	switch node.Kind {
	case yaml.MappingNode:
		if node.Style&yaml.TaggedStyle != 0 && node.Tag != "!!map" && node.Tag != "tag:yaml.org,2002:map" {
			return nil, fmt.Errorf("mapping has incompatible explicit tag %q", node.Tag)
		}
		value := map[string]any{}
		seen := map[string]struct{}{}
		for index := 0; index < len(node.Content); index += 2 {
			keyValue, err := yamlJSONValue(node.Content[index])
			if err != nil {
				return nil, err
			}
			key, ok := keyValue.(string)
			if !ok {
				return nil, errors.New("manifest mapping keys must be strings")
			}
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("duplicate mapping key %q", key)
			}
			seen[key] = struct{}{}
			child, err := yamlJSONValue(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			value[key] = child
		}
		return value, nil
	case yaml.SequenceNode:
		if node.Style&yaml.TaggedStyle != 0 && node.Tag != "!!seq" && node.Tag != "tag:yaml.org,2002:seq" {
			return nil, fmt.Errorf("sequence has incompatible explicit tag %q", node.Tag)
		}
		value := make([]any, 0, len(node.Content))
		for _, childNode := range node.Content {
			child, err := yamlJSONValue(childNode)
			if err != nil {
				return nil, err
			}
			value = append(value, child)
		}
		return value, nil
	case yaml.ScalarNode:
		return yamlJSONScalar(node)
	default:
		return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
	}
}

func decodeStrictJSON(document []byte) (any, error) {
	if !utf8.Valid(document) {
		return nil, errors.New("JSON is not valid UTF-8")
	}
	if err := validateJSONSurrogateEscapes(document); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON contains trailing content")
		}
		return nil, err
	}
	return value, nil
}

func validateJSONSurrogateEscapes(document []byte) error {
	for index := 0; index < len(document); index++ {
		if document[index] != '"' {
			continue
		}
		index++
		for index < len(document) && document[index] != '"' {
			if document[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(document) {
				return errors.New("unterminated JSON string escape")
			}
			if document[index+1] != 'u' {
				index += 2
				continue
			}
			unit, err := parseJSONUTF16Unit(document, index)
			if err != nil {
				return err
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				next := index + 6
				if next+5 >= len(document) || document[next] != '\\' || document[next+1] != 'u' {
					return errors.New("JSON string contains an unpaired high surrogate escape")
				}
				low, err := parseJSONUTF16Unit(document, next)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return errors.New("JSON string contains an unpaired high surrogate escape")
				}
				index = next + 6
				continue
			}
			if unit >= 0xdc00 && unit <= 0xdfff {
				return errors.New("JSON string contains an unpaired low surrogate escape")
			}
			index += 6
		}
	}
	return nil
}

func parseJSONUTF16Unit(document []byte, escapeIndex int) (uint16, error) {
	if escapeIndex+5 >= len(document) {
		return 0, errors.New("truncated JSON unicode escape")
	}
	value, err := strconv.ParseUint(string(document[escapeIndex+2:escapeIndex+6]), 16, 16)
	if err != nil {
		return 0, errors.New("invalid JSON unicode escape")
	}
	return uint16(value), nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if number, ok := token.(json.Number); ok {
		return normalizeJSONNumber(number.String())
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		value := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is not a string")
			}
			if _, exists := value[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			child, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			value[key] = child
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid JSON object terminator")
		}
		return value, nil
	case '[':
		value := []any{}
		for decoder.More() {
			child, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			value = append(value, child)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid JSON array terminator")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func yamlJSONScalar(node *yaml.Node) (any, error) {
	explicit := node.Style&yaml.TaggedStyle != 0
	if explicit {
		switch node.Tag {
		case "!!str", "tag:yaml.org,2002:str":
			return node.Value, nil
		case "!!null", "tag:yaml.org,2002:null":
			if node.Value != "null" {
				return nil, fmt.Errorf("explicit null must use the JSON literal null, got %q", node.Value)
			}
			return nil, nil
		case "!!bool", "tag:yaml.org,2002:bool":
			if node.Value == "true" {
				return true, nil
			}
			if node.Value == "false" {
				return false, nil
			}
			return nil, fmt.Errorf("explicit boolean must use true or false, got %q", node.Value)
		case "!!int", "!!float", "tag:yaml.org,2002:int", "tag:yaml.org,2002:float":
			return normalizeJSONNumber(node.Value)
		}
	}
	if node.Style == yaml.DoubleQuotedStyle || node.Style == yaml.SingleQuotedStyle || node.Style == yaml.LiteralStyle || node.Style == yaml.FoldedStyle {
		return node.Value, nil
	}
	switch node.Value {
	case "null":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if jsonNumberPattern.MatchString(node.Value) {
		return normalizeJSONNumber(node.Value)
	}
	return node.Value, nil
}

func jsonCompatibleYAMLTag(tag string) bool {
	return map[string]bool{
		"!!map": true, "tag:yaml.org,2002:map": true,
		"!!seq": true, "tag:yaml.org,2002:seq": true,
		"!!str": true, "tag:yaml.org,2002:str": true,
		"!!null": true, "tag:yaml.org,2002:null": true,
		"!!bool": true, "tag:yaml.org,2002:bool": true,
		"!!int": true, "tag:yaml.org,2002:int": true,
		"!!float": true, "tag:yaml.org,2002:float": true,
	}[tag]
}

func normalizeJSONNumber(raw string) (float64, error) {
	if !jsonNumberPattern.MatchString(raw) {
		return 0, fmt.Errorf("non-JSON numeric scalar %q is not allowed by an explicit numeric tag", raw)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, fmt.Errorf("number %q is not a finite IEEE-754 value", raw)
	}
	significand := raw
	if exponent := strings.IndexAny(raw, "eE"); exponent >= 0 {
		significand = raw[:exponent]
	}
	if value == 0 && strings.ContainsAny(significand, "123456789") {
		return 0, fmt.Errorf("number %q underflows the interoperable IEEE-754 range", raw)
	}
	if math.Trunc(value) == value && math.Abs(value) > 9007199254740991 {
		return 0, fmt.Errorf("integer %q exceeds the interoperable safe range", raw)
	}
	return value, nil
}

func cloneObject(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed), true
		}
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case float64:
		if math.Trunc(typed) == typed && typed >= math.MinInt64 && typed <= math.MaxInt64 {
			return int64(typed), true
		}
	}
	return 0, false
}

func stringSet(value any) map[string]struct{} {
	result := map[string]struct{}{}
	values, _ := value.([]any)
	for _, item := range values {
		if text, ok := item.(string); ok {
			result[text] = struct{}{}
		}
	}
	return result
}

func newSchemaCompiler() *jsonschema.Compiler {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{Name: "date-time", Validate: validatePortableDateTime})
	compiler.AssertVocabs()
	compiler.UseRegexpEngine(compileECMAScript)
	compiler.UseLoader(rejectingSchemaLoader{})
	return compiler
}

type rejectingSchemaLoader struct{}

func (rejectingSchemaLoader) Load(rawURL string) (any, error) {
	return nil, fmt.Errorf("offline schema loader rejected unresolved reference %q", rawURL)
}

func compileECMAScript(value string) (jsonschema.Regexp, error) {
	translated, err := translateSchemaNonCapturingGroups(value)
	if err != nil {
		return nil, err
	}
	compiled, err := regexp.Compile(translated)
	if err != nil {
		return nil, fmt.Errorf("pattern is outside the AROP linear-time regexp profile: %w", err)
	}
	return compiled, nil
}

func translateSchemaNonCapturingGroups(value string) (string, error) {
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
				return "", errors.New("POSIX character classes are outside the AROP linear-time regexp profile")
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
			return "", errors.New("unescaped dot is outside the AROP linear-time regexp profile")
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
