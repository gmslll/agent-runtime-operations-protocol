// Command harness independently verifies the P07 three-language code-generation
// spike and emits the repository's standard auditable report. The generator is
// deliberately treated as untrusted: it may propose provenance, but this
// program rediscovers every input and output used for acceptance.
package main

import (
	"bufio"
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

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	expectedCommand = "make test-codegen-pipeline"
	checkerPath     = "conformance/fixtures/codegen-spike/testdata/harness/main.go"
	fixtureRoot     = "conformance/fixtures/codegen-spike"
	pipelinePath    = fixtureRoot + "/pipeline.json"
	casesPath       = fixtureRoot + "/cases.json"
	expectedRoot    = fixtureRoot + "/expected"
	alternateRoot   = fixtureRoot + "/alternate"
	alternateConfig = alternateRoot + "/pipeline.json"
	alternateCases  = alternateRoot + "/cases.json"
	buildRoot       = "build/codegen/P07"
	provenanceName  = "provenance.json"
)

var generatedPaths = []string{
	"go/models.gen.go",
	"go/models_gen_test.go",
	"python/models_gen.py",
	"python/probe.py",
	"typescript/models.gen.ts",
	"typescript/probe.ts",
}

type fileEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type fileManifest struct {
	Entries []fileEntry `json:"entries"`
	SHA256  string      `json:"sha256"`
}

type casesDocument struct {
	SchemaVersion int        `json:"schema_version"`
	Valid         []caseItem `json:"valid"`
	Forward       []caseItem `json:"forward"`
	Invalid       []caseItem `json:"invalid"`
}

type pipelineDocument struct {
	SchemaVersion  int                   `json:"schema_version"`
	PipelineID     string                `json:"pipeline_id"`
	MappingProfile string                `json:"mapping_profile"`
	Resources      []pipelineResource    `json:"resources"`
	Roots          []pipelineRoot        `json:"roots"`
	Fixtures       pipelineFixtures      `json:"fixtures"`
	Outputs        map[string]outputPair `json:"outputs"`
}

type pipelineResource struct {
	Path string `json:"path"`
	URI  string `json:"uri"`
}

type pipelineRoot struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

type pipelineFixtures struct {
	Valid   []caseItem `json:"valid"`
	Forward []caseItem `json:"forward"`
	Invalid []caseItem `json:"invalid"`
}

type outputPair struct {
	Model string `json:"model"`
	Probe string `json:"probe"`
}

type caseItem struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type probeResult struct {
	SchemaVersion int               `json:"schema_version"`
	Language      string            `json:"language"`
	Cases         []probeCaseResult `json:"cases"`
	Rejections    []probeCaseResult `json:"rejections"`
}

type probeCaseResult struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
}

type provenanceDocument struct {
	SchemaVersion  int                  `json:"schema_version"`
	PipelineID     string               `json:"pipeline_id"`
	MappingProfile string               `json:"mapping_profile"`
	Command        provenanceCommand    `json:"command"`
	Generator      provenanceGenerator  `json:"generator"`
	Configuration  provenanceFile       `json:"configuration"`
	Resources      []provenanceResource `json:"resources"`
	Fixtures       []provenanceFixture  `json:"fixtures"`
	InputsSHA256   string               `json:"inputs_sha256"`
	Outputs        []provenanceOutput   `json:"outputs"`
	OutputsSHA256  string               `json:"outputs_sha256"`
	Checks         []provenanceCheck    `json:"checks"`
}

type provenanceCommand struct {
	Entry  string `json:"entry"`
	Config string `json:"config"`
	Output string `json:"output"`
	Result string `json:"result"`
}

type provenanceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode"`
}

type provenanceGenerator struct {
	Path   string           `json:"path"`
	SHA256 string           `json:"sha256"`
	Files  []provenanceFile `json:"files"`
}

type provenanceResource struct {
	Path   string `json:"path"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode"`
}

type provenanceFixture struct {
	ID     string `json:"id"`
	Class  string `json:"class"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode"`
}

