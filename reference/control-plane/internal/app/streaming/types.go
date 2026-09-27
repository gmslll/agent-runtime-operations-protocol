package streaming

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

const (
	MaxSafeInteger  = uint64(9007199254740991)
	MaxReplayEvents = 256
)

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Request struct {
	Caller   run.Caller
	RunID    string
	After    uint64
	Metadata platform.RequestMetadata
}

func (request Request) Validate() error {
	if request.Caller.Validate() != nil || !prefixedUUID("run_", request.RunID) || request.After > MaxSafeInteger {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type Record struct {
	Sequence  uint64
	EventType string
	Envelope  json.RawMessage
}

func (record Record) Validate(runID string) error {
	if record.Sequence == 0 || record.Sequence > MaxSafeInteger || record.EventType == "" || len(record.EventType) > 128 || !jsonObject(record.Envelope) {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	decoded, err := streamwire.DecodeStreamEvent(record.Envelope)
	if err != nil || decoded.Runsequence == nil || uint64(*decoded.Runsequence) != record.Sequence || string(decoded.Runid) != runID || decoded.Type != record.EventType {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}

type Page struct {
	FirstAvailable uint64
	Latest         uint64
	Terminal       bool
	Records        []Record
}

func (page Page) Validate(runID string, after uint64) error {
	if page.FirstAvailable > MaxSafeInteger || page.Latest > MaxSafeInteger || len(page.Records) > MaxReplayEvents || page.FirstAvailable > page.Latest && page.FirstAvailable != page.Latest+1 {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if after == MaxSafeInteger {
		if len(page.Records) != 0 {
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		return nil
	}
	want := after + 1
	for _, record := range page.Records {
		if want == 0 || record.Sequence != want || record.Sequence > page.Latest || record.Validate(runID) != nil {
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		want++
	}
	return nil
}

func prefixedUUID(prefix, value string) bool {
	return len(value) == len(prefix)+36 && value[:len(prefix)] == prefix && uuidV7Pattern.MatchString(value[len(prefix):])
}

func jsonObject(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]json.RawMessage
	if decoder.Decode(&value) != nil || value == nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}
