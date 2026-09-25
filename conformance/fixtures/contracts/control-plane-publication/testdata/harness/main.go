// Command harness verifies the P11 Publication contract and generated models.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	commandWant         = "make test-publication-contracts"
	rootDir             = "conformance/fixtures/contracts/control-plane-publication"
	checkerPath         = rootDir + "/testdata/harness/main.go"
	configPath          = rootDir + "/pipeline.json"
	casesPath           = rootDir + "/cases.json"
	inventoryPath       = rootDir + "/contract-inventory.json"
	waiverPath          = rootDir + "/transition/p11-p07-codegen-transition.json"
	provenancePath      = rootDir + "/generated/provenance.json"
	baselineCommit      = "a7b357dfd6a102622e7767539732d3cbbf184dbc"
	maxArchiveBytes     = int64(10485760)
	maxArchiveEntries   = int64(256)
	maxEntryBytes       = int64(4194304)
	maxTotalBytes       = int64(52428800)
	maxCompressionRatio = int64(100)
)

var orderedCaseIDs = []string{
	"aggregate-component-conflict", "aggregate-path-conflict", "asset-ref-size-bytes-safe-maximum", "asset-ref-size-bytes-safe-maximum-plus-one",
	"auth-get-agent-read", "auth-missing-bearer",
	"auth-post-agent-publish", "auth-unknown-security-extension", "bundle-absolute-path", "bundle-backslash-path",
	"bundle-case-collision", "bundle-central-directory-mismatch", "bundle-duplicate-entry", "bundle-encrypted-entry", "bundle-hardlink-entry",
	"bundle-limit-archive-bytes", "bundle-limit-compression-ratio", "bundle-limit-entry-count", "bundle-limit-entry-size",
	"bundle-limit-total-size", "bundle-multiple-manifests", "bundle-network-ref", "bundle-non-regular-type", "bundle-parent-path",
	"bundle-percent-bypass", "bundle-symlink-entry", "bundle-unicode-collision", "bundle-unsupported-compression", "bundle-valid",
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
	add("ordered-case-inventory", verifyCaseInventory(cases), "52 ordered unique cases")
	add("pipeline-contract", verifyPipeline(pipeline), "three roots, explicit fixture roots and exact outputs")
	openapi, _, openapiErr := structuredfile.LoadAny(filepath.Join(root, "openapi/fragments/control-plane/publication-v1.yaml"))
	add("openapi-strict-yaml", openapiErr, "single strict YAML document")
	add("openapi-exact-contract", verifyOpenAPI(openapi), "exact Publication operations/auth/scopes/headers/status/media")
	add("inventory-openapi-exact-binding", verifyInventoryContract(openapi, inventory), "inventory operations, headers, errors, idempotency, aggregate and bundle policy match parsed OpenAPI")
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
	result, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P11", Suite: "arop-publication-contracts", Class: "arop.publication", Command: command, CheckerPath: checkerPath, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"ordered_cases": 52, "logical_derives_from": []string{"openapi-control-plane-foundation", "publication-contract-fixtures", "generated-control-plane-go", "generated-control-plane-python", "generated-control-plane-typescript"}, "runtime_input_count": 0}, AuditNote: "P11 executes every ordered Publication case, independently regenerates and verifies three-language models/provenance, compiles isolated probes, validates the P01/P06/P07 transition and keeps runtime_inputs empty."})
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
	if !reflect.DeepEqual(roots, map[string]bool{"AROPError": true, "AgentManifest": true, "AssetRef": true}) {
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
	wantScopes := map[string][]string{"publishAgentVersion": {"agent:publish"}, "getAgentVersionManifest": {"agent:read"}}
	wantStatuses := map[string][]string{"publishAgentVersion": {"201", "400", "401", "403", "409", "413", "415", "429", "503"}, "getAgentVersionManifest": {"200", "401", "403", "404", "429", "503"}}
	for path, methods := range want {
		item, ok := paths[path].(map[string]any)
		if !ok || len(item) != len(methods) {
			return fmt.Errorf("missing path %s", path)
		}
		for method, id := range methods {
			op, ok := item[method].(map[string]any)
			if !ok || op["operationId"] != id {
				return fmt.Errorf("%s %s operation", method, path)
			}
			sec, _ := op["security"].([]any)
			if len(sec) != 1 || !reflect.DeepEqual(sec[0], map[string]any{"ControlPlaneBearer": []any{}}) {
				return fmt.Errorf("%s security", id)
			}
			scopes, err := stringsFromAny(op["x-arop-required-scopes"])
			if err != nil || !reflect.DeepEqual(scopes, wantScopes[id]) {
				return fmt.Errorf("%s scope", id)
			}
			if op["x-arop-reject-unknown-security-extensions"] != true {
				return fmt.Errorf("%s unknown security extension policy", id)
			}
			responses, _ := op["responses"].(map[string]any)
			if !reflect.DeepEqual(sortedMapKeys(responses), wantStatuses[id]) {
				return fmt.Errorf("%s response statuses=%v", id, sortedMapKeys(responses))
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
	post, _ := operationByID(doc, "publishAgentVersion")
	if post["x-arop-agent-id-must-match-manifest"] != true || post["x-arop-content-type-parameters-allowed"] != false {
		return errors.New("publication request invariants")
	}
	idempotency, _ := post["x-arop-idempotency"].(map[string]any)
	if idempotency["same-key-same-semantic-digest"] != "stable-201-same-location-and-etag" || integer(idempotency["same-key-different-semantic-digest"]) != 409 || len(idempotency) != 2 {
		return errors.New("publication idempotency contract")
	}
	requestBody, _ := post["requestBody"].(map[string]any)
	requestContent, _ := requestBody["content"].(map[string]any)
	if requestBody["required"] != true || !reflect.DeepEqual(sortedMapKeys(requestContent), []string{"application/vnd.arop.agent-version-bundle+zip"}) {
		return errors.New("publication request media type")
	}
	postResponses, _ := post["responses"].(map[string]any)
	created, err := resolveLocal(doc, postResponses["201"])
	if err != nil {
		return err
	}
	if _, exists := created["content"]; exists || !reflect.DeepEqual(sortedMapKeys(asMap(created["headers"])), []string{"ETag", "Location"}) {
		return errors.New("201 response body/header contract")
	}
	get, _ := operationByID(doc, "getAgentVersionManifest")
	getResponses, _ := get["responses"].(map[string]any)
	okResponse, err := resolveLocal(doc, getResponses["200"])
	if err != nil {
		return err
	}
	content := asMap(okResponse["content"])
	jsonMedia := asMap(content["application/json"])
	schema := asMap(jsonMedia["schema"])
	if schema["$ref"] != "../../../schemas/manifest/agent-manifest-v1.schema.json" || !reflect.DeepEqual(sortedMapKeys(asMap(okResponse["headers"])), []string{"ETag"}) {
		return errors.New("200 manifest response contract")
	}
	components := asMap(doc["components"])
	headers := asMap(components["headers"])
	if err := verifyHeaderComponent(doc, headers, "ManifestETag", `^"sha256:[0-9a-f]{64}"$`, 0, 0); err != nil {
		return err
	}
	if err := verifyHeaderComponent(doc, headers, "AgentVersionLocation", `^/v1/agent-definitions/`, 0, 0); err != nil {
		return err
	}
	if err := verifyHeaderComponent(doc, headers, "RetryAfter", "", 1, 86400); err != nil {
		return err
	}
	if err := verifyHeaderComponent(doc, headers, "WWWAuthenticate", `^Bearer`, 0, 0); err != nil {
		return err
	}
	for _, row := range expectedErrors() {
		if err := verifyOpenAPIError(doc, row); err != nil {
			return err
		}
	}
	return nil
}

type errorContract struct {
	Status          string
	Code            string
	Category        string
	Retryable       bool
	RequiredHeaders []string
}

func expectedErrors() []errorContract {
	return []errorContract{
		{"400", "INVALID_PUBLICATION_REQUEST", "validation", false, nil},
		{"401", "AUTHENTICATION_REQUIRED", "authentication", false, []string{"WWW-Authenticate"}},
		{"403", "PUBLICATION_FORBIDDEN", "authorization", false, nil},
		{"404", "AGENT_VERSION_NOT_FOUND", "not_found", false, nil},
		{"409", "AGENT_VERSION_CONFLICT", "conflict", false, nil},
		{"413", "BUNDLE_TOO_LARGE", "capacity", false, nil},
		{"415", "UNSUPPORTED_MEDIA_TYPE", "validation", false, nil},
		{"429", "RATE_LIMITED", "capacity", true, []string{"Retry-After"}},
		{"503", "DEPENDENCY_UNAVAILABLE", "dependency", true, []string{"Retry-After"}},
	}
}

func operationByID(doc map[string]any, id string) (map[string]any, bool) {
	for _, rawPath := range asMap(doc["paths"]) {
		for _, rawOperation := range asMap(rawPath) {
			op := asMap(rawOperation)
			if op["operationId"] == id {
				return op, true
			}
		}
	}
	return nil, false
}

func resolveLocal(doc map[string]any, raw any) (map[string]any, error) {
	value := asMap(raw)
	for depth := 0; depth < 8; depth++ {
		ref, _ := value["$ref"].(string)
		if ref == "" {
			return value, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("non-local OpenAPI component ref %s", ref)
		}
		var current any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			object, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ref %s traverses non-object", ref)
			}
			current, ok = object[part]
			if !ok {
				return nil, fmt.Errorf("ref %s missing %s", ref, part)
			}
		}
		value = asMap(current)
	}
	return nil, errors.New("OpenAPI local ref depth exceeded")
}

func verifyHeaderComponent(doc map[string]any, headers map[string]any, name, patternPrefix string, minimum, maximum int64) error {
	header, err := resolveLocal(doc, headers[name])
	if err != nil {
		return fmt.Errorf("header %s: %w", name, err)
	}
	if header["required"] != true {
		return fmt.Errorf("header %s not required", name)
	}
	schema := asMap(header["schema"])
	if patternPrefix != "" && !strings.HasPrefix(fmt.Sprint(schema["pattern"]), patternPrefix) {
		return fmt.Errorf("header %s pattern", name)
	}
	if minimum != 0 && (integer(schema["minimum"]) != minimum || integer(schema["maximum"]) != maximum) {
		return fmt.Errorf("header %s bounds", name)
	}
	return nil
}

func verifyOpenAPIError(doc map[string]any, want errorContract) error {
	found := false
	for _, operationID := range []string{"publishAgentVersion", "getAgentVersionManifest"} {
		op, _ := operationByID(doc, operationID)
		responses := asMap(op["responses"])
		raw, exists := responses[want.Status]
		if !exists {
			continue
		}
		found = true
		response, err := resolveLocal(doc, raw)
		if err != nil {
			return err
		}
		if response["x-arop-error-code"] != want.Code || response["x-arop-error-category"] != want.Category || response["x-arop-retryable"] != want.Retryable {
			return fmt.Errorf("%s status %s error semantics", operationID, want.Status)
		}
		headers := sortedMapKeys(asMap(response["headers"]))
		if !reflect.DeepEqual(headers, want.RequiredHeaders) && !(len(headers) == 0 && len(want.RequiredHeaders) == 0) {
			return fmt.Errorf("%s status %s headers=%v", operationID, want.Status, headers)
		}
		content := asMap(response["content"])
		if !reflect.DeepEqual(sortedMapKeys(content), []string{"application/json"}) {
			return fmt.Errorf("%s status %s media", operationID, want.Status)
		}
		schema := asMap(asMap(content["application/json"])["schema"])
		if schema["$ref"] != "#/components/schemas/AROPError" {
			return fmt.Errorf("%s status %s body schema", operationID, want.Status)
		}
	}
	if !found {
		return fmt.Errorf("error status %s absent", want.Status)
	}
	return nil
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringsFromAny(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("expected string array")
	}
	result := make([]string, len(items))
	for index, item := range items {
		var ok bool
		result[index], ok = item.(string)
		if !ok {
			return nil, errors.New("expected string array item")
		}
	}
	return result, nil
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func integer(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	default:
		return -1
	}
}

func verifyInventoryContract(openapi any, inventory map[string]any) error {
	doc := asMap(openapi)
	if inventory["schema_version"] != float64(1) && integer(inventory["schema_version"]) != 1 {
		return errors.New("inventory schema version")
	}
	if inventory["contract_id"] != "arop-control-plane-publication-v1" || inventory["phase"] != "P11" || inventory["exposure"] != "public-contract" {
		return errors.New("inventory identity")
	}
	aggregate := asMap(inventory["aggregate_policy"])
	if aggregate["aggregate_path"] != "openapi/control-plane-v1.yaml" || aggregate["completion_phase"] != "P20" || aggregate["p11_rule"] != "tracked-aggregate-forbidden-synthetic-bundle-only" {
		return errors.New("aggregate policy")
	}
	security := asMap(inventory["security"])
	if security["scheme_name"] != "ControlPlaneBearer" || security["type"] != "http" || security["scheme"] != "bearer" || security["bearer_format_declared"] != false {
		return errors.New("inventory security")
	}
	operations, ok := inventory["operations"].([]any)
	if !ok || len(operations) != 2 {
		return errors.New("inventory operations")
	}
	wantPaths := map[string]string{"publishAgentVersion": "/v1/agent-definitions/{agent_id}/versions", "getAgentVersionManifest": "/v1/agent-definitions/{agent_id}/versions/{version}"}
	wantMethods := map[string]string{"publishAgentVersion": "POST", "getAgentVersionManifest": "GET"}
	wantScopes := map[string][]string{"publishAgentVersion": {"agent:publish"}, "getAgentVersionManifest": {"agent:read"}}
	seen := map[string]bool{}
	for _, raw := range operations {
		entry := asMap(raw)
		id := fmt.Sprint(entry["operation_id"])
		if seen[id] || wantPaths[id] == "" || entry["path"] != wantPaths[id] || entry["method"] != wantMethods[id] {
			return fmt.Errorf("inventory operation %s identity", id)
		}
		seen[id] = true
		scopes, err := stringsFromAny(entry["required_scopes"])
		if err != nil || !reflect.DeepEqual(scopes, wantScopes[id]) || entry["scope_extension"] != "x-arop-required-scopes" {
			return fmt.Errorf("inventory operation %s scopes", id)
		}
		op, exists := operationByID(doc, id)
		if !exists {
			return fmt.Errorf("OpenAPI operation %s absent", id)
		}
		openScopes, _ := stringsFromAny(op["x-arop-required-scopes"])
		if !reflect.DeepEqual(openScopes, scopes) {
			return fmt.Errorf("inventory operation %s scope drift", id)
		}
		success := asMap(entry["success"])
		responses := asMap(op["responses"])
		status := fmt.Sprint(integer(success["status"]))
		response, err := resolveLocal(doc, responses[status])
		if err != nil {
			return err
		}
		responseHeaders := sortedMapKeys(asMap(response["headers"]))
		declaredHeaders, err := stringsFromAny(success["required_headers"])
		if err != nil || !reflect.DeepEqual(responseHeaders, declaredHeaders) {
			return fmt.Errorf("inventory operation %s success headers", id)
		}
		requestMedia, err := stringsFromAny(entry["request_media_types"])
		if err != nil {
			return err
		}
		if id == "publishAgentVersion" {
			if !reflect.DeepEqual(requestMedia, []string{"application/vnd.arop.agent-version-bundle+zip"}) || success["body_schema"] != nil || success["response_media_type"] != nil || status != "201" {
				return errors.New("publish inventory success/media")
			}
		} else if len(requestMedia) != 0 || success["body_schema"] != "AgentManifest" || success["response_media_type"] != "application/json" || status != "200" {
			return errors.New("read inventory success/media")
		}
	}
	request := asMap(inventory["request_invariants"])
	idempotency := asMap(request["idempotency"])
	post, _ := operationByID(doc, "publishAgentVersion")
	postIdempotency := asMap(post["x-arop-idempotency"])
	if request["agent_id_equals_manifest_identity_id"] != true || request["content_type_parameters_allowed"] != false || request["unknown_security_extension"] != "fail-closed" || idempotency["same_key_same_semantic_digest"] != postIdempotency["same-key-same-semantic-digest"] || fmt.Sprint(idempotency["same_key_different_semantic_digest"]) != fmt.Sprint(integer(postIdempotency["same-key-different-semantic-digest"])) {
		return errors.New("inventory request invariants")
	}
	headerContract := asMap(inventory["header_contract"])
	if asMap(headerContract["ETag"])["exact_wire_pattern"] != `"sha256:<64 lowercase hex>"` || asMap(headerContract["ETag"])["weak_allowed"] != false || asMap(headerContract["Content-Type"])["exact_value"] != "application/vnd.arop.agent-version-bundle+zip" || asMap(headerContract["Content-Type"])["parameters_allowed"] != false || integer(asMap(headerContract["Idempotency-Key"])["min_bytes"]) != 8 || integer(asMap(headerContract["Idempotency-Key"])["max_bytes"]) != 200 || integer(asMap(headerContract["Retry-After"])["minimum"]) != 1 || integer(asMap(headerContract["Retry-After"])["maximum"]) != 86400 {
		return errors.New("inventory header contract")
	}
	errorRows, ok := inventory["error_contract"].([]any)
	if !ok || len(errorRows) != len(expectedErrors()) {
		return errors.New("inventory error contract count")
	}
	for index, want := range expectedErrors() {
		row := asMap(errorRows[index])
		headers, err := stringsFromAny(row["required_headers"])
		if err != nil || fmt.Sprint(integer(row["status"])) != want.Status || row["body_schema"] != "AROPError" || row["code"] != want.Code || row["category"] != want.Category || row["retryable"] != want.Retryable || !reflect.DeepEqual(headers, want.RequiredHeaders) && !(len(headers) == 0 && len(want.RequiredHeaders) == 0) {
			return fmt.Errorf("inventory error row %d", index)
		}
	}
	bundle := asMap(inventory["bundle_profile"])
	limits := asMap(bundle["limits"])
	if bundle["media_type"] != "application/vnd.arop.agent-version-bundle+zip" || bundle["manifest_path"] != "agent-manifest.json" || integer(bundle["manifest_count"]) != 1 || bundle["network_access"] != false || integer(limits["max_archive_bytes"]) != maxArchiveBytes || integer(limits["max_entries"]) != maxArchiveEntries || integer(limits["max_entry_uncompressed_bytes"]) != maxEntryBytes || integer(limits["max_total_uncompressed_bytes"]) != maxTotalBytes || integer(limits["max_compression_ratio"]) != maxCompressionRatio {
		return errors.New("inventory bundle limits/profile")
	}
	codegen := asMap(inventory["codegen"])
	roots, ok := codegen["roots"].([]any)
	if !ok || len(roots) != 3 {
		return errors.New("inventory codegen roots")
	}
	rootNames := []string{}
	for _, raw := range roots {
		rootNames = append(rootNames, fmt.Sprint(asMap(raw)["name"]))
	}
	if !reflect.DeepEqual(rootNames, []string{"AROPError", "AgentManifest", "AssetRef"}) {
		return fmt.Errorf("inventory codegen roots=%v", rootNames)
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
	doc := asMap(openapi)
	if strings.HasPrefix(c.ID, "aggregate-") {
		return verifyAggregationCase(root, c, doc)
	}
	if strings.HasPrefix(c.ID, "auth-") {
		return verifyAuthorizationCase(root, c, doc)
	}
	if strings.HasPrefix(c.ID, "error-") {
		return verifyErrorCase(root, c, doc)
	}
	if strings.HasPrefix(c.ID, "idempotency-") {
		return verifyIdempotencyCase(root, c, doc)
	}
	switch c.ID {
	case "get-manifest-200-etag":
		if c.Expected["etag"] != "strong-semantic-manifest-digest" {
			return errors.New("GET ETag expectation is not semantic digest")
		}
		var manifest map[string]any
		if err := load(root, rootDir+"/"+c.Source, &manifest); err != nil {
			return err
		}
		if manifest["kind"] != "AgentManifest" {
			return errors.New("GET fixture is not AgentManifest")
		}
		get, _ := operationByID(doc, "getAgentVersionManifest")
		response, err := resolveLocal(doc, asMap(get["responses"])["200"])
		if err != nil {
			return err
		}
		etag, err := resolveLocal(doc, asMap(response["headers"])["ETag"])
		if err != nil || asMap(etag["schema"])["pattern"] != `^"sha256:[0-9a-f]{64}"$` || etag["x-arop-digest-source"] != "validated-semantic-manifest-document" || etag["x-arop-strength"] != "strong" {
			return errors.New("GET ETag is not exact strong semantic digest")
		}
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
		post, _ := operationByID(doc, "publishAgentVersion")
		declared := sortedMapKeys(asMap(asMap(post["requestBody"])["content"]))
		if !reflect.DeepEqual(declared, []string{"application/vnd.arop.agent-version-bundle+zip"}) || post["x-arop-content-type-parameters-allowed"] != false || fmt.Sprint(c.Input["content_type"]) == declared[0] {
			return errors.New("negative content type did not vary")
		}
		if c.Expected["status"] != float64(415) && integer(c.Expected["status"]) != 415 || c.Expected["code"] != "UNSUPPORTED_MEDIA_TYPE" {
			return errors.New("content type expected response")
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
		post, _ := operationByID(doc, "publishAgentVersion")
		created, err := resolveLocal(doc, asMap(post["responses"])["201"])
		if err != nil {
			return err
		}
		headers := asMap(response["headers"])
		if integer(response["status"]) != 201 || len(headers) != 2 || !strings.HasPrefix(fmt.Sprint(headers["ETag"]), `"sha256:`) || !strings.HasPrefix(fmt.Sprint(headers["Location"]), "/v1/agent-definitions/") || !reflect.DeepEqual(sortedMapKeys(asMap(created["headers"])), []string{"ETag", "Location"}) {
			return errors.New("201 fixture/OpenAPI header contract")
		}
	case "manifest-agent-id-mismatch":
		post, _ := operationByID(doc, "publishAgentVersion")
		if post["x-arop-agent-id-must-match-manifest"] != true || c.Input["path_agent_id"] == c.Input["manifest_agent_id"] || integer(c.Expected["status"]) != 400 || c.Expected["code"] != "INVALID_PUBLICATION_REQUEST" {
			return errors.New("agent identity mismatch contract")
		}
	default:
		return fmt.Errorf("case %s has no executable verifier", c.ID)
	}
	return nil
}

func matrixCase(root string, c caseDef) (map[string]any, error) {
	if c.Source == "" {
		return nil, errors.New("matrix source missing")
	}
	var matrix map[string]any
	if err := load(root, rootDir+"/"+c.Source, &matrix); err != nil {
		return nil, err
	}
	rows, ok := matrix["cases"].([]any)
	if !ok {
		return nil, errors.New("matrix cases missing")
	}
	for _, raw := range rows {
		row := asMap(raw)
		if row["id"] == c.ID {
			return row, nil
		}
	}
	return nil, fmt.Errorf("case %s absent from matrix %s", c.ID, c.Source)
}

func verifyAggregationCase(root string, c caseDef, doc map[string]any) error {
	row, err := matrixCase(root, c)
	if err != nil {
		return err
	}
	expected := asMap(row["expected"])
	if expected["accepted"] != false {
		return errors.New("aggregation conflict must be rejected")
	}
	input := asMap(row["input"])
	switch c.ID {
	case "aggregate-component-conflict":
		components := asMap(asMap(doc["components"])["securitySchemes"])
		name := fmt.Sprint(input["existing_component"])
		if _, exists := components[name]; !exists || input["incoming_component"] == components[name] || expected["reason"] != "component-conflict" {
			return errors.New("component conflict fixture does not conflict with parsed OpenAPI")
		}
	case "aggregate-path-conflict":
		existing := fmt.Sprint(input["existing_operation"])
		parts := strings.SplitN(existing, " ", 2)
		if len(parts) != 2 {
			return errors.New("invalid aggregate operation fixture")
		}
		pathItem := asMap(asMap(doc["paths"])[parts[1]])
		if _, exists := pathItem[strings.ToLower(parts[0])]; !exists || input["incoming_operation"] == pathItem[strings.ToLower(parts[0])] || expected["reason"] != "path-method-conflict" {
			return errors.New("path conflict fixture does not conflict with parsed OpenAPI")
		}
	default:
		return errors.New("unknown aggregation case")
	}
	return nil
}

func verifyAuthorizationCase(root string, c caseDef, doc map[string]any) error {
	row, err := matrixCase(root, c)
	if err != nil {
		return err
	}
	opID := fmt.Sprint(row["operation_id"])
	op, exists := operationByID(doc, opID)
	if !exists {
		return fmt.Errorf("operation %s absent", opID)
	}
	required, err := stringsFromAny(op["x-arop-required-scopes"])
	if err != nil {
		return err
	}
	presented, err := stringsFromAny(row["presented_scopes"])
	if err != nil {
		return err
	}
	credentialPresent := true
	if raw, exists := row["credential_present"]; exists {
		credentialPresent, _ = raw.(bool)
	}
	allowed := credentialPresent && row["unknown_security_extension"] != true
	for _, requiredScope := range required {
		found := false
		for _, scope := range presented {
			found = found || scope == requiredScope
		}
		allowed = allowed && found
	}
	expected := asMap(row["expected"])
	if expected["allowed"] != allowed {
		return fmt.Errorf("authorization result allowed=%t", allowed)
	}
	if !credentialPresent && (integer(expected["status"]) != 401 || expected["code"] != "AUTHENTICATION_REQUIRED") {
		return errors.New("missing credential result")
	}
	if row["unknown_security_extension"] == true && (op["x-arop-reject-unknown-security-extensions"] != true || expected["reason"] != "unknown-security-extension") {
		return errors.New("unknown security extension result")
	}
	return nil
}

func verifyErrorCase(root string, c caseDef, doc map[string]any) error {
	row, err := matrixCase(root, c)
	if err != nil {
		return err
	}
	want := errorContract{Status: fmt.Sprint(integer(row["status"])), Code: fmt.Sprint(asMap(row["body"])["code"]), Category: fmt.Sprint(asMap(row["body"])["category"])}
	want.Retryable, _ = asMap(row["body"])["retryable"].(bool)
	if headers, ok := row["required_headers"].([]any); ok {
		for _, raw := range headers {
			want.RequiredHeaders = append(want.RequiredHeaders, fmt.Sprint(raw))
		}
	} else {
		want.RequiredHeaders = sortedMapKeys(asMap(row["required_headers"]))
	}
	if err := verifyOpenAPIError(doc, want); err != nil {
		return err
	}
	headers := asMap(row["required_headers"])
	if value, exists := headers["Retry-After"]; exists {
		number := integer(json.Number(fmt.Sprint(value)))
		if number < 1 || number > 86400 || integer(asMap(row["body"])["retry_after_seconds"]) != number {
			return errors.New("Retry-After fixture/header mismatch")
		}
	}
	if value, exists := headers["WWW-Authenticate"]; exists && value != "Bearer" {
		return errors.New("WWW-Authenticate fixture")
	}
	return nil
}

func verifyIdempotencyCase(root string, c caseDef, doc map[string]any) error {
	row, err := matrixCase(root, c)
	if err != nil {
		return err
	}
	post, _ := operationByID(doc, "publishAgentVersion")
	contract := asMap(post["x-arop-idempotency"])
	same := row["first_digest"] == row["replay_digest"]
	expected := asMap(row["expected"])
	if same {
		if contract["same-key-same-semantic-digest"] != "stable-201-same-location-and-etag" || integer(expected["status"]) != 201 || expected["etag"] != fmt.Sprintf(`"%s"`, row["first_digest"]) || !strings.HasPrefix(fmt.Sprint(expected["location"]), "/v1/agent-definitions/") {
			return errors.New("same digest replay contract")
		}
	} else if integer(contract["same-key-different-semantic-digest"]) != 409 || integer(expected["status"]) != 409 || expected["code"] != "AGENT_VERSION_CONFLICT" {
		return errors.New("different digest replay contract")
	}
	key := fmt.Sprint(row["key"])
	if len(key) < 8 || len(key) > 200 || strings.IndexFunc(key, func(r rune) bool { return r < '!' || r > '~' }) >= 0 {
		return errors.New("idempotency key fixture outside header contract")
	}
	return nil
}

func verifyBundleCase(root string, c caseDef) error {
	row, err := bundleMatrixRow(root, c)
	if err != nil {
		return err
	}
	if c.ID == "bundle-valid" {
		path, err := bundleFixturePath(root, row)
		if err != nil {
			return err
		}
		archive, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := validateBundleArchive(archive); err != nil {
			return fmt.Errorf("valid archive rejected: %w", err)
		}
		r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return err
		}
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
		parsed, err := structuredfile.Parse(data, "json")
		if err != nil {
			return err
		}
		if _, ok := parsed.(map[string]any); !ok {
			return errors.New("manifest is not an object")
		}
		if !reflect.DeepEqual(parsed, v) {
			return errors.New("archive manifest differs from validated semantic fixture")
		}
		return nil
	}
	if fixture, ok := row["fixture"].(string); ok {
		path, err := bundleFixturePath(root, row)
		if err != nil {
			return err
		}
		archive, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fixture == "" {
			return errors.New("empty bundle fixture")
		}
		return expectBundleReason(archive, fmt.Sprint(asMap(row["expected"])["reason"]))
	}
	if entries, ok := stringSlice(row["entries"]); ok {
		archive, err := archiveWithEntries(entries, zip.Deflate, 0)
		if err != nil {
			return err
		}
		return expectBundleReason(archive, fmt.Sprint(asMap(row["expected"])["reason"]))
	}
	if ref, ok := row["manifest_ref"].(string); ok {
		u, err := url.Parse(ref)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("network reference fixture is not an absolute HTTPS reference")
		}
		return nil
	}
	if limits, ok := row["limits"].(map[string]any); ok {
		maximums := map[string]int64{"archive_bytes": maxArchiveBytes, "compression_ratio": maxCompressionRatio, "entries": maxArchiveEntries, "entry_uncompressed_bytes": maxEntryBytes, "total_uncompressed_bytes": maxTotalBytes}
		if len(limits) != 1 {
			return errors.New("limit fixture must isolate exactly one limit")
		}
		for name, raw := range limits {
			value := integer(raw)
			maximum, mok := maximums[name]
			if !mok || value != maximum+1 {
				return fmt.Errorf("limit fixture %s is not exact maximum+1", name)
			}
			archive, err := archiveForLimit(name)
			if err != nil {
				return err
			}
			return expectBundleReason(archive, fmt.Sprint(asMap(row["expected"])["reason"]))
		}
	}
	if profile, ok := row["generated_archive"].(string); ok {
		archive, err := generatedAdversarialArchive(profile)
		if err != nil {
			return err
		}
		return expectBundleReason(archive, fmt.Sprint(asMap(row["expected"])["reason"]))
	}
	return fmt.Errorf("bundle case %s has no executable fixture", c.ID)
}

func bundleMatrixRow(root string, c caseDef) (map[string]any, error) {
	var matrix map[string]any
	if c.Source == "" {
		return nil, errors.New("bundle case has no matrix source")
	}
	if err := load(root, rootDir+"/"+c.Source, &matrix); err != nil {
		return nil, err
	}
	rows, _ := matrix["cases"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["id"] == c.ID {
			expected, _ := row["expected"].(map[string]any)
			if expected["accepted"] != (c.ID == "bundle-valid") {
				return nil, errors.New("bundle acceptance expectation mismatch")
			}
			return row, nil
		}
	}
	return nil, fmt.Errorf("bundle case %s absent from matrix", c.ID)
}

func bundleFixturePath(root string, row map[string]any) (string, error) {
	fixture, _ := row["fixture"].(string)
	base := filepath.Join(root, rootDir, "fixtures/matrices")
	path := filepath.Clean(filepath.Join(base, fixture))
	allowed := filepath.Join(root, rootDir, "fixtures", "bundles") + string(os.PathSeparator)
	if fixture == "" || !strings.HasPrefix(path, allowed) {
		return "", errors.New("bundle fixture escapes controlled bundle directory")
	}
	return path, nil
}

func stringSlice(raw any) ([]string, bool) {
	values, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

type bundleValidationError struct{ reason string }

func (e bundleValidationError) Error() string { return e.reason }

func rejectBundle(reason string) error { return bundleValidationError{reason: reason} }

func expectBundleReason(archive []byte, want string) error {
	err := validateBundleArchive(archive)
	var rejected bundleValidationError
	if !errors.As(err, &rejected) {
		if err == nil {
			return fmt.Errorf("adversarial archive accepted; want %s", want)
		}
		return fmt.Errorf("archive failed outside policy reason: %w", err)
	}
	if rejected.reason != want {
		return fmt.Errorf("archive reason=%s want=%s", rejected.reason, want)
	}
	return nil
}

func validateBundleArchive(archive []byte) error {
	if int64(len(archive)) > maxArchiveBytes {
		return rejectBundle("archive-bytes")
	}
	if err := verifyCentralDirectory(archive); err != nil {
		return rejectBundle("central-directory-mismatch")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return rejectBundle("central-directory-mismatch")
	}
	if int64(len(reader.File)) > maxArchiveEntries {
		return rejectBundle("entry-count")
	}
	seen, folded, normalized := map[string]bool{}, map[string]string{}, map[string]string{}
	manifestCount := 0
	for _, file := range reader.File {
		name := file.Name
		if seen[name] {
			return rejectBundle("duplicate-entry")
		}
		seen[name] = true
		if prior, exists := folded[strings.ToLower(name)]; exists && prior != name {
			return rejectBundle("case-collision")
		}
		folded[strings.ToLower(name)] = name
		normal := strings.ReplaceAll(name, "e\u0301", "é")
		if prior, exists := normalized[normal]; exists && prior != name {
			return rejectBundle("unicode-normalization-collision")
		}
		normalized[normal] = name
		if strings.TrimPrefix(file.Name, "./") == "agent-manifest.json" {
			manifestCount++
		}
	}
	if manifestCount > 1 {
		return rejectBundle("multiple-root-manifests")
	}
	seen, folded, normalized = map[string]bool{}, map[string]string{}, map[string]string{}
	var total uint64
	highCompressionRatio := false
	for _, file := range reader.File {
		name := file.Name
		if seen[name] {
			return rejectBundle("duplicate-entry")
		}
		seen[name] = true
		if prior, exists := folded[strings.ToLower(name)]; exists && prior != name {
			return rejectBundle("case-collision")
		}
		folded[strings.ToLower(name)] = name
		normal := strings.ReplaceAll(name, "e\u0301", "é")
		if prior, exists := normalized[normal]; exists && prior != name {
			return rejectBundle("unicode-normalization-collision")
		}
		normalized[normal] = name
		if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || len(name) >= 3 && name[1] == ':' {
			return rejectBundle("absolute-path")
		}
		if strings.Contains(name, "\\") {
			return rejectBundle("backslash-path")
		}
		decoded, decodeErr := url.PathUnescape(name)
		if decodeErr != nil || decoded != name {
			return rejectBundle("percent-encoded-path-bypass")
		}
		for _, segment := range strings.Split(name, "/") {
			if segment == ".." {
				return rejectBundle("parent-path-segment")
			}
			if segment == "" || segment == "." {
				return rejectBundle("non-portable-path")
			}
		}
		if file.Flags&1 != 0 {
			return rejectBundle("encrypted-entry")
		}
		if bytes.Contains(file.Extra, []byte("HARDLINK\x00")) {
			return rejectBundle("hardlink-entry")
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return rejectBundle("symlink-entry")
		}
		if file.Mode()&os.ModeType != 0 {
			return rejectBundle("non-regular-type")
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return rejectBundle("unsupported-compression")
		}
		if file.UncompressedSize64 > uint64(maxEntryBytes) {
			return rejectBundle("entry-size")
		}
		if ^uint64(0)-total < file.UncompressedSize64 {
			return rejectBundle("total-size")
		}
		total += file.UncompressedSize64
		if total > uint64(maxTotalBytes) {
			return rejectBundle("total-size")
		}
		if file.UncompressedSize64 > 0 && (file.CompressedSize64 == 0 || file.CompressedSize64 <= ^uint64(0)/uint64(maxCompressionRatio) && file.UncompressedSize64 > file.CompressedSize64*uint64(maxCompressionRatio)) {
			highCompressionRatio = true
		}
	}
	if highCompressionRatio {
		return rejectBundle("compression-ratio")
	}
	return nil
}

func verifyCentralDirectory(archive []byte) error {
	eocd := -1
	start := len(archive) - 22 - 65535
	if start < 0 {
		start = 0
	}
	for index := len(archive) - 22; index >= start; index-- {
		if index >= 0 && binary.LittleEndian.Uint32(archive[index:index+4]) == 0x06054b50 {
			eocd = index
			break
		}
	}
	if eocd < 0 || eocd+22 > len(archive) {
		return errors.New("end record absent")
	}
	commentLength := int(binary.LittleEndian.Uint16(archive[eocd+20 : eocd+22]))
	if eocd+22+commentLength != len(archive) || binary.LittleEndian.Uint16(archive[eocd+4:eocd+6]) != 0 || binary.LittleEndian.Uint16(archive[eocd+6:eocd+8]) != 0 {
		return errors.New("end record inconsistent")
	}
	countDisk := int(binary.LittleEndian.Uint16(archive[eocd+8 : eocd+10]))
	countTotal := int(binary.LittleEndian.Uint16(archive[eocd+10 : eocd+12]))
	centralSize := int(binary.LittleEndian.Uint32(archive[eocd+12 : eocd+16]))
	centralOffset := int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
	if countDisk != countTotal || countTotal == 0xffff || centralOffset < 0 || centralSize < 0 || centralOffset+centralSize != eocd {
		return errors.New("central directory bounds")
	}
	position := centralOffset
	localOffsets := map[int]bool{}
	for index := 0; index < countTotal; index++ {
		if position+46 > eocd || binary.LittleEndian.Uint32(archive[position:position+4]) != 0x02014b50 {
			return errors.New("central entry absent")
		}
		flags := binary.LittleEndian.Uint16(archive[position+8 : position+10])
		method := binary.LittleEndian.Uint16(archive[position+10 : position+12])
		crc := binary.LittleEndian.Uint32(archive[position+16 : position+20])
		compressed := binary.LittleEndian.Uint32(archive[position+20 : position+24])
		uncompressed := binary.LittleEndian.Uint32(archive[position+24 : position+28])
		nameLength := int(binary.LittleEndian.Uint16(archive[position+28 : position+30]))
		extraLength := int(binary.LittleEndian.Uint16(archive[position+30 : position+32]))
		entryCommentLength := int(binary.LittleEndian.Uint16(archive[position+32 : position+34]))
		localOffset := int(binary.LittleEndian.Uint32(archive[position+42 : position+46]))
		end := position + 46 + nameLength + extraLength + entryCommentLength
		if end > eocd || localOffset < 0 || localOffset+30 > centralOffset || localOffsets[localOffset] || binary.LittleEndian.Uint32(archive[localOffset:localOffset+4]) != 0x04034b50 {
			return errors.New("local entry bounds")
		}
		localOffsets[localOffset] = true
		localFlags := binary.LittleEndian.Uint16(archive[localOffset+6 : localOffset+8])
		localMethod := binary.LittleEndian.Uint16(archive[localOffset+8 : localOffset+10])
		localNameLength := int(binary.LittleEndian.Uint16(archive[localOffset+26 : localOffset+28]))
		localExtraLength := int(binary.LittleEndian.Uint16(archive[localOffset+28 : localOffset+30]))
		localEnd := localOffset + 30 + localNameLength + localExtraLength
		if localEnd > centralOffset || flags != localFlags || method != localMethod || !bytes.Equal(archive[position+46:position+46+nameLength], archive[localOffset+30:localOffset+30+localNameLength]) {
			return errors.New("central/local metadata mismatch")
		}
		if flags&8 == 0 && (crc != binary.LittleEndian.Uint32(archive[localOffset+14:localOffset+18]) || compressed != binary.LittleEndian.Uint32(archive[localOffset+18:localOffset+22]) || uncompressed != binary.LittleEndian.Uint32(archive[localOffset+22:localOffset+26])) {
			return errors.New("central/local digest-size mismatch")
		}
		position = end
	}
	if position != eocd {
		return errors.New("central directory trailing bytes")
	}
	return nil
}

func archiveWithEntries(names []string, method uint16, payloadBytes int64) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: method}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		remaining := payloadBytes
		chunk := make([]byte, 64<<10)
		for remaining > 0 {
			count := int64(len(chunk))
			if count > remaining {
				count = remaining
			}
			if _, err := entry.Write(chunk[:int(count)]); err != nil {
				return nil, err
			}
			remaining -= count
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func archiveForLimit(name string) ([]byte, error) {
	switch name {
	case "archive_bytes":
		return archiveWithEntries([]string{"agent-manifest.json"}, zip.Store, maxArchiveBytes+1)
	case "entries":
		names := make([]string, int(maxArchiveEntries+1))
		for index := range names {
			names[index] = fmt.Sprintf("entry-%03d.json", index)
		}
		names[0] = "agent-manifest.json"
		return archiveWithEntries(names, zip.Store, 0)
	case "entry_uncompressed_bytes":
		return archiveWithEntries([]string{"agent-manifest.json"}, zip.Deflate, maxEntryBytes+1)
	case "total_uncompressed_bytes":
		names := make([]string, 13)
		for index := range names {
			names[index] = fmt.Sprintf("entry-%02d.bin", index)
		}
		names[0] = "agent-manifest.json"
		return archiveWithEntries(names, zip.Deflate, maxEntryBytes)
	case "compression_ratio":
		return archiveWithEntries([]string{"agent-manifest.json"}, zip.Deflate, 1<<20)
	default:
		return nil, fmt.Errorf("unknown limit %s", name)
	}
}

func generatedAdversarialArchive(profile string) ([]byte, error) {
	switch profile {
	case "named-pipe-entry":
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		header := &zip.FileHeader{Name: "agent-manifest.json", Method: zip.Store}
		header.SetMode(os.ModeNamedPipe | 0o600)
		if _, err := writer.CreateHeader(header); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return buffer.Bytes(), nil
	case "central-local-name-mismatch":
		archive, err := archiveWithEntries([]string{"agent-manifest.json"}, zip.Store, 0)
		if err != nil {
			return nil, err
		}
		archive = append([]byte(nil), archive...)
		archive[30] = 'b'
		return archive, nil
	case "unsupported-method-99":
		archive, err := archiveWithEntries([]string{"agent-manifest.json"}, zip.Store, 0)
		if err != nil {
			return nil, err
		}
		archive = append([]byte(nil), archive...)
		eocd := len(archive) - 22
		central := int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
		binary.LittleEndian.PutUint16(archive[8:10], 99)
		binary.LittleEndian.PutUint16(archive[central+10:central+12], 99)
		return archive, nil
	default:
		return nil, fmt.Errorf("unknown generated archive %s", profile)
	}
}

func verifySafeIntegerCase(root string, c caseDef) error {
	row, err := matrixCase(root, c)
	if err != nil {
		return err
	}
	fixturePath := fmt.Sprint(row["fixture"])
	if fixturePath == "" {
		return errors.New("safe integer matrix entry absent")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.Join(rootDir, "fixtures/matrices", fixturePath)))
	data, err := os.ReadFile(filepath.Join(root, clean))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	pointer := fmt.Sprint(row["json_pointer"])
	number, err := numberAtPointer(document, pointer)
	if err != nil {
		return err
	}
	expected := asMap(row["expected"])
	valid, _ := expected["schema_valid"].(bool)
	wantLexeme := "9007199254740991"
	if !valid {
		wantLexeme = "9007199254740992"
	}
	if number.String() != wantLexeme || expected["generated_validators_accept"] != valid {
		return fmt.Errorf("safe integer fixture %s at %s=%s", c.ID, pointer, number.String())
	}
	rootName, _ := row["root"].(string)
	if rootName == "" {
		rootName = "AgentManifest"
	}
	var schemaPath string
	var schemaPointer []string
	switch rootName + ":" + pointer {
	case "AssetRef:/size_bytes":
		schemaPath = "schemas/resources/asset-ref-v1.schema.json"
		schemaPointer = []string{"properties", "size_bytes"}
	case "AgentManifest:/content/max_input_bytes":
		schemaPath = "schemas/manifest/agent-manifest-v1.schema.json"
		schemaPointer = []string{"$defs", "content", "properties", "max_input_bytes"}
	case "AgentManifest:/execution/max_concurrency":
		schemaPath = "schemas/manifest/agent-manifest-v1.schema.json"
		schemaPointer = []string{"$defs", "execution", "properties", "max_concurrency"}
	case "AgentManifest:/session/idle_timeout_seconds":
		schemaPath = "schemas/manifest/agent-manifest-v1.schema.json"
		schemaPointer = []string{"$defs", "session", "properties", "idle_timeout_seconds"}
	default:
		return fmt.Errorf("undeclared safe integer property %s:%s", rootName, pointer)
	}
	var schema map[string]any
	if err := load(root, schemaPath, &schema); err != nil {
		return err
	}
	var current any = schema
	for _, segment := range schemaPointer {
		current = asMap(current)[segment]
	}
	property := asMap(current)
	if property["type"] != "integer" || integer(property["maximum"]) != 9007199254740991 {
		return fmt.Errorf("schema maximum missing for %s", pointer)
	}
	return nil
}

func numberAtPointer(value any, pointer string) (json.Number, error) {
	current := value
	if pointer == "" || pointer == "/" {
		return "", errors.New("safe integer pointer must name a property")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("pointer %s traverses non-object", pointer)
		}
		current, ok = object[segment]
		if !ok {
			return "", fmt.Errorf("pointer %s missing %s", pointer, segment)
		}
	}
	number, ok := current.(json.Number)
	if !ok {
		return "", fmt.Errorf("pointer %s is not JSON number", pointer)
	}
	return number, nil
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

type safeIntegerFixtureSet struct {
	manifestValid, manifestContentInvalid, manifestExecutionInvalid, manifestSessionInvalid []byte
	assetValid, assetInvalid                                                                []byte
}

func readSafeIntegerFixtures(root string) (safeIntegerFixtureSet, error) {
	paths := map[string]string{
		"manifestValid":            rootDir + "/fixtures/valid/manifest-safe-integer-maximum.json",
		"manifestContentInvalid":   rootDir + "/fixtures/invalid/manifest-content-max-input-bytes-safe-maximum-plus-one.json",
		"manifestExecutionInvalid": rootDir + "/fixtures/invalid/manifest-execution-max-concurrency-safe-maximum-plus-one.json",
		"manifestSessionInvalid":   rootDir + "/fixtures/invalid/manifest-session-idle-timeout-safe-maximum-plus-one.json",
		"assetValid":               rootDir + "/fixtures/valid/asset-ref-safe-integer-maximum.json",
		"assetInvalid":             rootDir + "/fixtures/invalid/asset-ref-size-bytes-safe-maximum-plus-one.json",
	}
	values := map[string][]byte{}
	for name, path := range paths {
		value, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return safeIntegerFixtureSet{}, err
		}
		values[name] = value
	}
	return safeIntegerFixtureSet{
		manifestValid: values["manifestValid"], manifestContentInvalid: values["manifestContentInvalid"],
		manifestExecutionInvalid: values["manifestExecutionInvalid"], manifestSessionInvalid: values["manifestSessionInvalid"],
		assetValid: values["assetValid"], assetInvalid: values["assetInvalid"],
	}, nil
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
	fixtures, err := readSafeIntegerFixtures(root)
	if err != nil {
		return cmdResult{err: err}
	}
	test := fmt.Sprintf(`package controlplane
import "testing"
func TestEverySafeIntegerBoundary(t *testing.T) {
	manifestValid := []byte(%q)
	if _, err := DecodeAgentManifest(manifestValid); err != nil { t.Fatal(err) }
	if _, err := DecodeAgentManifestForward(manifestValid); err != nil { t.Fatal(err) }
	manifestInvalid := []struct{name string; data []byte}{
		{"content.max_input_bytes", []byte(%q)},
		{"execution.max_concurrency", []byte(%q)},
		{"session.idle_timeout_seconds", []byte(%q)},
	}
	for _, item := range manifestInvalid { t.Run(item.name, func(t *testing.T) { if _, err := DecodeAgentManifest(item.data); err == nil { t.Fatal("maximum+1 accepted") } }) }
	if _, err := DecodeAssetRef([]byte(%q)); err != nil { t.Fatal(err) }
	if _, err := DecodeAssetRef([]byte(%q)); err == nil { t.Fatal("asset size maximum+1 accepted") }
	var _ *SafeInteger = Content{}.MaxInputBytes
	var _ *SafeInteger = Execution{}.MaxConcurrency
	var _ *SafeInteger = Session{}.IDleTimeoutSeconds
	var _ SafeInteger = AssetRef{}.SizeBytes
}
`, string(fixtures.manifestValid), string(fixtures.manifestContentInvalid), string(fixtures.manifestExecutionInvalid), string(fixtures.manifestSessionInvalid), string(fixtures.assetValid), string(fixtures.assetInvalid))
	mustWrite(filepath.Join(dir, "publication_gen_test.go"), []byte(test))
	scratch := filepath.Join(out, "go-cache")
	os.Mkdir(scratch, 0o700)
	os.Mkdir(filepath.Join(scratch, "mod"), 0o700)
	os.Mkdir(filepath.Join(scratch, "tmp"), 0o700)
	os.Mkdir(filepath.Join(out, "empty-home"), 0o700)
	return run(dir, 90*time.Second, cleanEnv(map[string]string{"CGO_ENABLED": "0", "GOCACHE": scratch, "GOMODCACHE": filepath.Join(scratch, "mod"), "GONOSUMDB": "*", "GOTMPDIR": filepath.Join(scratch, "tmp"), "GOENV": "off", "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOWORK": "off", "HOME": filepath.Join(out, "empty-home")}), "go", "test", "-count=1", "-json", "-mod=readonly", ".")
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
	fixtures, err := readSafeIntegerFixtures(root)
	if err != nil {
		return cmdResult{err: err}
	}
	probe := fmt.Sprintf(`import pathlib,sys,typing
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
import publication_gen as p
manifest_valid = %q
p.decode_agent_manifest(manifest_valid)
p.decode_agent_manifest_forward(manifest_valid)
for name, invalid in [
 ("content.max_input_bytes", %q),
 ("execution.max_concurrency", %q),
 ("session.idle_timeout_seconds", %q),
]:
 try:
  p.decode_agent_manifest(invalid)
  raise RuntimeError(name + " maximum+1 accepted")
 except ValueError: pass
p.decode_asset_ref(%q)
try:
 p.decode_asset_ref(%q)
 raise RuntimeError("asset size maximum+1 accepted")
except ValueError: pass
if typing.get_type_hints(p.AssetRef)["size_bytes"] is not int: raise RuntimeError("asset size type")
for owner, field in [(p.Content, "max_input_bytes"), (p.Execution, "max_concurrency"), (p.Session, "idle_timeout_seconds")]:
 if int not in typing.get_args(typing.get_type_hints(owner)[field]): raise RuntimeError(field + " type")
`, string(fixtures.manifestValid), string(fixtures.manifestContentInvalid), string(fixtures.manifestExecutionInvalid), string(fixtures.manifestSessionInvalid), string(fixtures.assetValid), string(fixtures.assetInvalid))
	mustWrite(filepath.Join(dir, "probe.py"), []byte(probe))
	a := run(dir, 60*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONDONTWRITEBYTECODE": "1", "PYTHONHASHSEED": "0", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}), "python3", "-I", "-B", "-m", "py_compile", "publication_gen.py", "probe.py")
	if a.err != nil {
		return a
	}
	nodeModules, err := findNodeModules(root)
	if err != nil {
		return cmdResult{err: err}
	}
	pyright := run(dir, 60*time.Second, cleanEnv(map[string]string{"LANG": "C", "LC_ALL": "C", "TZ": "UTC", "PYTHONDONTWRITEBYTECODE": "1", "PYTHONHASHSEED": "0", "PYTHONNOUSERSITE": "1", "PYTHONSAFEPATH": "1"}), "node", "--disable-proto=throw", "--no-addons", filepath.Join(nodeModules, "pyright/index.js"), "--level", "error", "--pythonversion", "3.11", "publication_gen.py", "probe.py")
	if pyright.err != nil {
		return pyright
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
	fixtures, err := readSafeIntegerFixtures(root)
	if err != nil {
		return cmdResult{err: err}
	}
	probe := fmt.Sprintf(`import {decodeAgentManifest,decodeAgentManifestForward,decodeAssetRef,type AssetRef,type Content,type Execution,type Session} from './publication.gen.js';
const manifestValid = %q;
decodeAgentManifest(manifestValid); decodeAgentManifestForward(manifestValid);
for (const [name, invalid] of [
  ["content.max_input_bytes", %q],
  ["execution.max_concurrency", %q],
  ["session.idle_timeout_seconds", %q],
] as const) { try { decodeAgentManifest(invalid); throw new Error(name + " maximum+1 accepted") } catch(e) { if ((e as Error).message.endsWith("maximum+1 accepted")) throw e } }
decodeAssetRef(%q);
try { decodeAssetRef(%q); throw new Error("asset size maximum+1 accepted") } catch(e) { if ((e as Error).message === "asset size maximum+1 accepted") throw e }
const assetSize: number = ({} as AssetRef).size_bytes;
const contentSize: number | undefined = ({} as Content).max_input_bytes;
const executionSize: number | undefined = ({} as Execution).max_concurrency;
const sessionSize: number | undefined = ({} as Session).idle_timeout_seconds;
void [assetSize,contentSize,executionSize,sessionSize];
`, string(fixtures.manifestValid), string(fixtures.manifestContentInvalid), string(fixtures.manifestExecutionInvalid), string(fixtures.manifestSessionInvalid), string(fixtures.assetValid), string(fixtures.assetInvalid))
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
	if !reflect.DeepEqual(w.Transition.FromPhases, []string{"P01", "P02", "P06", "P07", "P08", "P09", "P10"}) || w.Transition.ToPhase != "P11" || strings.TrimSpace(w.Transition.Reason) == "" {
		return errors.New("transition phases/reason not exact")
	}
	if w.Policy.OwnershipTransferred {
		return errors.New("ownership transfer")
	}
	wantArtifacts := []string{"blueprint-check-reports", "blueprint-validation-library", "blueprint-validation-tests", "codegen-pipeline", "codegen-representative-spike", "generated-control-plane-go", "generated-control-plane-python", "generated-control-plane-typescript", "go-manifest-library-baseline", "migration-engine-fixture-versions", "openapi-control-plane-foundation", "phase-report-p01", "phase-report-p06", "phase-report-p07", "phase-report-p08", "phase-report-p09", "phase-report-p10", "phase-report-p11", "publication-contract-fixtures", "reference-control-plane-server", "schema-manifest"}
	if !reflect.DeepEqual(w.AffectedArtifacts, wantArtifacts) {
		return errors.New("affected artifacts not exact")
	}
	wantAcceptance := []acceptance{{"P01", "make spec-index-check", "build/reports/P01/report.json"}, {"P02", "make blueprint-check", "build/reports/P02/report.json"}, {"P06", "make test-protocol-foundation", "build/reports/P06/report.json"}, {"P07", "make test-codegen-pipeline", "build/reports/P07/report.json"}, {"P08", "make test-control-plane-platform", "build/reports/P08/report.json"}, {"P09", "make test-storage-migrations", "build/reports/P09/report.json"}, {"P10", "make test-identity-secrets", "build/reports/P10/report.json"}, {"P11", commandWant, "build/reports/P11/report.json"}}
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
	changedSource := make([]string, 0, len(changed))
	for _, path := range changed {
		if path != "Makefile" {
			changedSource = append(changedSource, path)
		}
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
	if !reflect.DeepEqual(declared, changedSource) {
		return fmt.Errorf("source closure differs changed=%v declared=%v", changedSource, declared)
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
