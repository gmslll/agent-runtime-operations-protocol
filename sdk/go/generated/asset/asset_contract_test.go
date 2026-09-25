package asset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetExchangeFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "examples", "assets")
	valid := []string{
		"valid/upload-request.json",
		"valid/download-request.json",
		"valid/upload-grant.json",
		"valid/download-grant.json",
	}
	for _, name := range valid {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeAssetFixture(data, false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}
		if _, err := decodeAssetFixture(encoded, false); err != nil {
			t.Fatalf("%s round trip: %v", name, err)
		}
	}
	invalid := []string{
		"invalid/absolute-broker-url.json",
		"invalid/secret-and-endpoint.json",
		"invalid/partial-asset.json",
		"forward/grant-future-field.json",
	}
	for _, name := range invalid {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeAssetFixture(data, false); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func decodeAssetFixture(data []byte, forward bool) (any, error) {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return nil, err
	}
	switch discriminator.Kind {
	case "upload_request":
		if forward {
			return DecodeUploadRequestForward(data)
		}
		return DecodeUploadRequest(data)
	case "download_request":
		if forward {
			return DecodeDownloadRequestForward(data)
		}
		return DecodeDownloadRequest(data)
	case "grant":
		if forward {
			return DecodeGrantResponseForward(data)
		}
		return DecodeGrantResponse(data)
	default:
		return nil, &unknownAssetKind{kind: discriminator.Kind}
	}
}

type unknownAssetKind struct{ kind string }

func (failure *unknownAssetKind) Error() string { return "unknown asset kind: " + failure.kind }
