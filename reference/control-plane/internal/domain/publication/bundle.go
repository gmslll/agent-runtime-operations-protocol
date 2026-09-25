package publication

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	protocolmanifest "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
	"go.yaml.in/yaml/v3"
)

const (
	maxBundleEntries             = 256
	maxBundleEntryBytes          = 4 * 1024 * 1024
	maxBundleUncompressedBytes   = 50 * 1024 * 1024
	maxBundleCompressionRatio    = 100
	publicationManifestEntryName = "agent-manifest.json"
)

// OfflineBundleValidator validates and extracts a publication archive without
// any network-capable dependency. Temporary extraction is private, bounded,
// rejects links and aliases before writing, and is deleted before return.
type OfflineBundleValidator struct{}

func (OfflineBundleValidator) ValidateBundle(ctx context.Context, archive []byte) (ValidatedBundle, error) {
	if err := ctx.Err(); err != nil {
		return ValidatedBundle{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	entries, err := readSecureArchive(archive)
	if err != nil {
		return ValidatedBundle{}, err
	}
	manifestBytes := entries[publicationManifestEntryName]
	canonical, manifestObject, err := canonicalManifest(manifestBytes)
	if err != nil {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	if _, exists := manifestObject["endpoint"]; exists {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	identity, ok := manifestObject["identity"].(map[string]any)
	if !ok {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	agentID, idOK := identity["id"].(string)
	version, versionOK := identity["version"].(string)
	if !idOK || !versionOK {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}

	temporaryRoot, err := os.MkdirTemp("", "arop-publication-bundle-")
	if err != nil {
		return ValidatedBundle{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	defer os.RemoveAll(temporaryRoot)
	canonicalRoot, err := filepath.EvalSymlinks(temporaryRoot)
	if err != nil {
		return ValidatedBundle{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if err := writePrivateArchive(canonicalRoot, entries); err != nil {
		return ValidatedBundle{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	manifestDigest, err := protocolmanifest.DigestPackageFile(canonicalRoot, publicationManifestEntryName)
	if err != nil {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	if got := digestBytes(canonical); got != manifestDigest {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	closure, err := buildOfflineClosure(entries)
	if err != nil {
		return ValidatedBundle{}, err
	}
	hosts, err := manifestAllowedHosts(manifestObject)
	if err != nil {
		return ValidatedBundle{}, err
	}
	bundleDigest, err := semanticBundleDigest(entries, closure.documents)
	if err != nil {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	bundle := ValidatedBundle{AgentID: agentID, Version: version, ManifestDigest: manifestDigest, BundleSemanticDigest: bundleDigest, CanonicalManifest: canonical, References: closure.references, AllowedHosts: hosts}
	if err := bundle.Validate(); err != nil {
		return ValidatedBundle{}, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	return bundle, nil
}

// RFC8785ManifestDigester is the pinned manifest digest implementation shared
// by validator, service and persistence boundaries.
type RFC8785ManifestDigester struct{}

func (RFC8785ManifestDigester) DigestManifest(ctx context.Context, document []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	canonical, _, err := canonicalManifest(document)
	if err != nil {
		return "", err
	}
	return digestBytes(canonical), nil
}

// SHA256RequestFingerprinter canonicalizes the complete fingerprint before
// hashing. It never includes the raw bearer credential or idempotency key.
type SHA256RequestFingerprinter struct{}

func (SHA256RequestFingerprinter) DigestRequest(ctx context.Context, fingerprint RequestFingerprint) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := fingerprint.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return "", err
	}
	return hexDigest(encoded), nil
}

func readSecureArchive(archive []byte) (map[string][]byte, error) {
	if len(archive) == 0 || len(archive) > MaxBundleBytes {
		return nil, NewError(CategoryCapacity, ReasonBundleTooLarge)
	}
	if err := verifyCentralDirectory(archive); err != nil {
		return nil, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxBundleEntries {
		return nil, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	entries := make(map[string][]byte, len(reader.File))
	folded := make(map[string]string, len(reader.File))
	var total uint64
	for _, file := range reader.File {
		name := file.Name
		if !validPortablePath(name) || strings.HasPrefix(name, "./") || strings.Contains(name, "%") {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		if _, exists := entries[name]; exists {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		lower := strings.ToLower(name)
		if prior, exists := folded[lower]; exists && prior != name {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		folded[lower] = name
		if file.Flags & ^uint16(0x0808) != 0 || file.Flags&1 != 0 || file.Mode()&os.ModeType != 0 || file.Method != zip.Store && file.Method != zip.Deflate || bytes.Contains(file.Extra, []byte("HARDLINK\x00")) {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		if file.UncompressedSize64 > maxBundleEntryBytes || total > math.MaxUint64-file.UncompressedSize64 || total+file.UncompressedSize64 > maxBundleUncompressedBytes {
			return nil, NewError(CategoryCapacity, ReasonBundleTooLarge)
		}
		if file.CompressedSize64 == 0 && file.UncompressedSize64 != 0 || file.CompressedSize64 != 0 && file.UncompressedSize64 > file.CompressedSize64*maxBundleCompressionRatio {
			return nil, NewError(CategoryCapacity, ReasonBundleTooLarge)
		}
		stream, openErr := file.Open()
		if openErr != nil {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, maxBundleEntryBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || uint64(len(content)) != file.UncompressedSize64 || len(content) > maxBundleEntryBytes {
			return nil, NewError(CategoryValidation, ReasonBundleInvalid)
		}
		total += uint64(len(content))
		entries[name] = content
	}
	if _, exists := entries[publicationManifestEntryName]; !exists {
		return nil, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	return entries, nil
}

func writePrivateArchive(root string, entries map[string][]byte) error {
	paths := make([]string, 0, len(entries))
	for name := range entries {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		target := filepath.Join(root, filepath.FromSlash(name))
		if relative, err := filepath.Rel(root, target); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("archive path escaped private root")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, entries[name], 0o600); err != nil {
			return err
		}
	}
	return nil
}

func canonicalManifest(document []byte) ([]byte, map[string]any, error) {
	value, err := decodeStrictJSONObject(document)
	if err != nil {
		return nil, nil, err
	}
	delete(value, "manifest_digest")
	delete(value, "signature")
	delete(value, "signatures")
	canonical, err := encodeCanonical(value)
	return canonical, value, err
}

func decodeStrictJSONObject(document []byte) (map[string]any, error) {
	value, err := decodeStrictJSON(document)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("JSON root is not an object")
	}
	return object, nil
}

func decodeStrictJSON(document []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("manifest has trailing JSON")
	}
	return value, nil
}

func decodeUniqueJSON(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("non-string object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			child, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			child, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

func encodeCanonical(value any) ([]byte, error) {
	var output bytes.Buffer
	if err := appendCanonical(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func appendCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		appendCanonicalString(output, typed)
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return errors.New("invalid JCS number")
		}
		output.WriteString(jcsNumber(number))
	case []any:
		output.WriteByte('[')
		for index, child := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := appendCanonical(output, child); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return compareUTF16(keys[i], keys[j]) < 0 })
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			appendCanonicalString(output, key)
			output.WriteByte(':')
			if err := appendCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical value %T", value)
	}
	return nil
}

func appendCanonicalString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if character < 0x20 {
				fmt.Fprintf(output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
}

func jcsNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	absolute, sign := value, ""
	if absolute < 0 {
		absolute, sign = -absolute, "-"
	}
	format := byte('e')
	if absolute >= 1e-6 && absolute < 1e21 {
		format = 'f'
	}
	formatted := strconv.FormatFloat(absolute, format, -1, 64)
	if exponent := strings.IndexByte(formatted, 'e'); exponent > 0 && exponent+2 < len(formatted) && formatted[exponent+2] == '0' {
		formatted = formatted[:exponent+2] + formatted[exponent+3:]
	}
	return sign + formatted
}

func compareUTF16(left, right string) int {
	a, b := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for index := 0; index < len(a) && index < len(b); index++ {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

// semanticBundleDigest covers every archive entry. Documents reached by the
// authoritative offline reference closure are RFC8785-canonicalized. Extra
// JSON-compatible JSON/YAML/schema documents receive the same treatment;
// unreferenced opaque/binary extras remain allowed but are bound byte-for-byte.
func semanticBundleDigest(entries map[string][]byte, closureDocuments map[string]any) (string, error) {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		if err := binary.Write(hash, binary.BigEndian, uint64(len(name))); err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(name))
		content := entries[name]
		semantic, semanticDocument := closureDocuments[name]
		if !semanticDocument && semanticDocumentExtension(name) {
			parsed, err := parseSemanticDocument(name, content)
			if err != nil {
				return "", err
			}
			semantic, semanticDocument = parsed, true
		}
		if semanticDocument {
			canonical, err := encodeCanonical(semantic)
			if err != nil {
				return "", err
			}
			content = canonical
		}
		checksum := sha256.Sum256(content)
		if err := binary.Write(hash, binary.BigEndian, uint64(len(content))); err != nil {
			return "", err
		}
		_, _ = hash.Write(checksum[:])
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func manifestAllowedHosts(manifest map[string]any) ([]AllowedHost, error) {
	execution, _ := manifest["execution"].(map[string]any)
	network, _ := execution["network"].(map[string]any)
	raw, exists := network["allowed_hosts"]
	if !exists {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, NewError(CategoryValidation, ReasonAllowedHostDenied)
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, NewError(CategoryValidation, ReasonAllowedHostDenied)
		}
		values = append(values, value)
	}
	return classifyAllowedHosts(values)
}

type offlineBundleClosure struct {
	entries    map[string][]byte
	documents  map[string]any
	references []BundleReference
	visited    map[string]bool
	seenRefs   map[string]bool
}

func buildOfflineClosure(entries map[string][]byte) (*offlineBundleClosure, error) {
	manifest, err := parseSemanticDocument(publicationManifestEntryName, entries[publicationManifestEntryName])
	if err != nil {
		return nil, NewError(CategoryValidation, ReasonBundleInvalid)
	}
	closure := &offlineBundleClosure{entries: entries, documents: map[string]any{publicationManifestEntryName: manifest}, visited: map[string]bool{}, seenRefs: map[string]bool{}}
	if err := closure.walkManifest(publicationManifestEntryName, manifest); err != nil {
		return nil, err
	}
	sort.Slice(closure.references, func(i, j int) bool {
		left, right := closure.references[i], closure.references[j]
		return left.SourcePath+"\x00"+string(left.Kind)+"\x00"+left.TargetPath+left.Fragment < right.SourcePath+"\x00"+string(right.Kind)+"\x00"+right.TargetPath+right.Fragment
	})
	return closure, nil
}

func (closure *offlineBundleClosure) walkManifest(source string, node any) error {
	switch typed := node.(type) {
	case []any:
		for _, child := range typed {
			if err := closure.walkManifest(source, child); err != nil {
				return err
			}
		}
	case map[string]any:
		if raw, hasRef := typed["schema_ref"]; hasRef {
			if _, hasDigest := typed["schema_digest"]; hasDigest {
				if err := closure.follow(source, ReferenceExtensionSchemaRef, raw); err != nil {
					return err
				}
			}
		}
		for key, child := range typed {
			switch key {
			case "input_schema", "output_schema":
				if err := closure.walkSchema(source, child); err != nil {
					return err
				}
			case "data":
				if typed["schema_ref"] != nil {
					continue
				}
				if err := closure.walkManifest(source, child); err != nil {
					return err
				}
			case "schema_ref", "schema_digest":
				continue
			default:
				if err := closure.walkManifest(source, child); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (closure *offlineBundleClosure) walkSchema(source string, node any) error {
	switch typed := node.(type) {
	case []any:
		for _, child := range typed {
			if err := closure.walkSchema(source, child); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, key := range []string{"$ref", "$dynamicRef"} {
			if raw, exists := typed[key]; exists {
				if err := closure.follow(source, ReferenceSchemaRef, raw); err != nil {
					return err
				}
			}
		}
		if raw, exists := typed["$id"]; exists {
			if err := closure.record(source, ReferenceSchemaID, raw); err != nil {
				return err
			}
		}
		for key, child := range typed {
			switch {
			case key == "$ref" || key == "$dynamicRef" || key == "$id":
				continue
			case schemaValueKeyword(key):
				if err := closure.walkSchema(source, child); err != nil {
					return err
				}
			case schemaMapKeyword(key):
				children, ok := child.(map[string]any)
				if !ok {
					continue
				}
				for _, schemaChild := range children {
					if err := closure.walkSchema(source, schemaChild); err != nil {
						return err
					}
				}
			case schemaArrayKeyword(key):
				children, ok := child.([]any)
				if !ok {
					continue
				}
				for _, schemaChild := range children {
					if err := closure.walkSchema(source, schemaChild); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func schemaValueKeyword(key string) bool {
	switch key {
	case "additionalProperties", "unevaluatedProperties", "items", "contains", "unevaluatedItems", "propertyNames", "not", "if", "then", "else", "contentSchema":
		return true
	default:
		return false
	}
}
func schemaMapKeyword(key string) bool {
	return key == "$defs" || key == "definitions" || key == "properties" || key == "patternProperties" || key == "dependentSchemas"
}
func schemaArrayKeyword(key string) bool {
	return key == "allOf" || key == "anyOf" || key == "oneOf" || key == "prefixItems"
}

func (closure *offlineBundleClosure) record(source string, kind ReferenceKind, raw any) error {
	text, ok := raw.(string)
	if !ok {
		return NewError(CategoryValidation, ReasonReferenceDenied)
	}
	reference, err := classifyReference(source, kind, text)
	if err != nil {
		return err
	}
	key := reference.SourcePath + "\x00" + string(reference.Kind) + "\x00" + reference.TargetPath + "\x00" + reference.Fragment
	if !closure.seenRefs[key] {
		closure.seenRefs[key] = true
		closure.references = append(closure.references, reference)
	}
	return nil
}

func (closure *offlineBundleClosure) follow(source string, kind ReferenceKind, raw any) error {
	if err := closure.record(source, kind, raw); err != nil {
		return err
	}
	text := raw.(string)
	reference, _ := classifyReference(source, kind, text)
	target := reference.TargetPath
	if reference.Class == ReferenceDocumentFragment {
		target = source
	}
	if target == "" {
		return NewError(CategoryValidation, ReasonReferenceDenied)
	}
	if closure.visited[target] {
		return nil
	}
	document, exists := closure.documents[target]
	if !exists {
		content, found := closure.entries[target]
		if !found {
			return NewError(CategoryValidation, ReasonReferenceDenied)
		}
		parsed, err := parseSemanticDocument(target, content)
		if err != nil {
			return NewError(CategoryValidation, ReasonBundleInvalid)
		}
		document = parsed
		closure.documents[target] = parsed
	}
	closure.visited[target] = true
	return closure.walkSchema(target, document)
}

func semanticDocumentExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".yaml", ".yml", ".schema":
		return true
	default:
		return false
	}
}

func parseSemanticDocument(name string, content []byte) (any, error) {
	if strings.EqualFold(path.Ext(name), ".json") || name == publicationManifestEntryName {
		return decodeStrictJSON(content)
	}
	var root yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("semantic YAML must contain exactly one document")
	}
	if len(root.Content) != 1 {
		return nil, errors.New("semantic YAML root is invalid")
	}
	return yamlNodeValue(root.Content[0])
}

func yamlNodeValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		object := map[string]any{}
		for index := 0; index < len(node.Content); index += 2 {
			keyNode := node.Content[index]
			if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
				return nil, errors.New("YAML object key must be a string")
			}
			if _, exists := object[keyNode.Value]; exists {
				return nil, errors.New("duplicate YAML object key")
			}
			value, err := yamlNodeValue(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			object[keyNode.Value] = value
		}
		return object, nil
	case yaml.SequenceNode:
		array := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlNodeValue(child)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		return array, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			return node.Value == "true", nil
		case "!!str":
			return node.Value, nil
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		default:
			return nil, errors.New("YAML value is not JSON-compatible")
		}
	default:
		return nil, errors.New("YAML aliases and non-data nodes are forbidden")
	}
}

func classifyReference(source string, kind ReferenceKind, raw string) (BundleReference, error) {
	reference := BundleReference{SourcePath: source, Kind: kind}
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\%") {
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() {
		reference.Class = ReferenceNetwork
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	if parsed.Host != "" || strings.HasPrefix(raw, "//") {
		reference.Class = ReferenceAuthority
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	if parsed.RawQuery != "" {
		reference.Class = ReferenceNonPortable
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	if parsed.Fragment != "" {
		reference.Fragment = "#" + parsed.Fragment
		if !validPortableFragment(reference.Fragment) {
			return reference, NewError(CategoryValidation, ReasonReferenceDenied)
		}
	}
	if parsed.Path == "" {
		reference.Class = ReferenceDocumentFragment
		if err := reference.Validate(); err != nil {
			return reference, NewError(CategoryValidation, ReasonReferenceDenied)
		}
		return reference, nil
	}
	if strings.HasPrefix(parsed.Path, "/") {
		reference.Class = ReferenceAbsolutePath
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	target := path.Clean(path.Join(path.Dir(source), strings.TrimPrefix(parsed.Path, "./")))
	if target == ".." || strings.HasPrefix(target, "../") || !validPortablePath(target) {
		reference.Class = ReferenceParentEscape
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	reference.Class, reference.TargetPath = ReferenceBundleRelative, target
	if err := reference.Validate(); err != nil {
		return reference, NewError(CategoryValidation, ReasonReferenceDenied)
	}
	return reference, nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func hexDigest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func verifyCentralDirectory(archive []byte) error {
	eocd := -1
	start := len(archive) - 22 - 65535
	if start < 0 {
		start = 0
	}
	for index := len(archive) - 22; index >= start; index-- {
		if index >= 0 && binary.LittleEndian.Uint32(archive[index:index+4]) == 0x06054b50 {
			eocd = index
			break
		}
	}
	if eocd < 0 || eocd+22 > len(archive) {
		return errors.New("zip end record absent")
	}
	comment := int(binary.LittleEndian.Uint16(archive[eocd+20 : eocd+22]))
	diskNumber := binary.LittleEndian.Uint16(archive[eocd+4 : eocd+6])
	centralDisk := binary.LittleEndian.Uint16(archive[eocd+6 : eocd+8])
	countDisk := int(binary.LittleEndian.Uint16(archive[eocd+8 : eocd+10]))
	count := int(binary.LittleEndian.Uint16(archive[eocd+10 : eocd+12]))
	if eocd+22+comment != len(archive) || diskNumber != 0 || centralDisk != 0 || countDisk != count {
		return errors.New("multi-disk or trailing zip")
	}
	offset := int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
	size := int(binary.LittleEndian.Uint32(archive[eocd+12 : eocd+16]))
	if count == 0xffff || offset < 0 || size < 0 || offset+size != eocd {
		return errors.New("zip central bounds")
	}
	position := offset
	locals := map[int]bool{}
	type byteRange struct{ start, end int }
	ranges := make([]byteRange, 0, count)
	for index := 0; index < count; index++ {
		if position+46 > eocd || binary.LittleEndian.Uint32(archive[position:position+4]) != 0x02014b50 {
			return errors.New("zip central entry")
		}
		nameLen := int(binary.LittleEndian.Uint16(archive[position+28 : position+30]))
		extraLen := int(binary.LittleEndian.Uint16(archive[position+30 : position+32]))
		commentLen := int(binary.LittleEndian.Uint16(archive[position+32 : position+34]))
		local := int(binary.LittleEndian.Uint32(archive[position+42 : position+46]))
		end := position + 46 + nameLen + extraLen + commentLen
		if end > eocd || local < 0 || local+30 > offset || locals[local] || binary.LittleEndian.Uint32(archive[local:local+4]) != 0x04034b50 {
			return errors.New("zip local bounds")
		}
		locals[local] = true
		flags := binary.LittleEndian.Uint16(archive[position+8 : position+10])
		method := binary.LittleEndian.Uint16(archive[position+10 : position+12])
		crc := binary.LittleEndian.Uint32(archive[position+16 : position+20])
		compressed := binary.LittleEndian.Uint32(archive[position+20 : position+24])
		uncompressed := binary.LittleEndian.Uint32(archive[position+24 : position+28])
		localFlags := binary.LittleEndian.Uint16(archive[local+6 : local+8])
		localMethod := binary.LittleEndian.Uint16(archive[local+8 : local+10])
		localCRC := binary.LittleEndian.Uint32(archive[local+14 : local+18])
		localCompressed := binary.LittleEndian.Uint32(archive[local+18 : local+22])
		localUncompressed := binary.LittleEndian.Uint32(archive[local+22 : local+26])
		localNameLen := int(binary.LittleEndian.Uint16(archive[local+26 : local+28]))
		localExtraLen := int(binary.LittleEndian.Uint16(archive[local+28 : local+30]))
		localEnd := local + 30 + localNameLen + localExtraLen
		if localEnd > offset || flags != localFlags || method != localMethod || nameLen != localNameLen || !bytes.Equal(archive[position+46:position+46+nameLen], archive[local+30:local+30+localNameLen]) {
			return errors.New("zip central/local mismatch")
		}
		dataEnd64 := uint64(localEnd) + uint64(compressed)
		if dataEnd64 > uint64(offset) {
			return errors.New("zip compressed data bounds")
		}
		dataEnd := int(dataEnd64)
		if flags&8 == 0 {
			if localCRC != crc || localCompressed != compressed || localUncompressed != uncompressed {
				return errors.New("zip local digest-size mismatch")
			}
		} else {
			if localCRC != 0 || localCompressed != 0 || localUncompressed != 0 {
				return errors.New("zip descriptor local fields are nonzero")
			}
			descriptorEnd, err := verifyDataDescriptor(archive, dataEnd, offset, crc, compressed, uncompressed)
			if err != nil {
				return err
			}
			dataEnd = descriptorEnd
		}
		ranges = append(ranges, byteRange{start: local, end: dataEnd})
		position = end
	}
	if position != eocd {
		return errors.New("zip central trailing bytes")
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	for index, current := range ranges {
		if current.start < 0 || current.end <= current.start || current.end > offset {
			return errors.New("zip local data range invalid")
		}
		if index == 0 && current.start != 0 || index > 0 && current.start != ranges[index-1].end {
			return errors.New("zip local data ranges overlap or contain unbound bytes")
		}
	}
	if len(ranges) == 0 || ranges[len(ranges)-1].end != offset {
		return errors.New("zip local data does not meet central directory")
	}
	return nil
}

func verifyDataDescriptor(archive []byte, start, limit int, crc, compressed, uncompressed uint32) (int, error) {
	if start < 0 || start+12 > limit {
		return 0, errors.New("zip data descriptor absent")
	}
	position := start
	if position+4 <= limit && binary.LittleEndian.Uint32(archive[position:position+4]) == 0x08074b50 {
		position += 4
	}
	if position+12 > limit || binary.LittleEndian.Uint32(archive[position:position+4]) != crc || binary.LittleEndian.Uint32(archive[position+4:position+8]) != compressed || binary.LittleEndian.Uint32(archive[position+8:position+12]) != uncompressed {
		return 0, errors.New("zip data descriptor mismatch")
	}
	return position + 12, nil
}
