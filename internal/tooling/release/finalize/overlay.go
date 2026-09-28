package finalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type FileDelta struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Before string `json:"before_sha256"`
	After  string `json:"after_sha256"`
}

type Overlay struct {
	SchemaVersion int         `json:"schema_version"`
	Source        Freeze      `json:"source"`
	Logical       string      `json:"logical_version"`
	Files         []FileDelta `json:"files"`
	SHA256        string      `json:"sha256"`
}

var allowedOverlay = []*regexp.Regexp{
	regexp.MustCompile(`^VERSION$`), regexp.MustCompile(`^(?:go\.mod|go\.sum)$`), regexp.MustCompile(`^reference/control-plane/go\.(?:mod|sum)$`),
	regexp.MustCompile(`^sdk/python/(?:pyproject\.toml|[^/]+\.lock|dist/[^/]+\.(?:whl|tar\.gz))$`), regexp.MustCompile(`^sdk/typescript/(?:package\.json|package-lock\.json|dist/.*)$`),
	regexp.MustCompile(`^release/(?:oci|namespaces|checksums|metadata)\.(?:json|txt)$`),
}

func BuildOverlay(source Freeze, logical string, before, after map[string][]byte) (Overlay, error) {
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(logical) {
		return Overlay{}, errors.New("final logical version must be canonical and contain no rc suffix")
	}
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	keys := make([]string, 0, len(paths))
	for path := range paths {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	deltas := []FileDelta{}
	for _, path := range keys {
		left, leftOK := before[path]
		right, rightOK := after[path]
		if leftOK && rightOK && bytes.Equal(left, right) {
			continue
		}
		if !overlayPathAllowed(path) {
			return Overlay{}, fmt.Errorf("final overlay changes forbidden path %s", path)
		}
		status := "M"
		if !leftOK {
			status = "A"
		}
		if !rightOK {
			status = "D"
		}
		deltas = append(deltas, FileDelta{Path: path, Status: status, Before: digest(left), After: digest(right)})
	}
	if len(deltas) == 0 {
		return Overlay{}, errors.New("final overlay is empty")
	}
	canonical := struct {
		SchemaVersion int         `json:"schema_version"`
		Source        Freeze      `json:"source"`
		Logical       string      `json:"logical_version"`
		Files         []FileDelta `json:"files"`
	}{1, source, logical, deltas}
	data, _ := json.Marshal(canonical)
	return Overlay{1, source, logical, deltas, digest(data)}, nil
}

func overlayPathAllowed(path string) bool {
	if strings.Contains(path, "\\") || strings.HasPrefix(path, "/") || strings.Contains(path, "../") {
		return false
	}
	for _, pattern := range allowedOverlay {
		if pattern.MatchString(path) {
			return true
		}
	}
	return false
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
