package generatedcodec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEventFixturesAndForwardBoundary(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "conformance", "fixtures", "events")
	cases := []struct {
		name           string
		valid, invalid string
		forward        string
		decode         func([]byte) (any, error)
		decodeForward  func([]byte) (any, error)
	}{
		{"envelope", "envelope.valid.json", "envelope.invalid.json", "envelope.forward.json", func(data []byte) (any, error) { return DecodeEventEnvelope(data) }, func(data []byte) (any, error) { return DecodeEventEnvelopeForward(data) }},
		{"lifecycle", "lifecycle.valid.json", "lifecycle.invalid.json", "lifecycle.forward.json", func(data []byte) (any, error) { return DecodeLifecycleTerminalData(data) }, func(data []byte) (any, error) { return DecodeLifecycleStateDataForward(data) }},
		{"output", "output.valid.json", "output.invalid.json", "output.forward.json", func(data []byte) (any, error) { return DecodeOutputDeltaData(data) }, func(data []byte) (any, error) { return DecodeOutputStateDataForward(data) }},
		{"progress", "progress.valid.json", "progress.invalid.json", "progress.forward.json", func(data []byte) (any, error) { return DecodeProgressEventData(data) }, func(data []byte) (any, error) { return DecodeProgressEventDataForward(data) }},
		{"usage", "usage.valid.json", "usage.invalid.json", "usage.forward.json", func(data []byte) (any, error) { return DecodeUsageEventData(data) }, func(data []byte) (any, error) { return DecodeUsageEventDataForward(data) }},
		{"session", "session.valid.json", "session.invalid.json", "session.forward.json", func(data []byte) (any, error) { return DecodeEventSession(data) }, func(data []byte) (any, error) { return DecodeEventSessionRequestForward(data) }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			valid := readEventFixture(t, root, item.valid)
			decoded, err := item.decode(valid)
			if err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = item.decode(encoded); err != nil {
				t.Fatalf("round trip rejected: %v", err)
			}
			if _, err = item.decode(readEventFixture(t, root, item.invalid)); err == nil {
				t.Fatal("invalid fixture accepted")
			}
			forward := readEventFixture(t, root, item.forward)
			if _, err = item.decode(forward); err == nil {
				t.Fatal("strict decoder accepted a forward field")
			}
			forwardValue, err := item.decodeForward(forward)
			if err != nil {
				t.Fatalf("forward decoder rejected fixture: %v", err)
			}
			preserved, err := json.Marshal(forwardValue)
			if err != nil || !json.Valid(preserved) {
				t.Fatalf("forward wire was not preserved: %v", err)
			}
		})
	}
}

func readEventFixture(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
