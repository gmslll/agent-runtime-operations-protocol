package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const maxDefinitionBytes = 2 << 20

func LoadCatalog(root string) (*Catalog, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("resolve conformance root")
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return nil, errors.New("resolve conformance root")
	}
	scenarioValidator, err := loadValidator(resolvedRoot, "conformance/scenarios/schema.json")
	if err != nil {
		return nil, err
	}
	profileValidator, err := loadValidator(resolvedRoot, "conformance/profiles/schema.json")
	if err != nil {
		return nil, err
	}
	catalog := &Catalog{root: resolvedRoot, scenarios: map[string]catalogScenario{}, profiles: map[string]catalogProfile{}}
	if err := catalog.loadScenarios(scenarioValidator); err != nil {
		return nil, err
	}
	if err := catalog.loadProfiles(profileValidator); err != nil {
		return nil, err
	}
	if len(catalog.scenarios) == 0 || len(catalog.profiles) == 0 {
		return nil, errors.New("conformance catalog is empty")
	}
	if err := catalog.validateGraphs(); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (catalog *Catalog) loadScenarios(validator *jsonschema.Schema) error {
	base := filepath.Join(catalog.root, "conformance", "scenarios")
	return filepath.WalkDir(base, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("conformance scenario path is a symlink: %s", name)
		}
		if entry.IsDir() || filepath.Ext(name) != ".json" || filepath.Clean(name) == filepath.Join(base, "schema.json") {
			return nil
		}
		relative, err := filepath.Rel(catalog.root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		data, err := readRegular(catalog.root, relative, maxDefinitionBytes)
		if err != nil {
			return err
		}
		var scenario Scenario
		if err := protocolcore.DecodeAuthoring(data, &scenario); err != nil {
			return fmt.Errorf("decode scenario %s: %w", relative, err)
		}
		value, err := protocolcore.ParseJSON(data)
		if err != nil {
			return err
		}
		if err := validator.Validate(value); err != nil {
			return fmt.Errorf("validate scenario %s: %w", relative, err)
		}
		if _, exists := catalog.scenarios[scenario.ID]; exists {
			return fmt.Errorf("duplicate scenario ID %s", scenario.ID)
		}
		if err := validateRelativePath(scenario.Fixture.Path); err != nil {
			return fmt.Errorf("scenario %s fixture: %w", scenario.ID, err)
		}
		catalog.scenarios[scenario.ID] = catalogScenario{Scenario: scenario, Path: relative, Digest: digest(data)}
		return nil
	})
}

func (catalog *Catalog) loadProfiles(validator *jsonschema.Schema) error {
	base := filepath.Join(catalog.root, "conformance", "profiles")
	return filepath.WalkDir(base, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("conformance profile path is a symlink: %s", name)
		}
		extension := strings.ToLower(filepath.Ext(name))
		if entry.IsDir() || (extension != ".yaml" && extension != ".yml" && extension != ".json") || filepath.Clean(name) == filepath.Join(base, "schema.json") {
			return nil
		}
		relative, err := filepath.Rel(catalog.root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		data, err := readRegular(catalog.root, relative, maxDefinitionBytes)
		if err != nil {
			return err
		}
		var document ProfileCatalog
		if extension == ".json" {
			err = protocolcore.DecodeAuthoring(data, &document)
		} else {
			err = decodeYAML(data, &document)
		}
		if err != nil {
			return fmt.Errorf("decode profile catalog %s: %w", relative, err)
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return err
		}
		var value any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return err
		}
		if err := validator.Validate(value); err != nil {
			return fmt.Errorf("validate profile catalog %s: %w", relative, err)
		}
		for _, profile := range document.Profiles {
			if _, exists := catalog.profiles[profile.ID]; exists {
				return fmt.Errorf("duplicate profile ID %s", profile.ID)
			}
			catalog.profiles[profile.ID] = catalogProfile{Profile: profile, Path: relative, Digest: digest(data)}
		}
		return nil
	})
}

func (catalog *Catalog) validateGraphs() error {
	for id, scenario := range catalog.scenarios {
		seen := map[string]bool{}
		for _, dependency := range scenario.DependsOn {
			if dependency == id || seen[dependency] {
				return fmt.Errorf("scenario %s has duplicate or self dependency %s", id, dependency)
			}
			seen[dependency] = true
			if _, exists := catalog.scenarios[dependency]; !exists {
				return fmt.Errorf("scenario %s references unknown dependency %s", id, dependency)
			}
		}
	}
	if err := detectCycles(scenarioEdges(catalog.scenarios), "scenario"); err != nil {
		return err
	}
	for id, profile := range catalog.profiles {
		seenIncludes, seenScenarios := map[string]bool{}, map[string]bool{}
		for _, included := range profile.Includes {
			if included == id || seenIncludes[included] {
				return fmt.Errorf("profile %s has duplicate or self include %s", id, included)
			}
			seenIncludes[included] = true
			if _, exists := catalog.profiles[included]; !exists {
				return fmt.Errorf("profile %s includes unknown profile %s", id, included)
			}
		}
		for _, selected := range profile.Scenarios {
			if seenScenarios[selected.ID] {
				return fmt.Errorf("profile %s repeats scenario %s", id, selected.ID)
			}
			seenScenarios[selected.ID] = true
			if _, exists := catalog.scenarios[selected.ID]; !exists {
				return fmt.Errorf("profile %s references unknown scenario %s", id, selected.ID)
			}
		}
	}
	return detectCycles(profileEdges(catalog.profiles), "profile")
}

