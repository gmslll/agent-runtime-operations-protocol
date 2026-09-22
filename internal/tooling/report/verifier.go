package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/controlledinput"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type VerifyOptions struct {
	Root, ReportPath string
	AllowAncestor    bool
}

func gitBytes(root string, args ...string) ([]byte, error) {
	c := controlledinput.GitCommand(root, args...)
	return c.Output()
}
func gitString(root string, args ...string) (string, error) {
	out, err := gitBytes(root, args...)
	return strings.TrimSpace(string(out)), err
}
func safePath(root, value, label string) (string, error) {
	relative, err := structuredfile.SafeRelative(root, value)
	if err != nil {
		return "", fmt.Errorf("%s is not a safe repository-relative path: %s", label, value)
	}
	return structuredfile.RequireInsideFile(root, filepath.Join(root, filepath.FromSlash(relative)), label)
}

func Verify(options VerifyOptions) (*Report, string, error) {
	rootAbs, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, "", fmt.Errorf("repository root is invalid: %w", err)
	}
	rootCanonical, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, "", fmt.Errorf("repository root is unreadable: %w", err)
	}
	options.Root = rootCanonical
	reportPath := options.ReportPath
	if !filepath.IsAbs(reportPath) {
		reportPath = filepath.Join(options.Root, reportPath)
	}
	reportPath, err = structuredfile.RequireInsideFile(options.Root, reportPath, "report")
	if err != nil {
		return nil, "", fmt.Errorf("report must be inside repository")
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, "", fmt.Errorf("report is unreadable: %w", err)
	}
	r, err := decodeStrict(options.Root, data)
	if err != nil {
		return nil, "", err
	}
	problems := []string{}
	if r.SchemaVersion != 1 {
		problems = append(problems, "schema_version must be 1")
	}
	if len(r.Checks) == 0 {
		problems = append(problems, "checks must not be empty")
	}
	if r.GeneratedAt == "" {
		problems = append(problems, "generated_at is required")
	}
	names := map[string]bool{}
	failed := 0
	for _, c := range r.Checks {
		if names[c.Name] {
			problems = append(problems, "JSON check names are not unique")
		}
		names[c.Name] = true
		if !c.Passed {
			failed++
			found := false
			for _, e := range r.Errors {
				if strings.HasPrefix(e, c.Name+":") {
					found = true
				}
			}
			if !found {
				problems = append(problems, "failed check "+c.Name+" has no corresponding errors entry")
			}
		}
	}
	intSummary := func(k string) int {
		switch v := r.Summary[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			i, err := v.Int64()
			if err == nil {
				return int(i)
			}
		}
		return -1
	}
	for _, pair := range []struct {
		name             string
		actual, expected int
	}{{"provenance.testcase_count", r.Provenance.TestcaseCount, len(r.Checks)}, {"summary.checks", intSummary("checks"), len(r.Checks)}, {"summary.testcase_count", intSummary("testcase_count"), len(r.Checks)}, {"summary.failed", intSummary("failed"), failed}, {"summary.passed", intSummary("passed"), len(r.Checks) - failed}, {"errors.length", len(r.Errors), failed}} {
		if pair.actual != pair.expected {
			problems = append(problems, fmt.Sprintf("%s expected %d, got %d", pair.name, pair.expected, pair.actual))
		}
	}
	if r.Success != (failed == 0 && len(r.Errors) == 0) {
		problems = append(problems, "success does not match JSON failure/error counts")
	}
	type XFailure struct {
		Message string `xml:"message,attr"`
	}
	type XCase struct {
		Name     string     `xml:"name,attr"`
		Failures []XFailure `xml:"failure"`
	}
	type XSuite struct {
		Tests    int     `xml:"tests,attr"`
		Failures int     `xml:"failures,attr"`
		Cases    []XCase `xml:"testcase"`
	}
	var suite XSuite
	junit, je := os.ReadFile(filepath.Join(filepath.Dir(reportPath), "junit.xml"))
	if je != nil {
		problems = append(problems, "JUnit is unreadable: "+je.Error())
	} else if xml.Unmarshal(junit, &suite) != nil {
		problems = append(problems, "JUnit cannot be parsed")
	} else {
		if suite.Tests != len(r.Checks) || len(suite.Cases) != len(r.Checks) {
			problems = append(problems, "JUnit test count does not match JSON")
		}
		if suite.Failures != failed {
			problems = append(problems, "JUnit failure count does not match JSON")
		}
		for i, c := range r.Checks {
			if i >= len(suite.Cases) || suite.Cases[i].Name != c.Name {
				problems = append(problems, fmt.Sprintf("JUnit testcase %d does not match JSON", i))
				break
			}
			if (len(suite.Cases[i].Failures) > 0) == c.Passed {
				problems = append(problems, "JUnit testcase "+c.Name+" failure state does not match JSON")
			}
		}
	}
	current, ge := gitString(options.Root, "rev-parse", "HEAD")
	if ge != nil {
		problems = append(problems, "cannot verify current HEAD")
	}
	claimed := r.Provenance.Git.Head
	if len(claimed) != 40 {
		problems = append(problems, "report claimed HEAD is invalid: "+claimed)
	} else if _, e := gitString(options.Root, "cat-file", "-e", claimed+"^{commit}"); e != nil {
		problems = append(problems, "report claimed HEAD does not exist as a commit: "+claimed)
	}
	mode := "current-worktree"
	if claimed != current {
		mode = "ancestor-archive-only"
		if !options.AllowAncestor {
			problems = append(problems, "report HEAD is not current HEAD; rerun or opt in with ALLOW_ANCESTOR=1")
		} else if c := controlledinput.GitCommand(options.Root, "merge-base", "--is-ancestor", claimed, current); func() error { return c.Run() }() != nil {
			problems = append(problems, "report claimed HEAD is not an ancestor of current HEAD")
		}
	}
	phase := ""
	relReport, relErr := filepath.Rel(options.Root, reportPath)
	if relErr == nil {
		parts := strings.Split(filepath.ToSlash(relReport), "/")
		if len(parts) == 4 && parts[0] == "build" && parts[1] == "reports" && (parts[2] == "P01" || parts[2] == "P02") && parts[3] == "report.json" {
			phase = parts[2]
		}
	}
	if phase != "" {
		if r.Provenance.ControlledInputs == nil {
			problems = append(problems, phase+" report is missing controlled_inputs")
		} else {
			var discovered controlledinput.Manifest
			var discoverErr error
			if mode == "ancestor-archive-only" && options.AllowAncestor {
				discovered, discoverErr = controlledinput.AtCommit(options.Root, claimed, phase)
			} else {
				discovered, discoverErr = controlledinput.Current(options.Root, phase)
			}
			if discoverErr != nil {
				problems = append(problems, "cannot rediscover controlled inputs: "+discoverErr.Error())
			} else if r.Provenance.ControlledInputs.Source != "current-index-worktree" || !controlledinput.Equal(*r.Provenance.ControlledInputs, discovered) {
				problems = append(problems, "controlled input manifest does not exactly match independently discovered tracked tree")
			}
		}
	} else if r.Provenance.ControlledInputs != nil {
		problems = append(problems, "controlled_inputs is only valid for P01/P02 repository-wide reports")
	}
	checkerAbs, e := safePath(options.Root, r.Provenance.Checker.Path, "checker path")
	if e != nil {
		problems = append(problems, e.Error())
	} else {
		var content []byte
		if mode == "ancestor-archive-only" && options.AllowAncestor {
			content, e = gitBytes(options.Root, "show", claimed+":"+r.Provenance.Checker.Path)
		} else {
			content, e = os.ReadFile(checkerAbs)
		}
		if e != nil || Hash(content) != r.Provenance.Checker.SHA256 {
			problems = append(problems, "checker digest mismatch for "+r.Provenance.Checker.Path)
		}
	}
	paths := make([]string, 0, len(r.Provenance.Inputs.Files))
	for _, f := range r.Provenance.Inputs.Files {
		if _, e := safePath(options.Root, f.Path, "input path"); e != nil {
			problems = append(problems, e.Error())
		}
		paths = append(paths, f.Path)
	}
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	if !equalStrings(paths, sorted) {
		problems = append(problems, "report input paths are not in canonical sorted order")
	}
	if hasDup(paths) {
		problems = append(problems, "report input paths are not unique")
	}
	actual := []InputFile{}
	for _, p := range paths {
		var content []byte
		var e error
		if mode == "ancestor-archive-only" && options.AllowAncestor {
			content, e = gitBytes(options.Root, "show", claimed+":"+p)
		} else {
			content, e = os.ReadFile(filepath.Join(options.Root, filepath.FromSlash(p)))
		}
		if e != nil {
			problems = append(problems, "cannot verify report input "+p)
			continue
		}
		actual = append(actual, InputFile{p, Hash(content), int64(len(content))})
	}
	if !equalInputFiles(actual, r.Provenance.Inputs.Files) || Aggregate(actual) != r.Provenance.Inputs.SHA256 {
		problems = append(problems, "input digest/size mismatch")
	}
	runtimePaths := make([]string, 0, len(r.Provenance.RuntimeInputs.Files))
	runtimeActual := []InputFile{}
	for _, f := range r.Provenance.RuntimeInputs.Files {
		path, pathErr := safePath(options.Root, f.Path, "runtime input path")
		if pathErr != nil {
			problems = append(problems, pathErr.Error())
			continue
		}
		runtimePaths = append(runtimePaths, f.Path)
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			problems = append(problems, "cannot verify runtime input "+f.Path)
			continue
		}
		runtimeActual = append(runtimeActual, InputFile{f.Path, Hash(content), int64(len(content))})
	}
	runtimeSorted := append([]string{}, runtimePaths...)
	sort.Strings(runtimeSorted)
	if !equalStrings(runtimePaths, runtimeSorted) || hasDup(runtimePaths) {
		problems = append(problems, "runtime input paths must be unique and canonically sorted")
	}
	if !equalInputFiles(runtimeActual, r.Provenance.RuntimeInputs.Files) || Aggregate(runtimeActual) != r.Provenance.RuntimeInputs.SHA256 {
		problems = append(problems, "runtime input digest/size mismatch")
	}
	evidenceKinds := []string{}
	for _, item := range r.Provenance.RuntimeEvidence {
		evidenceKinds = append(evidenceKinds, item.Kind)
	}
	if !sort.StringsAreSorted(evidenceKinds) || hasDup(evidenceKinds) {
		problems = append(problems, "runtime evidence kinds must be unique and canonically sorted")
	}
	if mode == "current-worktree" {
		statusBytes, statusErr := gitBytes(options.Root, "status", "--porcelain=v1", "--untracked-files=all")
		status := strings.TrimRight(string(statusBytes), "\r\n")
		entries := []string{}
		if statusErr != nil {
			problems = append(problems, "cannot verify current dirty state")
		}
		if status != "" {
			entries = strings.Split(status, "\n")
		}
		if r.Provenance.Git.Dirty != (len(entries) > 0) || !equalStrings(r.Provenance.Git.DirtyEntries, entries) {
			problems = append(problems, "report dirty state does not match current worktree")
		}
		actualRuntime, runtimeErr := currentRuntime(options.Root)
		if runtimeErr != nil {
			problems = append(problems, "cannot verify current runtime: "+runtimeErr.Error())
		} else if r.Provenance.Runtime != actualRuntime {
			problems = append(problems, fmt.Sprintf("report runtime does not match current environment: reported=%+v actual=%+v", r.Provenance.Runtime, actualRuntime))
		}
	}
	if mode == "ancestor-archive-only" && options.AllowAncestor {
		if r.Provenance.Git.Dirty || len(r.Provenance.Git.DirtyEntries) > 0 {
			problems = append(problems, "historical report reuse requires a clean claimed-commit report")
		}
		if checkerAbs != "" {
			content, currentErr := os.ReadFile(checkerAbs)
			if currentErr != nil || Hash(content) != r.Provenance.Checker.SHA256 {
				problems = append(problems, "current checker changed since claimed commit; historical report must be rerun")
			}
		}
		for _, f := range r.Provenance.Inputs.Files {
			content, e := os.ReadFile(filepath.Join(options.Root, filepath.FromSlash(f.Path)))
			if e != nil || Hash(content) != f.SHA256 || int64(len(content)) != f.Bytes {
				problems = append(problems, "current reuse eligibility input digest/size mismatch for "+f.Path)
			}
		}
	}
	if len(problems) > 0 {
		return r, mode, fmt.Errorf("AROP report verification failed with %d error(s): %s", len(problems), strings.Join(problems, "; "))
	}
	return r, mode, nil
}

func decodeStrict(root string, data []byte) (*Report, error) {
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return nil, fmt.Errorf("report JSON: %w", err)
	}
	if err := schema.ValidateFile(root, "spec/schemas/check-report.schema.json", parsed); err != nil {
		return nil, fmt.Errorf("report schema: %w", err)
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("normalize report JSON: %w", err)
	}
	var r Report
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode report JSON: %w", err)
	}
	return &r, nil
}
func equalStrings(a, b []string) bool { return bytes.Equal(mustJSON(a), mustJSON(b)) }
func hasDup(v []string) bool {
	m := map[string]bool{}
	for _, s := range v {
		if m[s] {
			return true
		}
		m[s] = true
	}
	return false
}
func equalInputFiles(a, b []InputFile) bool { return bytes.Equal(mustJSON(a), mustJSON(b)) }
func mustJSON(v any) []byte                 { b, _ := json.Marshal(v); return b }