type provenanceOutput struct {
	Language string `json:"language"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
}

type provenanceCheck struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type commandResult struct {
	Argv   []string
	Stdout []byte
	Stderr []byte
	Err    error
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)

	checks := []report.Check{}
	add := func(name string, checkErr error, success string) {
		detail := success
		if checkErr != nil {
			detail = checkErr.Error()
		}
		checks = append(checks, report.Check{Name: name, Passed: checkErr == nil, Detail: detail})
	}

	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./" + fixtureRoot + "/testdata/harness"
	}
	var commandErr error
	if command != expectedCommand {
		commandErr = fmt.Errorf("command=%q want exact %q", command, expectedCommand)
	}
	add("exact-command", commandErr, expectedCommand)

	inputPaths, before, err := staticInputManifest(root)
	fatalIf(err, "P07 static input closure rejected before execution")
	add("static-input-closure-before-execution", nil, fmt.Sprintf("%d regular, symlink-free inputs; sha256=%s", len(before.Entries), before.SHA256))

	cases, err := loadCases(root)
	add("case-inventory", err, fmt.Sprintf("valid=%d forward=%d invalid=%d", len(cases.Valid), len(cases.Forward), len(cases.Invalid)))
	if err != nil {
		fatal(writeFailureReport(root, command, inputPaths, checks, nil, err))
	}

	fatal(secureMkdirAll(root, buildRoot))
	runA, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "run-a-")
	fatal(err)
	defer os.RemoveAll(runA)
	runB, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "run-b-")
	fatal(err)
	defer os.RemoveAll(runB)

	logs := map[string][]byte{}
	resultA := runGenerator(root, runA, pipelinePath)
	logs["generator-run-a"] = joinLog(resultA)
	add("generator-run-a", resultA.Err, "restricted Node generator completed")
	resultB := runGenerator(root, runB, pipelinePath)
	logs["generator-run-b"] = joinLog(resultB)
	add("generator-run-b", resultB.Err, "restricted Node generator completed")

	manifestA, errA := generatedManifest(runA)
	add("generated-run-a-exact-inventory", errA, manifestA.SHA256)
	manifestB, errB := generatedManifest(runB)
	add("generated-run-b-exact-inventory", errB, manifestB.SHA256)
	deterministicErr := errors.Join(errA, errB)
	if deterministicErr == nil && !reflect.DeepEqual(manifestA, manifestB) {
		deterministicErr = diffManifests("run-a", manifestA, "run-b", manifestB)
	}
	add("deterministic-regeneration", deterministicErr, "independent staging trees are byte-for-byte identical")

	tracked, trackedErr := goldenManifest(root)
	add("tracked-golden-exact-inventory", trackedErr, tracked.SHA256)
	driftErr := errors.Join(errA, trackedErr)
	if driftErr == nil && !reflect.DeepEqual(manifestA, tracked) {
		driftErr = diffManifests("generated", manifestA, "tracked-golden", tracked)
	}
	add("clean-regenerate-zero-drift", driftErr, "generated path/mode/bytes/digest inventory exactly matches tracked .golden files")

	provenanceA, provenanceABytes, provenanceAErr := loadProvenance(runA)
	add("provenance-run-a-strict", provenanceAErr, "strict deterministic provenance loaded")
	_, provenanceBBytes, provenanceBErr := loadProvenance(runB)
	add("provenance-run-b-strict", provenanceBErr, "strict deterministic provenance loaded")
	provenanceDeterminismErr := errors.Join(provenanceAErr, provenanceBErr)
	if provenanceDeterminismErr == nil && !bytes.Equal(provenanceABytes, provenanceBBytes) {
		provenanceDeterminismErr = errors.New("run-a and run-b provenance bytes differ")
	}
	add("provenance-deterministic", provenanceDeterminismErr, "provenance is independent of staging path and runtime clock")
	if provenanceAErr == nil && errA == nil {
		add("provenance-independently-recomputed", verifyProvenance(root, provenanceA, manifestA, cases), "generator/config/resources/fixtures/outputs were independently hashed")
		tampered := provenanceA
		tampered.OutputsSHA256 = strings.Repeat("0", 64)
		tamperErr := verifyProvenance(root, tampered, manifestA, cases)
		if tamperErr == nil {
			tamperErr = errors.New("independent verifier accepted tampered outputs_sha256")
		} else {
			tamperErr = nil
		}
		add("provenance-tamper-negative", tamperErr, "independent verifier rejected a self-reported digest mutation")
	}

	probeCases := append(append([]caseItem(nil), cases.Valid...), cases.Forward...)
	goResult := runGoCandidate(root, runA, probeCases, cases.Invalid)
	logs["go-compile-probe"] = joinLog(goResult)
	add("go-generated-compile-and-probe", goResult.Err, "offline go test compiled every generated Go source and passed the exact case inventory")

	pythonCompile, pythonProbe := runPythonCandidate(root, runA, probeCases, cases.Invalid)
	logs["python-compile"] = joinLog(pythonCompile)
	logs["python-probe"] = joinLog(pythonProbe)
	add("python-generated-compile", pythonCompile.Err, "python -I -S -B compiled generated sources")
	add("python-generated-probe", pythonProbe.Err, "Python probe passed the exact case inventory")

	pyright := runPyrightIfPresent(root, runA)
	if len(pyright.Argv) != 0 {
		logs["python-pyright"] = joinLog(pyright)
		add("python-generated-pyright", pyright.Err, "locked local Pyright accepted generated sources")
	} else {
		add("python-generated-pyright", nil, "locked Pyright dependency absent; runtime compile/import probe remains mandatory")
	}

	tsc, tsProbe := runTypeScriptCandidate(root, runA, probeCases, cases.Invalid)
	logs["typescript-compile"] = joinLog(tsc)
	logs["typescript-probe"] = joinLog(tsProbe)
	add("typescript-generated-typecheck", tsc.Err, "locked local TypeScript compiler accepted the exact generated source set under strict flags")
	add("typescript-generated-probe", tsProbe.Err, "restricted Node executed emitted TypeScript probe for the exact case inventory")

	alternate := runAlternateSuite(root, add, logs)

	negativeLog, negativeChecks := runNegativeConfigs(root, cases)
	logs["negative-configs"] = negativeLog
	checks = append(checks, negativeChecks...)

	afterPaths, after, afterErr := staticInputManifest(root)
	if afterErr == nil && !reflect.DeepEqual(inputPaths, afterPaths) {
		afterErr = errors.New("static input path set changed during P07 execution")
	}
	if afterErr == nil && !reflect.DeepEqual(before, after) {
		afterErr = diffManifests("before", before, "after", after)
	}
	add("static-input-closure-after-execution", afterErr, "tracked static input path/mode/bytes/digest manifest remained unchanged")

	runtimeEvidence := make([]report.RuntimeEvidence, 0, len(logs)+2)
	logKinds := make([]string, 0, len(logs))
	for kind := range logs {
		logKinds = append(logKinds, kind)
	}
	sort.Strings(logKinds)
	for _, kind := range logKinds {
		data := logs[kind]
		runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: kind, SHA256: report.Hash(data), Bytes: int64(len(data))})
	}
	if provenanceABytes != nil {
		runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "codegen-provenance", SHA256: report.Hash(provenanceABytes), Bytes: int64(len(provenanceABytes))})
	}
	manifestBytes, _ := json.Marshal(manifestA)
	runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "generated-output-manifest", SHA256: report.Hash(manifestBytes), Bytes: int64(len(manifestBytes))})

	result, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P07", Suite: "arop-codegen-pipeline", Class: "arop.codegen",
		Command: command, CheckerPath: checkerPath, InputPaths: inputPaths, RuntimeInputPaths: []string{}, RuntimeEvidence: runtimeEvidence,
		Checks: checks,
		Summary: map[string]any{
			"valid_cases": len(cases.Valid), "forward_cases": len(cases.Forward), "invalid_cases": len(cases.Invalid), "generated_files": len(manifestA.Entries),
			"alternate_valid_cases": len(alternate.Valid), "alternate_forward_cases": len(alternate.Forward), "alternate_invalid_cases": len(alternate.Invalid),
			"generated_outputs_sha256": manifestA.SHA256, "runtime_input_count": 0,
		},
		AuditNote: "P07 snapshots an exact symlink-free static closure before execution; regenerates both the representative spike and an unrelated alternate root into independent staging trees; independently verifies provenance, exact outputs, drift and hardcode isolation; and compiles and probes Go, Python, and TypeScript candidates; runtime_inputs is intentionally empty.",
	})
	fatal(err)
	verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P07/report.json"})
	fatal(verifyErr)
	if mode != "current-worktree" || verified.Success != result.Success {
		fatal(fmt.Errorf("P07 report self-verification returned mode=%s success=%t", mode, verified.Success))
	}
	if !result.Success {
		fatal(errors.New("P07 codegen pipeline checks failed; see build/reports/P07/report.json"))
	}
	fmt.Printf("AROP codegen pipeline passed: %d checks.\n", len(result.Checks))
}

func runAlternateSuite(root string, add func(string, error, string), logs map[string][]byte) casesDocument {
	cases, casesErr := loadCasesAt(root, alternateCases, alternateRoot)
	add("alternate:case-inventory", casesErr, fmt.Sprintf("valid=%d forward=%d invalid=%d", len(cases.Valid), len(cases.Forward), len(cases.Invalid)))
	pipeline, pipelineErr := loadPipelineAt(root, alternateConfig, alternateCases, alternateRoot)
	if pipelineErr == nil && (len(pipeline.Roots) != 1 || pipeline.Roots[0].Name != "AlternateEnvelope") {
		pipelineErr = fmt.Errorf("alternate root=%v want exact AlternateEnvelope", pipeline.Roots)
	}
	add("alternate:contract", pipelineErr, "AlternateEnvelope uses the same fixed pipeline, output, fixture and probe contracts")
	coverageErr := requireCaseCoverage(cases, map[string][]string{
		"valid":   {"alternate-inline-object", "alternate-typed-additional", "alternate-mathematical-integer", "alternate-ipv6-uri"},
		"forward": {"alternate-forward-union"},
		"invalid": {
			"alternate-closed-empty",
			"alternate-duplicate-key",
			"alternate-nonnullable-null",
			"alternate-bad-uri",
			"alternate-json-large-integer",
			"alternate-year-zero",
			"alternate-typed-additional-invalid",
		},
	})
	coverageErr = errors.Join(coverageErr, verifyAlternateSchemaContract(root))
	add("alternate:required-mapping-coverage", coverageErr, "inline and closed-empty objects, typed additionalProperties, a non-type const discriminator, forward union, duplicate-key, nullability, URI, safe-integer, calendar and mathematical-integer semantics have explicit authoritative cases")
	if casesErr != nil || pipelineErr != nil || coverageErr != nil {
		return cases
	}

	runA, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "alternate-a-")
	if err != nil {
		add("alternate:staging", err, "two independent staging directories created")
		return cases
	}
	defer os.RemoveAll(runA)
	runB, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "alternate-b-")
	if err != nil {
		add("alternate:staging", err, "two independent staging directories created")
		return cases
	}
	defer os.RemoveAll(runB)
	add("alternate:staging", nil, "two independent staging directories created")

	resultA := runGenerator(root, runA, alternateConfig)
	logs["alternate-generator-run-a"] = joinLog(resultA)
	add("alternate:generator-run-a", resultA.Err, "restricted Node generator completed for unrelated root")
	resultB := runGenerator(root, runB, alternateConfig)
	logs["alternate-generator-run-b"] = joinLog(resultB)
	add("alternate:generator-run-b", resultB.Err, "restricted Node generator completed for unrelated root")

	manifestA, errA := generatedManifest(runA)
	add("alternate:run-a-exact-inventory", errA, manifestA.SHA256)
	manifestB, errB := generatedManifest(runB)
	add("alternate:run-b-exact-inventory", errB, manifestB.SHA256)
	deterministicErr := errors.Join(errA, errB)
	if deterministicErr == nil && !reflect.DeepEqual(manifestA, manifestB) {
		deterministicErr = diffManifests("alternate-a", manifestA, "alternate-b", manifestB)
	}
	add("alternate:deterministic-regeneration", deterministicErr, "unrelated root staging trees are byte-for-byte identical")
	if encoded, marshalErr := json.Marshal(manifestA); marshalErr == nil {
		logs["alternate-generated-output-manifest"] = encoded
	}

	provenanceA, provenanceABytes, provenanceAErr := loadProvenance(runA)
	add("alternate:provenance-run-a-strict", provenanceAErr, "strict alternate provenance loaded")
	_, provenanceBBytes, provenanceBErr := loadProvenance(runB)
	add("alternate:provenance-run-b-strict", provenanceBErr, "strict alternate provenance loaded")
	provenanceDeterminismErr := errors.Join(provenanceAErr, provenanceBErr)
	if provenanceDeterminismErr == nil && !bytes.Equal(provenanceABytes, provenanceBBytes) {
		provenanceDeterminismErr = errors.New("alternate run-a and run-b provenance bytes differ")
	}
	add("alternate:provenance-deterministic", provenanceDeterminismErr, "alternate provenance is independent of staging path and runtime clock")
	provenanceVerifyErr := errors.Join(provenanceAErr, errA)
	if provenanceVerifyErr == nil {
		provenanceVerifyErr = verifyProvenanceAt(root, provenanceA, manifestA, cases, alternateConfig, alternateCases, alternateRoot)
	}
	add("alternate:provenance-independently-recomputed", provenanceVerifyErr, "alternate generator/config/resource/fixtures/outputs were independently hashed")
	if provenanceABytes != nil {
		logs["alternate-codegen-provenance"] = provenanceABytes
	}

	leakageErr := errA
	if leakageErr == nil {
		leakageErr = verifyAlternateOutputIsolation(runA, pipeline.Roots[0].Name)
	}
	add("alternate:no-spike-hardcode-leakage", leakageErr, "all six outputs use AlternateEnvelope and contain no spike-only root, package, type or field tokens")

	accepted := append(append([]caseItem(nil), cases.Valid...), cases.Forward...)
	goResult := runGoCandidate(root, runA, accepted, cases.Invalid)
	logs["alternate-go-compile-probe"] = joinLog(goResult)
	add("alternate:go-generated-compile-and-probe", goResult.Err, "offline Go compile and exact case/rejection subtests passed")
	pythonCompile, pythonProbe := runPythonCandidate(root, runA, accepted, cases.Invalid)
	logs["alternate-python-compile"] = joinLog(pythonCompile)
	logs["alternate-python-probe"] = joinLog(pythonProbe)
	add("alternate:python-generated-compile", pythonCompile.Err, "isolated Python compile passed")
	add("alternate:python-generated-probe", pythonProbe.Err, "Python exact cases and rejections passed")
	pyright := runPyrightIfPresent(root, runA)
	logs["alternate-python-pyright"] = joinLog(pyright)
	add("alternate:python-generated-pyright", pyright.Err, "locked local Pyright accepted alternate generated sources")
	tsc, tsProbe := runTypeScriptCandidate(root, runA, accepted, cases.Invalid)
	logs["alternate-typescript-compile"] = joinLog(tsc)
	logs["alternate-typescript-probe"] = joinLog(tsProbe)
	add("alternate:typescript-generated-typecheck", tsc.Err, "locked TypeScript strict compiler accepted alternate sources")
	add("alternate:typescript-generated-probe", tsProbe.Err, "restricted Node passed alternate exact cases and rejections")
	return cases
}

func verifyAlternateSchemaContract(root string) error {
	data, err := readRegular(root, alternateRoot+"/schema.json")
	if err != nil {
		return err
	}
	parsed, err := core.ParseJSON(data)
	if err != nil {
		return fmt.Errorf("strict alternate schema parse: %w", err)
	}
	document, ok := parsed.(map[string]any)
	if !ok {
		return errors.New("alternate schema is not an object")
	}
	properties, ok := document["properties"].(map[string]any)
	if !ok {
		return errors.New("alternate schema has no properties object")
	}
	closedEmpty := false
	for _, raw := range properties {
		property, propertyOK := raw.(map[string]any)
		if !propertyOK || property["type"] != "object" || property["additionalProperties"] != false {
			continue
		}
		declared, declaredOK := property["properties"].(map[string]any)
		if declaredOK && len(declared) == 0 {
			closedEmpty = true
			break
		}
	}
	if !closedEmpty {
		return errors.New("alternate schema lacks a closed object with an empty properties set")
	}

	message, ok := properties["message"].(map[string]any)
	if !ok {
		return errors.New("alternate schema lacks message union")
	}
	branches, ok := message["oneOf"].([]any)
	if !ok || len(branches) < 2 {
		return errors.New("alternate message union lacks at least two oneOf branches")
	}
	definitions, ok := document["$defs"].(map[string]any)
	if !ok {
		return errors.New("alternate schema lacks $defs")
	}
	discriminator := ""
	values := map[string]bool{}
	for index, rawBranch := range branches {
		branch, branchOK := rawBranch.(map[string]any)
		if !branchOK {
			return fmt.Errorf("alternate message oneOf[%d] is not an object", index)
		}
		reference, referenceOK := branch["$ref"].(string)
		const prefix = "#/$defs/"
		if !referenceOK || !strings.HasPrefix(reference, prefix) || strings.Contains(strings.TrimPrefix(reference, prefix), "/") {
			return fmt.Errorf("alternate message oneOf[%d] is not a local $defs reference", index)
		}
		definition, definitionOK := definitions[strings.TrimPrefix(reference, prefix)].(map[string]any)
		if !definitionOK {
			return fmt.Errorf("alternate message oneOf[%d] has unresolved reference %q", index, reference)
		}
		required, requiredOK := definition["required"].([]any)
		branchProperties, propertiesOK := definition["properties"].(map[string]any)
		if !requiredOK || !propertiesOK {
			return fmt.Errorf("alternate message oneOf[%d] lacks required/properties", index)
		}
		candidate := ""
		value := ""
		for _, rawName := range required {
			name, nameOK := rawName.(string)
			property, propertyOK := branchProperties[name].(map[string]any)
			constant, constOK := property["const"].(string)
			if nameOK && propertyOK && constOK {
				candidate, value = name, constant
				break
			}
		}
		if candidate == "" || candidate == "type" {
			return fmt.Errorf("alternate message oneOf[%d] must use a non-type required const discriminator", index)
		}
		if discriminator == "" {
			discriminator = candidate
		} else if discriminator != candidate {
			return fmt.Errorf("alternate message branches use different discriminators %q and %q", discriminator, candidate)
		}
		if values[value] {
			return fmt.Errorf("alternate message discriminator value %q is duplicated", value)
		}
		values[value] = true
	}
	return nil
}

func verifyAlternateOutputIsolation(output, rootName string) error {
	forbidden := []string{
		"CodegenSpikeEnvelope", "CodegenSpike", "codegenspike",
		"optional_nullable", "occurred_at", "safe_integer",
		"RepresentativeExtensionEnvelope", "TextPayload", "JsonPayload",
	}
	rootBearingModels := map[string]bool{
		"go/models.gen.go":         true,
		"python/models_gen.py":     true,
		"typescript/models.gen.ts": true,
	}
	problems := []string{}
	for _, relative := range generatedPaths {
		path := filepath.Join(output, filepath.FromSlash(relative))
		data, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, relative+": "+err.Error())
			continue
		}
		if rootBearingModels[relative] && !bytes.Contains(data, []byte(rootName)) {
			problems = append(problems, relative+" does not bind root "+rootName)
		}
		for _, token := range forbidden {
			if bytes.Contains(data, []byte(token)) {
				problems = append(problems, fmt.Sprintf("%s leaks spike-only token %q", relative, token))
			}
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func requireCaseCoverage(cases casesDocument, required map[string][]string) error {
	classes := map[string][]caseItem{"valid": cases.Valid, "forward": cases.Forward, "invalid": cases.Invalid}
	problems := []string{}
	for class, ids := range required {
		actual := map[string]bool{}
		for _, item := range classes[class] {
			actual[item.ID] = true
		}
		for _, id := range ids {
			if !actual[id] {
				problems = append(problems, class+" missing "+id)
			}
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func loadCases(root string) (casesDocument, error) {
	return loadCasesAt(root, casesPath, fixtureRoot)
}

func loadCasesAt(root, documentPath, fixtureBase string) (casesDocument, error) {
	var document casesDocument
	data, err := readRegular(root, documentPath)
	if err != nil {
		return document, err
	}
	if err := core.DecodeAuthoring(data, &document); err != nil {
		return document, fmt.Errorf("strict decode cases: %w", err)
	}
	if document.SchemaVersion != 1 {
		return document, fmt.Errorf("cases schema_version=%d want 1", document.SchemaVersion)
	}
	seen := map[string]bool{}
	rawInvalid := map[string]bool{
		"alternate-duplicate-key":      true,
		"alternate-unpaired-surrogate": true,
	}
	for class, items := range map[string][]caseItem{"valid": document.Valid, "forward": document.Forward, "invalid": document.Invalid} {
		if len(items) == 0 {
			return document, fmt.Errorf("cases.%s is empty", class)
		}
		for _, item := range items {
			if item.ID == "" || seen[item.ID] {
				return document, fmt.Errorf("empty or duplicate case id %q", item.ID)
			}
			seen[item.ID] = true
			wantPrefix := "fixtures/" + class + "/"
			wantSuffix := ".json"
			if class == "invalid" && rawInvalid[item.ID] {
				wantSuffix = ".json.invalid"
			}
			if !strings.HasPrefix(item.Path, wantPrefix) || !strings.HasSuffix(item.Path, wantSuffix) {
				return document, fmt.Errorf("case %s has noncanonical path %q", item.ID, item.Path)
			}
			if err := requireRegularFilePath(root, fixtureBase+"/"+item.Path); err != nil {
				return document, fmt.Errorf("case %s: %w", item.ID, err)
			}
		}
	}
	return document, nil
}

func runGenerator(root, output, config string) commandResult {
	relativeOutput, err := filepath.Rel(root, output)
	if err != nil {
		return commandResult{Err: err}
	}
	relativeOutput = filepath.ToSlash(relativeOutput)
	result := relativeOutput + "/" + provenanceName
	argv := []string{
		"node", "--permission", "--allow-fs-read=.", "--allow-fs-write=" + buildRoot,
		"--disable-proto=throw", "--no-addons", "scripts/generate.mjs",
		"--config", config, "--output", relativeOutput, "--result", result,
	}
	return run(root, 90*time.Second, cleanEnvironment(os.Environ(), map[string]string{
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0",
	}), argv...)
}

func generatedManifest(output string) (fileManifest, error) {
	if err := requireExactDirectories(output, []string{"go", "python", "typescript"}); err != nil {
		return fileManifest{}, err
	}
	entries := make([]fileEntry, 0, len(generatedPaths))
	actual, err := inventoryDirectory(output, func(path string) bool { return path != provenanceName })
	if err != nil {
		return fileManifest{}, err
	}
	want := append([]string(nil), generatedPaths...)
	sort.Strings(want)
	actualPaths := make([]string, len(actual))
	for index, entry := range actual {
		actualPaths[index] = entry.Path
	}
	if !reflect.DeepEqual(actualPaths, want) {
		return fileManifest{}, fmt.Errorf("generated paths=%v want exact=%v", actualPaths, want)
	}
	entries = append(entries, actual...)
	return makeManifest(entries), nil
}

func goldenManifest(root string) (fileManifest, error) {
	absolute := filepath.Join(root, filepath.FromSlash(expectedRoot))
	if err := requireExactDirectories(absolute, []string{"go", "python", "typescript"}); err != nil {
		return fileManifest{}, err
	}
	entries, err := inventoryDirectory(absolute, func(path string) bool { return true })
	if err != nil {
		return fileManifest{}, err
	}
	for index := range entries {
		if !strings.HasSuffix(entries[index].Path, ".golden") {
			return fileManifest{}, fmt.Errorf("unexpected non-golden file under expected/: %s", entries[index].Path)
		}
		entries[index].Path = strings.TrimSuffix(entries[index].Path, ".golden")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	want := append([]string(nil), generatedPaths...)
	sort.Strings(want)
	actual := make([]string, len(entries))
	for index, entry := range entries {
		actual[index] = entry.Path
	}
	if !reflect.DeepEqual(actual, want) {
		return fileManifest{}, fmt.Errorf("golden paths=%v want exact=%v", actual, want)
	}
	return makeManifest(entries), nil
}

func loadProvenance(output string) (provenanceDocument, []byte, error) {
	var document provenanceDocument
	path := filepath.Join(output, provenanceName)
	data, err := readRegularAbsolute(path)
	if err != nil {
		return document, nil, err
	}
	if err := core.DecodeAuthoring(data, &document); err != nil {
		return document, data, err
	}
	return document, data, nil
}

func verifyProvenance(root string, document provenanceDocument, outputs fileManifest, cases casesDocument) error {
	return verifyProvenanceAt(root, document, outputs, cases, pipelinePath, casesPath, fixtureRoot)
}

func verifyProvenanceAt(root string, document provenanceDocument, outputs fileManifest, cases casesDocument, configPath, caseDocumentPath, fixtureBase string) error {
	problems := []string{}
	pipeline, pipelineErr := loadPipelineAt(root, configPath, caseDocumentPath, fixtureBase)
	if pipelineErr != nil {
		return pipelineErr
	}
	if document.SchemaVersion != 1 || document.PipelineID != "arop-codegen-v1" || document.MappingProfile != "arop-wire-model-v1" {
		problems = append(problems, "unexpected schema_version/pipeline_id/mapping_profile")
	}
	wantCommand := provenanceCommand{Entry: "scripts/generate.mjs", Config: configPath, Output: "<output>", Result: "<output>/" + provenanceName}
	if document.Command != wantCommand {
		problems = append(problems, fmt.Sprintf("command=%+v want=%+v", document.Command, wantCommand))
	}
	verifyFile := func(label string, item provenanceFile, wantPath string) {
		data, err := readRegular(root, wantPath)
		if err != nil {
			problems = append(problems, label+": "+err.Error())
			return
		}
		modeErr := requireMode0644(root, wantPath)
		if item.Path != wantPath || item.SHA256 != digest(data) || item.Bytes != int64(len(data)) || item.Mode != "100644" || modeErr != nil {
			problems = append(problems, fmt.Sprintf("%s=%+v want path=%s sha256=%s bytes=%d mode=100644", label, item, wantPath, digest(data), len(data)))
		}
	}
	verifyFile("configuration", document.Configuration, configPath)
	wantGeneratorPaths := []string{"package-lock.json", "package.json", "scripts/generate.mjs", "scripts/lib/repository.mjs"}
	if len(document.Generator.Files) != len(wantGeneratorPaths) {
		problems = append(problems, fmt.Sprintf("generator.files=%d want=%d", len(document.Generator.Files), len(wantGeneratorPaths)))
	} else {
		generatorFiles := append([]provenanceFile(nil), document.Generator.Files...)
		sort.Slice(generatorFiles, func(i, j int) bool { return generatorFiles[i].Path < generatorFiles[j].Path })
		for index, wantPath := range wantGeneratorPaths {
			verifyFile(fmt.Sprintf("generator.files[%d]", index), generatorFiles[index], wantPath)
		}
	}
	generatorData, generatorErr := readRegular(root, "scripts/generate.mjs")
	if generatorErr != nil || document.Generator.Path != "scripts/generate.mjs" || document.Generator.SHA256 != digest(generatorData) {
		problems = append(problems, fmt.Sprintf("generator identity=%+v err=%v", document.Generator, generatorErr))
	}

	resourcePaths := make([]string, len(document.Resources))
	if len(document.Resources) != len(pipeline.Resources) {
		problems = append(problems, fmt.Sprintf("resources=%d want config exact=%d", len(document.Resources), len(pipeline.Resources)))
	}
	for index, item := range document.Resources {
		resourcePaths[index] = item.Path
		data, err := readRegular(root, item.Path)
		modeErr := requireMode0644(root, item.Path)
		if err != nil || modeErr != nil || item.Path == "" || item.URI == "" || item.SHA256 != digest(data) || item.Bytes != int64(len(data)) || item.Mode != "100644" {
			problems = append(problems, fmt.Sprintf("resource[%d] not independently bound: path=%q uri=%q err=%v", index, item.Path, item.URI, errors.Join(err, modeErr)))
		}
		if index >= len(pipeline.Resources) || item.Path != pipeline.Resources[index].Path || item.URI != pipeline.Resources[index].URI {
			problems = append(problems, fmt.Sprintf("resource[%d]=%s/%s not exact config entry", index, item.Path, item.URI))
		}
	}
	if !sort.StringsAreSorted(resourcePaths) || hasDuplicates(resourcePaths) {
		problems = append(problems, "resources are not unique and sorted by path")
	}

	wantFixtures := append(append(append([]caseItem(nil), cases.Valid...), cases.Forward...), cases.Invalid...)
	if len(document.Fixtures) != len(wantFixtures) {
		problems = append(problems, fmt.Sprintf("fixtures=%d want=%d", len(document.Fixtures), len(wantFixtures)))
	} else {
		for index, want := range wantFixtures {
			item := document.Fixtures[index]
			class := "valid"
			if index >= len(cases.Valid)+len(cases.Forward) {
				class = "invalid"
			} else if index >= len(cases.Valid) {
				class = "forward"
			}
			path := fixtureBase + "/" + want.Path
			data, err := readRegular(root, path)
			modeErr := requireMode0644(root, path)
			if err != nil || modeErr != nil || item.ID != want.ID || item.Class != class || item.Path != path || item.SHA256 != digest(data) || item.Bytes != int64(len(data)) || item.Mode != "100644" {
				problems = append(problems, fmt.Sprintf("fixture[%d] not independently bound: %+v err=%v", index, item, errors.Join(err, modeErr)))
			}
		}
	}

	if len(document.Outputs) != len(outputs.Entries) {
		problems = append(problems, fmt.Sprintf("outputs=%d want=%d", len(document.Outputs), len(outputs.Entries)))
	} else {
		for index, want := range outputs.Entries {
			item := document.Outputs[index]
			language := strings.SplitN(want.Path, "/", 2)[0]
			if item.Language != language || item.Path != want.Path || item.SHA256 != want.SHA256 || item.Bytes != want.Bytes || item.Mode != want.Mode {
				problems = append(problems, fmt.Sprintf("output[%d]=%+v want=%+v language=%s", index, item, want, language))
			}
		}
	}
	wantOutputDigest := provenanceOutputsDigest(document.Outputs)
	if document.OutputsSHA256 != wantOutputDigest {
		problems = append(problems, fmt.Sprintf("outputs_sha256=%s want independently recomputed=%s", document.OutputsSHA256, wantOutputDigest))
	}
	wantInputDigest := provenanceInputsDigest(document)
	if document.InputsSHA256 != wantInputDigest {
		problems = append(problems, fmt.Sprintf("inputs_sha256=%s want independently recomputed=%s", document.InputsSHA256, wantInputDigest))
	}
	wantChecks := make([]string, 0, len(cases.Valid)+len(cases.Forward)+len(cases.Invalid)+3)
	for _, fixtureClass := range []struct {
		name  string
		items []caseItem
	}{
		{name: "valid", items: cases.Valid},
		{name: "forward", items: cases.Forward},
		{name: "invalid", items: cases.Invalid},
	} {
		for _, item := range fixtureClass.items {
			wantChecks = append(wantChecks, "schema-"+fixtureClass.name+"-"+item.ID)
		}
	}
	wantChecks = append(wantChecks, "offline-resource-closure", "mapping-profile", "deterministic-output-set")
	if len(document.Checks) != len(wantChecks) {
		problems = append(problems, fmt.Sprintf("provenance checks=%d want exact=%d", len(document.Checks), len(wantChecks)))
	}
	seenChecks := map[string]bool{}
	for _, check := range document.Checks {
		if check.ID == "" || seenChecks[check.ID] || !check.Passed {
			problems = append(problems, fmt.Sprintf("invalid/duplicate/failed provenance check %+v", check))
		}
		seenChecks[check.ID] = true
	}
	for _, id := range wantChecks {
		if !seenChecks[id] {
			problems = append(problems, "missing exact provenance check "+id)
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func loadPipeline(root string) (pipelineDocument, error) {
	return loadPipelineAt(root, pipelinePath, casesPath, fixtureRoot)
}

func loadPipelineAt(root, documentPath, caseDocumentPath, fixtureBase string) (pipelineDocument, error) {
	var document pipelineDocument
	data, err := readRegular(root, documentPath)
	if err != nil {
		return document, err
	}
	if err := core.DecodeAuthoring(data, &document); err != nil {
		return document, fmt.Errorf("strict decode pipeline: %w", err)
	}
	if document.SchemaVersion != 1 || document.PipelineID != "arop-codegen-v1" || document.MappingProfile != "arop-wire-model-v1" {
		return document, errors.New("pipeline identity mismatch")
	}
	if len(document.Resources) == 0 || len(document.Roots) != 1 || len(document.Outputs) != 3 {
		return document, fmt.Errorf("pipeline closure resources=%d roots=%d outputs=%d", len(document.Resources), len(document.Roots), len(document.Outputs))
	}
	wantOutputs := map[string]outputPair{
		"go":         {Model: "go/models.gen.go", Probe: "go/models_gen_test.go"},
		"python":     {Model: "python/models_gen.py", Probe: "python/probe.py"},
		"typescript": {Model: "typescript/models.gen.ts", Probe: "typescript/probe.ts"},
	}
	if !reflect.DeepEqual(document.Outputs, wantOutputs) {
		return document, fmt.Errorf("pipeline outputs=%v want exact=%v", document.Outputs, wantOutputs)
	}
	cases, err := loadCasesAt(root, caseDocumentPath, fixtureBase)
	if err != nil {
		return document, err
	}
	if !reflect.DeepEqual(document.Fixtures.Valid, absoluteCasePaths(fixtureBase, cases.Valid)) || !reflect.DeepEqual(document.Fixtures.Forward, absoluteCasePaths(fixtureBase, cases.Forward)) || !reflect.DeepEqual(document.Fixtures.Invalid, absoluteCasePaths(fixtureBase, cases.Invalid)) {
		return document, errors.New("pipeline fixture lists do not exactly match cases.json")
	}
	return document, nil
}

func absoluteCasePaths(fixtureBase string, items []caseItem) []caseItem {
	result := append([]caseItem(nil), items...)
	for index := range result {
		result[index].Path = fixtureBase + "/" + result[index].Path
	}
	return result
}

func runGoCandidate(root, output string, accepted, rejected []caseItem) commandResult {
	directory := filepath.Join(output, "go")
	scratch, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "go-toolchain-")
	if err != nil {
		return commandResult{Err: err}
	}
	defer os.RemoveAll(scratch)
	for _, directory := range []string{"cache", "modcache", "tmp"} {
		if err := os.Mkdir(filepath.Join(scratch, directory), 0o755); err != nil {
			return commandResult{Err: err}
		}
	}
	result := run(directory, 90*time.Second, cleanEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0", "GOCACHE": filepath.Join(scratch, "cache"), "GOMODCACHE": filepath.Join(scratch, "modcache"),
		"GOTMPDIR": filepath.Join(scratch, "tmp"), "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOPROXY": "off",
		"GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOWORK": "off", "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	}), "go", "test", "-count=1", "-run=.", "-json", ".")
	if result.Err != nil {
		return result
	}
	if err := verifyGoTestEvents(result.Stdout, accepted, rejected); err != nil {
		result.Err = err
	}
	return result
}

func verifyGoTestEvents(data []byte, accepted, rejected []caseItem) error {
	type event struct {
		Action  string `json:"Action"`
		Test    string `json:"Test"`
		Package string `json:"Package"`
	}
	acceptedIDs := map[string]bool{}
	for _, item := range accepted {
		acceptedIDs[item.ID] = true
	}
	rejectedIDs := map[string]bool{}
	for _, item := range rejected {
		rejectedIDs[item.ID] = true
	}
	terminal := map[string]string{}
	packagePass := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var item event
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return fmt.Errorf("parse go test event: %w", err)
		}
		if item.Test != "" && (item.Action == "pass" || item.Action == "fail" || item.Action == "skip") {
			if prior := terminal[item.Test]; prior != "" {
				return fmt.Errorf("duplicate terminal event for %s: %s and %s", item.Test, prior, item.Action)
			}
			terminal[item.Test] = item.Action
		}
		if item.Test == "" && item.Action == "pass" {
			packagePass = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	acceptedRoot, rejectedRoot := "", ""
	for name, action := range terminal {
		rootName, caseID, hasCase := strings.Cut(name, "/")
		if !hasCase {
			continue
		}
		switch {
		case acceptedIDs[caseID]:
			if acceptedRoot != "" && acceptedRoot != rootName {
				return fmt.Errorf("accepted Go cases used multiple roots %q and %q", acceptedRoot, rootName)
			}
			acceptedRoot = rootName
		case rejectedIDs[caseID]:
			if rejectedRoot != "" && rejectedRoot != rootName {
				return fmt.Errorf("rejected Go cases used multiple roots %q and %q", rejectedRoot, rootName)
			}
			rejectedRoot = rootName
		default:
			return fmt.Errorf("unexpected Go subtest terminal %s=%s", name, action)
		}
	}
	if acceptedRoot == "" || rejectedRoot == "" || acceptedRoot == rejectedRoot {
		return fmt.Errorf("Go probe roots accepted=%q rejected=%q must be distinct and non-empty", acceptedRoot, rejectedRoot)
	}
	want := map[string]bool{acceptedRoot: true, rejectedRoot: true}
	for id := range acceptedIDs {
		want[acceptedRoot+"/"+id] = true
	}
	for id := range rejectedIDs {
		want[rejectedRoot+"/"+id] = true
	}
	for name := range want {
		if terminal[name] != "pass" {
			return fmt.Errorf("Go case %s terminal=%q want pass", name, terminal[name])
		}
	}
	for name, action := range terminal {
		if !want[name] {
			return fmt.Errorf("unexpected Go test terminal %s=%s", name, action)
		}
		if action != "pass" {
			return fmt.Errorf("Go test %s terminal=%s", name, action)
		}
	}
	if !packagePass {
		return errors.New("Go package had no pass terminal")
	}
	return nil
}

func runPythonCandidate(root, output string, accepted, rejected []caseItem) (commandResult, commandResult) {
	directory := filepath.Join(output, "python")
	models := filepath.Join(directory, "models_gen.py")
	probe := filepath.Join(directory, "probe.py")
	environment := cleanEnvironment(os.Environ(), map[string]string{
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONHASHSEED": "0", "PYTHONNOUSERSITE": "1",
		"PYTHONDONTWRITEBYTECODE": "1", "PYTHONSAFEPATH": "1",
	})
	compile := run(root, 60*time.Second, environment, "python3", "-I", "-S", "-B", "-c", "import pathlib; compile(pathlib.Path(__import__('sys').argv[1]).read_bytes(), __import__('sys').argv[1], 'exec'); compile(pathlib.Path(__import__('sys').argv[2]).read_bytes(), __import__('sys').argv[2], 'exec')", models, probe)
	if compile.Err != nil {
		return compile, commandResult{Err: errors.New("Python probe skipped because compile failed")}
	}
	probeResult := run(root, 60*time.Second, environment, "python3", "-I", "-S", "-B", "-c", "import runpy,sys; sys.path.insert(0,sys.argv[1]); runpy.run_path(sys.argv[2],run_name='__main__')", directory, probe)
	if probeResult.Err == nil {
		probeResult.Err = verifyProbeOutput(probeResult.Stdout, "python", accepted, rejected)
	}
	return compile, probeResult
}

func runPyrightIfPresent(root, output string) commandResult {
	entry := filepath.Join(root, "node_modules", "pyright", "index.js")
	if err := requireRegularAbsolute(entry); err != nil {
		return commandResult{Argv: []string{"node", "node_modules/pyright/index.js"}, Err: fmt.Errorf("locked Pyright is required: %w", err)}
	}
	models, _ := filepath.Rel(root, filepath.Join(output, "python", "models_gen.py"))
	probe, _ := filepath.Rel(root, filepath.Join(output, "python", "probe.py"))
	// Pyright currently uses Node internals that are rejected by Node's
	// experimental permission model. Keep this invocation pinned to the exact
	// local lockfile installation and retain the other process hardening flags.
	return run(root, 90*time.Second, cleanEnvironment(os.Environ(), map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC"}),
		"node", "--disable-proto=throw", "--no-addons", "node_modules/pyright/index.js",
		"--level", "error", "--pythonversion", "3.11", filepath.ToSlash(models), filepath.ToSlash(probe))
}

func runTypeScriptCandidate(root, output string, accepted, rejected []caseItem) (commandResult, commandResult) {
	tscPath := "node_modules/typescript/lib/tsc.js"
	if err := requireRegularFilePath(root, tscPath); err != nil {
		return commandResult{Err: fmt.Errorf("locked TypeScript compiler is required: %w", err)}, commandResult{Err: errors.New("TypeScript probe skipped because compiler is unavailable")}
	}
	jsDir, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "typescript-emit-")
	if err != nil {
		return commandResult{Err: err}, commandResult{Err: err}
	}
	defer os.RemoveAll(jsDir)
	relativeOutput, _ := filepath.Rel(root, filepath.Join(output, "typescript"))
	relativeJS, _ := filepath.Rel(root, jsDir)
	environment := cleanEnvironment(os.Environ(), map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "SOURCE_DATE_EPOCH": "0"})
	compile := run(root, 90*time.Second, environment,
		"node", "--permission", "--allow-fs-read=.", "--allow-fs-write="+buildRoot, "--disable-proto=throw", "--no-addons", tscPath,
		"--strict", "--exactOptionalPropertyTypes", "--noUncheckedIndexedAccess", "--useUnknownInCatchVariables", "--skipLibCheck", "false",
		"--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--outDir", filepath.ToSlash(relativeJS),
		filepath.ToSlash(filepath.Join(relativeOutput, "models.gen.ts")), filepath.ToSlash(filepath.Join(relativeOutput, "probe.ts")))
	if compile.Err != nil {
		return compile, commandResult{Err: errors.New("TypeScript probe skipped because compilation failed")}
	}
	probePath := filepath.Join(jsDir, "probe.js")
	probe := run(root, 60*time.Second, environment, "node", "--permission", "--allow-fs-read="+filepath.ToSlash(relativeJS), "--disable-proto=throw", "--no-addons", probePath)
	if probe.Err == nil {
		probe.Err = verifyProbeOutput(probe.Stdout, "typescript", accepted, rejected)
	}
	return compile, probe
}

func verifyProbeOutput(data []byte, language string, accepted, rejected []caseItem) error {
	var result probeResult
	if err := core.DecodeAuthoring(bytes.TrimSpace(data), &result); err != nil {
		return fmt.Errorf("strict decode %s probe output: %w", language, err)
	}
	if result.SchemaVersion != 1 || result.Language != language {
		return fmt.Errorf("%s probe identity schema_version=%d language=%q", language, result.SchemaVersion, result.Language)
	}
	if len(result.Cases) != len(accepted) {
		return fmt.Errorf("%s probe cases=%d want=%d", language, len(result.Cases), len(accepted))
	}
	for index, want := range accepted {
		actual := result.Cases[index]
		if actual.ID != want.ID || !actual.Passed {
			return fmt.Errorf("%s probe case[%d]=%+v want id=%s passed", language, index, actual, want.ID)
		}
	}
	if len(result.Rejections) != len(rejected) {
		return fmt.Errorf("%s probe rejections=%d want=%d", language, len(result.Rejections), len(rejected))
	}
	for index, want := range rejected {
		actual := result.Rejections[index]
		if actual.ID != want.ID || !actual.Passed {
			return fmt.Errorf("%s probe rejection[%d]=%+v want id=%s passed", language, index, actual, want.ID)
		}
	}
	return nil
}

func runNegativeConfigs(root string, cases casesDocument) ([]byte, []report.Check) {
	directory := filepath.Join(root, filepath.FromSlash(fixtureRoot+"/negative-configs"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return []byte(err.Error()), []report.Check{{Name: "negative-configs:inventory", Passed: false, Detail: err.Error()}}
	}
	expectations := []struct {
		name   string
		reason string
	}{
		{name: "partial-output", reason: "outputs is missing \"typescript\""},
		{name: "path-escape", reason: "outputs.go.model escapes its root"},
		{name: "provenance-tamper", reason: "provenance/result path is reserved"},
		{name: "remote-ref", reason: "can't resolve reference"},
		{name: "unknown-keyword", reason: "unknown schema keyword"},
		{name: "unsafe-integer", reason: "unsafe JSON number at /schema_version"},
	}
	wantNames := make([]string, len(expectations))
	for index, expectation := range expectations {
		wantNames[index] = expectation.name
	}
	files := []string{}
	var inventoryProblems []string
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			inventoryProblems = append(inventoryProblems, entry.Name())
			continue
		}
		if err := requireRegularAbsolute(filepath.Join(directory, entry.Name())); err != nil {
			inventoryProblems = append(inventoryProblems, entry.Name()+": "+err.Error())
			continue
		}
		files = append(files, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(files)
	sort.Strings(inventoryProblems)
	inventoryPassed := len(inventoryProblems) == 0 && reflect.DeepEqual(files, wantNames)
	checks := []report.Check{{Name: "negative-configs:inventory", Passed: inventoryPassed, Detail: fmt.Sprintf("actual=%v want=%v rejected_entries=%v", files, wantNames, inventoryProblems)}}
	var evidence bytes.Buffer
	for _, expectation := range expectations {
		name := expectation.name
		output, mkdirErr := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "negative-"+name+"-")
		if mkdirErr != nil {
			checks = append(checks, report.Check{Name: "negative-config:" + name, Passed: false, Detail: mkdirErr.Error()})
			continue
		}
		config := fixtureRoot + "/negative-configs/" + name + ".json"
		result := runGenerator(root, output, config)
		inventory, inventoryErr := inventoryDirectory(output, func(path string) bool { return true })
		directoryEntries, directoryErr := os.ReadDir(output)
		reasonMatched := result.Err != nil && strings.Contains(result.Err.Error(), expectation.reason)
		passed := reasonMatched && inventoryErr == nil && directoryErr == nil && len(inventory) == 0 && len(directoryEntries) == 0
		detail := "generator failed closed at exact target reason without partial output: " + expectation.reason
		if !passed {
			detail = fmt.Sprintf("reason=%q matched=%t err=%v inventoryErr=%v directoryErr=%v rootEntries=%d output=%v", expectation.reason, reasonMatched, result.Err, inventoryErr, directoryErr, len(directoryEntries), inventory)
		}
		checks = append(checks, report.Check{Name: "negative-config:" + name, Passed: passed, Detail: detail})
		fmt.Fprintf(&evidence, "%s\x00%t\x00%d\x00%d\n", name, passed, len(result.Stdout), len(result.Stderr))
		_ = os.RemoveAll(output)
	}
	keywordEvidence, keywordChecks := runUnsupportedKeywordMatrix(root, cases)
	evidence.Write(keywordEvidence)
	checks = append(checks, keywordChecks...)
	collisionCheck, collisionEvidence := runIdentifierCollisionNegative(root, cases)
	evidence.Write(collisionEvidence)
	checks = append(checks, collisionCheck)
	return evidence.Bytes(), checks
}

func runUnsupportedKeywordMatrix(root string, cases casesDocument) ([]byte, []report.Check) {
	type keywordCase struct {
		name     string
		property map[string]any
		reason   string
	}
	matrix := []keywordCase{
		{name: "minLength", property: map[string]any{"type": "string", "minLength": 1}},
		{name: "maxLength", property: map[string]any{"type": "string", "maxLength": 4}},
		{name: "pattern", property: map[string]any{"type": "string", "pattern": "^[a-z]+$"}},
		{name: "minItems", property: map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}},
		{name: "maxItems", property: map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 2}},
		{name: "uniqueItems", property: map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "uniqueItems": true}},
		{name: "minProperties", property: map[string]any{"type": "object", "additionalProperties": true, "minProperties": 1}},
		{name: "maxProperties", property: map[string]any{"type": "object", "additionalProperties": true, "maxProperties": 2}},
		{name: "allOf", property: map[string]any{"allOf": []any{map[string]any{"type": "string"}}}},
		{name: "not", property: map[string]any{"not": map[string]any{"type": "null"}}},
		{name: "number-minimum", property: map[string]any{"type": "number", "minimum": 0}, reason: "number bounds are unsupported"},
		{name: "number-maximum", property: map[string]any{"type": "number", "maximum": 1}, reason: "number bounds are unsupported"},
	}
	checks := make([]report.Check, 0, len(matrix))
	var evidence bytes.Buffer
	for _, item := range matrix {
		uri := "https://arop.invalid/conformance/negative/unsupported-" + item.name + ".schema.json"
		schema := map[string]any{
			"$schema":              "https://json-schema.org/draft/2020-12/schema",
			"$id":                  uri,
			"type":                 "object",
			"properties":           map[string]any{"value": item.property},
			"additionalProperties": false,
		}
		reason := item.reason
		if reason == "" {
			reason = "unknown schema keyword " + item.name
		}
		check, itemEvidence := runSyntheticGeneratorNegative(root, cases, "unsupported-keyword:"+item.name, schema, "UnsupportedKeywordEnvelope", reason)
		checks = append(checks, check)
		evidence.Write(itemEvidence)
	}
	return evidence.Bytes(), checks
}

func runIdentifierCollisionNegative(root string, cases casesDocument) (report.Check, []byte) {
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://arop.invalid/conformance/negative/identifier-collision.schema.json",
		"type":    "object",
		"required": []any{
			"foo_id", "fooID",
		},
		"properties": map[string]any{
			"foo_id": map[string]any{"type": "string"},
			"fooID":  map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
	return runSyntheticGeneratorNegative(root, cases, "identifier-collision", schema, "IdentifierCollisionEnvelope", "generated identifier collision")
}

func runSyntheticGeneratorNegative(root string, cases casesDocument, checkName string, schema map[string]any, rootName, wantReason string) (report.Check, []byte) {
	failure := func(err error) (report.Check, []byte) {
		return report.Check{Name: checkName, Passed: false, Detail: err.Error()}, nil
	}
	if len(cases.Valid) == 0 || len(cases.Forward) == 0 || len(cases.Invalid) == 0 {
		return failure(errors.New("synthetic generator negative requires non-empty valid, forward and invalid fixtures"))
	}
	workspace, err := os.MkdirTemp(filepath.Join(root, filepath.FromSlash(buildRoot)), "synthetic-negative-")
	if err != nil {
		return failure(err)
	}
	defer os.RemoveAll(workspace)
	relativeWorkspace, err := filepath.Rel(root, workspace)
	if err != nil {
		return failure(err)
	}
	relativeWorkspace = filepath.ToSlash(relativeWorkspace)
	uri, ok := schema["$id"].(string)
	if !ok || uri == "" {
		return failure(errors.New("synthetic negative schema has no string $id"))
	}
	schemaPath := relativeWorkspace + "/schema.json"
	configPath := relativeWorkspace + "/pipeline.json"
	config := pipelineDocument{
		SchemaVersion: 1, PipelineID: "arop-codegen-v1", MappingProfile: "arop-wire-model-v1",
		Resources: []pipelineResource{{Path: schemaPath, URI: uri}},
		Roots:     []pipelineRoot{{Ref: uri, Name: rootName}},
		Fixtures: pipelineFixtures{
			Valid:   absoluteCasePaths(fixtureRoot, cases.Valid[:1]),
			Forward: absoluteCasePaths(fixtureRoot, cases.Forward[:1]),
			Invalid: absoluteCasePaths(fixtureRoot, cases.Invalid[:1]),
		},
		Outputs: map[string]outputPair{
			"go":         {Model: "go/models.gen.go", Probe: "go/models_gen_test.go"},
			"python":     {Model: "python/models_gen.py", Probe: "python/probe.py"},
			"typescript": {Model: "typescript/models.gen.ts", Probe: "typescript/probe.ts"},
		},
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return failure(err)
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return failure(err)
	}
	schemaBytes = append(schemaBytes, '\n')
	configBytes = append(configBytes, '\n')
	if err := writeExclusiveRegular(filepath.Join(workspace, "schema.json"), schemaBytes); err != nil {
		return failure(err)
	}
	if err := writeExclusiveRegular(filepath.Join(workspace, "pipeline.json"), configBytes); err != nil {
		return failure(err)
	}
	output := filepath.Join(workspace, "output")
	if err := os.Mkdir(output, 0o755); err != nil {
		return failure(err)
	}
	result := runGenerator(root, output, configPath)
	inventory, inventoryErr := inventoryDirectory(output, func(string) bool { return true })
	rootEntries, readErr := os.ReadDir(output)
	passed := result.Err != nil && strings.Contains(result.Err.Error(), wantReason) && inventoryErr == nil && readErr == nil && len(inventory) == 0 && len(rootEntries) == 0
	detail := "generator failed closed without partial output at " + wantReason
	if !passed {
		detail = fmt.Sprintf("want=%q err=%v inventoryErr=%v readErr=%v rootEntries=%d output=%v", wantReason, result.Err, inventoryErr, readErr, len(rootEntries), inventory)
	}
	evidence := []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%d\x00%d\n", checkName, digest(schemaBytes), digest(configBytes), passed, len(result.Stdout), len(result.Stderr)))
	return report.Check{Name: checkName, Passed: passed, Detail: detail}, evidence
}

func writeExclusiveRegular(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func staticInputManifest(root string) ([]string, fileManifest, error) {
	paths := []string{
		"Makefile", "go.mod", "go.sum", "package.json", "package-lock.json", "scripts/generate.mjs", "scripts/lib/repository.mjs",
		"spec/schemas/check-report.schema.json", "spec/artifact-manifest.yaml", "docs/DECISIONS.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "docs/DEVELOPMENT_PLAN.md",
	}
	for _, directory := range []string{
		fixtureRoot, "schemas/common", "schemas/manifest", "schemas/resources", "internal/tooling/report", "internal/tooling/schema", "internal/tooling/structuredfile", "internal/tooling/controlledinput", "sdk/go/protocol/core",
	} {
		directoryPaths, err := regularFiles(root, directory)
		if err != nil {
			return nil, fileManifest{}, err
		}
		paths = append(paths, directoryPaths...)
	}
	unique := map[string]bool{}
	for _, path := range paths {
		unique[path] = true
	}
	paths = paths[:0]
	for path := range unique {
		if err := requireRegularFilePath(root, path); err != nil {
			return nil, fileManifest{}, fmt.Errorf("static input %s: %w", path, err)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]fileEntry, 0, len(paths))
	for _, path := range paths {
		data, err := readRegular(root, path)
		if err != nil {
			return nil, fileManifest{}, err
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, fileManifest{}, err
		}
		entries = append(entries, fileEntry{Path: path, Mode: modeString(info.Mode()), SHA256: digest(data), Bytes: int64(len(data))})
	}
	return paths, makeManifest(entries), nil
}

func regularFiles(root, relativeDirectory string) ([]string, error) {
	if err := requireRealDirectoryPath(root, relativeDirectory); err != nil {
		return nil, err
	}
	paths := []string{}
	err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relativeDirectory)), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("static closure contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("static closure contains non-regular file %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	return paths, err
}

func inventoryDirectory(root string, include func(string) bool) ([]fileEntry, error) {
	if err := requireRealDirectoryAbsolute(root); err != nil {
		return nil, err
	}
	entries := []fileEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("inventory contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("inventory contains non-regular file %s", path)
		}
		if info.Mode().Perm() != 0o644 {
			return fmt.Errorf("inventory file %s mode=%#o want 0644", path, info.Mode().Perm())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !include(relative) {
			return nil
		}
		data, err := readRegularAbsolute(path)
		if err != nil {
			return err
		}
		after, err := os.Lstat(path)
		if err != nil || !sameFileSnapshot(info, after) {
			return fmt.Errorf("inventory file changed while reading %s", path)
		}
		entries = append(entries, fileEntry{Path: relative, Mode: modeString(info.Mode()), SHA256: digest(data), Bytes: int64(len(data))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func requireExactDirectories(root string, want []string) error {
	if err := requireRealDirectoryAbsolute(root); err != nil {
		return err
	}
	directories := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory inventory contains symlink %s", path)
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		directories = append(directories, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(directories)
	expected := append([]string(nil), want...)
	sort.Strings(expected)
	if !reflect.DeepEqual(directories, expected) {
		return fmt.Errorf("directory inventory=%v want exact=%v", directories, expected)
	}
	return nil
}

func makeManifest(entries []fileEntry) fileManifest {
	entries = append([]fileEntry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var aggregate bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&aggregate, "%s\x00%s\x00%s\x00%d\n", entry.Path, entry.Mode, entry.SHA256, entry.Bytes)
	}
	return fileManifest{Entries: entries, SHA256: digest(aggregate.Bytes())}
}

func provenanceInputsDigest(document provenanceDocument) string {
	type input struct {
		kind, path, uri, sha256, mode string
		bytes                         int64
	}
	inputs := []input{{kind: "configuration", path: document.Configuration.Path, sha256: document.Configuration.SHA256, bytes: document.Configuration.Bytes, mode: document.Configuration.Mode}}
	for _, item := range document.Generator.Files {
		inputs = append(inputs, input{kind: "generator", path: item.Path, sha256: item.SHA256, bytes: item.Bytes, mode: item.Mode})
	}
	for _, item := range document.Resources {
		inputs = append(inputs, input{kind: "resource", path: item.Path, uri: item.URI, sha256: item.SHA256, bytes: item.Bytes, mode: item.Mode})
	}
	for _, item := range document.Fixtures {
		inputs = append(inputs, input{kind: "fixture", path: item.Path, sha256: item.SHA256, bytes: item.Bytes, mode: item.Mode})
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].path < inputs[j].path })
	var aggregate bytes.Buffer
	for _, item := range inputs {
		fmt.Fprintf(&aggregate, "%s\x00%s\x00%s\x00%s\x00%d\x00%s\n", item.kind, item.path, item.uri, item.sha256, item.bytes, item.mode)
	}
	return digest(aggregate.Bytes())
}

func provenanceOutputsDigest(outputs []provenanceOutput) string {
	ordered := append([]provenanceOutput(nil), outputs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var aggregate bytes.Buffer
	for _, item := range ordered {
		fmt.Fprintf(&aggregate, "%s\x00%s\x00%s\x00%d\x00%s\n", item.Path, item.Language, item.SHA256, item.Bytes, item.Mode)
	}
	return digest(aggregate.Bytes())
}

func diffManifests(leftName string, left fileManifest, rightName string, right fileManifest) error {
	return fmt.Errorf("%s sha256=%s entries=%v; %s sha256=%s entries=%v", leftName, left.SHA256, left.Entries, rightName, right.SHA256, right.Entries)
}

func readRegular(root, relative string) ([]byte, error) {
	if err := requireRegularFilePath(root, relative); err != nil {
		return nil, err
	}
	rooted, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	name := filepath.FromSlash(relative)
	pathBefore, err := rooted.Lstat(name)
	if err != nil {
		return nil, err
	}
	if pathBefore.Mode()&os.ModeSymlink != 0 || !pathBefore.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular non-symlink file: %s", relative)
	}
	file, err := rooted.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil || !sameFileSnapshot(pathBefore, openedBefore) {
		return nil, fmt.Errorf("file changed while opening %s", relative)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	openedAfter, openedErr := file.Stat()
	pathAfter, pathErr := rooted.Lstat(name)
	if openedErr != nil || pathErr != nil || !sameFileSnapshot(openedBefore, openedAfter) || !sameFileSnapshot(openedAfter, pathAfter) {
		return nil, fmt.Errorf("file changed while reading %s", relative)
	}
	return data, nil
}

func sameFileSnapshot(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func requireRegularFilePath(root, relative string) error {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if relative == "" || clean != relative || filepath.IsAbs(filepath.FromSlash(relative)) || relative == ".." || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("unsafe relative path %q", relative)
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
	if err := requireRealDirectoryPath(root, directory); err != nil {
		return err
	}
	return requireRegularAbsolute(filepath.Join(root, filepath.FromSlash(relative)))
}

func requireRegularAbsolute(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular non-symlink file: %s", path)
	}
	return nil
}

func readRegularAbsolute(path string) ([]byte, error) {
	if err := requireRealDirectoryAbsolute(filepath.Dir(path)); err != nil {
		return nil, err
	}
	pathBefore, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathBefore.Mode()&os.ModeSymlink != 0 || !pathBefore.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular non-symlink file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil || !sameFileSnapshot(pathBefore, openedBefore) {
		return nil, fmt.Errorf("file changed while opening %s", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	openedAfter, openedErr := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !sameFileSnapshot(openedBefore, openedAfter) || !sameFileSnapshot(openedAfter, pathAfter) {
		return nil, fmt.Errorf("file changed while reading %s", path)
	}
	return data, nil
}

func requireMode0644(root, relative string) error {
	if err := requireRegularFilePath(root, relative); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o644 {
		return fmt.Errorf("%s mode=%#o want 0644", relative, info.Mode().Perm())
	}
	return nil
}

func requireRealDirectoryPath(root, relative string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe directory path %q", relative)
	}
	current := absRoot
	if err := requireDirectoryComponent(current); err != nil {
		return err
	}
	if clean == "." {
		return nil
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("unsafe directory component %q", component)
		}
		current = filepath.Join(current, component)
		if err := requireDirectoryComponent(current); err != nil {
			return err
		}
	}
	return nil
}

func requireRealDirectoryAbsolute(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := string(filepath.Separator)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	for _, component := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		if err := requireDirectoryComponent(current); err != nil {
			return err
		}
	}
	return nil
}

func requireDirectoryComponent(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("directory component is not a real directory: %s", path)
	}
	return nil
}

func secureMkdirAll(root, relative string) error {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe mkdir path %q", relative)
	}
	current, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := requireDirectoryComponent(current); err != nil {
		return err
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("unsafe mkdir component %q", component)
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("mkdir path traverses non-directory/symlink %s", current)
		}
	}
	return nil
}

func run(directory string, timeout time.Duration, environment []string, argv ...string) commandResult {
	result := commandResult{Argv: append([]string(nil), argv...)}
	if len(argv) == 0 {
		result.Err = errors.New("empty command")
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result.Stdout, result.Stderr = stdout.Bytes(), stderr.Bytes()
	if ctx.Err() != nil {
		result.Err = fmt.Errorf("%s timed out after %s", argv[0], timeout)
	} else if err != nil {
		result.Err = fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return result
}

func cleanEnvironment(inherited []string, overrides map[string]string) []string {
	// Child tools receive an allowlisted environment, not a blocklist that can
	// silently miss a new loader, workspace, cache, or startup variable. PATH is
	// retained only so the repository's declared toolchain names can resolve;
	// every semantic setting is supplied explicitly below.
	allowed := map[string]bool{"PATH": true}
	result := make([]string, 0, len(inherited)+len(overrides))
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if allowed[upper] {
			result = append(result, item)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func writeFailureReport(root, command string, inputPaths []string, checks []report.Check, evidence []report.RuntimeEvidence, cause error) error {
	checks = append(checks, report.Check{Name: "harness-preflight", Passed: false, Detail: cause.Error()})
	_, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P07", Suite: "arop-codegen-pipeline", Class: "arop.codegen", Command: command,
		CheckerPath: checkerPath, InputPaths: inputPaths, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary: map[string]any{"runtime_input_count": 0}, AuditNote: "P07 preflight failed before generator execution.",
	})
	return errors.Join(cause, err)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func modeString(mode os.FileMode) string {
	if mode&0o111 != 0 {
		return "100755"
	}
	return "100644"
}

func joinLog(result commandResult) []byte {
	var output bytes.Buffer
	output.WriteString(strings.Join(result.Argv, "\x00"))
	output.WriteByte('\n')
	output.Write(result.Stdout)
	output.WriteByte('\n')
	output.Write(result.Stderr)
	if result.Err != nil {
		output.WriteString("\nerror=")
		output.WriteString(result.Err.Error())
	}
	return output.Bytes()
}

func hasDuplicates(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func fatalIf(err error, context string) {
	if err != nil {
		fatal(fmt.Errorf("%s: %w", context, err))
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
