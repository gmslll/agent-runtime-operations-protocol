package evidence

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const ExternalConformanceSchema = "spec/schemas/external-conformance-evidence.schema.json"

type ExternalArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type ExternalResult struct {
	ScenarioID string `json:"scenario_id"`
	Result     string `json:"result"`
}

type ExternalConformance struct {
	SchemaVersion int                `json:"schema_version"`
	RunnerSHA256  string             `json:"runner_sha256"`
	SuiteSHA256   string             `json:"suite_sha256"`
	Profiles      []string           `json:"profiles"`
	Artifacts     []ExternalArtifact `json:"artifacts"`
	Results       []ExternalResult   `json:"results"`
}

func ValidateExternalConformance(root string, data []byte, expectedProfiles []string) (ExternalConformance, error) {
	var value ExternalConformance
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return value, err
	}
	if err := schema.ValidateFile(root, ExternalConformanceSchema, parsed); err != nil {
		return value, err
	}
	if err := strictDecode(data, &value); err != nil {
		return value, err
	}
	if value.SchemaVersion != 1 || !isDigest(value.RunnerSHA256) || !isDigest(value.SuiteSHA256) || len(value.Profiles) == 0 || len(value.Artifacts) == 0 || len(value.Results) == 0 {
		return value, errors.New("external conformance evidence is incomplete")
	}
	if !equalStrings(value.Profiles, expectedProfiles) || !sort.StringsAreSorted(value.Profiles) {
		return value, errors.New("external conformance profile closure is not exact and ordered")
	}
	artifactNames := map[string]bool{}
	lastArtifact := ""
	for _, artifact := range value.Artifacts {
		if artifact.Name <= lastArtifact || artifactNames[artifact.Name] || !isDigest(artifact.SHA256) || artifact.Bytes < 1 {
			return value, errors.New("external conformance artifact inventory is invalid or unordered")
		}
		artifactNames[artifact.Name], lastArtifact = true, artifact.Name
	}
	resultIDs := map[string]bool{}
	lastResult := ""
	for _, result := range value.Results {
		if result.ScenarioID <= lastResult || resultIDs[result.ScenarioID] || result.Result != "PASS" {
			return value, fmt.Errorf("external conformance result %s is duplicate, unordered, or not PASS", result.ScenarioID)
		}
		resultIDs[result.ScenarioID], lastResult = true, result.ScenarioID
	}
	if containsSecretSentinel(data) {
		return value, errors.New("external conformance evidence contains secret material")
	}
	return value, nil
}
