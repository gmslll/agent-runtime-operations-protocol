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
	Git           GitProvenance `json:"git"`
	Command       string        `json:"command"`
	Runtime       Runtime       `json:"runtime"`
	Checker       DigestedPath  `json:"checker"`
	Inputs        InputDigest   `json:"inputs"`
	TestcaseCount int           `json:"testcase_count"`
	AuditNote     string        `json:"audit_note"`
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
func command(root, name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
func git(root string, args ...string) string {
	c := exec.Command("git", args...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
func gitStatus(root string) string {
	c := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all")
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strings.TrimRight(string(out), "\r\n")
}

func Write(options WriteOptions) (*Report, error) {
	inputs, err := DigestFiles(options.Root, options.InputPaths)
	if err != nil {
		return nil, err
	}
	checkerData, err := os.ReadFile(filepath.Join(options.Root, filepath.FromSlash(options.CheckerPath)))
	if err != nil {
		return nil, err
	}
	status := gitStatus(options.Root)
	dirty := []string{}
	if status != "" {
		dirty = strings.Split(status, "\n")
	}
	node := command(options.Root, "node", "--version")
	gov := command(options.Root, "go", "version")
	release := command(options.Root, "uname", "-r")
	checks := append([]Check{}, options.Checks...)
	provenanceProblems := []string{}
	head := git(options.Root, "rev-parse", "HEAD")
	if len(head) != 40 {
		provenanceProblems = append(provenanceProblems, "invalid git HEAD: "+head)
	}
	if strings.HasPrefix(gov, "unavailable:") {
		provenanceProblems = append(provenanceProblems, gov)
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
	report := &Report{1, time.Now().UTC().Format(time.RFC3339Nano), failed == 0, Provenance{GitProvenance{head, len(dirty) > 0, dirty}, options.Command, Runtime{node, gov, RuntimeOS{runtime.GOOS, release, runtime.GOARCH}}, DigestedPath{options.CheckerPath, Hash(checkerData)}, inputs, len(checks), options.AuditNote}, summary, checks, errors}
	dir := filepath.Join(options.Root, filepath.FromSlash(options.Directory))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644); err != nil {
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
	x, _ := xml.MarshalIndent(Suite{Name: options.Suite, Tests: len(checks), Failures: failed, Cases: cases}, "", "  ")
	x = append([]byte(xml.Header), x...)
	x = append(x, '\n')
	if err := os.WriteFile(filepath.Join(dir, "junit.xml"), x, 0644); err != nil {
		return nil, err
	}
	return report, nil
}
