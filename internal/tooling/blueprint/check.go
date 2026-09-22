// Package blueprint validates the schedulable implementation graph and the
// repository-wide implementation-runtime boundary.
package blueprint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	parse "github.com/tdewolff/parse/v2"
	js "github.com/tdewolff/parse/v2/js"
)

type Artifact struct {
	ID                    string   `json:"id" yaml:"id"`
	Path                  string   `json:"path" yaml:"path"`
	Status                string   `json:"status" yaml:"status"`
	Kind                  string   `json:"kind" yaml:"kind"`
	Authority             string   `json:"authority" yaml:"authority"`
	Language              string   `json:"language" yaml:"language"`
	Capabilities          []string `json:"capabilities" yaml:"capabilities"`
	Owner                 string   `json:"owner" yaml:"owner"`
	OwnerPhase            string   `json:"owner_phase" yaml:"owner_phase"`
	CompletionPhase       string   `json:"completion_phase" yaml:"completion_phase"`
	ProducerPhase         string   `json:"producer_phase" yaml:"producer_phase"`
	AcceptanceTest        string   `json:"acceptance_test" yaml:"acceptance_test"`
	Exposure              string   `json:"exposure" yaml:"exposure"`
	PathRole              string   `json:"path_role" yaml:"path_role"`
	FutureAction          string   `json:"future_action" yaml:"future_action"`
	FutureOwner           string   `json:"future_owner" yaml:"future_owner"`
	FutureOwnerPhase      string   `json:"future_owner_phase" yaml:"future_owner_phase"`
	FutureAcceptanceTest  string   `json:"future_acceptance_test" yaml:"future_acceptance_test"`
	ImplementationRuntime string   `json:"implementation_runtime" yaml:"implementation_runtime"`
	ToolScope             string   `json:"tool_scope" yaml:"tool_scope"`
	DerivesFrom           []string `json:"derives_from" yaml:"derives_from"`
	RuntimeInputs         []string `json:"runtime_inputs" yaml:"runtime_inputs"`
	FutureArtifacts       []string `json:"future_artifacts" yaml:"future_artifacts"`
}
type Manifest struct {
	SchemaVersion  int               `json:"schema_version" yaml:"schema_version"`
	CatalogID      string            `json:"catalog_id" yaml:"catalog_id"`
	Updated        string            `json:"updated" yaml:"updated"`
	Purpose        string            `json:"purpose" yaml:"purpose"`
	AuthorityChain []map[string]any  `json:"authority_chain" yaml:"authority_chain"`
	Statuses       map[string]string `json:"statuses" yaml:"statuses"`
	Artifacts      []Artifact        `json:"artifacts" yaml:"artifacts"`
}
type Requirements struct {
	SchemaVersion             int    `json:"schema_version" yaml:"schema_version"`
	Updated                   string `json:"updated" yaml:"updated"`
	Source                    string `json:"source" yaml:"source"`
	ImmutabilityRule          string `json:"immutability_rule" yaml:"immutability_rule"`
	StatementsDigestAlgorithm string `json:"statements_digest_algorithm" yaml:"statements_digest_algorithm"`
	StatementsDigest          string `json:"statements_digest" yaml:"statements_digest"`
	Requirements              []struct {
		ID                  string   `json:"id" yaml:"id"`
		StatementOriginalZH string   `json:"statement_original_zh" yaml:"statement_original_zh"`
		TranslationEN       string   `json:"translation_en" yaml:"translation_en"`
		Artifacts           []string `json:"artifacts" yaml:"artifacts"`
		Phases              []string `json:"phases" yaml:"phases"`
		Tests               []string `json:"tests" yaml:"tests"`
	} `json:"requirements" yaml:"requirements"`
}
type Baseline struct{ ID, Action, Phase string }
type Phase struct {
	ID, Title, Body, Type, Status, Owner, Components, Owned, Dependencies, Acceptance string
	Baselines                                                                         []Baseline
}

var phaseHeader = regexp.MustCompile(`(?m)^## (P[0-9]{2}) — ([^\n]+)$`)