func (catalog *Catalog) Resolve(profileID string, filters []string) ([]resolvedScenario, string, error) {
	profile, exists := catalog.profiles[profileID]
	if !exists {
		return nil, "", fmt.Errorf("unknown conformance profile %s", profileID)
	}
	required := map[string]bool{}
	visitedProfiles := map[string]bool{}
	var collectProfile func(string)
	collectProfile = func(id string) {
		if visitedProfiles[id] {
			return
		}
		visitedProfiles[id] = true
		current := catalog.profiles[id]
		for _, included := range current.Includes {
			collectProfile(included)
		}
		for _, selected := range current.Scenarios {
			required[selected.ID] = required[selected.ID] || selected.Required
		}
	}
	collectProfile(profileID)
	if len(filters) != 0 {
		filtered := map[string]bool{}
		for _, id := range filters {
			value, exists := required[id]
			if !exists {
				return nil, "", fmt.Errorf("scenario filter %s is outside profile %s", id, profileID)
			}
			if _, duplicate := filtered[id]; duplicate {
				return nil, "", fmt.Errorf("duplicate scenario filter %s", id)
			}
			filtered[id] = value
		}
		required = filtered
	}
	selected := map[string]bool{}
	var includeDependencies func(string, bool)
	includeDependencies = func(id string, isRequired bool) {
		selected[id] = selected[id] || isRequired
		for _, dependency := range catalog.scenarios[id].DependsOn {
			includeDependencies(dependency, isRequired)
		}
	}
	for id, isRequired := range required {
		includeDependencies(id, isRequired)
	}
	if len(selected) == 0 {
		return nil, "", fmt.Errorf("profile %s has no scenario closure", profileID)
	}
	ordered := []resolvedScenario{}
	visited := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		dependencies := append([]string(nil), catalog.scenarios[id].DependsOn...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if _, exists := selected[dependency]; exists {
				visit(dependency)
			}
		}
		ordered = append(ordered, resolvedScenario{catalogScenario: catalog.scenarios[id], Required: selected[id]})
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		visit(id)
	}
	return ordered, profile.Digest, nil
}

func loadValidator(root, relative string) (*jsonschema.Schema, error) {
	document, err := readRegular(root, relative, maxDefinitionBytes)
	if err != nil {
		return nil, err
	}
	value, err := protocolcore.ParseJSON(document)
	if err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", relative, err)
	}
	id := "https://arop.invalid/" + relative
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(id, value); err != nil {
		return nil, fmt.Errorf("register schema %s: %w", relative, err)
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile schema %s: %w", relative, err)
	}
	return compiled, nil
}

func decodeYAML(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing YAML document")
		}
		return err
	}
	return nil
}

func readRegular(root, relative string, limit int64) ([]byte, error) {
	if err := validateRelativePath(relative); err != nil {
		return nil, err
	}
	current := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("read %s: unavailable", relative)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("read %s: symlink path rejected", relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("read %s: parent is not a directory", relative)
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("read %s: not a regular file", relative)
		}
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, fmt.Errorf("read %s: unavailable", relative)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, fmt.Errorf("read %s: invalid size or type", relative)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("read %s: unavailable or oversized", relative)
	}
	after, err := os.Lstat(current)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() {
		return nil, fmt.Errorf("read %s: changed while reading", relative)
	}
	return data, nil
}

func validateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "\\") || filepath.ToSlash(filepath.Clean(value)) != value || value == "." || strings.HasPrefix(value, "../") {
		return errors.New("path must be canonical and package-relative")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path contains an unsafe segment")
		}
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func scenarioEdges(values map[string]catalogScenario) map[string][]string {
	result := map[string][]string{}
	for id, value := range values {
		result[id] = append([]string(nil), value.DependsOn...)
	}
	return result
}

func profileEdges(values map[string]catalogProfile) map[string][]string {
	result := map[string][]string{}
	for id, value := range values {
		result[id] = append([]string(nil), value.Includes...)
	}
	return result
}

func detectCycles(edges map[string][]string, kind string) error {
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("%s graph contains a cycle at %s", kind, id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, next := range edges[id] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(edges))
	for id := range edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
