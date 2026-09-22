// Package blueprint validates the schedulable implementation graph and the
// repository-wide implementation-runtime boundary.
package blueprint

import (
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
)

type Artifact struct {
	ID                    string   `yaml:"id"`
	Path                  string   `yaml:"path"`
	Status                string   `yaml:"status"`
	Kind                  string   `yaml:"kind"`
	Authority             string   `yaml:"authority"`
	Owner                 string   `yaml:"owner"`
	OwnerPhase            string   `yaml:"owner_phase"`
	CompletionPhase       string   `yaml:"completion_phase"`
	ProducerPhase         string   `yaml:"producer_phase"`
	AcceptanceTest        string   `yaml:"acceptance_test"`
	PathRole              string   `yaml:"path_role"`
	FutureAction          string   `yaml:"future_action"`
	FutureOwner           string   `yaml:"future_owner"`
	FutureOwnerPhase      string   `yaml:"future_owner_phase"`
	FutureAcceptanceTest  string   `yaml:"future_acceptance_test"`
	ImplementationRuntime string   `yaml:"implementation_runtime"`
	ToolScope             string   `yaml:"tool_scope"`
	DerivesFrom           []string `yaml:"derives_from"`
	RuntimeInputs         []string `yaml:"runtime_inputs"`
	FutureArtifacts       []string `yaml:"future_artifacts"`
}
type Manifest struct {
	Artifacts []Artifact `yaml:"artifacts"`
}
type Requirements struct {
	Requirements []struct {
		ID    string   `yaml:"id"`
		Tests []string `yaml:"tests"`
	} `yaml:"requirements"`
	VerificationCatalog map[string]any `yaml:"verification_catalog"`
}
type Baseline struct{ ID, Action string }
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
					p.Baselines = append(p.Baselines, Baseline{v[:open], v[open+1 : len(v)-1]})
				} else {
					p.Baselines = append(p.Baselines, Baseline{v, "invalid"})
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
		if ownerPhase[p.Owner] != "" {
			metadata = append(metadata, "duplicate owner "+p.Owner)
		}
		ownerPhase[p.Owner] = p.ID
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
		if !strings.Contains(p.Acceptance, "build/reports/"+p.ID+"/report.json") || !strings.Contains(p.Acceptance, "junit.xml") {
			metadata = append(metadata, p.ID+" missing report paths")
		}
	}
	record("dynamic-phase-sequence", len(phases) == 53 && len(metadata) == 0, fmt.Sprintf("%d phases; %s", len(phases), strings.Join(metadata, "; ")))
	depProblems := []string{}
	deps := map[string][]string{}
	phaseRef := regexp.MustCompile(`P[0-9]{2}`)
	for _, p := range phases {
		d := phaseRef.FindAllString(p.Dependencies, -1)
		if p.Dependencies == "none" {
			d = nil
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
		if a.OwnerPhase != "" {
			if planOwner[a.ID] != a.OwnerPhase {
				artifactProblems = append(artifactProblems, a.ID+" plan owner mismatch")
			}
			if byPhase[a.OwnerPhase].Owner != a.Owner {
				artifactProblems = append(artifactProblems, a.ID+" capability owner mismatch")
			}
		}
		if a.FutureOwnerPhase != "" {
			b, ok := planBaseline[a.ID]
			if !ok || b.Action != a.FutureAction || a.FutureOwner != byPhase[a.FutureOwnerPhase].Owner {
				artifactProblems = append(artifactProblems, a.ID+" future baseline mismatch")
			}
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
	}
	record("artifact-lifecycle-temporal-bidirectional", len(artifactProblems) == 0, fmt.Sprintf("%d artifacts; %s", len(manifest.Artifacts), strings.Join(artifactProblems, "; ")))
	dagProblems := graphProblems(manifest.Artifacts, byID, nil)
	record("artifact-runtime-combined-dag", len(dagProblems) == 0, strings.Join(dagProblems, "; "))
	reports := map[string]Artifact{}
	for _, a := range manifest.Artifacts {
		if a.Kind == "machine-reports" {
			reports[a.ProducerPhase] = a
		}
	}
	reportProblems := []string{}
	for _, p := range phases {
		r := reports[p.ID]
		if r.ID == "" || r.AcceptanceTest != "make-"+firstMake(p.Acceptance) {
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
	treeNeedles := []string{"openapi/fragments/control-plane/", "sdk/go/generated/", "sdk/python/src/arop/", "sdk/typescript/", "cmd/arop-conformance/", "reference/control-plane/", "internal/domain/", "internal/ports/", "migrations/sqlite/", "migrations/postgres/", "deployments/quickstart/", "deployments/production-reference/", "internal/tooling/"}
	missing := []string{}
	for _, n := range treeNeedles {
		if !strings.Contains(layout, n) {
			missing = append(missing, n)
		}
	}
	record("target-tree-declaration", len(missing) == 0, strings.Join(missing, ", "))
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
	allowedNode := map[string]bool{"schema-validation": true, "schema-codegen": true, "typescript-sdk": true, "npm-packaging": true}
	execKind := regexp.MustCompile(`tooling|validator|aggregator|build-primitive`)
	casePaths := map[string]string{}
	for _, a := range artifacts {
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
				resolved, resolveErr := filepath.EvalSymlinks(absolute)
				rel, relErr := filepath.Rel(root, resolved)
				if resolveErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					p = append(p, a.ID+" symlink escapes repository")
				}
			}
		}
	}
	var pkg map[string]any
	if json.Unmarshal([]byte(packageJSON), &pkg) == nil {
		scripts, _ := pkg["scripts"].(map[string]any)
		for name, raw := range scripts {
			v := fmt.Sprint(raw)
			if regexp.MustCompile(`(?i)(scripts/release/|blueprint-check\.mjs|verify-report\.mjs|planning-audit\.mjs|gate-check\.mjs|test-evidence-lineage\.mjs|test-report-verifier\.mjs)`).MatchString(v) {
				p = append(p, "package script "+name+" aliases forbidden Node governance/release tooling")
			}
		}
	}
	for _, issue := range commandSourceProblems("Makefile", makefile, false) {
		p = append(p, issue)
	}
	if root != "" {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() && (rel == ".git" || rel == "node_modules" || rel == "build" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "build/")) {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			lower := strings.ToLower(rel)
			ext := strings.ToLower(filepath.Ext(rel))
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			text := string(data)
			isNode := ext == ".mjs" || ext == ".js" || ext == ".cjs" || ext == ".ts" || strings.HasPrefix(text, "#!/usr/bin/env node") || strings.HasPrefix(text, "#!/usr/bin/node")
			if isNode {
				owner := artifactForPath(rel, artifacts)
				if owner.ID == "" || owner.ImplementationRuntime != "node" || !allowedNode[owner.ToolScope] {
					p = append(p, rel+" executable Node file is not covered by an allowed Node artifact")
				}
				if strings.HasPrefix(lower, "scripts/release/") {
					p = append(p, rel+" JavaScript is forbidden under scripts/release")
				}
				for _, imported := range nodeRelativeImports(text) {
					resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(rel), imported)))
					target := artifactForPath(resolved, artifacts)
					if target.ID != "" && target.ImplementationRuntime != "node" && target.ToolScope != "schema-validation" && target.ToolScope != "schema-codegen" && target.ToolScope != "typescript-sdk" && target.ToolScope != "npm-packaging" {
						p = append(p, rel+" imports non-Node-boundary artifact "+target.ID)
					}
					if owner.ID != "" && target.ID != "" && owner.ID != target.ID && !contains(owner.DerivesFrom, target.ID) {
						p = append(p, owner.ID+" imports helper "+target.ID+" without manifest derives_from")
					}
				}
			}
			if strings.HasPrefix(rel, ".github/workflows/") && (ext == ".yml" || ext == ".yaml") {
				p = append(p, commandSourceProblems(rel, text, false)...)
			}
			if strings.Contains(strings.ToLower(filepath.Base(rel)), "dockerfile") {
				p = append(p, commandSourceProblems(rel, text, true)...)
			}
			return nil
		})
	}
	return p
}
func boundaryNegativeProbes() []string {
	p := []string{}
	cases := []Artifact{{ID: "evil-release", Path: "scripts/release/evil.mjs", Kind: "release-validator", ImplementationRuntime: "node", ToolScope: "release"}, {ID: "fake-python", Path: "sdk/python/build.mjs", Kind: "build-primitive", ImplementationRuntime: "python", ToolScope: "python-package"}, {ID: "fake-go", Path: "tools/release.js", Kind: "release-tooling", ImplementationRuntime: "go", ToolScope: "release"}, {ID: "missing-meta", Path: "tools/run", Kind: "release-tooling"}, {ID: "hidden-release", Path: "scripts/schema/hidden.cjs", Kind: "release-tooling", ImplementationRuntime: "node", ToolScope: "release"}, {ID: "node-schema", Path: "scripts/schema.mjs", Kind: "tooling", ImplementationRuntime: "node", ToolScope: "schema-validation", DerivesFrom: []string{"go-release"}}, {ID: "go-release", Path: "internal/tooling/release/x.go", Kind: "release-tooling", ImplementationRuntime: "go", ToolScope: "release", DerivesFrom: []string{"structured-file-tools"}}, {ID: "structured-file-tools", Path: "scripts/lib/repository.mjs", Kind: "tooling", ImplementationRuntime: "node", ToolScope: "schema-validation"}, {ID: "npm-package-primitive", Path: "sdk/typescript/build.mjs", Kind: "build-primitive", ImplementationRuntime: "node", ToolScope: "npm-packaging", DerivesFrom: []string{"python-package"}}, {ID: "python-package", Path: "sdk/python/build.py", Kind: "build-primitive", ImplementationRuntime: "python", ToolScope: "python-package"}, {ID: "case-a", Path: "Tools/A.go"}, {ID: "case-b", Path: "tools/a.go"}}
	by := map[string]Artifact{}
	for _, a := range cases {
		by[a.ID] = a
	}
	if len(runtimeBoundary("", cases, by, "", `{"scripts":{}}`)) < 9 {
		p = append(p, "path/runtime/scope probes escaped")
	}
	for _, s := range []string{`{"scripts":{"evil":"node scripts/release/publish.mjs"}}`, `{"scripts":{"evil":"node scripts/gate-check.mjs"}}`, `{"scripts":{"evil":"npm run hidden","hidden":"node scripts/release/publish.mjs"}}`} {
		if len(runtimeBoundary("", nil, nil, "", s)) == 0 {
			p = append(p, "package alias probe escaped")
		}
	}
	for _, m := range []string{"x:\n\tnode -e 'x'", "x:\n\tnpx tsx evil.ts", "x:\n\tbun evil.ts", "x:\n\tdeno run evil.ts", "x:\n\tnode scripts/release/evil.mjs"} {
		if len(runtimeBoundary("", nil, nil, m, `{"scripts":{}}`)) == 0 {
			p = append(p, "Make runtime alias probe escaped")
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
func nodeRelativeImports(source string) []string {
	r := regexp.MustCompile(`(?m)(?:from\s+|import\s*\()?["'](\.{1,2}/[^"']+)["']`)
	out := []string{}
	for _, m := range r.FindAllStringSubmatch(source, -1) {
		out = append(out, m[1])
	}
	return out
}
func commandSourceProblems(name, source string, productionImage bool) []string {
	p := []string{}
	forbidden := regexp.MustCompile(`(?m)\b(node\s+-e|npm\s+exec|npx\b|tsx\b|bun\b|deno\b|node\s+scripts/release/|node\s+scripts/(?:blueprint-check|gate-check|planning-audit|verify-report|test-evidence-lineage|test-report-verifier))`)
	if forbidden.MatchString(source) {
		p = append(p, name+" invokes non-allowlisted Node runtime")
	}
	if productionImage && regexp.MustCompile(`(?m)\b(node|npm|npx|tsx|bun|deno)\b`).MatchString(source) {
		p = append(p, name+" production image contains Node runtime/tooling")
	}
	return p
}
func localLinks(root string) []string {
	p := []string{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil || d.IsDir() || !strings.HasSuffix(path, ".md") || strings.Contains(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
			return nil
		}
		b, _ := os.ReadFile(path)
		r := regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)
		for _, m := range r.FindAllStringSubmatch(string(b), -1) {
			target := strings.Trim(m[1], "<>")
			target = strings.Fields(target)[0]
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
