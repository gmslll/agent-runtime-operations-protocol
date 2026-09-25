package publication

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestOfflineBundleValidatorAcceptsContractFixture(t *testing.T) {
	t.Parallel()
	archive, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "conformance", "fixtures", "contracts", "control-plane-publication", "fixtures", "bundles", "valid-agent-version.zip"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readSecureArchive(archive)
	if err != nil {
		t.Fatalf("read secure archive: %v", err)
	}
	canonical, _, err := canonicalManifest(entries[publicationManifestEntryName])
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	if got := digestBytes(canonical); got == "" {
		t.Fatal("empty canonical digest")
	}
	rootDigest, err := (RFC8785ManifestDigester{}).DigestManifest(context.Background(), canonical)
	if err != nil {
		t.Fatalf("digest canonical manifest: %v", err)
	}
	if rootDigest != digestBytes(canonical) {
		t.Fatalf("canonical digest mismatch got=%s want=%s document=%s", digestBytes(canonical), rootDigest, canonical)
	}
	bundle, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive)
	if err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	if bundle.AgentID == "" || bundle.Version == "" || !digestPattern.MatchString(bundle.ManifestDigest) || !digestPattern.MatchString(bundle.BundleSemanticDigest) {
		t.Fatalf("incomplete validated bundle: %+v", bundle)
	}
	if bytes.Contains(bundle.CanonicalManifest, []byte("\n")) {
		t.Fatal("canonical manifest retained whitespace")
	}
}

func TestOfflineBundleValidatorRejectsEndpointNetworkRefsAndUnsafeArchives(t *testing.T) {
	t.Parallel()
	manifest := minimalManifest(`,"endpoint":"https://runtime.example.invalid"`)
	for name, archive := range map[string][]byte{
		"endpoint":          zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(manifest)}),
		"network-reference": zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(minimalManifestWithRef("https://example.invalid/schema.json"))}),
		"parent-entry":      zipBundle(t, map[string][]byte{"agent-manifest.json": []byte(minimalManifest("")), "../secret": []byte("sentinel")}),
		"missing-manifest":  zipBundle(t, map[string][]byte{"schema.json": []byte(`{}`)}),
	} {
		name, archive := name, archive
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
}

func TestOfflineBundleValidatorRunsAllTrackedP11Archives(t *testing.T) {
	t.Parallel()
	directory := filepath.Join("..", "..", "..", "..", "..", "conformance", "fixtures", "contracts", "control-plane-publication", "fixtures", "bundles")
	paths, err := filepath.Glob(filepath.Join(directory, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 7 {
		t.Fatalf("tracked archive inventory too small: %d", len(paths))
	}
	for _, archivePath := range paths {
		archivePath := archivePath
		t.Run(filepath.Base(archivePath), func(t *testing.T) {
			archive, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive)
			if filepath.Base(archivePath) == "valid-agent-version.zip" && err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			if filepath.Base(archivePath) != "valid-agent-version.zip" && err == nil {
				t.Fatal("adversarial fixture accepted")
			}
		})
	}
}

func TestOfflineBundleValidatorRejectsZeroMultipleAndCentralMismatch(t *testing.T) {
	t.Parallel()
	manifest := []byte(minimalManifest(""))
	zero := zipBundle(t, map[string][]byte{"schema.json": []byte(`{}`)})
	multiple := zipBundle(t, map[string][]byte{"agent-manifest.json": manifest, "Agent-Manifest.json": manifest})
	valid := zipBundle(t, map[string][]byte{"agent-manifest.json": manifest})
	mutations := map[string]func([]byte){
		"disk-count": func(value []byte) {
			eocd := findEOCD(t, value)
			binary.LittleEndian.PutUint16(value[eocd+8:eocd+10], 2)
		},
		"flags":        func(value []byte) { central := findCentral(t, value); value[central+8] ^= 1 },
		"method":       func(value []byte) { central := findCentral(t, value); value[central+10] ^= 1 },
		"crc":          func(value []byte) { central := findCentral(t, value); value[central+16] ^= 1 },
		"compressed":   func(value []byte) { central := findCentral(t, value); value[central+20] ^= 1 },
		"uncompressed": func(value []byte) { central := findCentral(t, value); value[central+24] ^= 1 },
	}
	archives := map[string][]byte{"zero-manifest": zero, "multiple-manifest": multiple}
	for name, mutate := range mutations {
		candidate := bytes.Clone(valid)
		mutate(candidate)
		archives[name] = candidate
	}
	for name, archive := range archives {
		name, archive := name, archive
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), archive); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestBundleEntryAndCountLimitBoundaries(t *testing.T) {
	manifest := []byte(minimalManifest(""))
	exactEntries := map[string][]byte{"agent-manifest.json": manifest}
	for index := 1; index < maxBundleEntries; index++ {
		exactEntries["extras/e"+leftPad(index)+".bin"] = nil
	}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundle(t, exactEntries)); err != nil {
		t.Fatalf("exact entry limit rejected: %v", err)
	}
	exactEntries["extras/overflow.bin"] = nil
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundle(t, exactEntries)); err == nil {
		t.Fatal("entry limit+1 accepted")
	}
	exactSize := map[string][]byte{"agent-manifest.json": manifest, "payload.bin": make([]byte, maxBundleEntryBytes)}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundleInOrder(t, exactSize, sortedKeys(exactSize), zip.Store)); err != nil {
		t.Fatalf("exact entry size rejected: %v", err)
	}
	exactSize["payload.bin"] = make([]byte, maxBundleEntryBytes+1)
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundleInOrder(t, exactSize, sortedKeys(exactSize), zip.Store)); err == nil {
		t.Fatal("entry size+1 accepted")
	}
}

