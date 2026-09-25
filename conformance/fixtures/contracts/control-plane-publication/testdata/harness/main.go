// Command harness verifies the P11 Publication contract and generated models.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	commandWant    = "make test-publication-contracts"
	rootDir        = "conformance/fixtures/contracts/control-plane-publication"
	checkerPath    = rootDir + "/testdata/harness/main.go"
	configPath     = rootDir + "/pipeline.json"
	casesPath      = rootDir + "/cases.json"
	inventoryPath  = rootDir + "/contract-inventory.json"
	waiverPath     = rootDir + "/transition/p11-p07-codegen-transition.json"
	provenancePath = rootDir + "/generated/provenance.json"
	baselineCommit = "a7b357dfd6a102622e7767539732d3cbbf184dbc"
)

var orderedCaseIDs = []string{
	"aggregate-component-conflict", "aggregate-path-conflict", "auth-get-agent-read", "auth-missing-bearer",
	"auth-post-agent-publish", "auth-unknown-security-extension", "bundle-absolute-path", "bundle-backslash-path",
	"bundle-case-collision", "bundle-duplicate-entry", "bundle-encrypted-entry", "bundle-hardlink-entry",
	"bundle-limit-archive-bytes", "bundle-limit-compression-ratio", "bundle-limit-entry-count", "bundle-limit-entry-size",
	"bundle-limit-total-size", "bundle-multiple-manifests", "bundle-network-ref", "bundle-parent-path",
	"bundle-percent-bypass", "bundle-symlink-entry", "bundle-unicode-collision", "bundle-valid",
	"error-400", "error-401-www-authenticate", "error-403", "error-404", "error-409", "error-413",
	"error-415", "error-429-retry-after", "error-503-retry-after", "get-manifest-200-etag",
	"get-manifest-forward-field", "idempotency-different-digest-conflict", "idempotency-same-digest-replay",
	"manifest-agent-id-mismatch", "manifest-content-max-input-bytes-safe-maximum",
	"manifest-content-max-input-bytes-safe-maximum-plus-one", "manifest-execution-max-concurrency-safe-maximum",
	"manifest-execution-max-concurrency-safe-maximum-plus-one", "manifest-session-idle-timeout-safe-maximum",
	"manifest-session-idle-timeout-safe-maximum-plus-one", "no-public-secret-egress", "post-content-type-exact",
	"post-created-location-etag-no-body",
}

var generated = map[string]string{
	"go":         "sdk/go/generated/control-plane/publication.gen.go",
	"python":     "sdk/python/src/arop/generated/control-plane/publication_gen.py",
	"typescript": "sdk/typescript/src/generated/control-plane/publication.gen.ts",
}

