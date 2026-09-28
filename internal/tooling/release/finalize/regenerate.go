package finalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
)

type PublicConfig struct {
	GoModule       string `json:"go_module"`
	PythonPackage  string `json:"python_package"`
	NPMPackage     string `json:"npm_package"`
	OCIRepository  string `json:"oci_repository"`
	SchemaBaseURI  string `json:"schema_base_uri"`
	EventNamespace string `json:"event_namespace"`
}

type Mutation struct {
	Path   string `json:"path"`
	Before string `json:"before_sha256"`
	After  string `json:"after_sha256"`
	Bytes  []byte `json:"-"`
}

var (
	goModulePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+/[A-Za-z0-9._~/-]+$`)
	pythonPattern   = regexp.MustCompile(`^[a-z0-9]+(?:[-_.][a-z0-9]+)*$`)
	npmPattern      = regexp.MustCompile(`^@[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
	ociPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+(?::[0-9]+)?/[a-z0-9][a-z0-9._/-]*$`)
)

func ValidatePublicConfig(config PublicConfig) error {
	values := []string{config.GoModule, config.PythonPackage, config.NPMPackage, config.OCIRepository, config.SchemaBaseURI, config.EventNamespace}
	for _, value := range values {
		lower := strings.ToLower(value)
		if value == "" || strings.Contains(lower, "example") || strings.Contains(lower, "placeholder") || strings.Contains(lower, "todo") || strings.Contains(lower, "replace") || strings.ContainsAny(value, " \t\r\n") {
			return errors.New("public namespace contains an empty, whitespace or placeholder value")
		}
	}
	if !goModulePattern.MatchString(config.GoModule) || !pythonPattern.MatchString(config.PythonPackage) || !npmPattern.MatchString(config.NPMPackage) || !ociPattern.MatchString(config.OCIRepository) || !strings.HasPrefix(config.SchemaBaseURI, "https://") || strings.Contains(config.SchemaBaseURI, "#") || strings.Contains(config.SchemaBaseURI, "?") || !strings.Contains(config.EventNamespace, ".") {
		return errors.New("public namespace syntax is invalid")
	}
	return nil
}

// RegenerateTree deterministically updates only the frozen public metadata surface.
func RegenerateTree(files map[string][]byte, config PublicConfig, mapper *versionpolicy.Mapper, logical string) ([]Mutation, error) {
	if err := ValidatePublicConfig(config); err != nil {
		return nil, err
	}
	versions, err := mapper.Map(logical)
	if err != nil {
		return nil, err
	}
	replacements := map[string]map[string]string{
		"VERSION":                        {"__WHOLE__": logical},
		"go.mod":                         {"__MODULE__": config.GoModule},
		"reference/control-plane/go.mod": {"__ROOT_MODULE__": config.GoModule, "__ROOT_VERSION__": versions.Go},
		"sdk/python/pyproject.toml":      {"__PYTHON_NAME__": config.PythonPackage, "__PYTHON_VERSION__": versions.Python},
		"sdk/typescript/package.json":    {"__NPM_NAME__": config.NPMPackage, "__NPM_VERSION__": versions.NPM},
		"release/oci.json":               {"__OCI_REPOSITORY__": config.OCIRepository, "__OCI_VERSION__": versions.OCI},
		"release/namespaces.json":        {"__SCHEMA_BASE_URI__": config.SchemaBaseURI, "__EVENT_NAMESPACE__": config.EventNamespace},
	}
	paths := make([]string, 0, len(replacements))
	for path := range replacements {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	mutations := []Mutation{}
	for _, path := range paths {
		before, exists := files[path]
		if !exists {
			return nil, fmt.Errorf("required freeze input is missing: %s", path)
		}
		after := append([]byte{}, before...)
		if whole, ok := replacements[path]["__WHOLE__"]; ok {
			after = []byte(whole + "\n")
		} else {
			for token, value := range replacements[path] {
				count := bytes.Count(after, []byte(token))
				if count == 0 && bytes.Contains(after, []byte(value)) {
					continue
				}
				if count != 1 {
					return nil, fmt.Errorf("%s must contain token %s exactly once", path, token)
				}
				after = bytes.ReplaceAll(after, []byte(token), []byte(value))
			}
		}
		if bytes.Contains(bytes.ToLower(after), []byte("placeholder")) || bytes.Contains(after, []byte("__")) {
			return nil, fmt.Errorf("%s retains a placeholder", path)
		}
		if !bytes.Equal(before, after) {
			mutations = append(mutations, Mutation{Path: path, Before: digest(before), After: digest(after), Bytes: after})
		}
	}
	return mutations, nil
}

func Apply(files map[string][]byte, mutations []Mutation) map[string][]byte {
	result := map[string][]byte{}
	for path, data := range files {
		result[path] = append([]byte{}, data...)
	}
	for _, mutation := range mutations {
		result[mutation.Path] = append([]byte{}, mutation.Bytes...)
	}
	return result
}

func ConfigDigest(config PublicConfig) (string, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}