func meta(body, key string) string {
	r := regexp.MustCompile(`(?m)^- \*\*` + regexp.QuoteMeta(key) + `:\*\* (.+)$`)
	m := r.FindStringSubmatch(body)
	if len(m) == 0 {
		return ""
	}
	return strings.TrimSpace(m[1])
}
func parsePhases(text string) []Phase {
	matches := phaseHeader.FindAllStringSubmatchIndex(text, -1)
	out := []Phase{}
	for i, m := range matches {
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := text[m[1]:end]
		p := Phase{ID: text[m[2]:m[3]], Title: text[m[4]:m[5]], Body: body}
		p.Type = meta(body, "Type")
		p.Status = meta(body, "Status")
		p.Owner = meta(body, "Capability owner")
		p.Components = meta(body, "Components")
		p.Owned = meta(body, "Artifacts owned")
		p.Dependencies = meta(body, "Dependencies")
		p.Acceptance = meta(body, "Machine acceptance")
		raw := meta(body, "Baselines transitioned")
		if raw != "" && raw != "none" {
			for _, v := range strings.Split(raw, ",") {
				v = strings.TrimSpace(v)
				open := strings.Index(v, "(")
				if open > 0 && strings.HasSuffix(v, ")") {
					p.Baselines = append(p.Baselines, Baseline{v[:open], v[open+1 : len(v)-1], p.ID})
				} else {
					p.Baselines = append(p.Baselines, Baseline{v, "invalid", p.ID})
				}
			}
		}
		out = append(out, p)
	}
	return out
}
func list(raw string) []string {
	if raw == "" || raw == "none" {
		return nil
	}
	v := strings.Split(raw, ",")
	for i := range v {
		v[i] = strings.TrimSpace(v[i])
	}
	return v
}
func phaseNum(id string) int { n, _ := strconv.Atoi(strings.TrimPrefix(id, "P")); return n }
func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
func Run(root string) ([]report.Check, map[string]any, []string) {
	checks := []report.Check{}
	errors := []string{}
	record := func(name string, ok bool, detail string) {
		checks = append(checks, report.Check{name, ok, detail})
		if !ok {
			errors = append(errors, name+": "+detail)
		}
	}
	read := func(path string) string {
		b, e := os.ReadFile(filepath.Join(root, path))
		if e != nil {
			record("read:"+path, false, e.Error())
			return ""
		}
		return string(b)
	}
	plan := read("docs/DEVELOPMENT_PLAN.md")
	layout := read("docs/DIRECTORY_STRUCTURE.md")
	blue := read("docs/IMPLEMENTATION_BLUEPRINT.md")
	architecture := read("docs/ARCHITECTURE.md")
	decisions := read("docs/DECISIONS.md")
	makefile := read("Makefile")
	packageJSON := read("package.json")
	var manifest Manifest
	record("artifact-manifest-load", structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest) == nil, fmt.Sprintf("%d artifacts loaded", len(manifest.Artifacts)))
	var req Requirements
	_ = structuredfile.Load(filepath.Join(root, "spec/requirements.yaml"), &req)
	phases := parsePhases(plan)
	byPhase := map[string]Phase{}
	ownerPhase := map[string]string{}
	planOwner := map[string]string{}
	planBaseline := map[string]Baseline{}
	metadata := []string{}
	allowedTypes := map[string]bool{"implement": true, "refactor": true, "verify · review": true, "verify · spec": true, "gate": true, "deliver": true}
	componentWhitelist := map[string]bool{"governance": true, "planning": true, "repository": true, "protocol": true, "codegen": true, "control-plane": true, "operations": true, "storage": true, "identity": true, "publication": true, "assets": true, "registry": true, "sdk-go": true, "run": true, "dispatch": true, "events": true, "delivery": true, "streaming": true, "worker": true, "sdk-python": true, "sdk-typescript": true, "interop": true, "conformance": true, "fault-ha": true, "quickstart": true, "deployment": true, "release": true}
	acceptanceByPhase := map[string][]string{}
	implementationOwners := map[string]bool{}
	for i, p := range phases {
		byPhase[p.ID] = p
		if p.ID != fmt.Sprintf("P%02d", i+1) {
			metadata = append(metadata, "non-continuous "+p.ID)
		}
		if !allowedTypes[p.Type] {
			metadata = append(metadata, p.ID+" illegal type "+p.Type)
		}
		for _, v := range []struct{ k, s string }{{"Status", p.Status}, {"Capability owner", p.Owner}, {"Components", p.Components}, {"Goal", meta(p.Body, "Goal")}, {"Scope", meta(p.Body, "Scope")}, {"Dependencies", p.Dependencies}, {"First-path invariants", meta(p.Body, "First-path invariants")}, {"Machine acceptance", p.Acceptance}, {"Rollback point", meta(p.Body, "Rollback point")}, {"Definition of done", meta(p.Body, "Definition of done")}} {
			if v.s == "" {
				metadata = append(metadata, p.ID+" missing "+v.k)
			}
		}
		if (p.Type == "implement" || p.Type == "refactor") && implementationOwners[p.Owner] {
			metadata = append(metadata, "duplicate implement owner "+p.Owner)
		}
		if p.Type == "implement" || p.Type == "refactor" {
			implementationOwners[p.Owner] = true
		}
		ownerPhase[p.Owner] = p.ID
		components := list(p.Components)
		if len(components) == 0 {
			metadata = append(metadata, p.ID+" has no component")
		}
		for _, component := range components {
			if !componentWhitelist[component] {
				metadata = append(metadata, p.ID+" invalid component "+component)
			}
		}
		owned := list(p.Owned)
		if (p.Type == "implement" || p.Type == "refactor") && len(owned) == 0 {
			metadata = append(metadata, p.ID+" owns no artifacts")
		}
		if p.Type != "implement" && p.Type != "refactor" && len(owned) > 0 {
			metadata = append(metadata, p.ID+" non-implementation owns artifacts")
		}
		for _, id := range owned {
			if planOwner[id] != "" {
				metadata = append(metadata, id+" plan-owned twice")
			}
			planOwner[id] = p.ID
		}
		for _, b := range p.Baselines {
			if b.Action != "migrate-retire" && b.Action != "repair-retain" && b.Action != "extend-retain" {
				metadata = append(metadata, p.ID+" invalid baseline "+b.ID)
			}
			if _, ok := planBaseline[b.ID]; ok {
				metadata = append(metadata, b.ID+" baseline twice")
			}
			planBaseline[b.ID] = b
		}
		commands := makeCommands(p.Acceptance)
		acceptanceByPhase[p.ID] = commands
		if len(commands) != 1 {
			metadata = append(metadata, p.ID+" needs exactly one Make command")
		}
		if !strings.Contains(p.Acceptance, "build/reports/"+p.ID+"/report.json") || !strings.Contains(p.Acceptance, "junit.xml") {
			metadata = append(metadata, p.ID+" missing report paths")
		}
	}
	sequenceOK := len(phases) == 53 && len(phases) > 0 && phases[len(phases)-1].Owner == "v1-delivery"
	for i, p := range phases {
		sequenceOK = sequenceOK && p.ID == fmt.Sprintf("P%02d", i+1)
	}
	record("dynamic-phase-sequence", sequenceOK, fmt.Sprintf("%d phases ending with %s", len(phases), func() string {
		if len(phases) == 0 {
			return "none"
		}
		return phases[len(phases)-1].Owner
	}()))
	record("phase-metadata-and-reports", len(metadata) == 0, strings.Join(metadata, "; "))
	depProblems := []string{}
	deps := map[string][]string{}
	phaseRef := regexp.MustCompile(`P[0-9]{2}`)
	for _, p := range phases {
		d := phaseRef.FindAllString(p.Dependencies, -1)
		if p.Dependencies == "none" {
			d = nil
		} else if len(d) == 0 {
			depProblems = append(depProblems, p.ID+" has unparseable dependencies")
		}
		for _, x := range d {
			if byPhase[x].ID == "" || phaseNum(x) >= phaseNum(p.ID) {
				depProblems = append(depProblems, p.ID+" invalid dependency "+x)
			}
		}
		deps[p.ID] = d
	}
	state := map[string]int{}
	var visitPhase func(string)
	visitPhase = func(id string) {
		if state[id] == 1 {
			depProblems = append(depProblems, "cycle at "+id)
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		for _, d := range deps[id] {
			visitPhase(d)
		}
		state[id] = 2
	}
	for _, p := range phases {
		visitPhase(p.ID)
	}
	record("phase-dependency-dag", len(depProblems) == 0, strings.Join(depProblems, "; "))
	byID := map[string]Artifact{}
	artifactProblems := []string{}
	pathOwner := map[string]string{}
	for _, a := range manifest.Artifacts {
		if byID[a.ID].ID != "" {
			artifactProblems = append(artifactProblems, "duplicate id "+a.ID)
		}
		byID[a.ID] = a
		if prior := pathOwner[a.Path]; prior != "" && prior != a.ID {
			artifactProblems = append(artifactProblems, "duplicate path "+a.Path)
		}
		pathOwner[a.Path] = a.ID
		if a.Status == "present" {
			if _, e := os.Lstat(filepath.Join(root, a.Path)); e != nil {
				artifactProblems = append(artifactProblems, "present path missing "+a.ID)
			}
		}
		roles := 0
		if a.OwnerPhase != "" {
			roles++
		}
		if a.CompletionPhase != "" {
			roles++
		}
		if a.ProducerPhase != "" {
			roles++
		}
		if a.Status == "planned" && roles != 1 {
			artifactProblems = append(artifactProblems, a.ID+" planned lifecycle roles !=1")
		}
		for _, lifecycle := range []string{a.OwnerPhase, a.CompletionPhase, a.ProducerPhase} {
			if lifecycle != "" && byPhase[lifecycle].ID == "" {
				artifactProblems = append(artifactProblems, a.ID+" unknown lifecycle phase "+lifecycle)
			}
		}
		if a.OwnerPhase != "" {
			if a.Owner == "" || a.AcceptanceTest == "" || a.Exposure == "" || a.PathRole != "concrete" {
				artifactProblems = append(artifactProblems, a.ID+" invalid concrete source metadata")
			}
			if planOwner[a.ID] != a.OwnerPhase {
				artifactProblems = append(artifactProblems, a.ID+" plan owner mismatch")
			}
			if byPhase[a.OwnerPhase].Owner != a.Owner {
				artifactProblems = append(artifactProblems, a.ID+" capability owner mismatch")
			}
			if len(acceptanceByPhase[a.OwnerPhase]) != 1 || acceptanceByPhase[a.OwnerPhase][0] != a.AcceptanceTest {
				artifactProblems = append(artifactProblems, a.ID+" acceptance mismatch")
			}
		}
		if a.CompletionPhase != "" && a.PathRole != "aggregate" && a.PathRole != "container" {
			artifactProblems = append(artifactProblems, a.ID+" completion is not aggregate/container")
		}
		if a.ProducerPhase != "" && !map[string]bool{"machine-reports": true, "canonical-evidence-summary": true, "detached-evidence-summary": true, "detached-evidence-bundle": true}[a.Kind] {
			artifactProblems = append(artifactProblems, a.ID+" invalid producer kind")
		}
		hasFuture := a.FutureAction != "" || a.FutureOwner != "" || a.FutureOwnerPhase != "" || a.FutureAcceptanceTest != "" || a.FutureArtifacts != nil
		if hasFuture {
			if a.Status != "present" || a.FutureAction == "" || a.FutureOwner == "" || a.FutureOwnerPhase == "" || a.FutureAcceptanceTest == "" || a.FutureArtifacts == nil {
				artifactProblems = append(artifactProblems, a.ID+" future transition requires all future_* fields on a present artifact")
			}
			b, ok := planBaseline[a.ID]
			futurePhase := byPhase[a.FutureOwnerPhase]
			if futurePhase.ID == "" || (futurePhase.Type != "implement" && futurePhase.Type != "refactor") || !ok || b.Action != a.FutureAction || b.Phase != a.FutureOwnerPhase || a.FutureOwner != futurePhase.Owner || len(acceptanceByPhase[a.FutureOwnerPhase]) != 1 || acceptanceByPhase[a.FutureOwnerPhase][0] != a.FutureAcceptanceTest {
				artifactProblems = append(artifactProblems, a.ID+" future baseline mismatch")
			}
			for _, future := range a.FutureArtifacts {
				if future == "" {
					artifactProblems = append(artifactProblems, a.ID+" has empty future artifact")
				}
			}
		} else if a.Status == "present" && strings.HasSuffix(a.Kind, "baseline") {
			artifactProblems = append(artifactProblems, a.ID+" present baseline has no future transition")
		}
		for _, d := range append(append([]string{}, a.DerivesFrom...), a.RuntimeInputs...) {
			if d == a.ID || byID[d].ID == "" { /* checked after full map below */
			}
		}
	}
	for id, phase := range planOwner {
		a := byID[id]
		if a.ID == "" || a.OwnerPhase != phase {
			artifactProblems = append(artifactProblems, id+" missing manifest owner")
		}
	}
	for id, b := range planBaseline {
		a := byID[id]
		if a.ID == "" || a.FutureOwnerPhase == "" || a.FutureAction != b.Action {
			artifactProblems = append(artifactProblems, id+" missing future owner")
		}
	}
	for _, a := range manifest.Artifacts {
		for _, d := range append(append([]string{}, a.DerivesFrom...), a.RuntimeInputs...) {
			if byID[d].ID == "" {
				artifactProblems = append(artifactProblems, a.ID+" unknown input "+d)
			}
			if d == a.ID {
				artifactProblems = append(artifactProblems, a.ID+" self dependency")
			}
		}
		for _, d := range a.DerivesFrom {
			if dependency := byID[d]; dependency.ID != "" && availability(dependency) > availability(a) {
				artifactProblems = append(artifactProblems, fmt.Sprintf("%s@%d depends on future %s@%d", a.ID, availability(a), d, availability(dependency)))
			}
		}
		if a.CompletionPhase != "" {
			for _, d := range a.DerivesFrom {
				if dependency := byID[d]; dependency.ID != "" && availability(dependency) > phaseNum(a.CompletionPhase) {
					artifactProblems = append(artifactProblems, a.ID+" completes before "+d)
				}
			}
		}
		for _, future := range a.FutureArtifacts {
			if byID[future].ID == "" {
				artifactProblems = append(artifactProblems, a.ID+" unknown future artifact "+future)
			}
		}
	}
	record("artifact-lifecycle-temporal-bidirectional", len(artifactProblems) == 0, fmt.Sprintf("%d artifacts; %s", len(manifest.Artifacts), strings.Join(artifactProblems, "; ")))
	dagProblems := graphProblems(manifest.Artifacts, byID, nil)
	record("artifact-runtime-combined-dag", len(dagProblems) == 0, strings.Join(dagProblems, "; "))
	reports := map[string]Artifact{}
	reportCounts := map[string]int{}
	for _, a := range manifest.Artifacts {
		if a.Kind == "machine-reports" {
			reports[a.ProducerPhase] = a
			reportCounts[a.ProducerPhase]++
		}
	}
	reportProblems := []string{}
	for _, p := range phases {
		r := reports[p.ID]
		if reportCounts[p.ID] != 1 || r.ID == "" || r.Path != "build/reports/"+p.ID || r.OwnerPhase != "" || r.CompletionPhase != "" || r.AcceptanceTest != "make-"+firstMake(p.Acceptance) {
			reportProblems = append(reportProblems, p.ID+" report missing/acceptance mismatch")
		}
	}
	record("all-phase-report-producers", len(reportProblems) == 0, strings.Join(reportProblems, "; "))
	closure := reportClosure(phases, manifest.Artifacts, byID, planOwner, reports)
	record("phase-report-input-closure", len(closure) == 0, strings.Join(closure, "; "))
	probes := reportNegativeProbes(phases, byID, planOwner, reports)
	record("report-closure-negative-probes", len(probes) == 0, strings.Join(probes, "; "))
	boundary := runtimeBoundary(root, manifest.Artifacts, byID, makefile, packageJSON)
	record("implementation-runtime-boundary", len(boundary) == 0, strings.Join(boundary, "; "))
	boundaryProbes := boundaryNegativeProbes()
	record("implementation-runtime-negative-probes", len(boundaryProbes) == 0, strings.Join(boundaryProbes, "; "))
	treeNeedles := []string{"<!-- blueprint-target-tree:v1 -->", "openapi/fragments/control-plane/", "openapi/control-plane-v1.yaml", "sdk/go/generated/", "sdk/python/src/arop/", "sdk/typescript/", "cmd/arop-conformance/", "conformance/", "reference/control-plane/", "internal/domain/", "internal/ports/", "migrations/sqlite/", "migrations/postgres/", "reference/agents/go-http/", "deployments/quickstart/", "deployments/production-reference/", ".github/workflows/", "internal/tooling/"}
	missing := []string{}
	for _, n := range treeNeedles {
		if !strings.Contains(layout, n) {
			missing = append(missing, n)
		}
	}
	record("target-tree-declaration", len(missing) == 0, strings.Join(missing, ", "))
	for _, check := range parityChecks(root, plan, layout, blue, architecture, decisions, makefile, read("README.md"), read("AGENTS.md"), read("docs/SDK_AND_DX.md"), read("docs/PUBLIC_PROJECT_AND_ADOPTION.md"), phases, byPhase, ownerPhase, deps, manifest.Artifacts, byID, planOwner, planBaseline, acceptanceByPhase, reports, req) {
		record(check.Name, check.Passed, check.Detail)
	}
	record("architecture-runtime-policy", strings.Contains(architecture, "Node") && strings.Contains(decisions, "D-061") && strings.Contains(blue, "implementation_runtime"), "IR-03 runtime boundary is explicit in architecture, decisions and blueprint")
	record("console-isolation", !strings.Contains(strings.ToLower(plan+layout), "kinglucky-agent-console/") && !strings.Contains(strings.ToLower(plan+layout), "../kinglucky-agent-console"), "protocol plan does not modify Console")
	linkProblems := localLinks(root)
	record("markdown-local-link-closure", len(linkProblems) == 0, strings.Join(linkProblems, ", "))
	reqProblems := []string{}
	if len(req.Requirements) != 14 {
		reqProblems = append(reqProblems, fmt.Sprintf("expected 14 requirements got %d", len(req.Requirements)))
	}
	for _, r := range req.Requirements {
		if len(r.Tests) == 0 {
			reqProblems = append(reqProblems, r.ID+" has no verification")
		}
	}
	record("requirement-traceability", len(reqProblems) == 0, strings.Join(reqProblems, "; "))
	summary := map[string]any{"phases": len(phases), "artifacts": len(manifest.Artifacts), "implement_or_refactor_phases": countType(phases, "implement") + countType(phases, "refactor"), "runtime_boundary": "go-governance-release; node-schema-typescript-npm; python-pep517"}
	return checks, summary, errors
}
func firstMake(v string) string {
	r := regexp.MustCompile(`make ([a-z0-9-]+)`)
	m := r.FindStringSubmatch(v)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
func makeCommands(v string) []string {
	r := regexp.MustCompile(`\bmake ([a-z0-9]+(?:-[a-z0-9]+)*)`)
	out := []string{}
	for _, match := range r.FindAllStringSubmatch(v, -1) {
		out = append(out, "make-"+match[1])
	}
	return out
}

func parityChecks(root, plan, layout, blueprint, architecture, decisions, makefile, readme, agents, sdk, publicAdoption string, phases []Phase, byPhase map[string]Phase, ownerPhase map[string]string, dependencies map[string][]string, artifacts []Artifact, byID map[string]Artifact, planOwner map[string]string, planBaseline map[string]Baseline, acceptance map[string][]string, reports map[string]Artifact, requirements Requirements) []report.Check {
	checks := []report.Check{}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{name, passed, detail})
	}

	add("review-candidate-status", strings.Contains(layout, "status: review-candidate") && strings.Contains(blueprint, "status: review-candidate") && strings.Contains(blueprint, "P04 用户 Gate 前") && strings.Contains(readme, "不表示规划已冻结"), "layout and blueprint remain review candidates before P04")
	modulePattern := regexp.MustCompile(`<!--\s*blueprint-module:\s*([^>]+?)\s*-->`)
	modules := []string{}
	for _, match := range modulePattern.FindAllStringSubmatch(layout, -1) {
		modules = append(modules, strings.TrimSpace(match[1]))
	}
	add("two-module-bootstrap", equalStrings(modules, []string{"go.mod", "reference/control-plane/go.mod"}) && strings.Contains(layout, "不建第三个") && strings.Contains(layout, "真实") && strings.Contains(layout, "go.work") && strings.Contains(layout, "临时 Go proxy") && strings.Contains(plan, "GOWORK=off") && strings.Contains(plan, "root pseudo-version"), strings.Join(modules, ", "))
	lowerPlanLayout := strings.ToLower(layout + "\n" + plan)
	vendorNeutral := strings.Contains(layout, "cmd/arop-conformance") && strings.Contains(layout, "语言中立") && strings.Contains(blueprint, "不依赖 Reference") && strings.Contains(architecture, "不提供厂商专属 Adapter")
	for _, forbidden := range []string{"cc-connect-adapter", "feishu-adapter", "codex-adapter"} {
		vendorNeutral = vendorNeutral && !strings.Contains(lowerPlanLayout, forbidden)
	}
	add("portable-vendor-neutral", vendorNeutral, "portable runner, neutral fixtures, no vendor adapter")

	requiredOwners := []string{"spec-governance", "implementation-planning", "independent-reviewer", "project-owner", "repository-layout", "protocol-foundation", "code-generation", "control-plane-platform", "control-plane-storage", "identity-secret-foundation", "publication-contracts", "publication-service", "asset-broker", "registry-core", "registry-api-sdk", "registry-recovery", "registry-verification", "run-service", "dispatch-security", "event-ledger", "go-provider-delivery", "streaming-delivery", "run-delivery-verification", "worker-service", "go-worker-client", "operations-security-review", "python-provider", "typescript-consumer", "interop-a2a", "interop-mcp", "interop-ard", "interop-observability", "portable-conformance", "server-conformance", "fault-ha-harness", "sqlite-quickstart", "production-deployment", "resilience-verification", "release-supply-chain", "release-evidence-tooling", "release-lineage-tooling", "release-finalization-tooling", "release-dry-run-verification", "release-readiness-review", "public-governance", "public-artifact-generation", "rc-source-freeze", "public-release-verification", "v1-rc-delivery", "external-conformance-review", "v1-freeze-overlay-review", "v1-release-approval", "v1-delivery"}
	ownerProblems := []string{}
	last := 0
	for _, owner := range requiredOwners {
		phase := ownerPhase[owner]
		if phase == "" {
			ownerProblems = append(ownerProblems, "missing "+owner)
			continue
		}
		if phaseNum(phase) <= last {
			ownerProblems = append(ownerProblems, owner+" out of order")
		}
		last = phaseNum(phase)
	}
	add("capability-owner-order", len(ownerProblems) == 0, strings.Join(ownerProblems, "; "))
	p01, p02, p03, p04 := byPhase["P01"], byPhase["P02"], byPhase["P03"], byPhase["P04"]
	gateOK := p01.Type == "implement" && p01.Status == "complete" && p02.Type == "implement" && p02.Status == "complete" && p03.Type == "verify · review" && p03.Status == "pending-review" && p04.Type == "gate" && p04.Status == "blocked" && strings.Contains(p03.Body, "must_fix_count=0") && strings.Contains(p04.Body, "summary_sha256") && contains(dependencies["P04"], "P03")
	add("planning-audit-user-gate", gateOK, "P01/P02 complete, P03 pending review, P04 blocked and dependent")
	firstRules := map[string][]string{"control-plane-platform": {"Clock/ID/Fault", "Audit/Trace"}, "control-plane-storage": {"durable Audit", "fixture versions", "readiness"}, "identity-secret-foundation": {"Credential", "SecretRef", "durable Audit"}, "publication-service": {"静态 URL", "离线"}, "asset-broker": {"DNS/IP", "redirect"}, "run-service": {"Outbox", "Cancel", "Deadline", "Usage", "Audit", "Trace", "effect_id"}, "dispatch-security": {"Signer/KMS/SecretRef", "public JWKS", "rotation state"}, "event-ledger": {"append-only", "Inbox", "capacity 释放同事务"}, "go-provider-delivery": {"DurableStore", "SQLite adapter/migration", "effect_id", "crash-before/after-effect", "DNS/IP 重检"}, "worker-service": {"claim 与 Attempt lease 原子", "Inbox/Outbox/effect"}}
	firstProblems := []string{}
	for owner, needles := range firstRules {
		body := byPhase[ownerPhase[owner]].Body
		for _, needle := range needles {
			if !strings.Contains(body, needle) {
				firstProblems = append(firstProblems, owner+":"+needle)
			}
		}
	}
	add("first-path-invariants", len(firstProblems) == 0, strings.Join(firstProblems, "; "))

	pathProblems := []string{}
	casePaths := map[string]string{}
	for i, left := range artifacts {
		folded := strings.ToLower(filepath.ToSlash(filepath.Clean(left.Path)))
		if prior := casePaths[folded]; prior != "" && prior != left.Path {
			pathProblems = append(pathProblems, "case collision "+prior+"/"+left.Path)
		}
		casePaths[folded] = left.Path
		for _, right := range artifacts[i+1:] {
			lp := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(left.Path)), "/")
			rp := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(right.Path)), "/")
			if lp == rp {
				pathProblems = append(pathProblems, "duplicate "+lp)
			} else if strings.HasPrefix(rp, lp+"/") && left.PathRole != "aggregate" && left.PathRole != "container" {
				pathProblems = append(pathProblems, left.ID+" covers "+right.ID)
			} else if strings.HasPrefix(lp, rp+"/") && right.PathRole != "aggregate" && right.PathRole != "container" {
				pathProblems = append(pathProblems, right.ID+" covers "+left.ID)
			}
		}
	}
	add("artifact-path-overlap", len(pathProblems) == 0, strings.Join(pathProblems, "; "))
	critical := []string{"sdk-go-protocol-core", "base-state-machine-fixtures", "conformance-harness-base", "codegen-pipeline", "codegen-representative-spike", "reference-control-plane-server", "audit-trace-ports", "audit-trace-memory-bootstrap", "migration-engine", "migration-engine-fixture-versions", "sqlite-uow-storage-adapter", "postgres-uow-storage-adapter", "durable-audit-storage", "identity-service", "credential-store", "reference-secret-exchange", "registry-api-service", "direct-proxy-delivery-service", "provider-durable-store-port", "reference-provider-sqlite-adapter", "reference-provider-sqlite-migration", "streaming-service", "go-streaming-client", "typescript-streaming-client", "release-package-orchestrator", "supply-chain-orchestrator", "go-consumer-core", "go-consumer-sdk", "python-runtime-registration-client", "python-worker-client", "python-package-metadata", "typescript-package-metadata", "go-release-primitive", "conformance-scenario-schema", "conformance-profile-schema", "conformance-core-scenarios", "conformance-v1-profiles", "conformance-fault-ha-scenarios", "production-conformance-profile", "release-version-policy-schema", "release-version-policy", "release-version-mapper", "oidc-release-workflow", "oidc-release-workflow-lock", "oidc-release-workflow-policy", "detached-release-evidence-envelope-schema", "detached-release-evidence-verifier", "trusted-release-role-registry", "cross-commit-lineage-verifier", "rc-source-freeze-checker", "final-equivalence-attestation-schema", "freeze-overlay-checker", "payload-equivalence-checker", "final-delivery-checker", "blueprint-validation-library", "machine-report-verifier-library", "go-schema-validator"}
	missingCritical := []string{}
	for _, id := range critical {
		if byID[id].ID == "" {
			missingCritical = append(missingCritical, id)
		}
	}
	add("critical-artifact-owners", len(missingCritical) == 0, strings.Join(missingCritical, ", "))
	codegenProblems := []string{}
	for _, domain := range []string{"control-plane", "asset", "registry", "run", "dispatch", "event", "streaming", "worker"} {
		for _, language := range []string{"go", "python", "typescript"} {
			id := "generated-" + domain + "-" + language
			if !contains(byID[id].DerivesFrom, "codegen-pipeline") {
				codegenProblems = append(codegenProblems, id)
			}
		}
	}
	for _, id := range byID["codegen-pipeline"].DerivesFrom {
		if strings.HasPrefix(id, "generated-") {
			codegenProblems = append(codegenProblems, "pipeline reverse dependency")
		}
	}
	add("schema-fixture-codegen-increments", len(codegenProblems) == 0, strings.Join(codegenProblems, "; "))
	migrationProblems := []string{}
	for _, item := range []struct{ domain, phase string }{{"base", "P09"}, {"identity", "P10"}, {"publication", "P12"}, {"asset", "P13"}, {"registry", "P14"}, {"run", "P18"}, {"dispatch", "P19"}, {"event", "P20"}, {"worker", "P24"}} {
		for _, engine := range []string{"sqlite", "postgres"} {
			if byID[engine+"-migration-"+item.domain].OwnerPhase != item.phase {
				migrationProblems = append(migrationProblems, engine+"/"+item.domain)
			}
		}
		for _, needle := range []string{"empty", "N-1→N", "idempotent", "dirty"} {
			if !strings.Contains(byPhase[item.phase].Body, needle) {
				migrationProblems = append(migrationProblems, item.phase+":"+needle)
			}
		}
	}
	if !strings.Contains(byPhase["P16"].Body, "不新增 migration") {
		migrationProblems = append(migrationProblems, "P16 no-migration")
	}
	if strings.Contains(byPhase["P21"].Owned, "postgres") {
		migrationProblems = append(migrationProblems, "P21 provider postgres")
	}
	if !strings.Contains(byPhase["P18"].Body, "effect_id") {
		migrationProblems = append(migrationProblems, "P18 effect_id")
	}
	add("concrete-migration-increments", len(migrationProblems) == 0, strings.Join(migrationProblems, "; "))
	providerOK := byID["provider-durable-store-port"].Path == "sdk/go/provider/durable_store.go" && byID["reference-provider-sqlite-adapter"].Path == "reference/agents/go-http/internal/storage/sqlite" && strings.HasPrefix(byID["reference-provider-sqlite-migration"].Path, "reference/agents/go-http/migrations/sqlite/") && strings.Contains(layout, "DurableStore") && strings.Contains(sdk, "不属于 Control Plane 双库矩阵")
	add("provider-storage-boundary", providerOK, "public SDK port and reference-local SQLite boundary")

	releaseChecks := releaseParity(plan, blueprint, decisions, publicAdoption, phases, byPhase, ownerPhase, dependencies, byID)
	checks = append(checks, releaseChecks...)

	implementedTargets := map[string]bool{}
	targetPattern := regexp.MustCompile(`(?m)^([a-z0-9]+(?:-[a-z0-9]+)*):(?:\s|$)`)
	for _, m := range targetPattern.FindAllStringSubmatch(makefile, -1) {
		implementedTargets["make-"+m[1]] = true
	}
	makeProblems := []string{}
	for _, id := range []string{"P01", "P02", "P03", "P04"} {
		for _, target := range acceptance[id] {
			if !implementedTargets[target] {
				makeProblems = append(makeProblems, id+" missing "+target)
			}
		}
	}
	add("current-make-targets", len(makeProblems) == 0, strings.Join(makeProblems, "; "))
	requirementProblems := []string{}
	for _, requirement := range requirements.Requirements {
		allowed := map[string]bool{}
		for _, phase := range requirement.Phases {
			if byPhase[phase].ID == "" {
				requirementProblems = append(requirementProblems, requirement.ID+" unknown "+phase)
			}
			matched := false
			for _, test := range acceptance[phase] {
				allowed[test] = true
				if contains(requirement.Tests, test) {
					matched = true
				}
			}
			if !matched {
				requirementProblems = append(requirementProblems, requirement.ID+" lacks "+phase+" acceptance")
			}
		}
		for _, test := range requirement.Tests {
			if !allowed[test] {
				requirementProblems = append(requirementProblems, requirement.ID+" non-phase test "+test)
			}
		}
		for _, artifact := range requirement.Artifacts {
			if byID[artifact].ID == "" {
				requirementProblems = append(requirementProblems, requirement.ID+" unknown artifact "+artifact)
			}
		}
	}
	add("requirement-traceability-bidirectional", len(requirements.Requirements) == 14 && len(requirementProblems) == 0, strings.Join(requirementProblems, "; "))
	consoleOK := strings.Contains(blueprint, "kinglucky-agent-console") && strings.Contains(blueprint, "不在本实施范围内") && strings.Contains(blueprint, "P01–P53") && strings.Contains(architecture, "P01–P53") && strings.Contains(plan, "本仓库 P01–P53 不修改 `kinglucky-agent-console`") && strings.Contains(readme, "不依赖 Console") && strings.Contains(agents, "kinglucky-agent-console") && !strings.Contains(layout, "kinglucky-agent-console/")
	add("console-isolation-full", consoleOK, "Console remains downstream and outside P01-P53")
	semanticProblems := []string{}
	for _, needle := range []string{"Authoring strict", "Consumer forward compatible", "Offline closure", "原始事件是不可变 append-only", "语义一致”而非“SQL 一致", "P16 明确复用 P14 ledger/watermark", "P07 交付可复用的三语言 pipeline", "预期子报告 ID/数量", "release_approver", "Sigstore identity"} {
		if !strings.Contains(blueprint+"\n"+sdk, needle) {
			semanticProblems = append(semanticProblems, needle)
		}
	}
	add("blueprint-cross-cutting-rules", len(semanticProblems) == 0, strings.Join(semanticProblems, ", "))
	v01Problems := publicV01Problems(root)
	add("controlled-v01-language", len(v01Problems) == 0, strings.Join(v01Problems, "; "))
	probe := publicV01LineProblems("Milestone: publish public v0.1 before the v1 release candidate.", "negative-probe.md")
	add("controlled-v01-negative-probe", len(probe) == 1, fmt.Sprintf("%d synthetic violations", len(probe)))
	return checks
}