func TestBundleArchiveTotalAndCompressionLimits(t *testing.T) {
	manifest := []byte(minimalManifest(""))
	exactArchive := exactArchiveSize(t, manifest, MaxBundleBytes)
	if len(exactArchive) != MaxBundleBytes {
		t.Fatalf("archive bytes=%d", len(exactArchive))
	}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), exactArchive); err != nil {
		t.Fatalf("exact archive limit rejected: %v", err)
	}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), append(bytes.Clone(exactArchive), 0)); err == nil {
		t.Fatal("archive limit+1 accepted")
	}

	entries := map[string][]byte{"agent-manifest.json": manifest}
	for index := 0; index < 12; index++ {
		entries["payload/p"+leftPad(index)+".bin"] = compressiblePayload(maxBundleEntryBytes)
	}
	remaining := maxBundleUncompressedBytes - 12*maxBundleEntryBytes - len(manifest)
	entries["payload/remainder.bin"] = compressiblePayload(remaining)
	exactTotal := zipBundleInOrder(t, entries, sortedKeys(entries), zip.Deflate)
	if len(exactTotal) > MaxBundleBytes {
		t.Fatalf("test archive exceeds compressed limit: %d", len(exactTotal))
	}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), exactTotal); err != nil {
		t.Fatalf("exact total limit rejected: %v", err)
	}
	entries["payload/remainder.bin"] = append(entries["payload/remainder.bin"], 1)
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundleInOrder(t, entries, sortedKeys(entries), zip.Deflate)); err == nil {
		t.Fatal("total limit+1 accepted")
	}

	bomb := map[string][]byte{"agent-manifest.json": manifest, "bomb.bin": make([]byte, 1<<20)}
	if _, err := (OfflineBundleValidator{}).ValidateBundle(context.Background(), zipBundle(t, bomb)); err == nil {
		t.Fatal("compression ratio violation accepted")
	}
}

func TestBundleSemanticDigestIgnoresZipOrderAndMetadata(t *testing.T) {
	t.Parallel()
	entries := map[string][]byte{"agent-manifest.json": []byte(minimalManifestWithRef("./schemas/input.json")), "schemas/input.json": []byte(`{"type":"object","title":"a","properties":{"b":{"type":"string"},"a":{"type":"boolean"}}}`)}
	one := zipBundleInOrder(t, entries, []string{"agent-manifest.json", "schemas/input.json"}, zip.Store)
	formatted := map[string][]byte{"agent-manifest.json": entries["agent-manifest.json"], "schemas/input.json": []byte("{\n  \"properties\": {\"a\": {\"type\": \"boolean\"}, \"b\": {\"type\": \"string\"}},\n  \"title\": \"a\",\n  \"type\": \"object\"\n}")}
	two := zipBundleInOrder(t, formatted, []string{"schemas/input.json", "agent-manifest.json"}, zip.Deflate)
	validator := OfflineBundleValidator{}
	first, err := validator.ValidateBundle(context.Background(), one)
	if err != nil {
		t.Fatal(err)
	}
	second, err := validator.ValidateBundle(context.Background(), two)
	if err != nil {
		t.Fatal(err)
	}
	if first.BundleSemanticDigest != second.BundleSemanticDigest {
		t.Fatalf("semantic digest changed across packaging: %s != %s", first.BundleSemanticDigest, second.BundleSemanticDigest)
	}
	formatted["schemas/input.json"] = bytes.Replace(formatted["schemas/input.json"], []byte(`"title": "a"`), []byte(`"title": "b"`), 1)
	changed, err := validator.ValidateBundle(context.Background(), zipBundle(t, formatted))
	if err != nil {
		t.Fatal(err)
	}
	if changed.BundleSemanticDigest == first.BundleSemanticDigest {
		t.Fatal("one semantic schema edit was not bound")
	}
}

