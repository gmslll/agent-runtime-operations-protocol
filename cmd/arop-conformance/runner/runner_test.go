package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunProfileClosureFilteringAndDigestReports(t *testing.T) {
	root := repositoryRoot(t)
	target := buildDriver(t, root)
	report, err := Run(context.Background(), Config{Root: root, Profile: "control-plane", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.PassedCount != 3 || report.FailedCount != 0 || len(report.Results) != 3 || !digestValue(report.ProfileSHA256) || !digestValue(report.TargetSHA256) {
		t.Fatalf("report=%+v", report)
	}
	if report.Results[0].ID != "core.event-envelope.valid" || report.Results[1].ID != "core.manifest.valid" || report.Results[2].ID != "core.trace-context.vectors" {
		t.Fatalf("deterministic closure=%+v", report.Results)
	}
	for _, result := range report.Results {
		if !result.Required || !digestValue(result.ScenarioSHA256) || !digestValue(result.FixtureSHA256) || result.FixtureBytes <= 0 {
			t.Fatalf("result=%+v", result)
		}
	}
	jsonReport, err := EncodeJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	junit, err := EncodeJUnit(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{report.ProfileSHA256, report.TargetSHA256, report.Results[0].ScenarioSHA256, report.Results[0].FixtureSHA256} {
		if !bytes.Contains(jsonReport, []byte(value)) || !bytes.Contains(junit, []byte(value)) {
			t.Fatalf("digest %s missing from JSON or JUnit", value)
		}
	}
	filtered, err := Run(context.Background(), Config{Root: root, Profile: "control-plane", ScenarioFilters: []string{"core.trace-context.vectors"}, Target: target})
	if err != nil || !filtered.Passed || len(filtered.Results) != 2 || filtered.Results[0].ID != "core.event-envelope.valid" || filtered.Results[1].ID != "core.trace-context.vectors" {
		t.Fatalf("filtered=%+v err=%v", filtered, err)
	}
	if _, err := Run(context.Background(), Config{Root: root, Profile: "control-plane", ScenarioFilters: []string{"unknown.scenario"}, Target: target}); err == nil {
		t.Fatal("unknown filter accepted")
	}
}

func TestRequiredSkipTimeoutAndInvalidTargetResponsesFailClosed(t *testing.T) {
	root := repositoryRoot(t)
	for _, test := range []struct {
		name    string
		script  string
		timeout time.Duration
		message string
	}{
		{"required-skip", `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"protocol":"arop-conformance-driver/v1","scenario_id":"core.manifest.valid","outcome":"skip","message":"secret detail"}'
`, 0, "required scenario cannot be skipped"},
		{"timeout", `#!/bin/sh
cat >/dev/null
sleep 2
`, 25 * time.Millisecond, "target timed out"},
		{"unknown-field", `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"protocol":"arop-conformance-driver/v1","scenario_id":"core.manifest.valid","outcome":"pass","extra":true}'
`, 0, "target response is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := scriptTarget(t, test.script)
			report, err := Run(context.Background(), Config{Root: root, Profile: "core-provider", ScenarioFilters: []string{"core.manifest.valid"}, Target: target, TimeoutCeiling: test.timeout})
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || report.FailedCount != 1 || len(report.Results) != 1 || report.Results[0].Outcome != "fail" || report.Results[0].Message != test.message || strings.Contains(report.Results[0].Message, "secret") {
				t.Fatalf("report=%+v", report)
			}
		})
	}
}

func TestCatalogRejectsDuplicateUnknownCycleAndTamperedFixture(t *testing.T) {
	source := repositoryRoot(t)
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"duplicate-profile", func(t *testing.T, root string) {
			path := filepath.Join(root, "conformance/profiles/v1/core.yaml")
			data := readFile(t, path)
			data = bytes.Replace(data, []byte("id: streaming"), []byte("id: core-provider"), 1)
			writeFile(t, path, data)
		}},
		{"unknown-profile", func(t *testing.T, root string) {
			path := filepath.Join(root, "conformance/profiles/v1/core.yaml")
			data := readFile(t, path)
			data = bytes.Replace(data, []byte("includes: [core-provider]"), []byte("includes: [missing-profile]"), 1)
			writeFile(t, path, data)
		}},
		{"scenario-cycle", func(t *testing.T, root string) {
			path := filepath.Join(root, "conformance/scenarios/core/event-envelope-valid.json")
			data := readFile(t, path)
			data = bytes.Replace(data, []byte(`"depends_on": []`), []byte(`"depends_on": ["core.event-envelope.valid"]`), 1)
			writeFile(t, path, data)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := copyCatalog(t, source)
			test.mutate(t, root)
			if _, err := LoadCatalog(root); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
	root := copyCatalog(t, source)
	target := buildDriver(t, source)
	path := filepath.Join(root, "examples/manifests/publication-v1.json")
	writeFile(t, path, append(readFile(t, path), '\n'))
	if _, err := Run(context.Background(), Config{Root: root, Profile: "core-provider", ScenarioFilters: []string{"core.manifest.valid"}, Target: target}); err == nil {
		t.Fatal("tampered fixture accepted")
	}
}

func TestExecuteCLIWritesJSONAndJUnitAndRejectsSymlinkOutput(t *testing.T) {
	root := repositoryRoot(t)
	target := buildDriver(t, root)
	directory := resolvedTempDir(t)
	jsonPath, junitPath := filepath.Join(directory, "report.json"), filepath.Join(directory, "junit.xml")
	var stdout, stderr bytes.Buffer
	err := ExecuteCLI(context.Background(), []string{"--root", root, "--profile", "core-provider", "--target", target, "--scenario", "core.event-envelope.valid", "--json", jsonPath, "--junit", junitPath}, &stdout, &stderr)
	if err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%s stderr=%s err=%v", stdout.String(), stderr.String(), err)
	}
	var report Report
	if err := json.Unmarshal(readFile(t, jsonPath), &report); err != nil || !report.Passed || len(report.Results) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if !bytes.Contains(readFile(t, junitPath), []byte(report.TargetSHA256)) {
		t.Fatal("JUnit omitted target digest")
	}
	targetFile := filepath.Join(directory, "real.json")
	writeFile(t, targetFile, []byte("keep"))
	symlink := filepath.Join(directory, "symlink.json")
	if err := os.Symlink(targetFile, symlink); err != nil {
		t.Fatal(err)
	}
	if err := WriteReports(report, symlink, ""); err == nil {
		t.Fatal("symlink report output accepted")
	}
}

func TestTargetEnvironmentIsOfflineAndSecretFree(t *testing.T) {
	root := repositoryRoot(t)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "top-secret")
	t.Setenv("AROP_CONTROL_PLANE_TOKEN", "top-secret")
	target := scriptTarget(t, `#!/bin/sh
cat >/dev/null
[ "$AROP_CONFORMANCE_OFFLINE" = 1 ] || exit 2
[ -z "$AWS_SECRET_ACCESS_KEY" ] || exit 3
[ -z "$AROP_CONTROL_PLANE_TOKEN" ] || exit 4
printf '%s\n' '{"protocol":"arop-conformance-driver/v1","scenario_id":"core.manifest.valid","outcome":"pass"}'
`)
	report, err := Run(context.Background(), Config{Root: root, Profile: "core-provider", ScenarioFilters: []string{"core.manifest.valid"}, Target: target})
	if err != nil || !report.Passed {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func buildDriver(t *testing.T, root string) string {
	t.Helper()
	directory := resolvedTempDir(t)
	target := filepath.Join(directory, "driver")
	command := exec.Command("go", "build", "-o", target, "./cmd/arop-conformance/testdata/driver")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build driver: %v: %s", err, output)
	}
	return target
}

func scriptTarget(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(resolvedTempDir(t), "driver")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func copyCatalog(t *testing.T, source string) string {
	t.Helper()
	root := resolvedTempDir(t)
	for _, relative := range []string{
		"conformance/scenarios/schema.json",
		"conformance/scenarios/core/manifest-valid.json",
		"conformance/scenarios/core/event-envelope-valid.json",
		"conformance/scenarios/core/trace-vectors.json",
		"conformance/profiles/schema.json",
		"conformance/profiles/v1/core.yaml",
		"examples/manifests/publication-v1.json",
		"conformance/fixtures/events/envelope.valid.json",
		"conformance/fixtures/state-machines/base/wire-vectors.json",
	} {
		destination := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, destination, readFile(t, filepath.Join(source, filepath.FromSlash(relative))))
	}
	return root
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func digestValue(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:")
}