func releaseParity(plan, blueprint, decisions, publicAdoption string, phases []Phase, byPhase map[string]Phase, ownerPhase map[string]string, dependencies map[string][]string, byID map[string]Artifact) []report.Check {
	checks := []report.Check{}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{name, passed, detail})
	}
	sequence := []struct{ owner, phase, kind string }{{"release-supply-chain", "P39", "implement"}, {"release-evidence-tooling", "P40", "implement"}, {"release-lineage-tooling", "P41", "implement"}, {"release-finalization-tooling", "P42", "implement"}, {"release-dry-run-verification", "P43", "verify · spec"}, {"release-readiness-review", "P44", "verify · review"}, {"public-governance", "P45", "gate"}, {"public-artifact-generation", "P46", "deliver"}, {"rc-source-freeze", "P47", "deliver"}, {"public-release-verification", "P48", "verify · review"}, {"v1-rc-delivery", "P49", "deliver"}, {"external-conformance-review", "P50", "gate"}, {"v1-freeze-overlay-review", "P51", "verify · review"}, {"v1-release-approval", "P52", "gate"}, {"v1-delivery", "P53", "deliver"}}
	problems := []string{}
	for i, item := range sequence {
		p := byPhase[item.phase]
		if ownerPhase[item.owner] != item.phase || p.Type != item.kind {
			problems = append(problems, item.owner+" mapping")
		}
		if i > 0 && !contains(dependencies[item.phase], sequence[i-1].phase) {
			problems = append(problems, item.phase+" dependency")
		}
	}
	needles := map[string][]string{
		"P39": {"P36 Go", "P27 Python", "P28 npm", "P37 Container", "不把 P05 proxy bootstrap 当打包 primitive"},
		"P40": {"root→timestamp→snapshot→targets", "TRUST_ROOT", "protected/pinned root", "Sigstore", "dry-run key", "不自动生成"},
		"P41": {"隔离 checkout", "CI provenance", "current inputs", "A 到 B"},
		"P42": {"unsigned canonical candidate", "parent=A", "release_approver", "bridged_reports"},
		"P43": {"只读", "不得在 verify 阶段补代码", "private/dev snapshot"},
		"P44": {"private code-complete", "不伪造外部配置/伙伴证据", "P49 v1 RC"},
		"P45": {"pinned trust root", "两名 Maintainer", "任意 CLI registry", "rollback/freeze"},
		"P46": {"RC source tree", "不创建 commit/tag", "nested `go.mod` 精确 root RC dependency"},
		"P47": {"clean RC source commit A", "expected tree digest", "不创 tag/package"},
		"P48": {"clean commit A", "只读", "不创建 commit/tag", "tracked tree 前后保持同一 digest"},
		"P49": {"reference/control-plane/v1.0.0-rc.N", "create-only", "annotated tag-object", "incident-blocked", "同一 A/digest", "绝不重标旧 A"},
		"P50": {"exact A commit/tree", "Schema Bundle", "Runner", "Artifact digests", "distinct `principal_id`"},
		"P51": {"unsigned canonical final overlay", "expected B tree", "parent=A", "不包含自签 attestation"},
		"P52": {"release_approver", "Sigstore", "expected B tree", "phase-policy digest", "bridged_reports", "dry-run key", "bound_evidence"},
		"P53": {"metadata-only commit B", "parent=A", "expected-previous CAS", "同一 B/同一 digest", "incident-blocked", "Conformance/SBOM/provenance"},
	}
	for phase, values := range needles {
		for _, needle := range values {
			if !strings.Contains(byPhase[phase].Body, needle) {
				problems = append(problems, phase+":"+needle)
			}
		}
	}
	add("release-a-b-chain", len(problems) == 0, strings.Join(problems, "; "))
	postGate := []string{}
	for _, phase := range phases {
		if phaseNum(phase.ID) > 45 && (phase.Type == "implement" || phase.Type == "refactor") {
			postGate = append(postGate, phase.ID)
		}
	}
	add("post-public-gate-freeze", len(postGate) == 0 && byID["public-namespace-regeneration"].OwnerPhase == "P42", strings.Join(postGate, ","))
	versionProblems := []string{}
	for _, id := range []string{"release-version-policy-schema", "release-version-policy", "release-version-mapper"} {
		if byID[id].OwnerPhase != "P39" {
			versionProblems = append(versionProblems, id)
		}
	}
	for _, needle := range []string{"1.0.0-rc.1", "1.0.0-rc.10", "Python `1.0.0rc1`", "v` 前缀", "PEP 440 输入", "RC leading zero", "build metadata", "version-policy digest"} {
		if !strings.Contains(plan, needle) {
			versionProblems = append(versionProblems, needle)
		}
	}
	for phase, values := range map[string][]string{"P43": {"全生态 pack/install", "同一冻结脚本"}, "P46": {"唯一无 `v` 逻辑版本", "Compatibility Matrix 不预填"}, "P48": {"RC1/RC10/final", "policy digest"}, "P51": {"Python `pyproject`/lock", "npm `package.json`/lock", "OCI/CLI/Schema Bundle metadata"}, "P52": {"version-policy digest", "bound_evidence"}, "P53": {"不盲传同一 VERSION", "分别发布"}} {
		for _, needle := range values {
			if !strings.Contains(byPhase[phase].Body, needle) {
				versionProblems = append(versionProblems, phase+":"+needle)
			}
		}
	}
	add("cross-ecosystem-version-policy", len(versionProblems) == 0, strings.Join(versionProblems, "; "))
	detachedProblems := []string{}
	for _, item := range []struct{ id, phase string }{{"verified-external-config-bundle-summary", "P45"}, {"rc-release-subject-manifest", "P49"}, {"rc-compatibility-detached-summary", "P50"}, {"final-overlay-candidate-bundle", "P51"}, {"verified-final-equivalence-attestation-summary", "P52"}, {"final-release-detached-evidence-bundle", "P53"}} {
		if byID[item.id].ProducerPhase != item.phase {
			detachedProblems = append(detachedProblems, item.id)
		}
	}
	if !strings.Contains(publicAdoption, "外部") || !strings.Contains(plan, "绝不代签") || !strings.Contains(plan, "不进入 B tree") {
		detachedProblems = append(detachedProblems, "external producer/validator/detached tree policy")
	}
	add("detached-external-evidence-chain", len(detachedProblems) == 0, strings.Join(detachedProblems, "; "))
	crossCommit := strings.Contains(blueprint, "历史普通 success 报告") && strings.Contains(blueprint, "隔离 checkout") && strings.Contains(blueprint, "受信 CI/OIDC/Sigstore provenance") && strings.Contains(blueprint, "current-input") && strings.Contains(blueprint, "P03/P04 的外部规划签署是独立类型") && strings.Contains(blueprint, "混合 commit 聚合不要求所有报告来自同一 commit") && strings.Contains(blueprint, "P53 使用 commit A 报告") && strings.Contains(blueprint, "P52 attestation 是唯一 A→B bridge") && strings.Contains(decisions, "P03/P04 历史规划签署与机器报告分类") && strings.Contains(decisions, "混合 commit 聚合") && strings.Contains(decisions, "P52 专用外部签名 equivalence attestation")
	add("cross-commit-report-policy", crossCommit, "ordinary reports, planning signatures, mixed commits and A-to-B bridge are distinct")
	firstRC := strings.Contains(plan, "取消独立公共 v0.1") && strings.Contains(plan, "P44 前所有产物都是 private/dev snapshot") && strings.Contains(blueprint, "P44 及之前所有制品只允许 private/dev snapshot") && strings.Contains(blueprint, "首个公开候选是 P49") && strings.Contains(publicAdoption, "P44 及之前所有验证制品仅是 private/dev snapshot")
	add("first-public-v1-rc", firstRC, "P44 and earlier private/dev; P49 is first public RC")
	return checks
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func publicV01LineProblems(source, label string) []string {
	problems := []string{}
	allowed := regexp.MustCompile(`(?i)(取消独立公共 v0\.1|不用 v0\.1|不得用 v0\.1|no (?:separate )?public v0\.1)`)
	for index, line := range strings.Split(source, "\n") {
		if regexp.MustCompile(`(?i)\bv0\.1\b`).MatchString(line) && !allowed.MatchString(line) {
			problems = append(problems, fmt.Sprintf("%s:%d", label, index+1))
		}
	}
	return problems
}

