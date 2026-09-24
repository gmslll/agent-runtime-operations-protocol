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
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	Path             = "reference/control-plane/internal/identity/testdata/transition/p10-baseline-transition-waiver.json"
	OldP09WaiverPath = "reference/control-plane/internal/storage/migrate/testdata/engine-versions/baseline-transition-waiver.json"
)

func main() {
	root, err := structuredfile.FindRoot(".")
	if err != nil {
		fatal(err)
	}
	if len(os.Args) != 2 {
		fatal(errors.New("usage: transitioncheck <validate|negative|allowed-packages>"))
	}
	switch os.Args[1] {
	case "validate":
		err = Validate(root, nil)
	case "negative":
		err = Negative(root, nil)
	case "allowed-packages":
		var packages []string
		packages, err = allowedPackages(root, "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane")
		if err == nil {
			for _, pkg := range packages {
				fmt.Println(pkg)
			}
		}
	default:
		err = errors.New("unknown transitioncheck mode")
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

type Source struct {
	Path           string   `json:"path"`
	Scope          string   `json:"scope"`
	ChangeType     string   `json:"change_type"`
	BaselineSHA256 *string  `json:"baseline_sha256"`
	CurrentSHA256  *string  `json:"current_sha256"`
	TouchCommits   []string `json:"touch_commits"`
	ArtifactIDs    []string `json:"artifact_ids"`
	OwnerPhases    []string `json:"owner_phases"`
}

type Acceptance struct {
	Phase   string `json:"phase"`
	Command string `json:"command"`
	Report  string `json:"report"`
}

type Waiver struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
		OwnershipTransferred bool   `json:"ownership_transferred"`
	} `json:"policy"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string     `json:"affected_artifacts"`
	SourceClosure     []Source     `json:"source_closure"`
	Acceptance        []Acceptance `json:"acceptance"`
	Constraints       []string     `json:"constraints"`
}

type manifest struct {
	SchemaVersion  int               `json:"schema_version"`
	CatalogID      string            `json:"catalog_id"`
	Updated        string            `json:"updated"`
	Purpose        string            `json:"purpose"`
	AuthorityChain []map[string]any  `json:"authority_chain"`
	Statuses       map[string]string `json:"statuses"`
	Artifacts      []artifact        `json:"artifacts"`
}
type artifact struct {
	ID                    string   `json:"id"`
	Path                  string   `json:"path"`
	Status                string   `json:"status"`
	Kind                  string   `json:"kind"`
	Authority             string   `json:"authority"`
	Language              string   `json:"language"`
	Capabilities          []string `json:"capabilities"`
	Owner                 string   `json:"owner"`
	OwnerPhase            string   `json:"owner_phase"`
	CompletionPhase       string   `json:"completion_phase"`
	ProducerPhase         string   `json:"producer_phase"`
	AcceptanceTest        string   `json:"acceptance_test"`
	Exposure              string   `json:"exposure"`
	PathRole              string   `json:"path_role"`
	FutureAction          string   `json:"future_action"`
	FutureOwner           string   `json:"future_owner"`
	FutureOwnerPhase      string   `json:"future_owner_phase"`
	FutureAcceptanceTest  string   `json:"future_acceptance_test"`
	ImplementationRuntime string   `json:"implementation_runtime"`
	ToolScope             string   `json:"tool_scope"`
	DerivesFrom           []string `json:"derives_from"`
	RuntimeInputs         []string `json:"runtime_inputs"`
	FutureArtifacts       []string `json:"future_artifacts"`
}

func expectedDeclared() Waiver {
	var w Waiver
	w.SchemaVersion = 1
	w.WaiverID = "P10-P08-P09-BASELINE-TRANSITION-001"
	w.Status = "declared"
	w.Policy.OwnerPhaseSemantics = "first-introduction-and-accountability"
	w.Transition.FromPhases = []string{"P08", "P09"}
	w.Transition.ToPhase = "P10"
	w.Transition.Reason = "P10 composes identity, credential, SecretRef, and identity migrations through the accepted P08/P09 reference implementation without transferring accountable ownership or weakening historical phase evidence."
	s := func(path, scope, change string, ids, phases []string) Source {
		return Source{Path: path, Scope: scope, ChangeType: change, TouchCommits: []string{}, ArtifactIDs: ids, OwnerPhases: phases}
	}
	w.SourceClosure = []Source{
		s("reference/control-plane/cmd/aropd/main.go", "early-owned", "modify", []string{"phase-report-p08", "reference-control-plane-server"}, []string{"P08"}),
		s("reference/control-plane/cmd/aropd/main_test.go", "early-owned", "modify", []string{"phase-report-p08", "reference-control-plane-server"}, []string{"P08"}),
		s("reference/control-plane/internal/app/platform/testdata/harness/main.go", "early-owned", "modify", []string{"control-plane-platform-foundation", "phase-report-p08"}, []string{"P08"}),
		s(Path, "carrier", "add", []string{"p10-baseline-transition-waiver", "phase-report-p10"}, []string{"P10"}),
		s("reference/control-plane/internal/storage/migrate/catalog_closure.go", "early-owned", "modify", []string{"migration-engine", "phase-report-p09"}, []string{"P09"}),
		s("reference/control-plane/internal/storage/migrate/catalog_closure_test.go", "early-owned", "modify", []string{"migration-engine", "phase-report-p09"}, []string{"P09"}),
		s("reference/control-plane/internal/storage/migrate/production_catalog.go", "early-owned", "modify", []string{"migration-engine", "phase-report-p09"}, []string{"P09"}),
		s("reference/control-plane/internal/storage/migrate/testdata/engine-versions/composition/p09_storage_acceptance_test.go", "early-owned", "modify", []string{"migration-engine-fixture-versions", "phase-report-p09"}, []string{"P09"}),
		s("reference/control-plane/internal/storage/migrate/testdata/engine-versions/harness/main.go", "early-owned", "modify", []string{"migration-engine-fixture-versions", "phase-report-p09"}, []string{"P09"}),
		s("reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go", "early-owned", "add", []string{"migration-engine-fixture-versions", "phase-report-p09"}, []string{"P09"}),
	}
	w.AffectedArtifacts = unionArtifacts(w.SourceClosure)
	w.Acceptance = []Acceptance{{"P08", "make test-control-plane-platform", "build/reports/P08/report.json"}, {"P09", "make test-storage-migrations", "build/reports/P09/report.json"}, {"P10", "make test-identity-secrets", "build/reports/P10/report.json"}}
	w.Constraints = []string{"frozen-planning-inputs-remain-unchanged", "no-memory-fallback-in-durable-modes", "no-public-secret-value-api", "owner-phase-and-exposure-remain-unchanged", "p08-p09-p10-regressions-must-pass-on-one-head", "p09-historical-closure-has-a-fixed-endpoint", "p10-report-runtime-inputs-remain-empty", "validated-status-requires-exact-git-and-manifest-closure"}
	return w
}

func Load(root string) (Waiver, error) {
	data, err := readRegular(root, Path)
	if err != nil {
		return Waiver{}, err
	}
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return Waiver{}, fmt.Errorf("strict parse P10 waiver: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var w Waiver
	if err := decoder.Decode(&w); err != nil {
		return Waiver{}, fmt.Errorf("strict decode P10 waiver: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Waiver{}, errors.New("P10 waiver contains trailing JSON")
		}
		return Waiver{}, err
	}
	return w, nil
}

func Validate(root string, staticInputs []string) error {
	w, err := Load(root)
	if err != nil {
		return err
	}
	return validate(root, staticInputs, w)
}

func validate(root string, staticInputs []string, w Waiver) error {
	problems := []string{}
	if w.SchemaVersion != 1 || w.WaiverID != "P10-P08-P09-BASELINE-TRANSITION-001" || (w.Status != "declared" && w.Status != "validated") {
		problems = append(problems, "identity/version/status mismatch")
	}
	if w.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || w.Policy.OwnershipTransferred || !reflect.DeepEqual(w.Transition.FromPhases, []string{"P08", "P09"}) || w.Transition.ToPhase != "P10" || w.Transition.Reason == "" {
		problems = append(problems, "transition/ownership mismatch")
	}
	if !canonical(w.AffectedArtifacts) || !canonical(w.Constraints) || !canonicalSources(w.SourceClosure) {
		problems = append(problems, "arrays are not unique canonical order")
	}
	wantAcceptance := expectedDeclared().Acceptance
	if !reflect.DeepEqual(w.Acceptance, wantAcceptance) || !reflect.DeepEqual(w.Constraints, expectedDeclared().Constraints) {
		problems = append(problems, "acceptance/constraints mismatch")
	}
	if w.Status == "declared" && !reflect.DeepEqual(w, expectedDeclared()) {
		problems = append(problems, "declared waiver differs from canonical declaration")
	}
	if len(staticInputs) != 0 && !contains(staticInputs, Path) {
		problems = append(problems, "waiver absent from static inputs")
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(Path)))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		problems = append(problems, "waiver must be regular 0644")
	}
	m, err := loadManifest(root)
	if err != nil {
		return err
	}
	for _, source := range w.SourceClosure {
		if source.ChangeType != "add" && source.ChangeType != "modify" && source.ChangeType != "delete" {
			problems = append(problems, "invalid change_type for "+source.Path)
		}
		ids, phases, scope, err := mapSource(m, source.Path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !reflect.DeepEqual(source.ArtifactIDs, ids) || !reflect.DeepEqual(source.OwnerPhases, phases) || source.Scope != scope {
			problems = append(problems, "manifest source mapping mismatch for "+source.Path)
		}
		if w.Status == "declared" && (source.BaselineSHA256 != nil || source.CurrentSHA256 != nil || len(source.TouchCommits) != 0) {
			problems = append(problems, "declared source contains final evidence")
		}
	}
	if !reflect.DeepEqual(w.AffectedArtifacts, unionArtifacts(w.SourceClosure)) {
		problems = append(problems, "affected_artifacts do not equal independently mapped source DAG closure")
	}
	if len(problems) == 0 {
		if err := validateGit(root, w, m); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func Negative(root string, staticInputs []string) error {
	w, err := Load(root)
	if err != nil {
		return err
	}
	check := func(name string, candidate Waiver) error {
		if validate(root, staticInputs, candidate) == nil {
			return errors.New("production validator accepted " + name)
		}
		return nil
	}
	m, err := loadManifest(root)
	if err != nil {
		return err
	}
	intro, err := uniqueIntroduction(root, Path)
	if err != nil {
		return err
	}
	baseBytes, err := git(root, "rev-parse", intro+"^")
	if err != nil {
		return err
	}
	discovered, err := discoverSources(root, strings.TrimSpace(string(baseBytes)), m)
	if err != nil {
		return err
	}
	validated := clone(w)
	validated.Status = "validated"
	validated.SourceClosure = nil
	for _, path := range sortedSourcePaths(discovered) {
		validated.SourceClosure = append(validated.SourceClosure, discovered[path])
	}
	validated.AffectedArtifacts = unionArtifacts(validated.SourceClosure)
	if err := validate(root, staticInputs, validated); err != nil {
		return fmt.Errorf("production validator rejected synthesized valid closure: %w", err)
	}
	for i, s := range validated.SourceClosure {
		c := clone(validated)
		c.SourceClosure = append(c.SourceClosure[:i], c.SourceClosure[i+1:]...)
		c.AffectedArtifacts = unionArtifacts(c.SourceClosure)
		if err := check("validated omitted source "+s.Path, c); err != nil {
			return err
		}
	}
	for i, a := range validated.AffectedArtifacts {
		c := clone(validated)
		c.AffectedArtifacts = append(c.AffectedArtifacts[:i], c.AffectedArtifacts[i+1:]...)
		if err := check("validated omitted artifact "+a, c); err != nil {
			return err
		}
	}
	for i := range validated.Acceptance {
		c := clone(validated)
		c.Acceptance = append(c.Acceptance[:i], c.Acceptance[i+1:]...)
		if err := check("validated omitted acceptance", c); err != nil {
			return err
		}
	}
	for i := range validated.Constraints {
		c := clone(validated)
		c.Constraints = append(c.Constraints[:i], c.Constraints[i+1:]...)
		if err := check("validated omitted constraint", c); err != nil {
			return err
		}
	}
	mutations := []Waiver{clone(validated), clone(validated), clone(validated), clone(validated), clone(validated), clone(validated), clone(validated), clone(validated), clone(validated)}
	mutations[0].SourceClosure = append(mutations[0].SourceClosure, mutations[0].SourceClosure[0])
	mutations[1].SourceClosure[0].ArtifactIDs = []string{"phase-report-p10", "reference-control-plane-server"}
	mutations[2].SourceClosure[0].ChangeType = "delete"
	mutations[3].SourceClosure[0].ChangeType = "rename"
	mutations[4].Acceptance[0].Command = "make wrong"
	mutations[5].Constraints[0] = "wrong"
	mutations[6].SourceClosure = append(mutations[6].SourceClosure, Source{Path: "unexpected.go", Scope: "early-owned", ChangeType: "add", TouchCommits: []string{}, ArtifactIDs: []string{"migration-engine"}, OwnerPhases: []string{"P09"}})
	sort.Slice(mutations[6].SourceClosure, func(i, j int) bool { return mutations[6].SourceClosure[i].Path < mutations[6].SourceClosure[j].Path })
	mutations[7].SourceClosure[0].TouchCommits = []string{}
	bad := "sha256:" + strings.Repeat("0", 64)
	mutations[8].SourceClosure[0].CurrentSHA256 = &bad
	for i, c := range mutations {
		if err := check(fmt.Sprintf("mutation %d", i), c); err != nil {
			return err
		}
	}
	if _, err := decodeBytes([]byte(`{"schema_version":1,"schema_version":1}`)); err == nil {
		return errors.New("duplicate key accepted")
	}
	data, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(Path)))
	if _, err := decodeBytes(append(data, []byte(` {}`)...)); err == nil {
		return errors.New("trailing value accepted")
	}
	return nil
}

func AllowsP10ProductionPackage(root, importPath, nestedModule string) bool {
	packages, err := allowedPackages(root, nestedModule)
	if err != nil {
		return false
	}
	for _, pkg := range packages {
		if pkg == importPath {
			return true
		}
	}
	return false
}

func allowedPackages(root, nestedModule string) ([]string, error) {
	w, err := Load(root)
	if err != nil {
		return nil, err
	}
	if w.Status != "validated" {
		return []string{}, nil
	}
	if err := validate(root, nil, w); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, source := range w.SourceClosure {
		if source.Scope != "p10-production" || strings.HasSuffix(source.Path, "_test.go") || !strings.HasSuffix(source.Path, ".go") {
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(source.Path))
		if strings.HasPrefix(directory, "reference/control-plane/") {
			set[nestedModule+"/"+strings.TrimPrefix(directory, "reference/control-plane/")] = true
		}
	}
	return keys(set), nil
}

func P09Boundary(root string) (string, string, error) {
	intro, err := uniqueIntroduction(root, Path)
	if err != nil {
		return "", "", err
	}
	parentBytes, err := git(root, "rev-parse", intro+"^")
	if err != nil {
		return "", "", err
	}
	parent := strings.TrimSpace(string(parentBytes))
	endpointBytes, err := git(root, "log", "-1", "--format=%H", parent, "--", OldP09WaiverPath)
	if err != nil {
		return "", "", err
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	if len(endpoint) != 40 {
		return "", "", errors.New("missing P09 endpoint before P10 baseline")
	}
	later, err := git(root, "log", "--format=%H", endpoint+"..HEAD", "--", OldP09WaiverPath)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(string(later)) != "" {
		return "", "", errors.New("old P09 waiver changed after its accepted endpoint")
	}
	return parent, endpoint, nil
}

func validateGit(root string, w Waiver, m manifest) error {
	intro, err := uniqueIntroduction(root, Path)
	if err != nil {
		return err
	}
	baseBytes, err := git(root, "rev-parse", intro+"^")
	if err != nil {
		return err
	}
	baseline := strings.TrimSpace(string(baseBytes))
	discovered, err := discoverSources(root, baseline, m)
	if err != nil {
		return err
	}
	declared := map[string]Source{}
	for _, s := range w.SourceClosure {
		declared[s.Path] = s
	}
	if w.Status == "declared" {
		for path, got := range discovered {
			want, ok := declared[path]
			if !ok || want.ChangeType != got.ChangeType || want.Scope != got.Scope {
				return fmt.Errorf("declared waiver does not cover governed touch %s", path)
			}
		}
		return nil
	}
	if len(discovered) != len(declared) {
		return fmt.Errorf("validated closure=%d discovered=%d", len(declared), len(discovered))
	}
	for path, got := range discovered {
		want, ok := declared[path]
		if !ok || !reflect.DeepEqual(want, got) {
			return fmt.Errorf("validated source mismatch for %s", path)
		}
	}
	return nil
}

func discoverSources(root, baseline string, m manifest) (map[string]Source, error) {
	commitsBytes, err := git(root, "rev-list", "--reverse", baseline+"..HEAD")
	if err != nil {
		return nil, err
	}
	commits := strings.Fields(string(commitsBytes))
	touches := map[string][]string{}
	for _, commit := range commits {
		parentBytes, err := git(root, "rev-parse", commit+"^")
		if err != nil {
			return nil, err
		}
		parent := strings.TrimSpace(string(parentBytes))
		diff, err := git(root, "diff-tree", "--no-commit-id", "--name-status", "-r", "-z", "--no-renames", parent, commit, "--")
		if err != nil {
			return nil, err
		}
		parts := bytes.Split(diff, []byte{0})
		for i := 0; i+1 < len(parts); i += 2 {
			path := filepath.ToSlash(string(parts[i+1]))
			if path == "" {
				continue
			}
			if _, _, _, err := mapSource(m, path); err == nil {
				touches[path] = append(touches[path], commit)
			}
		}
	}
	result := map[string]Source{}
	for path, commits := range touches {
		ids, phases, scope, err := mapSource(m, path)
		if err != nil {
			return nil, err
		}
		base, baseOK, err := gitBlob(root, baseline, path)
		if err != nil {
			return nil, err
		}
		current, currentOK, err := worktreeBlob(root, path)
		if err != nil {
			return nil, err
		}
		change := "modify"
		if !baseOK {
			change = "add"
		}
		if !currentOK {
			change = "delete"
		}
		var baseHash, currentHash *string
		if baseOK {
			v := report.Hash(base)
			baseHash = &v
		}
		if currentOK {
			v := report.Hash(current)
			currentHash = &v
		}
		result[path] = Source{path, scope, change, baseHash, currentHash, commits, ids, phases}
	}
	return result, nil
}

func mapSource(m manifest, path string) ([]string, []string, string, error) {
	direct := []artifact{}
	for _, a := range m.Artifacts {
		if a.PathRole != "concrete" || a.Path == "" {
			continue
		}
		exact := path == a.Path
		within := !strings.Contains(filepath.Base(a.Path), ".") && strings.HasPrefix(path, strings.TrimSuffix(a.Path, "/")+"/")
		samePkg := strings.HasSuffix(path, ".go") && strings.HasSuffix(a.Path, ".go") && filepath.ToSlash(filepath.Dir(path)) == filepath.ToSlash(filepath.Dir(a.Path))
		if exact || within || samePkg {
			direct = append(direct, a)
		}
	}
	if len(direct) == 0 {
		return nil, nil, "", errors.New("no manifest owner for " + path)
	}
	// Exact and directory ownership outrank package inference. Package inference must be unique.
	best := []artifact{}
	for _, a := range direct {
		if path == a.Path || (!strings.Contains(filepath.Base(a.Path), ".") && strings.HasPrefix(path, strings.TrimSuffix(a.Path, "/")+"/")) {
			best = append(best, a)
		}
	}
	if len(best) > 0 {
		direct = best
	}
	ownerIDs := map[string]bool{}
	phases := map[string]bool{}
	for _, a := range direct {
		phase := a.OwnerPhase
		if phase == "" {
			phase = a.ProducerPhase
		}
		if phase == "" {
			continue
		}
		ownerIDs[a.ID] = true
		phases[phase] = true
	}
	if len(ownerIDs) != 1 {
		return nil, nil, "", fmt.Errorf("source %s has %d manifest owners", path, len(ownerIDs))
	}
	ids := map[string]bool{}
	for id := range ownerIDs {
		ids[id] = true
	}
	changed := true
	for changed {
		changed = false
		for _, a := range m.Artifacts {
			if a.PathRole != "concrete" || a.ProducerPhase == "" {
				continue
			}
			for _, dep := range a.DerivesFrom {
				if ids[dep] && !ids[a.ID] {
					ids[a.ID] = true
					phases[a.ProducerPhase] = true
					changed = true
				}
			}
		}
	}
	scope := "p10-production"
	for phase := range phases {
		if phase == "P08" || phase == "P09" {
			scope = "early-owned"
		}
	}
	if path == Path {
		scope = "carrier"
	}
	return keys(ids), keys(phases), scope, nil
}

func loadManifest(root string) (manifest, error) {
	var m manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &m); err != nil {
		return m, err
	}
	return m, nil
}
func uniqueIntroduction(root, path string) (string, error) {
	out, err := git(root, "log", "--diff-filter=A", "--format=%H", "--", path)
	if err != nil {
		return "", err
	}
	v := strings.Fields(string(out))
	if len(v) != 1 {
		return "", fmt.Errorf("%s introduction commits=%d want=1", path, len(v))
	}
	return v[0], nil
}
func git(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "TZ=UTC"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func gitBlob(root, commit, path string) ([]byte, bool, error) {
	out, err := git(root, "show", commit+":"+path)
	if err != nil {
		if _, e := git(root, "cat-file", "-e", commit+":"+path); e != nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}
func worktreeBlob(root, path string) ([]byte, bool, error) {
	data, err := readRegular(root, path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func readRegular(root, path string) ([]byte, error) {
	absolute := filepath.Join(root, filepath.FromSlash(path))
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not regular: " + path)
	}
	return os.ReadFile(absolute)
}
func canonical(v []string) bool {
	if len(v) == 0 {
		return false
	}
	for i, s := range v {
		if s == "" || (i > 0 && v[i-1] >= s) {
			return false
		}
	}
	return true
}
func canonicalSources(v []Source) bool {
	if len(v) == 0 {
		return false
	}
	for i, s := range v {
		if s.Path == "" || (i > 0 && v[i-1].Path >= s.Path) || !canonical(s.ArtifactIDs) || !canonical(s.OwnerPhases) {
			return false
		}
		seenCommits := map[string]bool{}
		for _, c := range s.TouchCommits {
			if len(c) != 40 || seenCommits[c] {
				return false
			}
			seenCommits[c] = true
		}
	}
	return true
}
func unionArtifacts(v []Source) []string {
	m := map[string]bool{}
	for _, s := range v {
		for _, id := range s.ArtifactIDs {
			m[id] = true
		}
	}
	return keys(m)
}
func keys(m map[string]bool) []string {
	v := make([]string, 0, len(m))
	for k := range m {
		v = append(v, k)
	}
	sort.Strings(v)
	return v
}
func sortedSourcePaths(m map[string]Source) []string {
	v := make([]string, 0, len(m))
	for k := range m {
		v = append(v, k)
	}
	sort.Strings(v)
	return v
}
func contains(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}
func clone(w Waiver) Waiver {
	data, _ := json.Marshal(w)
	var c Waiver
	_ = json.Unmarshal(data, &c)
	return c
}
func decodeBytes(data []byte) (Waiver, error) {
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return Waiver{}, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var w Waiver
	if err := d.Decode(&w); err != nil {
		return w, err
	}
	var x any
	if err := d.Decode(&x); !errors.Is(err, io.EOF) {
		if err == nil {
			return w, errors.New("trailing")
		}
		return w, err
	}
	return w, nil
}
