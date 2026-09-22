// Package report implements the language-neutral machine report contract in Go.
package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}
type InputFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type InputDigest struct {
	SHA256 string      `json:"sha256"`
	Files  []InputFile `json:"files"`
}
type RuntimeEvidence struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type GitProvenance struct {
	Head         string   `json:"head"`
	Dirty        bool     `json:"dirty"`
	DirtyEntries []string `json:"dirty_entries"`
}
type RuntimeOS struct {
	Platform string `json:"platform"`
	Release  string `json:"release"`
	Arch     string `json:"arch"`
}
type Runtime struct {
	Node string    `json:"node"`
	Go   string    `json:"go"`
	OS   RuntimeOS `json:"os"`
}
type DigestedPath struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Provenance struct {
	Git             GitProvenance     `json:"git"`
	Command         string            `json:"command"`
	Runtime         Runtime           `json:"runtime"`
	Checker         DigestedPath      `json:"checker"`
	Inputs          InputDigest       `json:"inputs"`
	RuntimeInputs   InputDigest       `json:"runtime_inputs"`
	RuntimeEvidence []RuntimeEvidence `json:"runtime_evidence"`
	TestcaseCount   int               `json:"testcase_count"`
	AuditNote       string            `json:"audit_note"`
}
type Report struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   string         `json:"generated_at"`
	Success       bool           `json:"success"`
	Provenance    Provenance     `json:"provenance"`
	Summary       map[string]any `json:"summary"`
	Checks        []Check        `json:"checks"`
	Errors        []string       `json:"errors"`
}
type WriteOptions struct {
	Root, Directory, Suite, Class, Command, CheckerPath, AuditNote string
	InputPaths                                                     []string
	RuntimeInputPaths                                              []string
	RuntimeEvidence                                                []RuntimeEvidence
	Checks                                                         []Check
	Summary                                                        map[string]any
}

func Hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func Aggregate(files []InputFile) string {
	var b bytes.Buffer
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\x00%d\n", f.Path, f.SHA256, f.Bytes)
	}
	return Hash(b.Bytes())
}
func DigestFiles(root string, paths []string) (InputDigest, error) {
	unique := map[string]bool{}
	for _, p := range paths {
		unique[filepath.ToSlash(filepath.Clean(p))] = true
	}
	keys := make([]string, 0, len(unique))
	for p := range unique {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	files := make([]InputFile, 0, len(keys))
	for _, p := range keys {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return InputDigest{}, fmt.Errorf("digest %s: %w", p, err)
		}
		files = append(files, InputFile{p, Hash(data), int64(len(data))})
	}
	return InputDigest{Aggregate(files), files}, nil
}
func command(root, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("%s returned empty output", name)
	}
	return value, nil
}
func git(root string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
func gitStatus(root string) (string, error) {
	c := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func currentRuntime(root string) (Runtime, error) {
	node, err := command(root, "node", "--version")
	if err != nil {
		return Runtime{}, err
	}
	gov, err := command(root, "go", "version")
	if err != nil {
		return Runtime{}, err
	}
	release, err := command(root, "uname", "-r")
	if err != nil {
		return Runtime{}, err
	}
	return Runtime{Node: node, Go: gov, OS: RuntimeOS{Platform: runtime.GOOS, Release: release, Arch: runtime.GOARCH}}, nil
}

func Write(options WriteOptions) (*Report, error) {
	inputs, err := DigestFiles(options.Root, options.InputPaths)
	if err != nil {
		return nil, err
	}
	runtimeInputs, err := DigestFiles(options.Root, options.RuntimeInputPaths)
	if err != nil {
		return nil, err
	}
	checkerData, err := os.ReadFile(filepath.Join(options.Root, filepath.FromSlash(options.CheckerPath)))
	if err != nil {
		return nil, err
	}
	status, err := gitStatus(options.Root)
	if err != nil {
		return nil, err
	}
	dirty := []string{}
	if status != "" {
		dirty = strings.Split(status, "\n")
	}
	runtimeSnapshot, err := currentRuntime(options.Root)
	if err != nil {
		return nil, err
	}
	checks := append([]Check{}, options.Checks...)
	provenanceProblems := []string{}
	head, err := git(options.Root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if len(head) != 40 {
		provenanceProblems = append(provenanceProblems, "invalid git HEAD: "+head)
	}
	if options.Command == "" {
		provenanceProblems = append(provenanceProblems, "actual command is empty")
	}
	pc := Check{"report-provenance", len(provenanceProblems) == 0, "exact HEAD, dirty state, command, Node/Go/OS, input digest and checker digest captured"}
	if !pc.Passed {
		pc.Detail = strings.Join(provenanceProblems, "; ")
	}
	checks = append(checks, pc)
	errors := []string{}
	failed := 0
	for _, c := range checks {
		if !c.Passed {
			failed++
			errors = append(errors, c.Name+": "+c.Detail)
		}
	}
	summary := map[string]any{}
	for k, v := range options.Summary {
		summary[k] = v
	}
	summary["checks"] = len(checks)
	summary["passed"] = len(checks) - failed
	summary["failed"] = failed
	summary["testcase_count"] = len(checks)
	evidence := append([]RuntimeEvidence{}, options.RuntimeEvidence...)
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	report := &Report{1, time.Now().UTC().Format(time.RFC3339Nano), failed == 0, Provenance{GitProvenance{head, len(dirty) > 0, dirty}, options.Command, runtimeSnapshot, DigestedPath{options.CheckerPath, Hash(checkerData)}, inputs, runtimeInputs, evidence, len(checks), options.AuditNote}, summary, checks, errors}
	data, err := marshalValidated(options.Root, report)
	if err != nil {
		return nil, err
	}
	type Failure struct {
		XMLName xml.Name `xml:"failure"`
		Message string   `xml:"message,attr"`
	}
	type Case struct {
		XMLName xml.Name `xml:"testcase"`
		Class   string   `xml:"classname,attr"`
		Name    string   `xml:"name,attr"`
		Failure *Failure `xml:",omitempty"`
	}
	type Suite struct {
		XMLName  xml.Name `xml:"testsuite"`
		Name     string   `xml:"name,attr"`
		Tests    int      `xml:"tests,attr"`
		Failures int      `xml:"failures,attr"`
		Cases    []Case   `xml:"testcase"`
	}
	cases := []Case{}
	for _, c := range checks {
		tc := Case{Class: options.Class, Name: c.Name}
		if !c.Passed {
			tc.Failure = &Failure{Message: c.Detail}
		}
		cases = append(cases, tc)
	}
	x, err := xml.MarshalIndent(Suite{Name: options.Suite, Tests: len(checks), Failures: failed, Cases: cases}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal JUnit XML: %w", err)
	}
	x = append([]byte(xml.Header), x...)
	x = append(x, '\n')
	dir := filepath.Join(options.Root, filepath.FromSlash(options.Directory))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "junit.xml"), x, 0644); err != nil {
		return nil, err
	}
	return report, nil
}

func marshalValidated(root string, value *Report) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal report JSON: %w", err)
	}
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return nil, fmt.Errorf("self-parse report JSON: %w", err)
	}
	if err := schema.ValidateFile(root, "spec/schemas/check-report.schema.json", parsed); err != nil {
		return nil, fmt.Errorf("self-validate report JSON: %w", err)
	}
	return data, nil
}