func publicV01Problems(root string) []string {
	problems := []string{}
	for _, top := range []string{"README.md", "SECURITY.md", "CONTRIBUTING.md"} {
		data, err := os.ReadFile(filepath.Join(root, top))
		if err != nil {
			problems = append(problems, top+": "+err.Error())
			continue
		}
		problems = append(problems, publicV01LineProblems(string(data), top)...)
	}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		problems = append(problems, publicV01LineProblems(string(data), filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		problems = append(problems, "controlled scan: "+err.Error())
	}
	return problems
}
func availability(a Artifact) int {
	for _, x := range []string{a.OwnerPhase, a.CompletionPhase, a.ProducerPhase} {
		if x != "" {
			return phaseNum(x)
		}
	}
	return 0
}
func graphProblems(artifacts []Artifact, byID map[string]Artifact, override map[string][]string) []string {
	p := []string{}
	state := map[string]int{}
	var visit func(string)
	inputs := func(a Artifact) []string {
		if override != nil && override[a.ID] != nil {
			return override[a.ID]
		}
		return append(append([]string{}, a.DerivesFrom...), a.RuntimeInputs...)
	}
	visit = func(id string) {
		if state[id] == 1 {
			p = append(p, "cycle at "+id)
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		a := byID[id]
		for _, d := range inputs(a) {
			if input := byID[d]; input.ID != "" {
				if a.ProducerPhase != "" && contains(a.RuntimeInputs, d) && availability(input) >= availability(a) {
					p = append(p, a.ID+" runtime input not prior "+d)
				}
				visit(d)
			}
		}
		state[id] = 2
	}
	for _, a := range artifacts {
		visit(a.ID)
	}
	return p
}
func reportClosure(phases []Phase, artifacts []Artifact, byID map[string]Artifact, planOwner map[string]string, reports map[string]Artifact) []string {
	p := []string{}
	for _, phase := range phases {
		r := reports[phase.ID]
		if phase.Type == "implement" || phase.Type == "refactor" {
			for id, owner := range planOwner {
				if owner == phase.ID && !contains(r.DerivesFrom, id) {
					p = append(p, phase.ID+" report misses owned "+id)
				}
			}
			for _, b := range phase.Baselines {
				if !contains(r.DerivesFrom, b.ID) {
					p = append(p, phase.ID+" report misses baseline "+b.ID)
				}
			}
		}
		for _, d := range r.DerivesFrom {
			if byID[d].Kind == "machine-reports" || byID[d].Kind == "canonical-evidence-summary" {
				p = append(p, phase.ID+" machine report puts runtime evidence in derives_from: "+d)
			}
		}
	}
	exact := map[string][]string{"P04": {"P03", "P03-summary"}, "P17": {"P14", "P15", "P16"}, "P23": {"P18", "P19", "P20", "P21", "P22"}, "P26": {"P08", "P10", "P12", "P13", "P18", "P19", "P20", "P21", "P22", "P23", "P24", "P25"}, "P38": {"P09", "P10", "P12", "P13", "P14", "P16", "P18", "P19", "P20", "P21", "P24", "P27", "P34", "P35", "P37"}, "P43": {"P39", "P40", "P41", "P42"}}
	for i := 45; i <= 53; i++ {
		exact[fmt.Sprintf("P%02d", i)] = []string{fmt.Sprintf("P%02d", i-1)}
	}
	all := []string{}
	for i := 1; i <= 43; i++ {
		all = append(all, fmt.Sprintf("P%02d", i))
	}
	exact["P44"] = all
	for _, phase := range phases {
		expectedPhases, has := exact[phase.ID]
		expected := []string{}
		if has {
			for _, id := range expectedPhases {
				if id == "P03-summary" {
					expected = append(expected, "planning-audit-canonical-summary")
				} else {
					expected = append(expected, reports[id].ID)
				}
			}
		}
		if !same(reports[phase.ID].RuntimeInputs, expected) {
			p = append(p, phase.ID+" runtime_inputs must be exact")
		}
	}
	if !same(reports["P43"].DerivesFrom, []string{"release-package-orchestrator", "supply-chain-orchestrator", "public-release-aggregator", "report-provenance-verifier", "public-namespace-regeneration", "rc-source-freeze-checker", "freeze-overlay-checker"}) {
		p = append(p, "P43 static toolchain must be exact")
	}
	if !same(reports["P44"].DerivesFrom, []string{"public-release-aggregator", "report-provenance-verifier", "release-report-integration"}) {
		p = append(p, "P44 static toolchain must be exact")
	}
	return p
}
func reportNegativeProbes(phases []Phase, byID map[string]Artifact, owners map[string]string, reports map[string]Artifact) []string {
	p := []string{}
	for _, phase := range []string{"P01", "P40", "P41"} {
		owned := ""
		for id, o := range owners {
			if o == phase {
				owned = id
				break
			}
		}
		if owned == "" || !contains(reports[phase].DerivesFrom, owned) {
			p = append(p, "probe setup invalid "+phase)
			continue
		}
		mut := reports[phase]
		mut.DerivesFrom = remove(mut.DerivesFrom, owned)
		if !contains(reportClosure(phases, replaceArtifact(byID, mut), indexArtifacts(replaceArtifact(byID, mut)), owners, replaceReport(reports, mut)), phase+" report misses owned "+owned) {
			p = append(p, "missing-owned escaped "+phase)
		}
	}
	p04 := reports["P04"]
	p04.DerivesFrom = append(p04.DerivesFrom, "planning-audit-reports")
	p04.RuntimeInputs = remove(p04.RuntimeInputs, "planning-audit-reports")
	if len(reportClosure(phases, replaceArtifact(byID, p04), indexArtifacts(replaceArtifact(byID, p04)), owners, replaceReport(reports, p04))) == 0 {
		p = append(p, "static-runtime-misclassification escaped")
	}
	p44 := reports["P44"]
	p44.RuntimeInputs = remove(p44.RuntimeInputs, reports["P05"].ID)
	if len(reportClosure(phases, replaceArtifact(byID, p44), indexArtifacts(replaceArtifact(byID, p44)), owners, replaceReport(reports, p44))) == 0 {
		p = append(p, "missing-P05 escaped")
	}
	p44 = reports["P44"]
	p44.RuntimeInputs = append(p44.RuntimeInputs, reports["P45"].ID)
	if len(reportClosure(phases, replaceArtifact(byID, p44), indexArtifacts(replaceArtifact(byID, p44)), owners, replaceReport(reports, p44))) == 0 {
		p = append(p, "extra-P45 escaped")
	}
	p17 := reports["P17"]
	p17.RuntimeInputs = []string{reports["P18"].ID}
	futureArtifacts := replaceArtifact(byID, p17)
	if len(graphProblems(futureArtifacts, indexArtifacts(futureArtifacts), nil)) == 0 {
		p = append(p, "future-runtime escaped")
	}
	p14 := reports["P14"]
	p14.RuntimeInputs = []string{reports["P17"].ID}
	cycleArtifacts := replaceArtifact(byID, p14)
	if len(graphProblems(cycleArtifacts, indexArtifacts(cycleArtifacts), nil)) == 0 {
		p = append(p, "runtime-cycle escaped")
	}
	return p
}
func runtimeBoundary(root string, artifacts []Artifact, byID map[string]Artifact, makefile, packageJSON string) []string {
	p := []string{}
	canonicalRoot := root
	if root != "" {
		absoluteRoot, absErr := filepath.Abs(root)
		if absErr != nil {
			p = append(p, "repository root resolution failed: "+absErr.Error())
		} else if resolvedRoot, resolveErr := filepath.EvalSymlinks(absoluteRoot); resolveErr != nil {
			p = append(p, "repository root symlink resolution failed: "+resolveErr.Error())
		} else {
			canonicalRoot = resolvedRoot
		}
	}
	allowedNode := map[string]bool{"schema-validation": true, "schema-codegen": true, "typescript-sdk": true, "npm-packaging": true}
	allowedNodeKinds := map[string]map[string]bool{
		"schema-validation": {"tooling": true, "tooling-baseline": true, "tooling-helper": true},
		"schema-codegen":    {"tooling": true, "tooling-helper": true},
		"typescript-sdk":    {"typescript-source": true, "tooling-helper": true},
		"npm-packaging":     {"build-primitive": true, "tooling-helper": true},
	}
	execKind := regexp.MustCompile(`tooling|validator|aggregator|build-primitive`)
	casePaths := map[string]string{}
	for _, a := range artifacts {
		if _, err := structuredfile.SafeRelative(".", a.Path); err != nil {
			p = append(p, a.ID+" unsafe artifact path: "+err.Error())
		}
		folded := strings.ToLower(filepath.ToSlash(a.Path))
		if prior := casePaths[folded]; prior != "" && prior != a.Path {
			p = append(p, "case-insensitive path collision: "+prior+" / "+a.Path)
		}
		casePaths[folded] = a.Path
		executable := a.ImplementationRuntime != "" || execKind.MatchString(a.Kind)
		if executable && (a.ImplementationRuntime == "" || a.ToolScope == "") {
			p = append(p, a.ID+" executable artifact missing implementation_runtime/tool_scope")
			continue
		}
		ext := strings.ToLower(filepath.Ext(a.Path))
		actualNode := ext == ".mjs" || ext == ".js" || ext == ".cjs" || ext == ".ts"
		if actualNode && a.ImplementationRuntime != "node" {
			p = append(p, a.ID+" path is Node but metadata is "+a.ImplementationRuntime)
		}
		if a.ImplementationRuntime == "node" && !allowedNode[a.ToolScope] {
			p = append(p, a.ID+" Node scope forbidden: "+a.ToolScope)
		}
		if a.ImplementationRuntime == "node" && !allowedNodeKinds[a.ToolScope][a.Kind] {
			p = append(p, a.ID+" Node path/kind/tool_scope tuple is not allowlisted")
		}
		if a.ImplementationRuntime == "node" && !actualNode && a.PathRole == "concrete" {
			p = append(p, a.ID+" claims Node but path is not Node")
		}
		if a.ImplementationRuntime == "go" && actualNode {
			p = append(p, a.ID+" claims Go but path is Node")
		}
		if a.ImplementationRuntime == "python" && ext != ".py" && a.PathRole == "concrete" {
			p = append(p, a.ID+" Python primitive is not native Python")
		}
		if strings.HasPrefix(strings.ToLower(a.Path), "scripts/release/") && actualNode {
			p = append(p, a.ID+" release JavaScript forbidden")
		}
		if a.ImplementationRuntime == "node" {
			for _, d := range a.DerivesFrom {
				dep := byID[d]
				if dep.ImplementationRuntime == "go" || strings.Contains(dep.ToolScope, "release") || dep.ToolScope == "evidence" || dep.ToolScope == "report" {
					p = append(p, a.ID+" Node helper depends on generic/release artifact "+d)
				}
			}
		}
		if (a.ImplementationRuntime == "go" || strings.Contains(a.ToolScope, "release")) && contains(a.DerivesFrom, "structured-file-tools") {
			p = append(p, a.ID+" Go/release tool depends on Node structured-file-tools")
		}
		if a.ID == "npm-package-primitive" {
			for _, d := range a.DerivesFrom {
				scope := byID[d].ToolScope
				if scope == "python-package" || scope == "go-build" || scope == "container-build" {
					p = append(p, "npm primitive crosses ecosystem to "+d)
				}
			}
		}
		if root != "" && a.Status == "present" {
			absolute := filepath.Join(root, filepath.FromSlash(a.Path))
			if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
				if a.ImplementationRuntime == "node" {
					p = append(p, a.ID+" Node execution closure must not contain symlinks")
				} else {
					resolved, resolveErr := filepath.EvalSymlinks(absolute)
					rel, relErr := filepath.Rel(canonicalRoot, resolved)
					if resolveErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						p = append(p, a.ID+" symlink escapes repository")
					}
				}
			}
		}
	}
	var pkg map[string]any
	declaredNodePackages := map[string]bool{}
	if parsed, err := structuredfile.Parse([]byte(packageJSON), "json"); err != nil {
		p = append(p, "package.json strict parse: "+err.Error())
	} else if parsedMap, ok := parsed.(map[string]any); ok {
		pkg = parsedMap
		for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
			dependencies, _ := pkg[field].(map[string]any)
			for name := range dependencies {
				declaredNodePackages[name] = true
			}
		}
		scripts, _ := pkg["scripts"].(map[string]any)
		for name, raw := range scripts {
			v := fmt.Sprint(raw)
			if regexp.MustCompile(`(?i)(scripts/release/|blueprint-check\.mjs|verify-report\.mjs|planning-audit\.mjs|gate-check\.mjs|test-evidence-lineage\.mjs|test-report-verifier\.mjs)`).MatchString(v) {
				p = append(p, "package script "+name+" aliases forbidden Node governance/release tooling")
			}
		}
		for _, problem := range packageScriptProblems(scripts, artifacts) {
			p = append(p, problem)
		}
	}
	for _, issue := range commandSourceProblems("Makefile", makefile, false, artifacts) {
		p = append(p, issue)
	}
	if root != "" {
		importedNodeHelpers := map[string]bool{}
		walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				p = append(p, "repository walk failed at "+path+": "+err.Error())
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				p = append(p, "repository relative path failed at "+path+": "+relErr.Error())
				return nil
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() && (rel == ".git" || rel == "node_modules" || rel == "build" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "build/")) {
				return filepath.SkipDir
			}
			info, lstatErr := os.Lstat(path)
			if lstatErr != nil {
				p = append(p, rel+" lstat failed: "+lstatErr.Error())
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(path)
				resolvedRel, resolvedRelErr := filepath.Rel(canonicalRoot, resolved)
				if resolveErr != nil || resolvedRelErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
					p = append(p, rel+" symlink escapes repository or cannot be resolved")
					return nil
				}
				owner := artifactForExactPath(rel, artifacts)
				resolvedOwner := artifactForExactPath(filepath.ToSlash(resolvedRel), artifacts)
				if owner.ID == "" || resolvedOwner.ID == "" {
					p = append(p, rel+" symlink enters an unregistered artifact")
				}
			}
			if d.IsDir() {
				return nil
			}
			lower := strings.ToLower(rel)
			ext := strings.ToLower(filepath.Ext(rel))
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				p = append(p, rel+" read failed: "+readErr.Error())
				return nil
			}
			text := string(data)
			isNode := ext == ".mjs" || ext == ".js" || ext == ".cjs" || ext == ".ts" || nodeShebang(text)
			if isNode {
				owner := artifactForExactPath(rel, artifacts)
				if owner.ID == "" || owner.ImplementationRuntime != "node" || !allowedNode[owner.ToolScope] {
					p = append(p, rel+" executable Node file is not covered by an exact allowed Node artifact")
					return nil
				}
				if info.Mode()&os.ModeSymlink != 0 || pathContainsSymlink(root, rel) {
					p = append(p, rel+" Node execution closure must not contain symlinks")
					return nil
				}
				if nodeShebang(text) {
					p = append(p, rel+" Node shebang execution is forbidden; use an exact manifest-owned Node invocation template")
				}
				if strings.HasPrefix(lower, "scripts/release/") {
					p = append(p, rel+" JavaScript is forbidden under scripts/release")
				}
				analysis, analysisErr := analyzeNodeSource(text)
				if analysisErr != nil {
					p = append(p, rel+" JavaScript AST parse failed: "+analysisErr.Error())
					return nil
				}
				for _, problem := range analysis.Problems {
					p = append(p, rel+" "+problem)
				}
				for _, imported := range analysis.StaticImports {
					problems, helperID := validateNodeStaticImport(root, rel, imported, owner, artifacts, declaredNodePackages)
					p = append(p, problems...)
					if helperID != "" {
						importedNodeHelpers[helperID] = true
					}
				}
			}
			if strings.HasPrefix(rel, ".github/workflows/") && (ext == ".yml" || ext == ".yaml") {
				p = append(p, commandSourceProblems(rel, text, false, artifacts)...)
			}
			base := strings.ToLower(filepath.Base(rel))
			if strings.Contains(base, "dockerfile") || strings.HasPrefix(base, "containerfile") {
				p = append(p, commandSourceProblems(rel, text, true, artifacts)...)
			}
			return nil
		})
		if walkErr != nil {
			p = append(p, "repository walk failed: "+walkErr.Error())
		}
		for _, artifact := range artifacts {
			if artifact.Status == "present" && artifact.ImplementationRuntime == "node" && artifact.Kind == "tooling-helper" && !importedNodeHelpers[artifact.ID] {
				p = append(p, artifact.ID+" Node helper is outside every declared entry/helper import closure")
			}
		}
	}
	return p
}
func boundaryNegativeProbes() []string {
	p := []string{}
	base := []Artifact{{ID: "allowed", Path: "scripts/validate.mjs", Kind: "tooling", ImplementationRuntime: "node", ToolScope: "schema-validation"}}
	artifactCases := []struct {
		name     string
		artifact Artifact
	}{
		{"planned-release-node", Artifact{ID: "evil", Path: "scripts/release/evil.mjs", Kind: "release-validator", ImplementationRuntime: "node", ToolScope: "release"}},
		{"python-mjs", Artifact{ID: "evil", Path: "sdk/python/build.mjs", Kind: "build-primitive", ImplementationRuntime: "python", ToolScope: "python-package"}},
		{"go-js", Artifact{ID: "evil", Path: "tools/release.js", Kind: "release-tooling", ImplementationRuntime: "go", ToolScope: "release"}},
		{"missing-runtime-scope", Artifact{ID: "evil", Path: "tools/run", Kind: "release-tooling"}},
		{"forbidden-scope-under-schema-path", Artifact{ID: "evil", Path: "scripts/schema/hidden.cjs", Kind: "release-tooling", ImplementationRuntime: "node", ToolScope: "release"}},
		{"case-collision", Artifact{ID: "evil", Path: "Scripts/Validate.mjs", Kind: "tooling", ImplementationRuntime: "node", ToolScope: "schema-validation"}},
	}
	for _, tc := range artifactCases {
		artifacts := append(append([]Artifact{}, base...), tc.artifact)
		by := indexArtifacts(artifacts)
		if len(runtimeBoundary("", artifacts, by, "", `{"scripts":{}}`)) == 0 {
			p = append(p, tc.name+" escaped")
		}
	}
	for name, shebang := range map[string]string{"env-node": "#!/usr/bin/env node", "env-s-node": "\ufeff  #!/usr/bin/env -S node\r", "env-s-quoted-node": `#!/usr/bin/env -S 'node --no-warnings'`, "env-assignment-node": "#!/usr/bin/env MODE=strict node", "env-split-string-node": "#!/usr/bin/env --split-string=node", "nvm-node": "#!/Users/example/.nvm/versions/node/v22/bin/node", "usr-node": "#!/usr/bin/node", "usr-nodejs": "#!/usr/bin/nodejs", "env-nodejs": "#!/usr/bin/env nodejs"} {
		if !nodeShebang(shebang + "\n") {
			p = append(p, name+" shebang escaped")
		}
	}
	for name, source := range map[string]string{"make-node-var": "NODE_BIN:=node\nx:\n\t$(NODE_BIN) -e x", "make-env-var": "RUNTIME=node\nx:\n\t$${RUNTIME} scripts/release/evil.mjs", "make-env-wrapper": "x:\n\tenv node scripts/release/evil.mjs", "make-npx": "x:\n\tnpx tsx evil.ts", "make-bun": "x:\n\tbun evil.ts", "make-deno": "x:\n\tdeno run evil.ts"} {
		if len(commandSourceProblems(name, source, false, base)) == 0 {
			p = append(p, name+" escaped")
		}
	}
	for name, scripts := range map[string]map[string]any{
		"package-indirect": {"a": "npm run b", "b": "npm run c", "c": "node tools/evil"},
		"package-cycle":    {"a": "npm run b", "b": "npm run a"},
		"package-npx":      {"a": "npx tsx tools/evil.ts"},
		"package-nodejs":   {"a": "nodejs tools/evil"},
	} {
		if len(packageScriptProblems(scripts, base)) == 0 {
			p = append(p, name+" escaped")
		}
	}
	return p
}
func artifactForPath(path string, artifacts []Artifact) Artifact {
	path = filepath.ToSlash(filepath.Clean(path))
	best := Artifact{}
	for _, a := range artifacts {
		ap := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(a.Path)), "/")
		if path == ap || strings.HasPrefix(path, ap+"/") {
			if len(ap) > len(best.Path) {
				best = a
			}
		}
	}
	return best
}

