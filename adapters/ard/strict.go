package ard

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

var knownFields = map[string]bool{"@context": true, "@id": true, "identifier": true, "displayName": true, "type": true, "url": true, "data": true, "representativeQueries": true, "capabilities": true, "description": true, "tags": true, "version": true, "updatedAt": true, "metadata": true, "TrustManifest": true}

func DecodeEntry(document []byte) (Entry, string, error) {
	var raw map[string]json.RawMessage
	if err := protocolcore.DecodeAuthoring(document, &raw); err != nil {
		return Entry{}, "", err
	}
	if raw == nil {
		return Entry{}, "", errors.New("ARD entry must be an object")
	}
	type wire Entry
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(document))
	if err := decoder.Decode(&value); err != nil {
		return Entry{}, "", err
	}
	entry := Entry(value)
	entry.Extensions = map[string]json.RawMessage{}
	for key, item := range raw {
		if !knownFields[key] {
			if !strings.Contains(key, ":") {
				return Entry{}, "", errors.New("unknown ARD term is not namespaced")
			}
			entry.Extensions[key] = append(json.RawMessage(nil), item...)
		}
	}
	if len(entry.Extensions) == 0 {
		entry.Extensions = nil
	}
	if err := validateEntry(entry); err != nil {
		return Entry{}, "", err
	}
	sourceDigest, err := digest(document)
	if err != nil {
		return Entry{}, "", err
	}
	return entry, sourceDigest, nil
}

func EncodeEntry(entry Entry) ([]byte, error) {
	if err := validateEntry(entry); err != nil {
		return nil, err
	}
	type wire Entry
	base, err := json.Marshal(wire(entry))
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(base, &object); err != nil {
		return nil, err
	}
	if entry.Data != nil {
		inline, err := json.Marshal(entry.Data)
		if err != nil {
			return nil, err
		}
		object["data"] = inline
	}
	for key, value := range entry.Extensions {
		if knownFields[key] {
			return nil, errors.New("ARD extension collides with a core term")
		}
		if !strings.Contains(key, ":") {
			return nil, errors.New("ARD extension term must be namespaced")
		}
		if !json.Valid(value) {
			return nil, errors.New("ARD extension is invalid JSON")
		}
		object[key] = append(json.RawMessage(nil), value...)
	}
	return json.MarshalIndent(object, "", "  ")
}
