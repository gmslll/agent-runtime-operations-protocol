// Package specindex owns repository governance checks which are outside the
// IR-03 Node boundary. Node may validate schemas; Go owns decisions, graph and
// traceability policy, canonical bindings, publication language and links.
package specindex

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
)

func Run(root string) ([]report.Check, map[string]any, []string) {
	checks := []report.Check{}
	errors := []string{}
	record := func(name string, ok bool, detail string) {
		checks = append(checks, report.Check{Name: name, Passed: ok, Detail: detail})
		if !ok {
			errors = append(errors, name+": "+detail)
		}
	}
	decisions, err := os.ReadFile(filepath.Join(root, "docs/DECISIONS.md"))
	if err != nil {
		record("decision-definitions", false, err.Error())
	} else {
		definitions, duplicates := decisionDefinitions(string(decisions))
		record("decision-definitions", len(definitions) > 0 && len(duplicates) == 0, strings.Join(duplicates, ", "))
		unknown, walkErr := decisionReferenceProblems(root, definitions)
		if walkErr != nil {
			unknown = append(unknown, walkErr.Error())
		}
		record("decision-references", len(unknown) == 0, strings.Join(unknown, ", "))
	}
	manifestValue, _, manifestErr := schema.ValidatePath(root, "spec/schemas/artifact-manifest.schema.json", "spec/artifact-manifest.yaml")
	requirementsValue, _, requirementsErr := schema.ValidatePath(root, "spec/schemas/requirements.schema.json", "spec/requirements.yaml")
	conflictsValue, _, conflictsErr := schema.ValidatePath(root, "spec/schemas/conflicts.schema.json", "spec/conflicts.yaml")
	artifactProblems, artifactCount := artifactCatalogProblems(root, manifestValue)
	if manifestErr != nil {
		artifactProblems = append(artifactProblems, manifestErr.Error())
	}
	record("artifact-catalog-governance", len(artifactProblems) == 0, strings.Join(artifactProblems, "; "))
	requirementProblems, requirementCount := immutableRequirementProblems(requirementsValue, manifestValue)
	if requirementsErr != nil {
		requirementProblems = append(requirementProblems, requirementsErr.Error())
	}
	record("immutable-requirements-governance", requirementCount == 14 && len(requirementProblems) == 0, strings.Join(requirementProblems, "; "))
	conflictProblems, conflictCount := conflictLedgerProblems(conflictsValue, manifestValue)
	if conflictsErr != nil {
		conflictProblems = append(conflictProblems, conflictsErr.Error())
	}
	record("conflict-ledger-governance", len(conflictProblems) == 0, strings.Join(conflictProblems, "; "))
	bindings, bindingErr := canonicalBindingProblems(root)
	if bindingErr != nil {
		bindings = append(bindings, bindingErr.Error())
	}
	record("canonical-http-bindings", len(bindings) == 0, strings.Join(bindings, "; "))
	probe := legacyBindingProblems(`{"event_sink":"https://control.invalid/v1/agent-runs/run_1/events"}
{"cancel_url":"/v1/runs/run_1/cancel"}
POST /v1/runs/{run_id}/cancel`)
	record("canonical-binding-negative-probes", len(probe) == 3, fmt.Sprintf("%d legacy aliases rejected", len(probe)))
	v01, v01Err := publicationV01Problems(root)
	if v01Err != nil {
		v01 = append(v01, v01Err.Error())
	}
	record("controlled-publication-v01-language", len(v01) == 0, strings.Join(v01, "; "))
	record("controlled-publication-v01-negative-probe", len(publicationV01LineProblems("publish public v0.1 before v1", "probe")) == 1, "positive public v0.1 milestone rejected")
	links, linkErr := markdownLinkProblems(root)
	if linkErr != nil {
		links = append(links, linkErr.Error())
	}
	record("markdown-local-links-governance", len(links) == 0, strings.Join(links, "; "))
	return checks, map[string]any{"artifacts": artifactCount, "immutable_requirements": requirementCount, "conflicts": conflictCount}, errors
}

