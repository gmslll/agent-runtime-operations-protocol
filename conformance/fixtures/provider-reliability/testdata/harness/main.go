// Command harness is the sole writer of the P21 Direct/Proxy Provider report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-direct-proxy-provider"
	checker  = "conformance/fixtures/provider-reliability/testdata/harness/main.go"
	waiver   = "conformance/fixtures/provider-reliability/testdata/transition/p21-baseline-transition-waiver.json"
	baseline = "be3f4e1e3a0b7ec0dc59774eec0813c8ed2cc993"
)

var requiredOwnedArtifacts = []string{
	"direct-proxy-delivery-service",
	"go-consumer-core",
	"go-provider-sdk",
	"openapi-agent-runtime",
	"provider-durable-store-contract",
	"provider-durable-store-port",
	"provider-reliability-fixtures",
	"reference-http-agent",
	"reference-provider-sqlite-adapter",
	"reference-provider-sqlite-migration",
}

type transition struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
		OwnershipTransferred bool   `json:"ownership_transferred"`
	} `json:"policy"`
	Baseline   struct{ Rule, Commit string } `json:"baseline"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string                                  `json:"affected_artifacts"`
	SourceClosure     []string                                  `json:"source_closure"`
	Acceptance        []struct{ Phase, Command, Report string } `json:"acceptance"`
	Constraints       []string                                  `json:"constraints"`
}

type discoveredTransition struct{ sources, artifacts []string }

type cases struct {
	SchemaVersion int      `json:"schema_version"`
	Provider      []string `json:"provider"`
	Consumer      []string `json:"consumer"`
	Security      []string `json:"security"`
}

type commandResult struct {
	output []byte
	err    error
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", command))
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, err := trackedInputs(root)
	add("p21-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	runtimeInputs, err := declaredRuntimeInputs(root)
	add("p21-runtime-inputs-empty", errors.Join(err, requireEmpty(runtimeInputs)), "manifest runtime_inputs is empty")
	add("p21-owned-artifact-inventory", validateOwnedArtifacts(root), "manifest declares the exact ten P21-owned artifacts")
	transitionErr := validateTransition(root)
	if os.Getenv("AROP_PRINT_P21_TRANSITION") == "1" {
		fatal(transitionErr)
		return
	}
	add("p21-transition-waiver", transitionErr, "waiver exactly binds Git changes, manifest owners, acceptance and constraints")
	caseEvidence, err := validateCases(root)
	add("p21-reliability-case-inventory", err, "provider, consumer and security case inventory is exact")
	contractEvidence, err := validateContracts(root)
	add("p21-runtime-and-store-contracts", err, "Agent Runtime OpenAPI and durable store contract are strict and contain no SSE")

	rootTests := run(root, nil, "go", "test", "-race", "-count=1", "./sdk/go/provider", "./sdk/go/consumer", "./reference/agents/go-http/...")
	add("p21-root-provider-consumer-reference-tests", rootTests.err, "Provider, Consumer and reference Agent tests pass under race detection")
	add("p21-root-test-terminals", rejectIncompleteTests(rootTests.output), "root test stream has no skip, cache or no-tests terminal")
	rootVet := run(root, nil, "go", "vet", "./sdk/go/provider", "./sdk/go/consumer", "./reference/agents/go-http/...")
	add("p21-root-provider-consumer-reference-vet", rootVet.err, "Provider, Consumer and reference Agent pass go vet")

	nestedTests, nestedVet := runNested(root)
	add("p21-control-plane-proxy-tests", nestedTests.err, "proxy delivery, dispatch reauthentication, composition and catalogs pass under race detection")
	add("p21-control-plane-test-terminals", rejectIncompleteTests(nestedTests.output), "Control Plane test stream has no skip, cache or no-tests terminal")
	add("p21-control-plane-proxy-vet", nestedVet.err, "proxy delivery and composition pass go vet")

	p20 := run(root, nil, "make", "test-event-ledger")
	if p20.err == nil {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/P20/report.json")
		p20.output = append(p20.output, verified.output...)
		p20.err = verified.err
	}
	add("p21-p20-regression", p20.err, "P20 and its predecessor chain pass on the current head")

	evidence := []report.RuntimeEvidence{
		{Kind: "p21-case-inventory", SHA256: report.Hash(caseEvidence), Bytes: int64(len(caseEvidence))},
		{Kind: "p21-contract-validation", SHA256: report.Hash(contractEvidence), Bytes: int64(len(contractEvidence))},
		{Kind: "p21-root-tests", SHA256: report.Hash(rootTests.output), Bytes: int64(len(rootTests.output))},
		{Kind: "p21-root-vet", SHA256: report.Hash(rootVet.output), Bytes: int64(len(rootVet.output))},
		{Kind: "p21-control-plane-tests", SHA256: report.Hash(nestedTests.output), Bytes: int64(len(nestedTests.output))},
		{Kind: "p21-control-plane-vet", SHA256: report.Hash(nestedVet.output), Bytes: int64(len(nestedVet.output))},
		{Kind: "p20-regression", SHA256: report.Hash(p20.output), Bytes: int64(len(p20.output))},
	}
	for _, phase := range []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15", "P16", "P17", "P18", "P19", "P20"} {
		verified := run(root, nil, "make", "verify-report", "REPORT=build/reports/"+phase+"/report.json")
		add("p21-"+strings.ToLower(phase)+"-report", verified.err, phase+" report is current and verified")
		evidence = append(evidence, report.RuntimeEvidence{Kind: strings.ToLower(phase) + "-report-verification", SHA256: report.Hash(verified.output), Bytes: int64(len(verified.output))})
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P21", Suite: "AROP P21 direct proxy provider", Class: "p21.provider",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 10, "provider_database_engines": 1, "runtime_inputs": len(runtimeInputs)},
		AuditNote: "P21 binds the exact ten manifest-owned artifacts and an independently discovered Git/manifest transition closure. The Go Provider exposes Direct/Proxy create, query and command without SSE, strictly validates ES256 tickets and persists Inbox, Outbox and effect state transactionally. The trusted Go Consumer separates Control Plane and Runtime credentials, obtains a fresh Attempt after expiry, rejects redirects without credential replay and revalidates DNS/IP on every connection. Proxy delivery reauthenticates the durable Attempt and ignores caller-selected endpoints. The public DurableStore remains driver-free; the reference HTTP Agent alone owns its exact SQLite migration, tamper detection and crash recovery. Prior phase reports enter only as digest-and-byte runtime evidence and runtime_inputs remains empty.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P21/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P21 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P21 checks failed; see build/reports/P21/report.json"))
	}
	fmt.Printf("AROP Direct/Proxy Provider passed: %d checks.\n", len(checks))
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	paths := []string{}
	for _, item := range bytes.Split(result.output, []byte{0}) {
		if len(item) != 0 {
			paths = append(paths, string(item))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func declaredRuntimeInputs(root string) ([]string, error) {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return nil, err
	}
	var found []string
	matches := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "phase-report-p21" {
			matches++
			found = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("phase-report-p21 entries=%d", matches)
	}
	return found, nil
}

func requireEmpty(items []string) error {
	if len(items) != 0 {
		return fmt.Errorf("runtime_inputs must be empty, got %q", items)
	}
	return nil
}

func validateOwnedArtifacts(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	found := []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P21" {
			found = append(found, artifact.ID)
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-direct-proxy-provider" {
				return fmt.Errorf("P21 artifact %s has invalid ownership metadata", artifact.ID)
			}
		}
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, requiredOwnedArtifacts) {
		return fmt.Errorf("P21 owned artifacts=%v want=%v", found, requiredOwnedArtifacts)
	}
	return nil
}

func validateCases(root string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(root, "conformance/fixtures/provider-reliability/cases.json"))
	if err != nil {
		return nil, err
	}
	var value cases
	if err := strictJSON(data, &value); err != nil {
		return nil, err
	}
	if value.SchemaVersion != 1 || len(value.Provider) != 9 || len(value.Consumer) != 6 || len(value.Security) != 5 || !unique(value.Provider) || !unique(value.Consumer) || !unique(value.Security) {
		return nil, errors.New("provider reliability inventory is incomplete or duplicated")
	}
	return data, nil
}

func validateContracts(root string) ([]byte, error) {
	paths := []string{"openapi/agent-runtime-v1.yaml", "conformance/contracts/provider-durable-store.yaml"}
	var combined []byte
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		var value any
		if err := structuredfile.Load(filepath.Join(root, path), &value); err != nil {
			return nil, err
		}
		combined = append(combined, data...)
		combined = append(combined, 0)
	}
	openapi := string(combined)
	for _, required := range []string{"/v1/runs:", "/v1/runs/{run_id}:", "/v1/runs/{run_id}/commands:", "RunBearer", "202", "DurableStore", "Inbox", "Outbox", "effect_id"} {
		if !strings.Contains(openapi, required) {
			return nil, fmt.Errorf("P21 contracts omit %q", required)
		}
	}
	for _, forbidden := range []string{"text/event-stream", "EventSource", "Server-Sent"} {
		if strings.Contains(openapi, forbidden) {
			return nil, fmt.Errorf("P21 contracts contain forbidden streaming surface %q", forbidden)
		}
	}
	return combined, nil
}

func validateTransition(root string) error {
	data, err := os.ReadFile(filepath.Join(root, waiver))
	if err != nil {
		return err
	}
	var value transition
	if err := strictJSON(data, &value); err != nil {
		return err
	}
	discovered, err := discoverTransition(root)
	if err != nil {
		return err
	}
	if os.Getenv("AROP_PRINT_P21_TRANSITION") == "1" {
		fmt.Printf("sources=%q\nartifacts=%q\n", discovered.sources, discovered.artifacts)
	}
	if err := validateTransitionCandidate(value, discovered); err != nil {
		return err
	}
	return validateTransitionNegatives(value, discovered)
}

func discoverTransition(root string) (discoveredTransition, error) {
	intro := run(root, nil, "git", "log", "--format=%H", "--diff-filter=A", "--", waiver)
	commits := strings.Fields(string(intro.output))
	if intro.err != nil || len(commits) != 1 {
		return discoveredTransition{}, errors.New("transition carrier must have one introduction commit")
	}
	parent := run(root, nil, "git", "rev-parse", commits[0]+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return discoveredTransition{}, errors.New("transition carrier introduction parent is not the frozen P20 endpoint")
	}
	diff := run(root, nil, "git", "diff", "--no-renames", "-z", "--name-status", baseline+"..HEAD")
	if diff.err != nil {
		return discoveredTransition{}, diff.err
	}
	sources, err := parseChangedSources(diff.output)
	if err != nil {
		return discoveredTransition{}, err
	}
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return discoveredTransition{}, err
	}
	artifacts, err := discoverArtifacts(root, manifest, sources)
	return discoveredTransition{sources: sources, artifacts: artifacts}, err
}

func parseChangedSources(data []byte) ([]string, error) {
	fields, paths := bytes.Split(data, []byte{0}), map[string]bool{}
	for index := 0; index < len(fields) && len(fields[index]) != 0; {
		status := string(fields[index])
		index++
		if status == "" || !strings.Contains("ACMRTD", status[:1]) {
			return nil, fmt.Errorf("unsupported Git status %q", status)
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if index+count > len(fields) {
			return nil, errors.New("truncated Git name-status stream")
		}
		for range count {
			path := string(fields[index])
			index++
			if path != "" && !strings.HasPrefix(path, "build/reports/") {
				paths[path] = true
			}
		}
	}
	result := keys(paths)
	return result, nil
}

func discoverArtifacts(root string, manifest blueprint.Manifest, sources []string) ([]string, error) {
	found, byID := map[string]bool{}, map[string]blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	for _, source := range sources {
		if source == "Makefile" {
			for _, artifact := range manifest.Artifacts {
				phase := artifact.OwnerPhase
				if phase == "" {
					phase = artifact.ProducerPhase
				}
				if artifact.PathRole == "concrete" && artifact.AcceptanceTest == "make-test-direct-proxy-provider" && phaseNumber(phase) <= 21 {
					found[artifact.ID] = true
				}
			}
			continue
		}
		best, matches := -1, []blueprint.Artifact{}
		for _, artifact := range manifest.Artifacts {
			phase := artifact.OwnerPhase
			if phase == "" {
				phase = artifact.ProducerPhase
			}
			if artifact.PathRole != "concrete" || artifact.Path == "" || phaseNumber(phase) > 21 {
				continue
			}
			score := artifactScore(root, artifact.Path, source)
			if score > best {
				best, matches = score, []blueprint.Artifact{artifact}
			} else if score >= 0 && score == best {
				matches = append(matches, artifact)
			}
		}
		if best < 0 || len(matches) == 0 {
			return nil, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			found[artifact.ID] = true
		}
	}
	for {
		added := false
		for _, artifact := range manifest.Artifacts {
			phase := artifact.OwnerPhase
			if phase == "" {
				phase = artifact.ProducerPhase
			}
			if artifact.PathRole != "concrete" || phaseNumber(phase) > 21 || found[artifact.ID] {
				continue
			}
			for id := range found {
				if depends(artifact.ID, id, byID, map[string]bool{}) {
					found[artifact.ID], added = true, true
					break
				}
			}
		}
		if !added {
			break
		}
	}
	for _, id := range append(append([]string{}, requiredOwnedArtifacts...), "phase-report-p21") {
		if !found[id] {
			return nil, fmt.Errorf("artifact discovery omitted required %s", id)
		}
	}
	return keys(found), nil
}

func artifactScore(root, artifactPath, source string) int {
	artifactPath = filepath.ToSlash(filepath.Clean(artifactPath))
	if artifactPath == source {
		return 3*len(artifactPath) + 2
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifactPath))); err == nil && info.IsDir() && strings.HasPrefix(source, artifactPath+"/") {
		return 3*len(artifactPath) + 1
	}
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(artifactPath)))
	if directory != "." && (filepath.ToSlash(filepath.Dir(filepath.FromSlash(source))) == directory || strings.HasPrefix(source, directory+"/")) {
		return 2 * len(directory)
	}
	if source == "reference/control-plane/go.sum" && artifactPath == "reference/control-plane/go.mod" {
		return 1
	}
	return -1
}

func depends(candidate, target string, byID map[string]blueprint.Artifact, visiting map[string]bool) bool {
	if candidate == target {
		return true
	}
	if visiting[candidate] {
		return false
	}
	visiting[candidate] = true
	defer delete(visiting, candidate)
	for _, dependency := range byID[candidate].DerivesFrom {
		if depends(dependency, target, byID, visiting) {
			return true
		}
	}
	return false
}

func validateTransitionCandidate(value transition, discovered discoveredTransition) error {
	wantFrom := []string{"P01", "P02", "P05", "P06", "P07", "P08", "P09", "P10", "P11", "P12", "P13", "P14", "P15", "P16", "P17", "P18", "P19", "P20"}
	if value.SchemaVersion != 1 || value.WaiverID != "P21-DIRECT-PROXY-PROVIDER-TRANSITION-001" || value.Status != "validated" || value.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || value.Policy.OwnershipTransferred || value.Baseline.Rule != "parent-of-unique-waiver-introduction-commit" || value.Baseline.Commit != baseline || value.Transition.ToPhase != "P21" || strings.TrimSpace(value.Transition.Reason) == "" || !reflect.DeepEqual(value.Transition.FromPhases, wantFrom) {
		return errors.New("transition identity, baseline or ownership is invalid")
	}
	if !reflect.DeepEqual(value.SourceClosure, discovered.sources) || !reflect.DeepEqual(value.AffectedArtifacts, discovered.artifacts) {
		return fmt.Errorf("transition exact closure mismatch: sources=%q artifacts=%q", discovered.sources, discovered.artifacts)
	}
	if !reflect.DeepEqual(value.Acceptance, canonicalAcceptance()) || !reflect.DeepEqual(value.Constraints, canonicalConstraints()) {
		return errors.New("transition acceptance or constraints mismatch")
	}
	return nil
}

func canonicalAcceptance() []struct{ Phase, Command, Report string } {
	commands := []struct{ phase, command string }{
		{"P01", "make spec-index-check"}, {"P02", "make blueprint-check"}, {"P05", "make test-go-workspace"}, {"P06", "make test-protocol-foundation"}, {"P07", "make test-codegen-pipeline"}, {"P08", "make test-control-plane-platform"}, {"P09", "make test-storage-migrations"}, {"P10", "make test-identity-secrets"}, {"P11", "make test-publication-contracts"}, {"P12", "make test-publication-service"}, {"P13", "make test-asset-broker"}, {"P14", "make test-registry-core"}, {"P15", "make test-registry-api"}, {"P16", "make test-registry-recovery"}, {"P17", "make verify-registry"}, {"P18", "make test-run-lifecycle"}, {"P19", "make test-dispatch-ticket"}, {"P20", "make test-event-ledger"}, {"P21", command},
	}
	result := make([]struct{ Phase, Command, Report string }, 0, len(commands))
	for _, item := range commands {
		result = append(result, struct{ Phase, Command, Report string }{item.phase, item.command, "build/reports/" + item.phase + "/report.json"})
	}
	return result
}

func canonicalConstraints() []string {
	return []string{
		"browser-callers-use-bff-or-proxy-and-never-receive-run-token",
		"consumer-separates-control-plane-and-runtime-credentials-and-never-logs-or-persists-them",
		"ticket-expiry-requires-one-fresh-control-plane-redispatch-key-and-never-reuses-the-expired-attempt",
		"direct-and-proxy-use-only-the-control-plane-selected-durable-attempt-endpoint",
		"every-network-connection-revalidates-all-dns-addresses-and-redirects-are-rejected-without-credential-replay",
		"provider-validates-es256-issuer-audience-subject-authorized-party-scope-run-attempt-deployment-instance-generation-fencing-profile-endpoint-and-time",
		"provider-inbox-outbox-effect-state-and-terminal-result-are-transactional-and-replay-safe",
		"started-but-not-completed-write-or-irreversible-effects-remain-uncertain-and-are-never-blindly-repeated",
		"cancel-deadline-trace-usage-and-first-terminal-semantics-are-durable",
		"public-go-provider-sdk-remains-database-driver-free",
		"reference-agent-alone-owns-sqlite-driver-migration-clean-oracle-and-crash-recovery",
		"agent-runtime-exposes-json-create-query-command-and-no-sse-route",
		"owner-phase-accountability-does-not-transfer",
		"p21-report-runtime-inputs-remain-empty",
	}
}

func validateTransitionNegatives(valid transition, discovered discoveredTransition) error {
	type mutation struct {
		name string
		fn   func(*transition)
	}
	mutations := []mutation{}
	appendGroup := func(name string, length int, omit, substitute func(*transition, int), add, duplicate func(*transition)) {
		for index := 0; index < length; index++ {
			i := index
			mutations = append(mutations, mutation{name + "-omission", func(value *transition) { omit(value, i) }}, mutation{name + "-substitution", func(value *transition) { substitute(value, i) }})
		}
		mutations = append(mutations, mutation{name + "-addition", add}, mutation{name + "-duplicate", duplicate})
	}
	appendGroup("source", len(valid.SourceClosure), func(v *transition, i int) { v.SourceClosure = append(v.SourceClosure[:i:i], v.SourceClosure[i+1:]...) }, func(v *transition, i int) { v.SourceClosure[i] = "unexpected/source" }, func(v *transition) { v.SourceClosure = append(v.SourceClosure, "unexpected/source") }, func(v *transition) { v.SourceClosure = append(v.SourceClosure, v.SourceClosure[0]) })
	appendGroup("artifact", len(valid.AffectedArtifacts), func(v *transition, i int) {
		v.AffectedArtifacts = append(v.AffectedArtifacts[:i:i], v.AffectedArtifacts[i+1:]...)
	}, func(v *transition, i int) { v.AffectedArtifacts[i] = "unexpected-artifact" }, func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, "unexpected-artifact") }, func(v *transition) { v.AffectedArtifacts = append(v.AffectedArtifacts, v.AffectedArtifacts[0]) })
	appendGroup("acceptance", len(valid.Acceptance), func(v *transition, i int) { v.Acceptance = append(v.Acceptance[:i:i], v.Acceptance[i+1:]...) }, func(v *transition, i int) { v.Acceptance[i].Command = "make unexpected" }, func(v *transition) {
		v.Acceptance = append(v.Acceptance, struct{ Phase, Command, Report string }{"P99", "make unexpected", "build/reports/P99/report.json"})
	}, func(v *transition) { v.Acceptance = append(v.Acceptance, v.Acceptance[0]) })
	appendGroup("constraint", len(valid.Constraints), func(v *transition, i int) { v.Constraints = append(v.Constraints[:i:i], v.Constraints[i+1:]...) }, func(v *transition, i int) { v.Constraints[i] = "unexpected-constraint" }, func(v *transition) { v.Constraints = append(v.Constraints, "unexpected-constraint") }, func(v *transition) { v.Constraints = append(v.Constraints, v.Constraints[0]) })
	for _, item := range mutations {
		copy := cloneTransition(valid)
		item.fn(&copy)
		if validateTransitionCandidate(copy, discovered) == nil {
			return fmt.Errorf("transition negative %s accepted", item.name)
		}
	}
	return nil
}

func cloneTransition(value transition) transition {
	data, _ := json.Marshal(value)
	var clone transition
	_ = json.Unmarshal(data, &clone)
	return clone
}

func runNested(root string) (commandResult, commandResult) {
	temporary, err := os.MkdirTemp("/tmp", "arop-p21-work-")
	if err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	defer os.RemoveAll(temporary)
	if err = os.Chmod(temporary, 0o700); err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	work := filepath.Join(temporary, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n"
	if err = os.WriteFile(work, []byte(body), 0o600); err != nil {
		return commandResult{err: err}, commandResult{err: err}
	}
	overrides := map[string]string{"GOWORK": work, "TMPDIR": temporary}
	directory := filepath.Join(root, "reference/control-plane")
	packages := []string{"./internal/app/delivery", "./internal/domain/dispatch/...", "./internal/app/platform/httpadapter", "./internal/storage/migrate", "./cmd/aropd"}
	testArgs := append([]string{"test", "-race", "-count=1"}, packages...)
	vetArgs := append([]string{"vet"}, packages...)
	return run(directory, overrides, "go", testArgs...), run(directory, overrides, "go", vetArgs...)
}

func rejectIncompleteTests(output []byte) error {
	text := string(output)
	for _, forbidden := range []string{"--- SKIP:", "\t[no test files]", "(cached)", "FAIL\t", "--- FAIL:"} {
		if strings.Contains(text, forbidden) {
			return fmt.Errorf("test output contains forbidden terminal %q", forbidden)
		}
	}
	return nil
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func unique(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func phaseNumber(phase string) int {
	if len(phase) != 3 || phase[0] != 'P' {
		return 1000
	}
	value, err := strconv.Atoi(phase[1:])
	if err != nil {
		return 1000
	}
	return value
}

func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	cmd := exec.Command(name, args...)
	cmd.Dir = directory
	cmd.Env = cleanEnvironment(overrides)
	output, err := cmd.CombinedOutput()
	return commandResult{output: output, err: commandError(name, args, output, err)}
}

func cleanEnvironment(overrides map[string]string) []string {
	banned := []string{"GOFLAGS=", "GOENV=", "GOWORK=", "GOCACHE=", "GOCACHEPROG=", "GOMODCACHE=", "GOTMPDIR=", "GOROOT=", "GOTOOLCHAIN=", "GOEXPERIMENT=", "CGO_ENABLED=", "NODE_OPTIONS=", "NODE_PATH=", "NPM_CONFIG_NODE_OPTIONS=", "PYTHONHOME=", "PYTHONPATH=", "PGHOST=", "PGPORT=", "PGPASSWORD=", "PGPASSFILE="}
	result := []string{}
	for _, entry := range os.Environ() {
		reject := false
		for _, prefix := range banned {
			if strings.HasPrefix(entry, prefix) {
				reject = true
				break
			}
		}
		if !reject {
			result = append(result, entry)
		}
	}
	defaults := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}
	for key, value := range overrides {
		defaults[key] = value
	}
	for key, value := range defaults {
		result = append(result, key+"="+value)
	}
	if _, present := defaults["PATH"]; !present {
		for index, entry := range result {
			if strings.HasPrefix(entry, "PATH=") && !strings.Contains(entry, "/opt/homebrew/opt/postgresql@16/bin") {
				result[index] = "PATH=/opt/homebrew/opt/postgresql@16/bin:" + strings.TrimPrefix(entry, "PATH=")
				break
			}
		}
	}
	return result
}

func commandError(name string, args []string, output []byte, err error) error {
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(output))
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(text) > 6000 {
		text = text[:6000]
	}
	return text
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