func artifactForExactPath(path string, artifacts []Artifact) Artifact {
	path = filepath.ToSlash(filepath.Clean(path))
	for _, artifact := range artifacts {
		if filepath.ToSlash(filepath.Clean(artifact.Path)) == path && artifact.PathRole == "concrete" {
			return artifact
		}
	}
	return Artifact{}
}

type nodeSourceAnalysis struct {
	StaticImports []string
	Problems      []string
}

type nodeASTVisitor struct {
	analysis *nodeSourceAnalysis
	stack    []js.INode
}

func (visitor *nodeASTVisitor) Enter(node js.INode) js.IVisitor {
	const suffix = "; Node tool scopes have no process, network, worker, native-addon, or dynamic-code allowlist"
	add := func(problem string) { visitor.analysis.Problems = append(visitor.analysis.Problems, problem+suffix) }
	var parent js.INode
	if len(visitor.stack) != 0 {
		parent = visitor.stack[len(visitor.stack)-1]
	}
	visitor.stack = append(visitor.stack, node)
	switch current := node.(type) {
	case *js.ImportStmt:
		if module, ok := decodeJSString(current.Module); ok {
			visitor.analysis.StaticImports = append(visitor.analysis.StaticImports, module)
		} else {
			add("has a non-canonical static import specifier")
		}
	case *js.ExportStmt:
		if current.Module != nil {
			if module, ok := decodeJSString(current.Module); ok {
				visitor.analysis.StaticImports = append(visitor.analysis.StaticImports, module)
			} else {
				add("has a non-canonical static export specifier")
			}
		}
	case *js.Var:
		name := string(current.Name())
		if strings.Contains(string(current.Data), `\`) || strings.Contains(name, `\`) {
			add("uses escaped JavaScript identifier spelling")
		}
		if name == "Reflect" {
			add("uses dangerous reflection identifier Reflect")
		}
		if map[string]bool{"require": true, "createRequire": true, "eval": true, "Function": true, "binding": true, "dlopen": true, "getBuiltinModule": true, "fetch": true, "WebSocket": true, "EventSource": true, "WebTransport": true, "Worker": true, "BroadcastChannel": true, "MessageChannel": true, "navigator": true, "global": true, "globalThis": true, "self": true, "window": true}[name] {
			add("uses forbidden dynamic loader/code identifier " + name)
		}
		if name == "process" && !allowedProcessReference(parent, current) {
			add("aliases or passes the process object outside its exact member allowlist")
		}
	case *js.CallExpr:
		name, _ := nodeExpressionName(current.X)
		last := name
		if dot := strings.LastIndexByte(last, '.'); dot >= 0 {
			last = last[dot+1:]
		}
		if name == "import" {
			add("uses dynamic import")
		}
		if map[string]bool{"require": true, "createRequire": true, "eval": true, "Function": true}[last] {
			add("calls forbidden dynamic loader/code primitive " + last)
		}
		if map[string]bool{"exec": true, "execFile": true, "execSync": true, "execFileSync": true, "spawn": true, "spawnSync": true, "fork": true}[last] {
			add("calls forbidden external-command primitive " + last)
		}
		if map[string]bool{"fetch": true, "WebSocket": true, "EventSource": true, "WebTransport": true}[last] {
			add("calls forbidden global network primitive " + last)
		}
		if map[string]bool{"binding": true, "dlopen": true, "getBuiltinModule": true}[last] && (strings.HasPrefix(name, "process.") || strings.HasPrefix(name, "globalThis.process.")) {
			add("uses process native loading escape " + last)
		}
		if strings.HasPrefix(name, "Reflect.") || dangerousObjectReflection(name) {
			add("uses dangerous reflection primitive " + name)
		}
	case *js.NewExpr:
		name, _ := nodeExpressionName(current.X)
		if name == "Function" || strings.HasSuffix(name, ".Function") {
			add("constructs Function")
		}
		last := name
		if dot := strings.LastIndexByte(last, '.'); dot >= 0 {
			last = last[dot+1:]
		}
		if map[string]bool{"Worker": true, "WebSocket": true, "EventSource": true, "WebTransport": true, "BroadcastChannel": true, "MessageChannel": true}[last] {
			add("constructs forbidden worker/network primitive " + last)
		}
	case *js.DotExpr:
		if name, ok := nodeExpressionName(current); ok {
			if dangerousMemberPath(name) {
				add("accesses dangerous runtime/reflection member " + name)
			}
			if forbiddenProcessMemberPath(name, parent) {
				add("accesses process member outside the exact argv/exit/result-file allowlist: " + name)
			}
		}
		if member, ok := nodePropertyIdentifier(current.Y); ok && member == "constructor" {
			add("accesses constructor-based dynamic code generation")
		}
	case *js.IndexExpr:
		base, _ := nodeExpressionName(current.X)
		member, constant := nodeStaticString(current.Y)
		if !constant && dangerousComputedBase(base) {
			add("uses dangerous computed runtime/reflection access on " + base)
		} else if constant && dangerousComputedMember(base, member) {
			add("uses dangerous computed runtime/reflection member " + member)
		}
	}
	return visitor
}

func (visitor *nodeASTVisitor) Exit(js.INode) {
	visitor.stack = visitor.stack[:len(visitor.stack)-1]
}

func allowedProcessReference(parent js.INode, current *js.Var) bool {
	switch value := parent.(type) {
	case *js.DotExpr:
		return value.X == current
	case *js.IndexExpr:
		return value.X == current
	default:
		return false
	}
}

func forbiddenProcessMemberPath(name string, parent js.INode) bool {
	if name == "globalThis.process" || strings.HasPrefix(name, "globalThis.process.") {
		return true
	}
	if name == "process.argv" || name == "process.exit" || name == "process.env.AROP_RESULT_FILE" {
		return false
	}
	if name == "process.env" {
		if outer, ok := parent.(*js.DotExpr); ok {
			outerName, _ := nodeExpressionName(outer)
			return outerName != "process.env.AROP_RESULT_FILE"
		}
		return true
	}
	return strings.HasPrefix(name, "process.")
}

func analyzeNodeSource(source string) (nodeSourceAnalysis, error) {
	tree, err := js.Parse(parse.NewInput(strings.NewReader(source)), js.Options{})
	if err != nil {
		return nodeSourceAnalysis{}, err
	}
	analysis := nodeSourceAnalysis{}
	if regexp.MustCompile(`\\u(?:[0-9A-Fa-f]{4}|\{[0-9A-Fa-f]{1,6}\})`).MatchString(source) {
		analysis.Problems = append(analysis.Problems, "uses Unicode escape spelling; Node boundary source must use canonical literal tokens")
	}
	js.Walk(&nodeASTVisitor{analysis: &analysis}, tree)
	analysis.StaticImports = uniqueStrings(analysis.StaticImports)
	analysis.Problems = uniqueStrings(analysis.Problems)
	return analysis, nil
}

func decodeJSString(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != raw[len(raw)-1] || (raw[0] != '\'' && raw[0] != '"') {
		return "", false
	}
	literal := js.LiteralExpr{TokenType: js.StringToken, Data: raw}
	var encoded bytes.Buffer
	if err := literal.JSON(&encoded); err != nil {
		return "", false
	}
	var value string
	if err := json.Unmarshal(encoded.Bytes(), &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func nodeExpressionName(expression js.IExpr) (string, bool) {
	switch value := expression.(type) {
	case *js.Var:
		if len(value.Data) != 0 {
			return string(value.Data), true
		}
		if len(value.Name()) != 0 {
			return string(value.Name()), true
		}
	case *js.LiteralExpr:
		if value.TokenType == js.IdentifierToken || value.TokenType == js.ImportToken || regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`).Match(value.Data) {
			return string(value.Data), true
		}
	case *js.GroupExpr:
		return nodeExpressionName(value.X)
	case *js.DotExpr:
		base, baseOK := nodeExpressionName(value.X)
		member, memberOK := nodePropertyIdentifier(value.Y)
		if baseOK && memberOK {
			return base + "." + member, true
		}
	case *js.IndexExpr:
		base, baseOK := nodeExpressionName(value.X)
		member, memberOK := nodeStaticString(value.Y)
		if baseOK && memberOK {
			return base + "." + member, true
		}
	}
	return "", false
}

