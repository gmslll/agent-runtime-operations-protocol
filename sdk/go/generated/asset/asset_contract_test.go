package asset

import (
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
		decoded, err := DecodeAssetExchange(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := decoded.MarshalJSON()
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}
		if _, err := DecodeAssetExchange(encoded); err != nil {
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
		if _, err := DecodeAssetExchange(data); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
