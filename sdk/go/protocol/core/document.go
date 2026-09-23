// Package core contains the handwritten, implementation-independent AROP v1
// wire primitives. Generated protocol models build on this package; this
// package deliberately contains no Control Plane or framework dependencies.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// UnknownFieldAction controls how a forward-compatible consumer treats a
// field that its local typed model does not know.
type UnknownFieldAction uint8

const (
	UnknownReject UnknownFieldAction = iota
	UnknownIgnore
	UnknownPreserve
)

// UnknownFieldPolicy is called with an RFC 6901 JSON Pointer and the parsed
// value. Security-sensitive unknown semantics should return UnknownReject.
type UnknownFieldPolicy func(pointer string, value any) UnknownFieldAction

// ForwardResult returns preserved optional fields separately from the typed
// model. Keys are RFC 6901 JSON Pointers and values are canonical JSON values.
type ForwardResult struct {
	Preserved map[string]json.RawMessage
}

// ParseJSON parses exactly one JSON value, keeps numbers lossless as
// json.Number, and rejects duplicate object keys at every nesting depth.
func ParseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("parse JSON: input is not valid UTF-8")
	}
	if err := rejectUnpairedSurrogateEscapes(data); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("parse JSON: trailing JSON value")
		}
		return nil, fmt.Errorf("parse JSON: trailing data: %w", err)
	}
	return value, nil
}

// encoding/json replaces unpaired UTF-16 surrogate escapes with U+FFFD. A
// strict wire parser must reject them instead, otherwise the decoded value no
// longer represents the bytes the publisher authored.
func rejectUnpairedSurrogateEscapes(data []byte) error {
	inString := false
	for index := 0; index < len(data); {
		if !inString {
			if data[index] == '"' {
				inString = true
			}
			index++
			continue
		}
		switch data[index] {
		case '"':
			inString = false
			index++
		case '\\':
			if index+1 >= len(data) {
				return nil // the JSON decoder reports the incomplete escape
			}
			if data[index+1] != 'u' {
				index += 2
				continue
			}
			code, ok := parseHexQuad(data, index+2)
			if !ok {
				return nil // the JSON decoder reports the malformed escape
			}
			if code >= 0xd800 && code <= 0xdbff {
				next := index + 6
				if next+6 > len(data) || data[next] != '\\' || data[next+1] != 'u' {
					return errors.New("unpaired high-surrogate Unicode escape")
				}
				low, lowOK := parseHexQuad(data, next+2)
				if !lowOK || low < 0xdc00 || low > 0xdfff {
					return errors.New("unpaired high-surrogate Unicode escape")
				}
				index = next + 6
				continue
			}
			if code >= 0xdc00 && code <= 0xdfff {
				return errors.New("unpaired low-surrogate Unicode escape")
			}
			index += 6
		default:
			index++
		}
	}
	return nil
}

func parseHexQuad(data []byte, start int) (uint16, bool) {
	if start+4 > len(data) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(data[start:start+4]), 16, 16)
	return uint16(value), err == nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			child, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid object terminator")
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			child, err := decodeJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid array terminator")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// DecodeAuthoring performs strict publisher/author decoding. Unknown fields,
// duplicate keys, trailing values and a non-pointer destination are rejected.
func DecodeAuthoring(data []byte, destination any) error {
	if err := requireDestination(destination); err != nil {
		return err
	}
	value, err := ParseJSON(data)
	if err != nil {
		return err
	}
	filtered, err := filterUnknown(value, reflect.TypeOf(destination).Elem(), "", func(string, any) UnknownFieldAction { return UnknownReject }, map[string]json.RawMessage{})
	if err != nil {
		return fmt.Errorf("decode authoring document: %w", err)
	}
	normalized, err := json.Marshal(filtered)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode authoring document: %w", err)
	}
	return nil
}