func TestBundleSemanticDigestCanonicalizesYAMLAndBindsOpaqueExtras(t *testing.T) {
	t.Parallel()
	manifest := []byte(minimalManifestWithRef("./schemas/input.schema"))
	oneEntries := map[string][]byte{"agent-manifest.json": manifest, "schemas/input.schema": []byte("type: object\nproperties:\n  value:\n    type: string\n"), "opaque.bin": []byte{0, 1, 2}}
	twoEntries := map[string][]byte{"agent-manifest.json": manifest, "schemas/input.schema": []byte("properties: {value: {type: string}}\ntype: object\n"), "opaque.bin": []byte{0, 1, 2}}
	validator := OfflineBundleValidator{}
	one, err := validator.ValidateBundle(context.Background(), zipBundle(t, oneEntries))
	if err != nil {
		t.Fatal(err)
	}
	two, err := validator.ValidateBundle(context.Background(), zipBundle(t, twoEntries))
	if err != nil {
		t.Fatal(err)
	}
	if one.BundleSemanticDigest != two.BundleSemanticDigest {
		t.Fatal("equivalent YAML changed semantic digest")
	}
	twoEntries["opaque.bin"] = []byte{0, 1, 3}
	changed, err := validator.ValidateBundle(context.Background(), zipBundle(t, twoEntries))
	if err != nil {
		t.Fatal(err)
	}
	if changed.BundleSemanticDigest == one.BundleSemanticDigest {
		t.Fatal("one-byte opaque change was not bound")
	}
}

func TestCanonicalManifestUsesRFC8785OrderingNumbersAndStrings(t *testing.T) {
	t.Parallel()
	document := []byte(`{"z":1.0,"a":"<\u2028","signature":"excluded"}`)
	canonical, _, err := canonicalManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(canonical), `{"a":"< ","z":1}`; got != want {
		t.Fatalf("canonical=%q want=%q", got, want)
	}
	digest, err := (RFC8785ManifestDigester{}).DigestManifest(context.Background(), document)
	if err != nil || digest != digestBytes(canonical) {
		t.Fatalf("digest=%s err=%v", digest, err)
	}
}

func zipBundle(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	return zipBundleInOrder(t, entries, names, zip.Deflate)
}

func sortedKeys(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func leftPad(value int) string {
	text := strconv.Itoa(value)
	return strings.Repeat("0", 4-len(text)) + text
}
func findEOCD(t *testing.T, archive []byte) int {
	t.Helper()
	for index := len(archive) - 22; index >= 0; index-- {
		if binary.LittleEndian.Uint32(archive[index:index+4]) == 0x06054b50 {
			return index
		}
	}
	t.Fatal("EOCD missing")
	return 0
}
func findCentral(t *testing.T, archive []byte) int {
	t.Helper()
	eocd := findEOCD(t, archive)
	return int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
}

func zipBundleInOrder(t *testing.T, entries map[string][]byte, names []string, method uint16) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: method}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func exactArchiveSize(t *testing.T, manifest []byte, target int) []byte {
	t.Helper()
	last := 2*1024*1024 - 64*1024
	entries := map[string][]byte{"agent-manifest.json": manifest, "a.bin": make([]byte, 4*1024*1024), "b.bin": make([]byte, 4*1024*1024), "c.bin": make([]byte, last)}
	base := zipBundleWithComment(t, entries, nil)
	delta := target - len(base)
	if delta < 0 || delta > 65535 {
		t.Fatalf("cannot tune archive boundary base=%d target=%d", len(base), target)
	}
	return zipBundleWithComment(t, entries, bytes.Repeat([]byte{'x'}, delta))
}

func zipBundleWithComment(t *testing.T, entries map[string][]byte, comment []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	if err := writer.SetComment(string(comment)); err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(entries) {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func compressiblePayload(size int) []byte {
	result := make([]byte, 0, size)
	state := uint32(0x12345678)
	for len(result) < size {
		block := make([]byte, 4096)
		for index := range block {
			state = state*1664525 + 1013904223
			block[index] = byte(state >> 24)
		}
		for repeat := 0; repeat < 8 && len(result) < size; repeat++ {
			remaining := size - len(result)
			if remaining < len(block) {
				result = append(result, block[:remaining]...)
			} else {
				result = append(result, block...)
			}
		}
	}
	return result
}

func minimalManifest(extra string) string {
	return `{"protocol":"arop/v1","kind":"AgentManifest","identity":{"id":"example.agent","version":"1.0.0","name":"Example","summary":"Example","owner":{"team":"example-team"}},"skills":[{"id":"default","name":"Default","invoke_modes":["params"],"input_schema":{"type":"object"},"output_schema":{"type":"object"}}],"execution":{"default_timeout_seconds":1,"max_timeout_seconds":2,"effects":{"level":"none","idempotency":"supported","human_confirmation":false},"capabilities":{}}` + extra + `}`
}

func minimalManifestWithRef(reference string) string {
	return strings.Replace(minimalManifest(""), `"input_schema":{"type":"object"}`, `"input_schema":{"$ref":"`+reference+`"}`, 1)
}