func nodePropertyIdentifier(expression js.IExpr) (string, bool) {
	switch value := expression.(type) {
	case *js.Var:
		if len(value.Data) != 0 {
			return string(value.Data), true
		}
		if len(value.Name()) != 0 {
			return string(value.Name()), true
		}
	case *js.LiteralExpr:
		if regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`).Match(value.Data) {
			return string(value.Data), true
		}
	}
	text := expression.String()
	if regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`).MatchString(text) {
		return text, true
	}
	return "", false
}

func nodeStaticString(expression js.IExpr) (string, bool) {
	switch value := expression.(type) {
	case *js.LiteralExpr:
		if value.TokenType == js.StringToken {
			return decodeJSString(value.Data)
		}
	case *js.GroupExpr:
		return nodeStaticString(value.X)
	case *js.BinaryExpr:
		if value.Op == js.AddToken {
			left, leftOK := nodeStaticString(value.X)
			right, rightOK := nodeStaticString(value.Y)
			if leftOK && rightOK {
				return left + right, true
			}
		}
	}
	return "", false
}

func dangerousComputedBase(base string) bool {
	return base == "process" || base == "globalThis" || base == "globalThis.process" || base == "module" || base == "Reflect" || base == "Object"
}

func dangerousComputedMember(base, member string) bool {
	if member == "constructor" {
		return true
	}
	if (base == "process" || base == "globalThis.process") && member != "" {
		return true
	}
	if base == "globalThis" && (member == "Reflect" || member == "Object" || member == "process") {
		return true
	}
	if map[string]bool{"process": true, "require": true, "createRequire": true, "eval": true, "Function": true, "binding": true, "dlopen": true, "getBuiltinModule": true, "constructor": true, "_load": true}[member] {
		return dangerousComputedBase(base) || strings.HasPrefix(base, "globalThis.") || strings.HasPrefix(base, "module.")
	}
	return (base == "Reflect" || base == "Object") && map[string]bool{"get": true, "apply": true, "construct": true, "getOwnPropertyDescriptor": true, "getOwnPropertyDescriptors": true}[member]
}

