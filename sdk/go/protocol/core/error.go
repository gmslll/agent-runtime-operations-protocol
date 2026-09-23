package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

type ErrorCategory string
type ErrorCode string

const (
	ErrorValidation     ErrorCategory = "validation"
	ErrorAuthentication ErrorCategory = "authentication"
	ErrorAuthorization  ErrorCategory = "authorization"
	ErrorNotFound       ErrorCategory = "not_found"
	ErrorConflict       ErrorCategory = "conflict"
	ErrorCapacity       ErrorCategory = "capacity"
	ErrorDependency     ErrorCategory = "dependency"
	ErrorTimeout        ErrorCategory = "timeout"
	ErrorCancelled      ErrorCategory = "cancelled"
	ErrorProtocol       ErrorCategory = "protocol"
	ErrorInternal       ErrorCategory = "internal"
)

const (
	CodeDependencyUnavailable      ErrorCode = "DEPENDENCY_UNAVAILABLE"
	CodeEventIDConflict            ErrorCode = "EVENT_ID_CONFLICT"
	CodeInstanceGenerationFenced   ErrorCode = "INSTANCE_GENERATION_FENCED"
	CodeProtocolVersionUnsupported ErrorCode = "PROTOCOL_VERSION_UNSUPPORTED"
	CodeRegistryRevisionCompacted  ErrorCode = "REGISTRY_REVISION_COMPACTED"
	CodeResourceVersionConflict    ErrorCode = "RESOURCE_VERSION_CONFLICT"
	CodeStreamCursorExpired        ErrorCode = "STREAM_CURSOR_EXPIRED"
)

type WireError struct {
	Code              ErrorCode      `json:"code"`
	Category          ErrorCategory  `json:"category"`
	Message           string         `json:"message"`
	Retryable         bool           `json:"retryable"`
	RetryAfterSeconds *int           `json:"retry_after_seconds,omitempty"`
	Details           map[string]any `json:"details,omitempty"`
	TraceID           string         `json:"trace_id,omitempty"`
}

var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*$`)
var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (wire WireError) Error() string { return string(wire.Code) + ": " + wire.Message }

// UnmarshalJSON keeps the public WireError convenient while preserving the
// JSON Schema distinction between an absent field, JSON null, and a concrete
// zero value. It is intentionally strict so every typed decoding path has the
// same required/non-null behavior as error.schema.json.
func (wire *WireError) UnmarshalJSON(data []byte) error {
	if wire == nil {
		return fmt.Errorf("unmarshal WireError into nil receiver")
	}
	parsed, err := ParseJSON(data)
	if err != nil {
		return err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return fmt.Errorf("WireError must be a JSON object")
	}
	known := map[string]bool{
		"code": true, "category": true, "message": true, "retryable": true,
		"retry_after_seconds": true, "details": true, "trace_id": true,
	}
	for key, value := range object {
		if !known[key] {
			return fmt.Errorf("unknown WireError field %q", key)
		}
		if value == nil {
			return fmt.Errorf("WireError field %q must not be null", key)
		}
	}
	for _, required := range []string{"code", "category", "message", "retryable"} {
		if _, present := object[required]; !present {
			return fmt.Errorf("WireError required field %q is absent", required)
		}
	}
	if number, present := object["retry_after_seconds"].(json.Number); present {
		if normalized, valid := normalizeIntegerLexeme(number.String()); valid {
			object["retry_after_seconds"] = json.Number(normalized)
		}
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("normalize WireError: %w", err)
	}
	type plainWireError WireError
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	var decoded plainWireError
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode WireError: %w", err)
	}
	if err := WireError(decoded).Validate(); err != nil {
		return err
	}
	*wire = WireError(decoded)
	return nil
}

func (wire WireError) Validate() error {
	if len(wire.Code) < 3 || len(wire.Code) > 100 || !errorCodePattern.MatchString(string(wire.Code)) {
		return fmt.Errorf("invalid error code %q", wire.Code)
	}
	categories := map[ErrorCategory]bool{
		ErrorValidation: true, ErrorAuthentication: true, ErrorAuthorization: true,
		ErrorNotFound: true, ErrorConflict: true, ErrorCapacity: true,
		ErrorDependency: true, ErrorTimeout: true, ErrorCancelled: true,
		ErrorProtocol: true, ErrorInternal: true,
	}
	if !categories[wire.Category] {
		return fmt.Errorf("invalid error category %q", wire.Category)
	}
	messageLength := utf8.RuneCountInString(wire.Message)
	if !utf8.ValidString(wire.Message) || messageLength == 0 || messageLength > 2000 {
		return fmt.Errorf("error message must contain 1..2000 Unicode characters")
	}
	if wire.RetryAfterSeconds != nil && (*wire.RetryAfterSeconds < 0 || *wire.RetryAfterSeconds > 86400) {
		return fmt.Errorf("retry_after_seconds is outside 0..86400")
	}
	if wire.TraceID != "" && !traceIDPattern.MatchString(wire.TraceID) {
		return fmt.Errorf("invalid trace_id")
	}
	return nil
}