func decisionDefinitions(source string) (map[string]bool, []string) {
	re := regexp.MustCompile(`(?m)^\|\s*([DPC]-[0-9]{3})\s*\|`)
	counts := map[string]int{}
	for _, match := range re.FindAllStringSubmatch(source, -1) {
		counts[match[1]]++
	}
	definitions := map[string]bool{}
	duplicates := []string{}
	for id, count := range counts {
		definitions[id] = true
		if count != 1 {
			duplicates = append(duplicates, id)
		}
	}
	sort.Strings(duplicates)
	return definitions, duplicates
}

func controlledWalk(root string, match func(string) bool, visit func(string, []byte)) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() && (rel == ".git" || rel == "node_modules" || rel == "build" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "build/")) {
			return filepath.SkipDir
		}
		if entry.IsDir() || !match(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(rel, data)
		return nil
	})
}

func decisionReferenceProblems(root string, definitions map[string]bool) ([]string, error) {
	problems := []string{}
	re := regexp.MustCompile(`\b[DPC]-[0-9]{3}\b`)
	err := controlledWalk(root, func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		return ext == ".md" || ext == ".yaml" || ext == ".yml"
	}, func(path string, data []byte) {
		for _, id := range re.FindAllString(string(data), -1) {
			if !definitions[id] {
				problems = append(problems, path+":"+id)
			}
		}
	})
	return problems, err
}

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func array(value any) []any           { result, _ := value.([]any); return result }

func artifactCatalogProblems(root string, value any) ([]string, int) {
	document := object(value)
	artifacts := array(document["artifacts"])
	problems := []string{}
	ids := map[string]bool{}
	paths := map[string]bool{}
	graph := map[string][]string{}
	for _, raw := range artifacts {
		a := object(raw)
		id, _ := a["id"].(string)
		path, _ := a["path"].(string)
		if id == "" || ids[id] {
			problems = append(problems, "duplicate/missing artifact id "+id)
		}
		ids[id] = true
		if path == "" || paths[path] {
			problems = append(problems, "duplicate/missing artifact path "+path)
		}
		paths[path] = true
		if a["status"] == "present" {
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
				problems = append(problems, id+" missing present path")
			}
		}
		for _, dependency := range array(a["derives_from"]) {
			if text, ok := dependency.(string); ok {
				graph[id] = append(graph[id], text)
			}
		}
	}
	for id, dependencies := range graph {
		for _, dependency := range dependencies {
			if !ids[dependency] {
				problems = append(problems, id+" unknown dependency "+dependency)
			}
		}
	}
	state := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		if state[id] == 1 {
			problems = append(problems, "artifact cycle at "+id)
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		for _, dependency := range graph[id] {
			visit(dependency)
		}
		state[id] = 2
	}
	for id := range ids {
		visit(id)
	}
	return problems, len(artifacts)
}

func immutableRequirementProblems(value, manifest any) ([]string, int) {
	items := array(object(value)["requirements"])
	artifacts := map[string]bool{}
	for _, raw := range array(object(manifest)["artifacts"]) {
		id, _ := object(raw)["id"].(string)
		artifacts[id] = true
	}
	problems := []string{}
	ids := map[string]bool{}
	for _, raw := range items {
		item := object(raw)
		id, _ := item["id"].(string)
		if ids[id] {
			problems = append(problems, "duplicate requirement "+id)
		}
		ids[id] = true
		for _, artifact := range array(item["artifacts"]) {
			name, _ := artifact.(string)
			if !artifacts[name] {
				problems = append(problems, id+" unknown artifact "+name)
			}
		}
	}
	return problems, len(items)
}