func dangerousMemberPath(name string) bool {
	return strings.HasPrefix(name, "Reflect.") || strings.HasPrefix(name, "globalThis.Reflect.") || name == "globalThis.Reflect" || name == "globalThis.Object" || strings.HasPrefix(name, "globalThis.Object.") || dangerousObjectReflection(name) || strings.Contains(name, ".constructor") ||
		(strings.HasPrefix(name, "process.") || strings.HasPrefix(name, "globalThis.process.")) &&
			(strings.HasSuffix(name, ".binding") || strings.HasSuffix(name, ".dlopen") || strings.HasSuffix(name, ".getBuiltinModule"))
}

func dangerousObjectReflection(name string) bool {
	return name == "Object.getOwnPropertyDescriptor" || name == "Object.getOwnPropertyDescriptors" || name == "Object.getPrototypeOf" || name == "Object.defineProperty" || name == "Object.setPrototypeOf"
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
func nodeShebang(source string) bool {
	first := strings.SplitN(strings.TrimLeft(source, "\ufeff \t\r\n"), "\n", 2)[0]
	first = strings.TrimSpace(strings.TrimSuffix(first, "\r"))
	if !strings.HasPrefix(first, "#!") {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(first, "#!")))
	if len(fields) == 0 {
		return false
	}
	interpreter := filepath.Base(fields[0])
	if interpreter == "env" {
		remainder := strings.Join(fields[1:], " ")
		return regexp.MustCompile(`(?i)\bnode(?:js)?\b`).MatchString(remainder) || containsObfuscatedNodeToken(remainder)
	}
	return interpreter == "node" || interpreter == "nodejs"
}

func resolveNodeImport(root, sourcePath, imported string) string {
	base := filepath.Clean(filepath.Join(filepath.Dir(sourcePath), imported))
	candidates := []string{base, base + ".mjs", base + ".js", base + ".cjs", base + ".ts", filepath.Join(base, "index.mjs"), filepath.Join(base, "index.js"), filepath.Join(base, "index.ts")}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(root, candidate)); err == nil {
			return filepath.ToSlash(candidate)
		}
	}
	return filepath.ToSlash(base)
}

func validateNodeStaticImport(root, sourcePath, imported string, source Artifact, artifacts []Artifact, declaredPackages map[string]bool) ([]string, string) {
	prefix := sourcePath + " imports " + imported
	if strings.HasPrefix(imported, "data:") || strings.HasPrefix(imported, "file:") {
		return []string{prefix + ": data/file imports are forbidden"}, ""
	}
	if filepath.IsAbs(imported) || regexp.MustCompile(`^[A-Za-z]:[\\/]`).MatchString(imported) {
		return []string{prefix + ": absolute imports are forbidden"}, ""
	}
	forbiddenBuiltin := map[string]bool{
		"child_process": true, "cluster": true, "dgram": true, "dns": true, "http": true, "http2": true, "https": true,
		"module": true, "net": true, "process": true, "tls": true, "vm": true, "wasi": true,
		"worker": true, "worker_threads": true,
	}
	allowedBuiltin := map[string]map[string]bool{
		"schema-validation": {"crypto": true, "fs": true, "fs/promises": true, "path": true, "url": true},
		"schema-codegen":    {"crypto": true, "fs": true, "fs/promises": true, "path": true, "url": true},
		"typescript-sdk":    {},
		"npm-packaging":     {"buffer": true, "crypto": true, "fs": true, "fs/promises": true, "path": true, "url": true},
	}
	if strings.HasPrefix(imported, "node:") {
		builtin := strings.TrimPrefix(imported, "node:")
		if forbiddenBuiltin[builtin] || !allowedBuiltin[source.ToolScope][builtin] {
			return []string{prefix + ": forbidden Node builtin for tool_scope " + source.ToolScope}, ""
		}
		return nil, ""
	}
	if strings.HasPrefix(imported, "./") || strings.HasPrefix(imported, "../") {
		resolved := resolveNodeImport(root, sourcePath, imported)
		if pathContainsSymlink(root, resolved) {
			return []string{prefix + ": Node import closure must not traverse symlinks"}, ""
		}
		target := artifactForExactPath(resolved, artifacts)
		if target.ID == "" {
			return []string{prefix + ": static relative import does not resolve to an exact concrete artifact"}, ""
		}
		if target.ImplementationRuntime != "node" || target.Kind != "tooling-helper" {
			return []string{prefix + ": static relative import target must be an exact Node tooling-helper"}, ""
		}
		compatibleScopes := map[string]map[string]bool{
			"schema-validation": {"schema-validation": true},
			"schema-codegen":    {"schema-validation": true, "schema-codegen": true},
			"typescript-sdk":    {"typescript-sdk": true},
			"npm-packaging":     {"npm-packaging": true},
		}
		if !compatibleScopes[source.ToolScope][target.ToolScope] {
			return []string{prefix + ": helper tool_scope is outside source closure"}, ""
		}
		if source.ID != target.ID && !contains(source.DerivesFrom, target.ID) {
			return []string{source.ID + " imports helper " + target.ID + " without manifest derives_from"}, ""
		}
		return nil, target.ID
	}
	if strings.HasPrefix(imported, "#") {
		return []string{prefix + ": package import aliases are forbidden; use an exact relative helper path"}, ""
	}
	packageName := imported
	if strings.HasPrefix(imported, "@") {
		parts := strings.Split(imported, "/")
		if len(parts) < 2 {
			return []string{prefix + ": malformed scoped bare import"}, ""
		}
		packageName = strings.Join(parts[:2], "/")
	} else if slash := strings.IndexByte(imported, '/'); slash >= 0 {
		packageName = imported[:slash]
	}
	if forbiddenBuiltin[packageName] {
		return []string{prefix + ": forbidden builtin import without node: prefix"}, ""
	}
	if strings.HasSuffix(strings.ToLower(imported), ".node") || map[string]bool{"bindings": true, "ffi-napi": true, "node-addon-api": true, "node-gyp-build": true}[packageName] {
		return []string{prefix + ": native addon imports are forbidden"}, ""
	}
	if !declaredPackages[packageName] {
		return []string{prefix + ": undeclared bare import"}, ""
	}
	return nil, ""
}