// DecodeForward performs consumer decoding. Unknown fields are never silently
// accepted: each one must be explicitly rejected, ignored, or preserved by
// policy. Duplicate keys and trailing values remain invalid in every mode.
func DecodeForward(data []byte, destination any, policy UnknownFieldPolicy) (ForwardResult, error) {
	if policy == nil {
		return ForwardResult{}, errors.New("forward consumer requires an explicit unknown-field policy")
	}
	value, err := ParseJSON(data)
	if err != nil {
		return ForwardResult{}, err
	}
	if err := requireDestination(destination); err != nil {
		return ForwardResult{}, err
	}
	typeOfDestination := reflect.TypeOf(destination)
	preserved := map[string]json.RawMessage{}
	filtered, err := filterUnknown(value, typeOfDestination.Elem(), "", policy, preserved)
	if err != nil {
		return ForwardResult{}, err
	}
	normalized, err := json.Marshal(filtered)
	if err != nil {
		return ForwardResult{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ForwardResult{}, fmt.Errorf("decode forward document: %w", err)
	}
	return ForwardResult{Preserved: preserved}, nil
}

func requireDestination(destination any) error {
	typeOfDestination := reflect.TypeOf(destination)
	if typeOfDestination == nil || typeOfDestination.Kind() != reflect.Pointer || typeOfDestination.Elem().Kind() == reflect.Invalid || reflect.ValueOf(destination).IsNil() {
		return errors.New("destination must be a non-nil pointer")
	}
	return nil
}

func filterUnknown(value any, target reflect.Type, pointer string, policy UnknownFieldPolicy, preserved map[string]json.RawMessage) (any, error) {
	if value == nil {
		base := target
		for base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
		if base == reflect.TypeOf(json.RawMessage{}) || base.Kind() == reflect.Interface {
			return value, nil
		}
		return nil, fmt.Errorf("null at %s is not valid for %s", displayPointer(pointer), target)
	}
	if implementsJSONUnmarshaler(target) {
		// A custom unmarshaler owns its non-null wire shape. Pre-filtering it
		// using the implementation struct fields would reject valid custom JSON
		// before UnmarshalJSON can apply its own strict contract. Typed null was
		// already rejected above and cannot be swallowed by custom code.
		return value, nil
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == reflect.TypeOf(json.RawMessage{}) || target.Kind() == reflect.Interface {
		return value, nil
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		fields := jsonFields(target)
		filtered := make(map[string]any, len(object))
		for key, child := range object {
			field, known := fields[key]
			childPointer := pointer + "/" + escapePointer(key)
			if !known {
				action := policy(childPointer, child)
				switch action {
				case UnknownIgnore:
					continue
				case UnknownPreserve:
					if err := validateJCSValue(child); err != nil {
						return nil, fmt.Errorf("preserve field at %s: %w", childPointer, err)
					}
					encoded, err := json.Marshal(child)
					if err != nil {
						return nil, err
					}
					canonical, err := canonicalJSONValue(encoded)
					if err != nil {
						return nil, fmt.Errorf("canonicalize preserved field at %s: %w", childPointer, err)
					}
					preserved[childPointer] = canonical
					continue
				default:
					return nil, fmt.Errorf("unknown field at %s rejected by policy", childPointer)
				}
			}
			normalized, err := filterUnknown(child, field, childPointer, policy, preserved)
			if err != nil {
				return nil, err
			}
			filtered[key] = normalized
		}
		return filtered, nil
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return value, nil
		}
		if target.Kind() == reflect.Array && len(array) != target.Len() {
			return nil, fmt.Errorf("array at %s has length %d, want exactly %d", displayPointer(pointer), len(array), target.Len())
		}
		filtered := make([]any, len(array))
		for i, child := range array {
			normalized, err := filterUnknown(child, target.Elem(), fmt.Sprintf("%s/%d", pointer, i), policy, preserved)
			if err != nil {
				return nil, err
			}
			filtered[i] = normalized
		}
		return filtered, nil
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		filtered := make(map[string]any, len(object))
		for key, child := range object {
			childPointer := pointer + "/" + escapePointer(key)
			normalized, err := filterUnknown(child, target.Elem(), childPointer, policy, preserved)
			if err != nil {
				return nil, err
			}
			filtered[key] = normalized
		}
		return filtered, nil
	default:
		if number, ok := value.(json.Number); ok && isIntegerKind(target.Kind()) {
			// JSON Schema's integer type is mathematical rather than lexical:
			// 1, 1.0, and 1e0 are the same integer. encoding/json rejects the
			// latter two for Go integer fields, so normalize exact integral
			// values before the final typed decode. Non-integral and out-of-range
			// values are still rejected by encoding/json.
			if normalized, valid := normalizeIntegerLexeme(number.String()); valid {
				return json.Number(normalized), nil
			}
		}
		return value, nil
	}
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func implementsJSONUnmarshaler(target reflect.Type) bool {
	if target.Implements(jsonUnmarshalerType) {
		return true
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
		if target.Implements(jsonUnmarshalerType) {
			return true
		}
	}
	return target.Kind() != reflect.Pointer && reflect.PointerTo(target).Implements(jsonUnmarshalerType)
}

func validateJCSValue(value any) error {
	switch typed := value.(type) {
	case json.Number:
		number, err := strconv.ParseFloat(typed.String(), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return fmt.Errorf("number %q is not a finite IEEE-754 value", typed)
		}
		if number == 0 && nonzeroJSONSignificand(typed.String()) {
			return fmt.Errorf("number %q underflows the interoperable IEEE-754 range", typed)
		}
		if math.Trunc(number) == number && math.Abs(number) > 9007199254740991 {
			return fmt.Errorf("integer %q exceeds the interoperable exact range", typed)
		}
		canonical, err := canonicalJSONValue([]byte(typed.String()))
		if err != nil {
			return fmt.Errorf("canonicalize number %q: %w", typed, err)
		}
		if !equivalentDecimalLexemes(typed.String(), string(canonical)) {
			return fmt.Errorf("number %q cannot be represented by RFC 8785 without changing its mathematical value", typed)
		}
	case []any:
		for _, child := range typed {
			if err := validateJCSValue(child); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, child := range typed {
			if err := validateJCSValue(child); err != nil {
				return err
			}
		}
	}
	return nil
}

type normalizedDecimal struct {
	negative bool
	digits   string
	exponent int64
	zero     bool
}

func equivalentDecimalLexemes(left, right string) bool {
	leftValue, leftOK := normalizeDecimalLexeme(left)
	rightValue, rightOK := normalizeDecimalLexeme(right)
	return leftOK && rightOK && leftValue == rightValue
}

func normalizeDecimalLexeme(value string) (normalizedDecimal, bool) {
	normalized := normalizedDecimal{}
	if strings.HasPrefix(value, "-") {
		normalized.negative = true
		value = value[1:]
	}
	if value == "" {
		return normalized, false
	}
	if marker := strings.IndexAny(value, "eE"); marker >= 0 {
		exponent, ok := wideDecimalExponent(value[marker+1:])
		if !ok {
			return normalized, false
		}
		normalized.exponent = exponent
		value = value[:marker]
	}
	fractionDigits := 0
	if point := strings.IndexByte(value, '.'); point >= 0 {
		fractionDigits = len(value) - point - 1
		value = value[:point] + value[point+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return normalizedDecimal{zero: true}, true
	}
	normalized.exponent -= int64(fractionDigits)
	trailing := len(value) - len(strings.TrimRight(value, "0"))
	value = value[:len(value)-trailing]
	normalized.exponent += int64(trailing)
	normalized.digits = value
	return normalized, true
}

func wideDecimalExponent(value string) (int64, bool) {
	negative := false
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		negative = value[0] == '-'
		value = value[1:]
	}
	if value == "" {
		return 0, false
	}
	const limit int64 = 1 << 60
	var exponent int64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		if exponent <= limit/10 {
			exponent = exponent*10 + int64(digit-'0')
			if exponent > limit {
				exponent = limit
			}
		}
	}
	if negative {
		exponent = -exponent
	}
	return exponent, true
}

func nonzeroJSONSignificand(value string) bool {
	if marker := strings.IndexAny(value, "eE"); marker >= 0 {
		value = value[:marker]
	}
	return strings.IndexAny(value, "123456789") >= 0
}

func canonicalJSONValue(encoded []byte) ([]byte, error) {
	wrapped := make([]byte, 0, len(encoded)+6)
	wrapped = append(wrapped, `{"v":`...)
	wrapped = append(wrapped, encoded...)
	wrapped = append(wrapped, '}')
	canonical, err := jsoncanonicalizer.Transform(wrapped)
	if err != nil {
		return nil, err
	}
	const prefix = `{"v":`
	if !bytes.HasPrefix(canonical, []byte(prefix)) || len(canonical) <= len(prefix) || canonical[len(canonical)-1] != '}' {
		return nil, fmt.Errorf("unexpected RFC 8785 wrapper result")
	}
	return append([]byte(nil), canonical[len(prefix):len(canonical)-1]...), nil
}

// normalizeIntegerLexeme converts a mathematically integral JSON number into
// a base-10 integer without expanding attacker-controlled exponents. Twenty
// significant decimal digits are sufficient for every Go integer target; a
// larger result is left for encoding/json to reject as out of range.
func normalizeIntegerLexeme(value string) (string, bool) {
	negative := false
	if strings.HasPrefix(value, "-") {
		negative = true
		value = value[1:]
	}
	exponent := int64(0)
	if marker := strings.IndexAny(value, "eE"); marker >= 0 {
		var valid bool
		exponent, valid = boundedDecimalExponent(value[marker+1:])
		if !valid {
			return "", false
		}
		value = value[:marker]
	}
	fractionDigits := 0
	if point := strings.IndexByte(value, '.'); point >= 0 {
		fractionDigits = len(value) - point - 1
		value = value[:point] + value[point+1:]
	}
	firstNonzero := strings.IndexAny(value, "123456789")
	if firstNonzero < 0 {
		return "0", true
	}
	value = value[firstNonzero:]
	scale := exponent - int64(fractionDigits)
	if scale < 0 {
		remove := -scale
		if remove > int64(len(value)) {
			return "", false
		}
		cut := len(value) - int(remove)
		for _, digit := range value[cut:] {
			if digit != '0' {
				return "", false
			}
		}
		value = value[:cut]
		value = strings.TrimLeft(value, "0")
		if value == "" {
			return "0", true
		}
	} else {
		if scale > 20 || len(value)+int(scale) > 20 {
			return "", false
		}
		value += strings.Repeat("0", int(scale))
	}
	if len(value) > 20 {
		return "", false
	}
	if negative {
		return "-" + value, true
	}
	return value, true
}

func boundedDecimalExponent(value string) (int64, bool) {
	negative := false
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		negative = value[0] == '-'
		value = value[1:]
	}
	if value == "" {
		return 0, false
	}
	var exponent int64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		// Anything outside this bound cannot normalize to a representable Go
		// integer, but keeping a signed sentinel avoids proportional work.
		if exponent > 1000 {
			exponent = 1001
			continue
		}
		exponent = exponent*10 + int64(digit-'0')
	}
	if negative {
		exponent = -exponent
	}
	return exponent, true
}

func isIntegerKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return false
	}
}

func displayPointer(pointer string) string {
	if pointer == "" {
		return "/"
	}
	return pointer
}

func jsonFields(target reflect.Type) map[string]reflect.Type {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	candidates := map[string][]jsonFieldCandidate{}
	collectJSONFieldCandidates(target, 0, map[reflect.Type]bool{}, candidates)
	fields := make(map[string]reflect.Type, len(candidates))
	for name, named := range candidates {
		minimumDepth := named[0].depth
		atMinimum := make([]jsonFieldCandidate, 0, len(named))
		for _, candidate := range named {
			if candidate.depth < minimumDepth {
				minimumDepth = candidate.depth
				atMinimum = atMinimum[:0]
			}
			if candidate.depth == minimumDepth {
				atMinimum = append(atMinimum, candidate)
			}
		}
		tagged := atMinimum[:0]
		for _, candidate := range atMinimum {
			if candidate.tagged {
				tagged = append(tagged, candidate)
			}
		}
		if len(tagged) > 0 {
			atMinimum = tagged
		}
		if len(atMinimum) == 1 {
			fields[name] = atMinimum[0].fieldType
		}
	}
	return fields
}

type jsonFieldCandidate struct {
	fieldType reflect.Type
	depth     int
	tagged    bool
}

