package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "conformance", "fixtures", "run")
	valid := []struct{ name, kind string }{{"run-request.valid.json", "request"}, {"result.valid.json", "result"}, {"run-status.valid.json", "status"}, {"command.valid.json", "command"}}
	for _, item := range valid {
		data, err := os.ReadFile(filepath.Join(root, item.name))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeFixture(item.kind, data)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeFixture(item.kind, encoded); err != nil {
			t.Fatalf("%s roundtrip: %v", item.name, err)
		}
	}
	invalid := []struct{ name, kind string }{{"run-request.invalid.json", "request"}, {"result.invalid.json", "result"}, {"run-status.invalid.json", "status"}}
	for _, item := range invalid {
		data, err := os.ReadFile(filepath.Join(root, item.name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeFixture(item.kind, data); err == nil {
			t.Fatalf("%s was accepted", item.name)
		}
	}
}

func decodeFixture(kind string, data []byte) (any, error) {
	switch kind {
	case "request":
		return DecodeRunRequest(data)
	case "result":
		return DecodeRunResult(data)
	case "status":
		return DecodeRunStatus(data)
	case "command":
		return DecodeRunCommand(data)
	default:
		panic("unknown fixture")
	}
}