func pathContainsSymlink(root, relative string) bool {
	clean, err := structuredfile.SafeRelative(".", filepath.ToSlash(relative))
	if err != nil {
		return true
	}
	current := root
	for _, component := range strings.Split(filepath.FromSlash(clean), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func packageScriptProblems(scripts map[string]any, artifacts []Artifact) []string {
	problems := []string{}
	state := map[string]int{}
	var visit func(string)
	visit = func(name string) {
		if state[name] == 1 {
			problems = append(problems, "package script alias cycle at "+name)
			return
		}
		if state[name] == 2 {
			return
		}
		state[name] = 1
		command, ok := scripts[name].(string)
		if !ok {
			problems = append(problems, "package script "+name+" is not a string")
			state[name] = 2
			return
		}
		command = normalizeCommandText(command)
		problems = append(problems, nodeCommandIndirectionProblems("package script "+name, command)...)
		aliasPattern := regexp.MustCompile(`(?:npm|pnpm)\s+run\s+([a-zA-Z0-9:_-]+)|\byarn\s+([a-zA-Z0-9:_-]+)`)
		aliasMatches := aliasPattern.FindAllStringSubmatch(command, -1)
		for _, match := range aliasMatches {
			alias := match[1]
			if alias == "" {
				alias = match[2]
			}
			if _, exists := scripts[alias]; !exists {
				problems = append(problems, "package script "+name+" references unknown alias "+alias)
			} else {
				visit(alias)
			}
		}
		if len(aliasMatches) > 0 && !regexp.MustCompile("^(?:(?:npm|pnpm) run [a-zA-Z0-9:_-]+|yarn [a-zA-Z0-9:_-]+)(?: -- [^;&|`]*)?$").MatchString(strings.TrimSpace(command)) {
			problems = append(problems, "package script "+name+" alias is not an exact recursive template")
		}
		if regexp.MustCompile(`\b(?:npm\s+exec|npx|tsx|bun|deno)\b`).MatchString(command) {
			problems = append(problems, "package script "+name+" invokes non-allowlisted runtime")
		}
		if regexp.MustCompile(`\b(?:env|sh|bash|zsh)\b`).MatchString(command) && regexp.MustCompile(`\bnode(?:js)?\b`).MatchString(command) {
			problems = append(problems, "package script "+name+" wraps Node in a non-exact environment/shell template")
		}
		if strings.Contains(command, "$") && regexp.MustCompile(`\bnode(?:js)?\b`).MatchString(command) {
			problems = append(problems, "package script "+name+" uses variable substitution around Node")
		}
		if strings.ContainsAny(command, ";|&`") && regexp.MustCompile(`\b(?:node|nodejs)\b`).MatchString(command) {
			problems = append(problems, "package script "+name+" must be one exact Node/alias template")
		}
		problems = append(problems, nodeInvocationProblems("package script "+name, command, artifacts)...)
		state[name] = 2
	}
	for name := range scripts {
		visit(name)
	}
	return problems
}

func commandSourceProblems(name, source string, productionImage bool, artifacts ...[]Artifact) []string {
	p := []string{}
	source = normalizeCommandText(source)
	variables := map[string]string{}
	for _, match := range regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*(?:(?::|\?|\+)?=|:)[ \t]*([^ \t\r\n#]+)`).FindAllStringSubmatch(source, -1) {
		variables[match[1]] = match[2]
	}
	expanded := source
	for iteration := 0; iteration < 8; iteration++ {
		prior := expanded
		for name, value := range variables {
			expanded = strings.ReplaceAll(expanded, "$${"+name+"}", value)
			expanded = strings.ReplaceAll(expanded, "$("+name+")", value)
			expanded = strings.ReplaceAll(expanded, "${"+name+"}", value)
			expanded = regexp.MustCompile(`\$`+regexp.QuoteMeta(name)+`\b`).ReplaceAllString(expanded, value)
		}
		if expanded == prior {
			break
		}
	}
	p = append(p, nodeCommandIndirectionProblems(name, expanded)...)
	forbidden := regexp.MustCompile(`(?m)\b(node(?:js)?\s+(?:-e|--eval)|npm\s+exec|npx\b|tsx\b|bun\b|deno\b|node(?:js)?\s+scripts/release/|node(?:js)?\s+scripts/(?:blueprint-check|gate-check|planning-audit|verify-report|test-evidence-lineage|test-report-verifier))`)
	if forbidden.MatchString(expanded) {
		p = append(p, name+" invokes non-allowlisted Node runtime")
	}
	if len(artifacts) > 0 {
		p = append(p, nodeInvocationProblems(name, expanded, artifacts[0])...)
	}
	if productionImage && regexp.MustCompile(`(?m)\b(node|nodejs|npm|npx|tsx|bun|deno)\b`).MatchString(expanded) {
		p = append(p, name+" production image contains Node runtime/tooling")
	}
	return p
}

func normalizeCommandText(source string) string {
	source = strings.ReplaceAll(source, "\\\r\n", " ")
	source = strings.ReplaceAll(source, "\\\n", " ")
	return strings.ReplaceAll(source, "\r\n", "\n")
}

func nodeCommandIndirectionProblems(context, command string) []string {
	problems := []string{}
	lower := strings.ToLower(command)
	if regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_])(?:node_options|node_path|npm_config_node_options)[ \t]*[?:+]?=`).MatchString(lower) {
		problems = append(problems, context+" sets a forbidden Node loader/search-path environment variable")
	}
	nodeWord := regexp.MustCompile(`(?i)\bnode(?:js)?\b`)
	if containsObfuscatedNodeToken(command) {
		problems = append(problems, context+" obfuscates the Node command with shell quotes or escapes")
	}
	if !nodeWord.MatchString(command) && !containsObfuscatedNodeToken(command) {
		return problems
	}
	for _, line := range strings.Split(command, "\n") {
		if !nodeWord.MatchString(line) {
			continue
		}
		if strings.Contains(line, "$(") || strings.Contains(line, "`") {
			problems = append(problems, context+" resolves Node through shell substitution")
		}
		if strings.Contains(line, "$") {
			problems = append(problems, context+" resolves Node through non-exact variable expansion")
		}
	}
	if regexp.MustCompile(`(?mi)(?:^|[;|& \t])(?:export[ \t]+)?PATH[ \t]*[?:+]?=[^\r\n;|&]*`).MatchString(command) {
		problems = append(problems, context+" mutates PATH in a Node command surface")
	}
	if regexp.MustCompile(`(?mi)(?:alias[ \t]+node(?:js)?[ \t]*=|function[ \t]+node(?:js)?\b|\bnode(?:js)?[ \t]*\(\)|\bhash[ \t]+-[^\r\n;|&]*\bnode(?:js)?\b)`).MatchString(command) {
		problems = append(problems, context+" redefines or rebinds the Node command")
	}
	if regexp.MustCompile(`(?m)(?:^|[ \t;|&"'])(?:/|\.\.?/)[^ \t\r\n;|&"']*node(?:js)?(?:[ \t\r\n;|&"']|$)`).MatchString(command) {
		problems = append(problems, context+" invokes Node through a non-exact filesystem path")
	}
	if regexp.MustCompile(`(?m)\b(?:env|command|xargs|exec|sh|bash|zsh|sudo|nice|time|nohup|stdbuf|chrt|ionice|setsid|taskset|timeout|builtin)\b[^\n;|&]*\bnode(?:js)?\b`).MatchString(lower) {
		problems = append(problems, context+" wraps Node in a non-exact shell/environment command")
	}
	if regexp.MustCompile(`(?mi)(?:^|[;|& \t])(?:[A-Za-z_][A-Za-z0-9_]*=[^\r\n;|& \t]+[ \t]+)+node(?:js)?\b`).MatchString(command) {
		problems = append(problems, context+" prefixes Node with non-exact shell environment assignments")
	}
	return problems
}

func containsObfuscatedNodeToken(source string) bool {
	normalized := make([]byte, 0, len(source))
	originalOffsets := make([]int, 0, len(source))
	for index := 0; index < len(source); index++ {
		switch source[index] {
		case '\'', '"':
			continue
		case '\\':
			if index+1 < len(source) {
				index++
				normalized = append(normalized, source[index])
				originalOffsets = append(originalOffsets, index)
			}
		default:
			normalized = append(normalized, source[index])
			originalOffsets = append(originalOffsets, index)
		}
	}
	for _, match := range regexp.MustCompile(`(?i)\bnode(?:js)?\b`).FindAllIndex(normalized, -1) {
		start := originalOffsets[match[0]]
		end := originalOffsets[match[1]-1] + 1
		fragment := source[start:end]
		if strings.ContainsAny(fragment, `\'"`) || start > 0 && strings.ContainsRune(`'"\`, rune(source[start-1])) || end < len(source) && strings.ContainsRune(`'"`, rune(source[end])) {
			return true
		}
	}
	return false
}

func nodeInvocationProblems(context, command string, artifacts []Artifact) []string {
	problems := []string{}
	pattern := regexp.MustCompile(`(?m)(?:^|[ \t;|&])((?:node|nodejs)[ \t]+([^\n;|&]+))`)
	for _, match := range pattern.FindAllStringSubmatch(command, -1) {
		fields := strings.Fields(match[1])
		if len(fields) < 2 {
			problems = append(problems, context+" uses incomplete Node invocation")
			continue
		}
		if fields[0] != "node" {
			problems = append(problems, context+" invokes non-allowlisted Node runtime")
			continue
		}
		required := map[string]bool{
			"--permission":          false,
			"--allow-fs-read=.":     false,
			"--disable-proto=throw": false,
			"--no-addons":           false,
		}
		targetPath := ""
		for _, field := range fields[1:] {
			field = strings.Trim(field, `"'`)
			if targetPath != "" {
				continue
			}
			if strings.HasPrefix(field, "-") {
				if _, ok := required[field]; ok {
					required[field] = true
					continue
				}
				if strings.HasPrefix(field, "--allow-fs-write=build/") || field == "--allow-fs-write=build" {
					continue
				}
				problems = append(problems, context+" uses forbidden/unknown Node flag "+field)
				continue
			}
			targetPath = filepath.ToSlash(filepath.Clean(field))
		}
		for flag, present := range required {
			if !present {
				problems = append(problems, context+" Node invocation is missing safe flag "+flag)
			}
		}
		if targetPath == "" {
			problems = append(problems, context+" uses inline/unknown Node entry")
			continue
		}
		target := artifactForExactPath(targetPath, artifacts)
		if target.ID == "" || target.ImplementationRuntime != "node" || target.Kind == "tooling-helper" {
			problems = append(problems, context+" invokes unapproved exact Node entry "+targetPath)
		}
	}
	return problems
}
func localLinks(root string) []string {
	p := []string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			p = append(p, "walk/read "+path+": "+e.Error())
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil && d.IsDir() && (rel == ".git" || rel == "node_modules" || rel == "build") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") || strings.Contains(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			p = append(p, "read "+path+": "+readErr.Error())
			return nil
		}
		r := regexp.MustCompile(`\[[^\]]+\]\(([^)]*)\)`)
		for _, m := range r.FindAllStringSubmatch(string(b), -1) {
			target := strings.Trim(m[1], "<>")
			fields := strings.Fields(target)
			if len(fields) == 0 {
				p = append(p, path+": empty local link")
				continue
			}
			target = fields[0]
			target = strings.Split(strings.Split(target, "#")[0], "?")[0]
			if target == "" || strings.HasPrefix(target, "http:") || strings.HasPrefix(target, "https:") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, e := os.Stat(filepath.Join(filepath.Dir(path), target)); e != nil {
				rel, _ := filepath.Rel(root, path)
				p = append(p, filepath.ToSlash(rel)+" -> "+target)
			}
		}
		return nil
	})
	if err != nil {
		p = append(p, "markdown walk: "+err.Error())
	}
	return p
}
func countType(p []Phase, t string) int {
	n := 0
	for _, x := range p {
		if x.Type == t {
			n++
		}
	}
	return n
}
func remove(v []string, s string) []string {
	o := []string{}
	for _, x := range v {
		if x != s {
			o = append(o, x)
		}
	}
	return o
}
func replaceReport(m map[string]Artifact, a Artifact) map[string]Artifact {
	o := map[string]Artifact{}
	for k, v := range m {
		o[k] = v
	}
	o[a.ProducerPhase] = a
	return o
}
func replaceArtifact(m map[string]Artifact, a Artifact) []Artifact {
	o := []Artifact{}
	for _, v := range m {
		if v.ID == a.ID {
			v = a
		}
		o = append(o, v)
	}
	return o
}
func indexArtifacts(v []Artifact) map[string]Artifact {
	o := map[string]Artifact{}
	for _, a := range v {
		o[a.ID] = a
	}
	return o
}