func collectJSONFieldCandidates(target reflect.Type, depth int, visiting map[reflect.Type]bool, candidates map[string][]jsonFieldCandidate) {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct || visiting[target] {
		return
	}
	visiting[target] = true
	defer delete(visiting, target)
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		rawTag, hasTag := field.Tag.Lookup("json")
		name, _, _ := strings.Cut(rawTag, ",")
		if name == "-" {
			continue
		}
		if name != "" && !isValidJSONTag(name) {
			name = ""
			hasTag = false
		}
		explicitName := hasTag && name != ""
		if field.Anonymous && name == "" {
			embeddedType := field.Type
			for embeddedType.Kind() == reflect.Pointer {
				embeddedType = embeddedType.Elem()
			}
			if embeddedType.Kind() == reflect.Struct {
				collectJSONFieldCandidates(embeddedType, depth+1, visiting, candidates)
				continue
			}
			if field.PkgPath != "" {
				continue
			}
			name = field.Name
		}
		if name == "" {
			name = field.Name
		}
		candidates[name] = append(candidates[name], jsonFieldCandidate{
			fieldType: field.Type,
			depth:     depth,
			tagged:    explicitName,
		})
	}
}

func isValidJSONTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, character := range tag {
		if strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", character) {
			continue
		}
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