func conflictLedgerProblems(value, manifest any) ([]string, int) {
	document := object(value)
	catalog := object(document["verification_catalog"])
	conflicts := array(document["conflicts"])
	artifactIDs := map[string]bool{}
	for _, raw := range array(object(manifest)["artifacts"]) {
		artifactIDs[fmt.Sprint(object(raw)["id"])] = true
	}
	problems := []string{}
	for name, raw := range catalog {
		owner := fmt.Sprint(object(raw)["owner_artifact"])
		if !artifactIDs[owner] {
			problems = append(problems, name+" unknown owner "+owner)
		}
	}
	for _, raw := range conflicts {
		conflict := object(raw)
		id := fmt.Sprint(conflict["id"])
		if conflict["status"] != "resolved" {
			problems = append(problems, id+" unresolved")
		}
		for _, verification := range array(conflict["verification"]) {
			name := fmt.Sprint(verification)
			if catalog[name] == nil {
				problems = append(problems, id+" unknown verification "+name)
			}
		}
	}
	return problems, len(conflicts)
}

func legacyBindingProblems(source string) []string {
	problems := []string{}
	for label, pattern := range map[string]string{"event_sink": `"event_sink"`, "cancel_url": `"cancel_url"`, "cancel endpoint": `POST /v1/runs/\{run_id\}/cancel`} {
		if regexp.MustCompile(pattern).MatchString(source) {
			problems = append(problems, label)
		}
	}
	return problems
}

func canonicalBindingProblems(root string) ([]string, error) {
	paths := []string{"docs/PROTOCOL_SPECIFICATION.md", "docs/RUN_AND_STREAMING.md", "docs/DECISIONS.md"}
	corpus := ""
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		corpus += string(data) + "\n"
	}
	problems := legacyBindingProblems(corpus)
	for _, required := range []string{"event_batch_url", "POST /v1/agent-runs/{run_id}/events:batch", "POST /v1/agent-runs/{run_id}/commands", "POST /v1/runs/{run_id}/commands"} {
		if !strings.Contains(corpus, required) {
			problems = append(problems, "missing "+required)
		}
	}
	return problems, nil
}

func publicationV01LineProblems(source, label string) []string {
	problems := []string{}
	positive := regexp.MustCompile(`(?i)\bv0\.1\b`)
	allowed := regexp.MustCompile(`(?i)(取消独立公共 v0\.1|不用 v0\.1|不得用 v0\.1|no (?:separate )?public v0\.1)`)
	for lineNumber, line := range strings.Split(source, "\n") {
		if positive.MatchString(line) && !allowed.MatchString(line) {
			problems = append(problems, fmt.Sprintf("%s:%d", label, lineNumber+1))
		}
	}
	return problems
}
func publicationV01Problems(root string) ([]string, error) {
	problems := []string{}
	err := controlledWalk(root, func(path string) bool {
		return path == "README.md" || path == "SECURITY.md" || path == "CONTRIBUTING.md" || (strings.HasPrefix(path, "docs/") && strings.HasSuffix(path, ".md"))
	}, func(path string, data []byte) {
		problems = append(problems, publicationV01LineProblems(string(data), path)...)
	})
	return problems, err
}

func markdownLinkProblems(root string) ([]string, error) {
	problems := []string{}
	pattern := regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)
	err := controlledWalk(root, func(path string) bool { return strings.HasSuffix(strings.ToLower(path), ".md") }, func(path string, data []byte) {
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			target := strings.Trim(strings.TrimSpace(match[1]), "<>")
			if fields := strings.Fields(target); len(fields) > 0 {
				target = fields[0]
			} else {
				continue
			}
			target = strings.Split(strings.Split(target, "#")[0], "?")[0]
			if target == "" || strings.HasPrefix(target, "http:") || strings.HasPrefix(target, "https:") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			decoded, err := url.PathUnescape(target)
			if err != nil {
				problems = append(problems, path+" invalid link "+target)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, filepath.Dir(path), filepath.FromSlash(decoded))); err != nil {
				problems = append(problems, path+" -> "+target)
			}
		}
	})
	return problems, err
}