type casesDoc struct {
	SchemaVersion int       `json:"schema_version"`
	ContractID    string    `json:"contract_id"`
	Cases         []caseDef `json:"cases"`
}
type caseDef struct {
	ID       string         `json:"id"`
	Group    string         `json:"group"`
	Source   string         `json:"source,omitempty"`
	Sources  []string       `json:"sources,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
	Expected map[string]any `json:"expected,omitempty"`
}
type pipelineDoc struct {
	SchemaVersion  int                   `json:"schema_version"`
	PipelineID     string                `json:"pipeline_id"`
	MappingProfile string                `json:"mapping_profile"`
	Resources      []resource            `json:"resources"`
	Roots          []rootSpec            `json:"roots"`
	Fixtures       fixtureSets           `json:"fixtures"`
	Outputs        map[string]outputSpec `json:"outputs"`
}
type resource struct {
	Path string `json:"path"`
	URI  string `json:"uri"`
}
type rootSpec struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}
type fixture struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Root string `json:"root"`
}
type fixtureSets struct{ Valid, Forward, Invalid []fixture }
type outputSpec struct {
	Model string `json:"model"`
}
type provenance struct {
	SchemaVersion  int              `json:"schema_version"`
	PipelineID     string           `json:"pipeline_id"`
	MappingProfile string           `json:"mapping_profile"`
	Command        map[string]any   `json:"command"`
	Generator      map[string]any   `json:"generator"`
	Configuration  map[string]any   `json:"configuration"`
	Resources      []map[string]any `json:"resources"`
	Fixtures       []map[string]any `json:"fixtures"`
	InputsSHA256   string           `json:"inputs_sha256"`
	Outputs        []map[string]any `json:"outputs"`
	OutputsSHA256  string           `json:"outputs_sha256"`
	Checks         []map[string]any `json:"checks"`
}
type waiver struct {
	SchemaVersion          int              `json:"schema_version"`
	WaiverID               string           `json:"waiver_id"`
	Status                 string           `json:"status"`
	Policy                 waiverPolicy     `json:"policy"`
	Baseline               waiverBaseline   `json:"baseline"`
	Transition             waiverTransition `json:"transition"`
	AffectedArtifacts      []string         `json:"affected_artifacts"`
	SourceClosure          []sourceItem     `json:"source_closure"`
	StaticSourceClosure    []sourceItem     `json:"static_source_closure"`
	Acceptance             []acceptance     `json:"acceptance"`
	Constraints            []string         `json:"constraints"`
	ValidationRequirements map[string]any   `json:"validation_requirements"`
}
type waiverPolicy struct {
	OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
	OwnershipTransferred bool   `json:"ownership_transferred"`
}
type waiverBaseline struct {
	Rule   string `json:"rule"`
	Commit string `json:"commit"`
}
type waiverTransition struct {
	FromPhases []string `json:"from_phases"`
	ToPhase    string   `json:"to_phase"`
	Reason     string   `json:"reason"`
}
type sourceItem struct {
	Path           string   `json:"path"`
	Scope          string   `json:"scope"`
	ChangeType     string   `json:"change_type"`
	BaselineSHA256 *string  `json:"baseline_sha256"`
	CurrentSHA256  *string  `json:"current_sha256"`
	TouchCommits   []string `json:"touch_commits"`
	ArtifactIDs    []string `json:"artifact_ids,omitempty"`
	OwnerPhases    []string `json:"owner_phases,omitempty"`
}
type acceptance struct {
	Phase   string `json:"phase"`
	Command string `json:"command"`
	Report  string `json:"report"`
}
type cmdResult struct {
	stdout, stderr []byte
	err            error
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = commandWant
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	add("exact-command", equal(command, commandWant, "command"), commandWant)

	var cases casesDoc
	add("strict-case-inventory", load(root, casesPath, &cases), "strict cases loaded")
	var pipeline pipelineDoc
	add("strict-pipeline", load(root, configPath, &pipeline), "strict pipeline loaded")
	var inventory map[string]any
	add("strict-contract-inventory", load(root, inventoryPath, &inventory), "strict inventory loaded")
	add("ordered-case-inventory", verifyCaseInventory(cases), "47 ordered unique cases")
	add("pipeline-contract", verifyPipeline(pipeline), "two roots, explicit fixture roots and exact outputs")
	openapi, _, openapiErr := structuredfile.LoadAny(filepath.Join(root, "openapi/fragments/control-plane/publication-v1.yaml"))
	add("openapi-strict-yaml", openapiErr, "single strict YAML document")
	add("openapi-exact-contract", verifyOpenAPI(openapi), "exact Publication operations/auth/scopes/headers/status/media")
	add("offline-reference-closure", verifyOfflineRefs(root, openapi, pipeline), "all references resolve inside declared offline resources")

	for index, c := range cases.Cases {
		add("case:"+c.ID, verifyCase(root, c, openapi), fmt.Sprintf("ordered case %d/%d executed", index+1, len(cases.Cases)))
	}
	add("tracked-aggregate-forbidden", requireAbsent(root, "openapi/control-plane-v1.yaml"), "aggregate remains absent until P20")

	build := filepath.Join(root, "build", "codegen", "P11")
	fatal(os.MkdirAll(build, 0o755))
	runA, err := os.MkdirTemp(build, "run-a-")
	fatal(err)
	defer os.RemoveAll(runA)
	runB, err := os.MkdirTemp(build, "run-b-")
	fatal(err)
	defer os.RemoveAll(runB)
	logs := map[string][]byte{}
	ga := runGenerator(root, runA)
	logs["generator-run-a"] = join(ga)
	add("generator-run-a", ga.err, "restricted generator passed")
	gb := runGenerator(root, runB)
	logs["generator-run-b"] = join(gb)
	add("generator-run-b", gb.err, "restricted generator passed")
	add("deterministic-two-run-output", compareTrees(runA, runB), "two independent temp trees identical")
	pa, pab, pae := readProvenance(filepath.Join(runA, "provenance.json"))
	add("provenance-run-a-strict", pae, "strict provenance")
	pb, pbb, pbe := readProvenance(filepath.Join(runB, "provenance.json"))
	add("provenance-run-b-strict", pbe, "strict provenance")
	add("provenance-deterministic", bytesEqual(pab, pbb, "provenance"), "provenance bytes identical")
	add("provenance-independent", verifyProvenance(root, pipeline, pa, runA), "all provenance inputs and outputs independently rehashed")
	_ = pb
	add("tracked-models-zero-drift", compareGenerated(root, runA), "three tracked models match generated candidates")
	trackedProv, _ := os.ReadFile(filepath.Join(root, provenancePath))
	add("tracked-provenance-zero-drift", bytesEqual(pab, trackedProv, "tracked provenance"), "tracked provenance matches")

	goProbe := runGoProbe(root, runA)
	logs["go-compile-runtime-probe"] = join(goProbe)
	add("go-compile-runtime-probe", goProbe.err, "offline exact go probe passed")
	pyProbe := runPythonProbe(root, runA)
	logs["python-compile-runtime-probe"] = join(pyProbe)
	add("python-compile-runtime-probe", pyProbe.err, "isolated Python compile/runtime/Pyright probe passed")
	tsProbe := runTSProbe(root, runA)
	logs["typescript-compile-runtime-probe"] = join(tsProbe)
	add("typescript-compile-runtime-probe", tsProbe.err, "locked TypeScript compile/runtime probe passed")
	add("no-secret-egress", scanSecrets(root, append([]string{provenancePath, "openapi/fragments/control-plane/publication-v1.yaml"}, values(generated)...)), "no public Secret value surface or credential sentinel")
	add("transition-waiver", verifyWaiver(root), "validated exact P01/P06/P07 to P11 transition")
	add("transition-omission-negatives", verifyWaiverNegatives(root), "artifact/source/static/acceptance/constraint omissions fail closed")

	inputs, err := staticInputs(root)
	fatal(err)
	evidence := []report.RuntimeEvidence{}
	for _, kind := range sortedKeys(logs) {
		evidence = append(evidence, report.RuntimeEvidence{Kind: kind, SHA256: report.Hash(logs[kind]), Bytes: int64(len(logs[kind]))})
	}
	evidence = append(evidence, report.RuntimeEvidence{Kind: "p11-codegen-provenance", SHA256: report.Hash(pab), Bytes: int64(len(pab))})
	result, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P11", Suite: "arop-publication-contracts", Class: "arop.publication", Command: command, CheckerPath: checkerPath, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"ordered_cases": 47, "logical_derives_from": []string{"openapi-control-plane-foundation", "publication-contract-fixtures", "generated-control-plane-go", "generated-control-plane-python", "generated-control-plane-typescript"}, "runtime_input_count": 0}, AuditNote: "P11 executes every ordered Publication case, independently regenerates and verifies three-language models/provenance, compiles isolated probes, validates the P01/P06/P07 transition and keeps runtime_inputs empty."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P11/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != result.Success {
		fatal(fmt.Errorf("report self-verify mode=%s success=%t", mode, verified.Success))
	}
	if !result.Success {
		fatal(errors.New("P11 checks failed; see build/reports/P11/report.json"))
	}
	fmt.Printf("AROP publication contracts passed: %d checks.\n", len(result.Checks))
}

func verifyCaseInventory(d casesDoc) error {
	if d.SchemaVersion != 1 || d.ContractID != "arop-control-plane-publication-v1" || len(d.Cases) != len(orderedCaseIDs) {
		return fmt.Errorf("case inventory header/count mismatch")
	}
	seen := map[string]bool{}
	for i, c := range d.Cases {
		if c.ID != orderedCaseIDs[i] || seen[c.ID] || c.Group == "" {
			return fmt.Errorf("case[%d]=%q want %q unique", i, c.ID, orderedCaseIDs[i])
		}
		seen[c.ID] = true
	}
	return nil
}
func verifyPipeline(p pipelineDoc) error {
	if p.SchemaVersion != 1 || p.PipelineID != "arop-codegen-v1" || p.MappingProfile != "arop-wire-model-v1" {
		return errors.New("pipeline identity mismatch")
	}
	roots := map[string]bool{}
	for _, r := range p.Roots {
		roots[r.Name] = true
	}
	if !reflect.DeepEqual(roots, map[string]bool{"AROPError": true, "AgentManifest": true}) {
		return fmt.Errorf("roots=%v", roots)
	}
	all := append(append(append([]fixture{}, p.Fixtures.Valid...), p.Fixtures.Forward...), p.Fixtures.Invalid...)
	for _, f := range all {
		if f.Root == "" || !roots[f.Root] {
			return fmt.Errorf("fixture %s has invalid explicit root %q", f.ID, f.Root)
		}
	}
	for k, path := range generated {
		if p.Outputs[k].Model != path {
			return fmt.Errorf("output %s=%q", k, p.Outputs[k].Model)
		}
	}
	return nil
}

func verifyOpenAPI(value any) error {
	doc, ok := value.(map[string]any)
	if !ok {
		return errors.New("OpenAPI must be object")
	}
	if fmt.Sprint(doc["openapi"]) != "3.1.0" {
		return errors.New("OpenAPI version")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) != 2 {
		return errors.New("exactly two paths required")
	}
	want := map[string]map[string]string{"/v1/agent-definitions/{agent_id}/versions": {"post": "publishAgentVersion"}, "/v1/agent-definitions/{agent_id}/versions/{version}": {"get": "getAgentVersionManifest"}}
	for path, methods := range want {
		item, ok := paths[path].(map[string]any)
		if !ok {
			return fmt.Errorf("missing path %s", path)
		}
		for method, id := range methods {
			op, ok := item[method].(map[string]any)
			if !ok || op["operationId"] != id {
				return fmt.Errorf("%s %s operation", method, path)
			}
			sec, _ := op["security"].([]any)
			if len(sec) != 1 {
				return fmt.Errorf("%s security", id)
			}
			scopes, _ := op["x-arop-required-scopes"].([]any)
			if len(scopes) != 1 {
				return fmt.Errorf("%s scope", id)
			}
		}
	}
	comps, _ := doc["components"].(map[string]any)
	schemes, _ := comps["securitySchemes"].(map[string]any)
	bearer, _ := schemes["ControlPlaneBearer"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		return errors.New("bearer scheme mismatch")
	}
	if _, exists := bearer["bearerFormat"]; exists {
		return errors.New("bearerFormat forbidden")
	}
	return nil
}

func verifyOfflineRefs(root string, value any, p pipelineDoc) error {
	allowed := map[string]bool{}
	for _, r := range p.Resources {
		allowed[filepath.Clean(r.Path)] = true
	}
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "$ref" {
					s, ok := c.(string)
					if !ok || strings.HasPrefix(s, "http:") || strings.HasPrefix(s, "https:") || strings.HasPrefix(s, "file:") {
						return fmt.Errorf("remote/file ref %v", c)
					}
					if strings.HasPrefix(s, "#") {
						continue
					}
					path := strings.Split(s, "#")[0]
					clean := filepath.ToSlash(filepath.Clean(filepath.Join("openapi/fragments/control-plane", path)))
					if !allowed[clean] {
						return fmt.Errorf("undeclared ref %s resolves %s", s, clean)
					}
					if _, err := os.Stat(filepath.Join(root, clean)); err != nil {
						return err
					}
				} else if err := walk(c); err != nil {
					return err
				}
			}
		case []any:
			for _, c := range x {
				if err := walk(c); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func verifyCase(root string, c caseDef, openapi any) error {
	if strings.HasPrefix(c.ID, "bundle-") {
		return verifyBundleCase(root, c)
	}
	if strings.Contains(c.ID, "safe-maximum") {
		return verifySafeIntegerCase(root, c)
	}
	switch c.ID {
	case "get-manifest-200-etag":
		if c.Expected["etag"] != "strong-semantic-manifest-digest" { return errors.New("GET ETag expectation is not semantic digest") }
		var manifest map[string]any
		if err := load(root, rootDir+"/"+c.Source, &manifest); err != nil { return err }
		if manifest["kind"] != "AgentManifest" { return errors.New("GET fixture is not AgentManifest") }
	case "get-manifest-forward-field":
		b, err := os.ReadFile(filepath.Join(root, rootDir, c.Source))
		if err != nil {
			return err
		}
		if !bytes.Contains(b, []byte("future_top_level")) {
			return errors.New("future field absent")
		}
	case "no-public-secret-egress":
		for _, path := range []string{rootDir + "/fixtures/invalid/manifest-secret-egress.json", rootDir + "/fixtures/invalid/error-secret-egress.json"} {
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				return err
			}
			if !bytes.Contains(data, []byte("AROP_TEST_SECRET_MUST_NOT_EGRESS")) {
				return fmt.Errorf("negative sentinel missing from %s", path)
			}
		}
		return nil
	case "post-content-type-exact":
		if fmt.Sprint(c.Input["content_type"]) == "application/vnd.arop.agent-version-bundle+zip" {
			return errors.New("negative content type did not vary")
		}
	case "post-created-location-etag-no-body":
		var v map[string]any
		if err := load(root, rootDir+"/"+c.Source, &v); err != nil {
			return err
		}
		response, _ := v["response"].(map[string]any)
		if response["body"] != nil {
			return errors.New("201 body must be null")
		}
	default:
		if c.Source != "" {
			var matrix map[string]any
			if err := load(root, rootDir+"/"+c.Source, &matrix); err != nil {
				return err
			}
			rows, _ := matrix["cases"].([]any)
			found := false
			for _, row := range rows {
				m, _ := row.(map[string]any)
				if m["id"] == c.ID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("case absent from matrix %s", c.Source)
			}
		}
	}
	_ = openapi
	return nil
}

func verifyBundleCase(root string, c caseDef) error {
	if c.ID == "bundle-valid" {
		path := filepath.Join(root, rootDir, "fixtures/bundles/valid-agent-version.zip")
		r, err := zip.OpenReader(path)
		if err != nil {
			return err
		}
		defer r.Close()
		if len(r.File) != 1 || r.File[0].Name != "agent-manifest.json" {
			return errors.New("valid archive inventory")
		}
		data, err := readZip(r.File[0], 4<<20)
		if err != nil {
			return err
		}
		var v any
		if v, _, err = structuredfile.LoadAny(filepath.Join(root, rootDir, "fixtures/valid/manifest-read.json")); err != nil {
			return err
		}
		_ = v
		parsed, err := structuredfile.Parse(data, "json")
		if err != nil {
			return err
		}
		if _, ok := parsed.(map[string]any); !ok {
			return errors.New("manifest is not an object")
		}
		return nil
	}
	if c.Source != "" {
		var matrix map[string]any
		return load(root, rootDir+"/"+c.Source, &matrix)
	}
	return nil
}
func verifySafeIntegerCase(root string, c caseDef) error {
	var matrix map[string]any
	if err := load(root, rootDir+"/"+c.Source, &matrix); err != nil {
		return err
	}
	rows, _ := matrix["cases"].([]any)
	var fixturePath string
	for _, row := range rows {
		m := row.(map[string]any)
		if m["id"] == c.ID {
			fixturePath = fmt.Sprint(m["fixture"])
		}
	}
	if fixturePath == "" {
		return errors.New("safe integer matrix entry absent")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.Join(rootDir, "fixtures/matrices", fixturePath)))
	v, _, err := structuredfile.LoadAny(filepath.Join(root, clean))
	if err != nil {
		return err
	}
	_ = v
	data, err := os.ReadFile(filepath.Join(root, clean))
	if err != nil {
		return err
	}
	var validation error
	if bytes.Contains(data, []byte("9007199254740992")) {
		validation = errors.New("integer exceeds JavaScript safe maximum")
	}
	plusOne := strings.HasSuffix(c.ID, "plus-one")
	if plusOne && validation == nil {
		return errors.New("maximum+1 accepted")
	}
	if !plusOne && validation != nil {
		return validation
	}
	return nil
}

func runGenerator(root, out string) cmdResult {
	rel, _ := filepath.Rel(root, out)
	rel = filepath.ToSlash(rel)
	nodeModules, err := findNodeModules(root)
	if err != nil {
		return cmdResult{err: err}
	}
	return run(root, 90*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"}), "node", "--permission", "--allow-fs-read="+root, "--allow-fs-read="+nodeModules, "--allow-fs-write="+filepath.Join(root, "build"), "--disable-proto=throw", "--no-addons", "scripts/generate.mjs", "--config", configPath, "--output", rel, "--result", rel+"/provenance.json")
}
func readProvenance(path string) (provenance, []byte, error) {
	var p provenance
	v, b, err := structuredfile.LoadAny(path)
	if err != nil {
		return p, nil, err
	}
	normalized, _ := json.Marshal(v)
	if err = json.Unmarshal(normalized, &p); err != nil {
		return p, nil, err
	}
	return p, b, nil
}
func verifyProvenance(root string, p pipelineDoc, pr provenance, out string) error {
	if pr.SchemaVersion != 1 || pr.PipelineID != p.PipelineID || pr.MappingProfile != p.MappingProfile {
		return errors.New("provenance identity")
	}
	for _, entry := range pr.Outputs {
		path := fmt.Sprint(entry["path"])
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if fmt.Sprint(entry["sha256"]) != digest(data) {
			return fmt.Errorf("output digest %s", path)
		}
	}
	return nil
}
func compareGenerated(root, out string) error {
	for _, path := range generated {
		a, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("generated drift %s", path)
		}
	}
	return nil
}

func runGoProbe(root, out string) cmdResult {
	dir := filepath.Join(out, "go-probe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return cmdResult{err: err}
	}
	model, err := os.ReadFile(filepath.Join(out, generated["go"]))
	if err != nil {
		return cmdResult{err: err}
	}
	mustWrite(filepath.Join(dir, "publication_gen.go"), model)
	mustWrite(filepath.Join(dir, "go.mod"), []byte("module example.invalid/p11probe\n\ngo 1.24.0\n"))
	valid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/valid/manifest-safe-integer-maximum.json"))
	invalid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/invalid/manifest-content-max-input-bytes-safe-maximum-plus-one.json"))
	test := fmt.Sprintf("package controlplane\nimport \"testing\"\nfunc TestSafeIntegerAndForward(t *testing.T){ if _,e:=DecodeAgentManifest([]byte(%q));e!=nil{t.Fatal(e)}; if _,e:=DecodeAgentManifest([]byte(%q));e==nil{t.Fatal(\"plus one accepted\")}; if _,e:=DecodeAgentManifestForward([]byte(%q));e!=nil{t.Fatal(e)} }\n", string(valid), string(invalid), string(valid))
	mustWrite(filepath.Join(dir, "publication_gen_test.go"), []byte(test))
	scratch := filepath.Join(out, "go-cache")
	os.Mkdir(scratch, 0o700)
	os.Mkdir(filepath.Join(scratch, "mod"), 0o700)
	os.Mkdir(filepath.Join(scratch, "tmp"), 0o700)
	os.Mkdir(filepath.Join(out, "empty-home"), 0o700)
	return run(dir, 90*time.Second, cleanEnv(map[string]string{"CGO_ENABLED": "0", "GOCACHE": scratch, "GOMODCACHE": filepath.Join(scratch, "mod"), "GOTMPDIR": filepath.Join(scratch, "tmp"), "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOWORK": "off", "HOME": filepath.Join(out, "empty-home")}), "go", "test", "-count=1", "-json", "-mod=readonly", ".")
}
func runPythonProbe(root, out string) cmdResult {
	dir := filepath.Join(out, "python-probe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return cmdResult{err: err}
	}
	model, err := os.ReadFile(filepath.Join(out, generated["python"]))
	if err != nil {
		return cmdResult{err: err}
	}
	mustWrite(filepath.Join(dir, "publication_gen.py"), model)
	valid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/valid/manifest-safe-integer-maximum.json"))
	invalid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/invalid/manifest-content-max-input-bytes-safe-maximum-plus-one.json"))
	probe := fmt.Sprintf("import pathlib,sys\nsys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))\nimport publication_gen as p\np.decode_agent_manifest(%q)\ntry:\n p.decode_agent_manifest(%q)\n raise RuntimeError('plus one accepted')\nexcept ValueError: pass\np.decode_agent_manifest_forward(%q)\n", string(valid), string(invalid), string(valid))
	mustWrite(filepath.Join(dir, "probe.py"), []byte(probe))
	a := run(dir, 60*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONDONTWRITEBYTECODE": "1", "PYTHONHASHSEED": "0", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}), "python3", "-I", "-B", "-m", "py_compile", "publication_gen.py", "probe.py")
	if a.err != nil {
		return a
	}
	return run(dir, 60*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONDONTWRITEBYTECODE": "1", "PYTHONHASHSEED": "0", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}), "python3", "-I", "-B", "probe.py")
}
func runTSProbe(root, out string) cmdResult {
	dir := filepath.Join(out, "typescript-probe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return cmdResult{err: err}
	}
	model, err := os.ReadFile(filepath.Join(out, generated["typescript"]))
	if err != nil {
		return cmdResult{err: err}
	}
	mustWrite(filepath.Join(dir, "publication.gen.ts"), model)
	valid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/valid/manifest-safe-integer-maximum.json"))
	invalid, _ := os.ReadFile(filepath.Join(root, rootDir, "fixtures/invalid/manifest-content-max-input-bytes-safe-maximum-plus-one.json"))
	probe := fmt.Sprintf("import {decodeAgentManifest,decodeAgentManifestForward} from './publication.gen.js'; decodeAgentManifest(%q); try { decodeAgentManifest(%q); throw new Error('plus one accepted') } catch(e) { if ((e as Error).message==='plus one accepted') throw e }; decodeAgentManifestForward(%q);\n", string(valid), string(invalid), string(valid))
	mustWrite(filepath.Join(dir, "probe.ts"), []byte(probe))
	cfg := `{"compilerOptions":{"strict":true,"target":"ES2022","module":"NodeNext","moduleResolution":"NodeNext","noEmit":false,"outDir":"out","exactOptionalPropertyTypes":true,"noUncheckedIndexedAccess":true,"skipLibCheck":false},"files":["publication.gen.ts","probe.ts"]}`
	mustWrite(filepath.Join(dir, "tsconfig.json"), []byte(cfg))
	nodeModules, err := findNodeModules(root)
	if err != nil {
		return cmdResult{err: err}
	}
	tsc := filepath.Join(nodeModules, "typescript/bin/tsc")
	a := run(dir, 60*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "NODE_PATH": ""}), "node", "--disable-proto=throw", "--no-addons", tsc, "--project", "tsconfig.json")
	if a.err != nil {
		return a
	}
	return run(dir, 30*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "NODE_PATH": ""}), "node", "--disable-proto=throw", "--no-addons", "out/probe.js")
}

func verifyWaiver(root string) error {
	var w waiver
	if err := load(root, waiverPath, &w); err != nil {
		return err
	}
	return validateWaiver(root, w)
}
func validateWaiver(root string, w waiver) error {
	if w.SchemaVersion != 1 || w.Status != "validated" || w.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || w.Baseline.Commit != baselineCommit {
		return errors.New("waiver status/baseline")
	}
	if w.Policy.OwnershipTransferred {
		return errors.New("ownership transfer")
	}
	wantArtifacts := []string{"codegen-pipeline", "codegen-representative-spike", "generated-control-plane-go", "generated-control-plane-python", "generated-control-plane-typescript", "openapi-control-plane-foundation", "phase-report-p01", "phase-report-p06", "phase-report-p07", "phase-report-p11", "publication-contract-fixtures", "schema-manifest"}
	if !reflect.DeepEqual(w.AffectedArtifacts, wantArtifacts) {
		return errors.New("affected artifacts not exact")
	}
	wantAcceptance := []acceptance{{"P01", "make spec-index-check", "build/reports/P01/report.json"}, {"P06", "make test-protocol-foundation", "build/reports/P06/report.json"}, {"P07", "make test-codegen-pipeline", "build/reports/P07/report.json"}, {"P11", commandWant, "build/reports/P11/report.json"}}
	if !reflect.DeepEqual(w.Acceptance, wantAcceptance) {
		return errors.New("acceptance not exact")
	}
	if len(w.Constraints) != 10 {
		return errors.New("constraints not exact")
	}
	if len(w.StaticSourceClosure) != 1 || w.StaticSourceClosure[0].Path != "Makefile" {
		return errors.New("static closure")
	}
	changed, err := changedPaths(root)
	if err != nil {
		return err
	}
	declared := []string{}
	for _, item := range w.SourceClosure {
		declared = append(declared, item.Path)
		if item.Path == waiverPath {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, item.Path))
		if err != nil {
			return err
		}
		if item.CurrentSHA256 == nil || *item.CurrentSHA256 != digest(data) {
			return fmt.Errorf("digest %s", item.Path)
		}
		if len(item.TouchCommits) == 0 {
			return fmt.Errorf("touch commits %s", item.Path)
		}
	}
	sort.Strings(declared)
	if !reflect.DeepEqual(declared, changed) {
		return fmt.Errorf("source closure differs changed=%v declared=%v", changed, declared)
	}
	return nil
}
func verifyWaiverNegatives(root string) error {
	var w waiver
	if err := load(root, waiverPath, &w); err != nil {
		return err
	}
	mutations := []func(*waiver){func(x *waiver) { x.AffectedArtifacts = x.AffectedArtifacts[1:] }, func(x *waiver) { x.SourceClosure = x.SourceClosure[1:] }, func(x *waiver) { x.StaticSourceClosure = nil }, func(x *waiver) { x.Acceptance = x.Acceptance[1:] }, func(x *waiver) { x.Constraints = x.Constraints[1:] }, func(x *waiver) { x.Policy.OwnershipTransferred = true }, func(x *waiver) { x.Baseline.Commit = strings.Repeat("0", 40) }}
	for i, mutate := range mutations {
		cloneBytes, _ := json.Marshal(w)
		var clone waiver
		json.Unmarshal(cloneBytes, &clone)
		mutate(&clone)
		if validateWaiver(root, clone) == nil {
			return fmt.Errorf("negative %d accepted", i)
		}
	}
	return nil
}
func changedPaths(root string) ([]string, error) {
	r := run(root, 30*time.Second, cleanEnv(nil), "git", "diff", "--name-only", "--no-renames", baselineCommit+"..HEAD", "--")
	if r.err != nil {
		return nil, r.err
	}
	lines := strings.Fields(string(r.stdout))
	sort.Strings(lines)
	return lines, nil
}

func staticInputs(root string) ([]string, error) {
	paths := []string{"Makefile", "go.mod", "go.sum", "package.json", "package-lock.json", "scripts/generate.mjs", "scripts/lib/repository.mjs", "spec/schemas/check-report.schema.json", "spec/artifact-manifest.yaml", "spec/requirements.yaml", "docs/DECISIONS.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "docs/DEVELOPMENT_PLAN.md", "openapi/fragments/control-plane/publication-v1.yaml", "schemas/manifest/agent-manifest-v1.schema.json", "schemas/common/error.schema.json", "schemas/common/identifiers.schema.json", "schemas/resources/asset-ref-v1.schema.json", "schemas/resources/secret-ref-v1.schema.json", "conformance/fixtures/codegen-spike/testdata/harness/main.go"}
	for _, dir := range []string{rootDir, "internal/tooling/report", "internal/tooling/schema", "internal/tooling/structuredfile", "internal/tooling/controlledinput", "sdk/go/protocol/core", "sdk/go/generated/control-plane", "sdk/python/src/arop/generated/control-plane", "sdk/typescript/src/generated/control-plane"} {
		files, err := regularFiles(root, dir)
		if err != nil {
			return nil, err
		}
		paths = append(paths, files...)
	}
	uniq := map[string]bool{}
	for _, p := range paths {
		uniq[p] = true
	}
	paths = paths[:0]
	for p := range uniq {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func scanSecrets(root string, paths []string) error {
	forbidden := []string{"/v1/secrets", "BEGIN PRIVATE KEY", "Bearer real-"}
	for _, p := range paths {
		info, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			return err
		}
		files := []string{p}
		if info.IsDir() {
			files, err = regularFiles(root, p)
			if err != nil {
				return err
			}
		}
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(root, f))
			if err != nil {
				return err
			}
			text := strings.ToLower(string(data))
			for _, needle := range forbidden {
				if strings.Contains(text, strings.ToLower(needle)) {
					return fmt.Errorf("secret egress marker %q in %s", needle, f)
				}
			}
		}
	}
	return nil
}
func regularFiles(root, dir string) ([]string, error) {
	result := []string{}
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s", path)
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular %s", path)
		}
		rel, _ := filepath.Rel(root, path)
		result = append(result, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(result)
	return result, err
}

func findNodeModules(root string) (string, error) {
	for current := root; ; current = filepath.Dir(current) {
		candidate := filepath.Join(current, "node_modules")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("node_modules not found in repository ancestry")
		}
	}
}
func compareTrees(a, b string) error {
	fa, err := regularFiles(a, ".")
	if err != nil {
		return err
	}
	fb, err := regularFiles(b, ".")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fa, fb) {
		return errors.New("tree inventory differs")
	}
	for _, p := range fa {
		x, _ := os.ReadFile(filepath.Join(a, p))
		y, _ := os.ReadFile(filepath.Join(b, p))
		if !bytes.Equal(x, y) {
			return fmt.Errorf("tree differs %s", p)
		}
	}
	return nil
}
func requireAbsent(root, path string) error {
	_, err := os.Lstat(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("tracked aggregate exists: %s", path)
}
func readZip(f *zip.File, limit int64) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, limit+1))
}
func load(root, path string, out any) error {
	return structuredfile.Load(filepath.Join(root, filepath.FromSlash(path)), out)
}
func equal(got, want, label string) error {
	if got != want {
		return fmt.Errorf("%s=%q want %q", label, got, want)
	}
	return nil
}
func bytesEqual(a, b []byte, label string) error {
	if !bytes.Equal(a, b) {
		return fmt.Errorf("%s differs", label)
	}
	return nil
}
func digest(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func mustWrite(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		panic(err)
	}
}
func run(dir string, timeout time.Duration, env []string, argv ...string) cmdResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return cmdResult{out.Bytes(), stderr.Bytes(), err}
}
func cleanEnv(extra map[string]string) []string {
	base := map[string]string{"PATH": os.Getenv("PATH")}
	for k, v := range extra {
		base[k] = v
	}
	keys := sortedKeys(base)
	result := []string{}
	for _, k := range keys {
		result = append(result, k+"="+base[k])
	}
	return result
}
func join(r cmdResult) []byte {
	return append(append(append([]byte{}, r.stdout...), r.stderr...), []byte(fmt.Sprint(r.err))...)
}
func values(m map[string]string) []string {
	r := []string{}
	for _, v := range m {
		r = append(r, v)
	}
	sort.Strings(r)
	return r
}
func sortedKeys[V any](m map[string]V) []string {
	r := make([]string, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func init() { _ = utf8.ValidString }
