// Package version maps the single AROP logical release version into ecosystem versions.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const PolicyPath = "spec/release/version-policy.yaml"

var logicalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.([1-9][0-9]*))?$`)

type Mapping struct {
	Prefix      string `json:"prefix" yaml:"prefix"`
	RCSeparator string `json:"rc_separator" yaml:"rc_separator"`
}

type Policy struct {
	SchemaVersion  int                `json:"schema_version" yaml:"schema_version"`
	LogicalPattern string             `json:"logical_pattern" yaml:"logical_pattern"`
	Mappings       map[string]Mapping `json:"mappings" yaml:"mappings"`
	ImmutableOrder []string           `json:"immutable_order" yaml:"immutable_order"`
	MutableChannel string             `json:"mutable_channel" yaml:"mutable_channel"`
}

type Versions struct {
	Logical      string `json:"logical"`
	Go           string `json:"go"`
	Python       string `json:"python"`
	NPM          string `json:"npm"`
	OCI          string `json:"oci"`
	CLI          string `json:"cli"`
	SchemaBundle string `json:"schema_bundle"`
	PolicyDigest string `json:"policy_digest"`
}

type Mapper struct {
	policy Policy
	digest string
}

func Load(root string) (Mapper, error) {
	path := filepath.Join(root, filepath.FromSlash(PolicyPath))
	data, err := os.ReadFile(path)
	if err != nil {
		return Mapper{}, fmt.Errorf("read version policy: %w", err)
	}
	parsed, err := structuredfile.Parse(data, "yaml")
	if err != nil {
		return Mapper{}, fmt.Errorf("parse version policy: %w", err)
	}
	if err := schema.ValidateFile(root, "spec/schemas/release-version-policy.schema.json", parsed); err != nil {
		return Mapper{}, fmt.Errorf("validate version policy: %w", err)
	}
	var policy Policy
	if err := structuredfile.Load(path, &policy); err != nil {
		return Mapper{}, fmt.Errorf("decode version policy: %w", err)
	}
	if err := validatePolicy(policy); err != nil {
		return Mapper{}, err
	}
	sum := sha256.Sum256(data)
	return Mapper{policy: policy, digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

func validatePolicy(policy Policy) error {
	wantOrder := []string{"go", "python", "npm", "oci", "cli", "schema_bundle"}
	if policy.SchemaVersion != 1 || policy.LogicalPattern != logicalPattern.String() || policy.MutableChannel != "stable" || !equalStrings(policy.ImmutableOrder, wantOrder) {
		return errors.New("version policy invariants are incomplete")
	}
	if len(policy.Mappings) != len(wantOrder) {
		return errors.New("version policy mapping inventory is not exact")
	}
	keys := make([]string, 0, len(policy.Mappings))
	for key := range policy.Mappings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := append([]string(nil), wantOrder...)
	sort.Strings(wantKeys)
	if !equalStrings(keys, wantKeys) {
		return errors.New("version policy mapping keys are not exact")
	}
	for key, mapping := range policy.Mappings {
		if key == "go" {
			if mapping.Prefix != "v" || mapping.RCSeparator != "-rc." {
				return errors.New("Go version mapping drifted")
			}
		} else if key == "python" {
			if mapping.Prefix != "" || mapping.RCSeparator != "rc" {
				return errors.New("Python version mapping drifted")
			}
		} else if mapping.Prefix != "" || mapping.RCSeparator != "-rc." {
			return fmt.Errorf("%s version mapping drifted", key)
		}
	}
	return nil
}

func (mapper Mapper) Map(logical string) (Versions, error) {
	match := logicalPattern.FindStringSubmatch(logical)
	if match == nil || strings.HasPrefix(logical, "v") || strings.Contains(logical, "+") {
		return Versions{}, errors.New("logical version is not canonical AROP SemVer")
	}
	base := strings.Join(match[1:4], ".")
	python := base
	if match[4] != "" {
		python += "rc" + match[4]
	}
	return Versions{
		Logical: logical, Go: "v" + logical, Python: python, NPM: logical,
		OCI: logical, CLI: logical, SchemaBundle: logical, PolicyDigest: mapper.digest,
	}, nil
}

func (mapper Mapper) Policy() Policy { return mapper.policy }
func (mapper Mapper) Digest() string { return mapper.digest }

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
