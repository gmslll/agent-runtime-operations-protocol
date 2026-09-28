// Package runner implements the reusable, offline AROP conformance runner.
// Scenarios and profiles select tests; they do not redefine protocol behavior.
package runner

import "time"

const DriverProtocol = "arop-conformance-driver/v1"

type Fixture struct {
	Path      string `json:"path" yaml:"path"`
	SHA256    string `json:"sha256" yaml:"sha256"`
	MediaType string `json:"media_type" yaml:"media_type"`
}

type Scenario struct {
	SchemaVersion int            `json:"schema_version" yaml:"schema_version"`
	ID            string         `json:"id" yaml:"id"`
	Title         string         `json:"title" yaml:"title"`
	Description   string         `json:"description,omitempty" yaml:"description,omitempty"`
	DependsOn     []string       `json:"depends_on" yaml:"depends_on"`
	Fixture       Fixture        `json:"fixture" yaml:"fixture"`
	TimeoutMS     int            `json:"timeout_ms" yaml:"timeout_ms"`
	Request       map[string]any `json:"request" yaml:"request"`
}

type ProfileScenario struct {
	ID       string `json:"id" yaml:"id"`
	Required bool   `json:"required" yaml:"required"`
}

type Profile struct {
	ID        string            `json:"id" yaml:"id"`
	Title     string            `json:"title" yaml:"title"`
	Includes  []string          `json:"includes" yaml:"includes"`
	Scenarios []ProfileScenario `json:"scenarios" yaml:"scenarios"`
}

type ProfileCatalog struct {
	SchemaVersion int       `json:"schema_version" yaml:"schema_version"`
	Profiles      []Profile `json:"profiles" yaml:"profiles"`
}

type InvocationFixture struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	Bytes      int64  `json:"bytes"`
	DataBase64 string `json:"data_base64"`
}

type Invocation struct {
	Protocol   string            `json:"protocol"`
	ScenarioID string            `json:"scenario_id"`
	Request    map[string]any    `json:"request"`
	Fixture    InvocationFixture `json:"fixture"`
}

type DriverResponse struct {
	Protocol   string `json:"protocol"`
	ScenarioID string `json:"scenario_id"`
	Outcome    string `json:"outcome"`
	Message    string `json:"message,omitempty"`
}

type ScenarioResult struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Required       bool   `json:"required"`
	Outcome        string `json:"outcome"`
	Message        string `json:"message,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	ScenarioSHA256 string `json:"scenario_sha256"`
	FixturePath    string `json:"fixture_path"`
	FixtureSHA256  string `json:"fixture_sha256"`
	FixtureBytes   int64  `json:"fixture_bytes"`
}

type Report struct {
	SchemaVersion int              `json:"schema_version"`
	Protocol      string           `json:"protocol"`
	Profile       string           `json:"profile"`
	ProfileSHA256 string           `json:"profile_sha256"`
	TargetSHA256  string           `json:"target_sha256"`
	StartedAt     string           `json:"started_at"`
	CompletedAt   string           `json:"completed_at"`
	Passed        bool             `json:"passed"`
	PassedCount   int              `json:"passed_count"`
	FailedCount   int              `json:"failed_count"`
	SkippedCount  int              `json:"skipped_count"`
	Results       []ScenarioResult `json:"results"`
}

type Config struct {
	Root            string
	Profile         string
	ScenarioFilters []string
	Target          string
	TimeoutCeiling  time.Duration
	Clock           func() time.Time
}

type catalogScenario struct {
	Scenario
	Path   string
	Digest string
}

type catalogProfile struct {
	Profile
	Path   string
	Digest string
}

type Catalog struct {
	root      string
	scenarios map[string]catalogScenario
	profiles  map[string]catalogProfile
}

type resolvedScenario struct {
	catalogScenario
	Required bool
}

func cloneRequest(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
