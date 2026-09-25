// Command arop-go-proxy-bootstrap verifies the two-module repository layout.
//
// The nested Reference Control Plane deliberately has no replace directive.
// This command reconstructs the exact root-module commit named by its
// pseudo-version, publishes that commit to a temporary file:// Go proxy, and
// tests the nested module with GOWORK=off and an isolated module cache.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/controlledinput"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	rootModulePath   = "github.com/gmslll/agent-runtime-operations-protocol"
	nestedModulePath = rootModulePath + "/reference/control-plane"
	nestedDirectory  = "reference/control-plane"
	placeholder      = "v0.0.0-00010101000000-000000000000"

	migrationCommit   = "e5cdf162f1bbfeb03a14897d7adc9f7b1a3b952b"
	p05Commit         = "0bbf501de769ea7b0d31e42f6acb9eebd74d0503"
	correctionBase    = "0d2a16db5b6c46197d68b2c0ff87fb450b203cfc"
	p12CorrectionBase = "24f62398736e6cac99eb8d6e6f011879dc7c05a4"
	p05Tree           = "550cb12448b52cd6b69dcb056ccb1d887db1201f"
	p05Bootstrap      = "1bc642d6648addb822daec4427d721c25d620043"
	p05Version        = "v0.0.0-20260923023550-1bc642d6648a"
	p05ZipH1          = "h1:Fd+w1T7K4+KQji8aWziLDhU5Pdm3Cxa5Ywy/2aizkDU="
	p05GoModH1        = "h1:ov6WTouj8Qdfs+WL7KgViSYyf6mbrYLBDM8N2EvrxxY="
	p08Commit         = "e92c2f5fc82ad7d1f0b2ac9c59d004db6419ec29"
	p09Commit         = "fcfa827482a5379c17c13edaefedc8a54a6f850c"
	currentPin        = "75ba8694c47f4500725113bbf5554dde4c901d03"
	currentVersion    = "v0.0.0-20260925112239-75ba8694c47f"
	currentZipH1      = "h1:oS44RCL3Y1czZCdgnt3Cn+LOPFhYKSIxC4ox5C+V3Yc="
	currentGoModH1    = "h1:N4IdtBpzQjKJuhxiIlhJsGn/zIiC1jgKPB9TZKRBmZk="
)

var transitionPaths = []string{
	"conformance/harness/base/testdata/transition/p06-p05-transition.json",
	"reference/control-plane/internal/app/platform/testdata/transition/p08-p05-transition.json",
	"reference/control-plane/internal/storage/migrate/testdata/engine-versions/p09-p05-transition.json",
	"cmd/arop/internal/commands/publish/testdata/transition/p12-p05-transition.json",
}

var pseudoVersionPattern = regexp.MustCompile(`^v0\.0\.0-(\d{14})-([0-9a-f]{12})$`)

type moduleFile struct {
	Module struct {
		Path string `json:"Path"`
	} `json:"Module"`
	Go      string `json:"Go"`
	Require []struct {
		Path     string `json:"Path"`
		Version  string `json:"Version"`
		Indirect bool   `json:"Indirect"`
	} `json:"Require"`
	Replace []struct {
		Old struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		} `json:"Old"`
		New struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		} `json:"New"`
	} `json:"Replace"`
}

type workspaceFile struct {
	Go  string `json:"Go"`
	Use []struct {
		DiskPath string `json:"DiskPath"`
	} `json:"Use"`
	Replace []any `json:"Replace"`
}

type downloadResult struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Info     string `json:"Info"`
	GoMod    string `json:"GoMod"`
	Zip      string `json:"Zip"`
	Dir      string `json:"Dir"`
	Sum      string `json:"Sum"`
	GoModSum string `json:"GoModSum"`
	Error    string `json:"Error"`
}

type listedModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Replace *listedModule `json:"Replace"`
	Dir     string        `json:"Dir"`
	GoMod   string        `json:"GoMod"`
}

type treeEntry struct {
	Mode, Type, Object, Path string
}

type rootPin struct {
	Commit  string `json:"commit"`
	Version string `json:"version"`
	ZipH1   string `json:"zip_h1"`
	GoModH1 string `json:"go_mod_h1"`
}

type pinTransition struct {
	Before rootPin `json:"before"`
	After  rootPin `json:"after"`
}

type transitionPolicy struct {
	OwnerPhaseSemantics        string `json:"owner_phase_semantics"`
	ExclusiveMutationOwnership bool   `json:"exclusive_mutation_ownership"`
	EvidenceMode               string `json:"evidence_mode"`
}

type pathDigest struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

type transitionHop struct {
	Commit       string       `json:"commit"`
	ChangedPaths []string     `json:"changed_paths"`
	Before       []pathDigest `json:"before"`
	After        []pathDigest `json:"after"`
}

type transitionTouch struct {
	Commit       string   `json:"commit"`
	ChangedPaths []string `json:"changed_paths"`
}

type transitionAcceptance struct {
	Phase   string `json:"phase"`
	Command string `json:"command"`
	Report  string `json:"report"`
}

type transitionRecord struct {
	SchemaVersion     int                    `json:"schema_version"`
	TransitionID      string                 `json:"transition_id"`
	Status            string                 `json:"status"`
	Policy            transitionPolicy       `json:"policy"`
	FromPhase         string                 `json:"from_phase"`
	ToPhase           string                 `json:"to_phase"`
	BaselineCommit    string                 `json:"baseline_commit"`
	ResultCommit      string                 `json:"result_commit"`
	Reason            string                 `json:"reason"`
	ChangedPaths      []string               `json:"changed_paths"`
	AffectedArtifacts []string               `json:"affected_artifacts"`
	History           []transitionHop        `json:"history"`
	RootPin           *pinTransition         `json:"root_pin"`
	Acceptance        []transitionAcceptance `json:"acceptance"`
	Constraints       []string               `json:"constraints"`
}

type artifactManifest struct {
	SchemaVersion int `yaml:"schema_version"`
	Artifacts     []struct {
		ID         string `yaml:"id"`
		Path       string `yaml:"path"`
		Status     string `yaml:"status"`
		OwnerPhase string `yaml:"owner_phase"`
		PathRole   string `yaml:"path_role"`
	} `yaml:"artifacts"`
}

type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	CFiles     []string
	CXXFiles   []string
	MFiles     []string
	HFiles     []string
	FFiles     []string
	SFiles     []string
	SysoFiles  []string
	EmbedFiles []string
}

type replayEvidence struct {
	Items                   []report.RuntimeEvidence
	ConsumedPaths           []string
	RecordedTransitionPaths []string
	AffectedArtifacts       []string
	CorrectionCommit        string
}

func main() {
	bootstrapGoSum := flag.Bool("bootstrap-go-sum", false, "mechanically generate only reference/control-plane/go.sum from the commit-backed temporary proxy")
	flag.Parse()
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if *bootstrapGoSum {
		fatal(prepareNestedGoSum(root))
		fmt.Println("Generated reference/control-plane/go.sum from the exact commit-backed temporary proxy; no P05 report was emitted.")
		return
	}

	checks := []report.Check{}
	record := func(name string, err error, success string) {
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail(err, success)})
	}

	modules, modulesErr := discoverModuleFiles(root)
	if modulesErr == nil {
		modulesErr = requireExactModules(modules)
	}
	record("exact-two-go-modules", modulesErr, strings.Join(modules, ", "))

	var trackedWorkErr error
	for _, workspaceState := range []string{"go.work", "go.work.sum"} {
		isTracked, trackedErr := tracked(root, workspaceState)
		if trackedErr != nil {
			trackedWorkErr = errors.Join(trackedWorkErr, trackedErr)
		} else if isTracked {
			trackedWorkErr = errors.Join(trackedWorkErr, fmt.Errorf("%s is tracked; only go.work.example may be committed", workspaceState))
		}
	}
	record("no-committed-go-work", trackedWorkErr, "go.work and go.work.sum are untracked/absent and GOWORK is forced off")

	rootModule, rootModuleErr := loadModule(root)
	record("root-module-contract", validateRootModule(rootModule, rootModuleErr), rootModulePath+" has no replace directives")
	nestedRoot := filepath.Join(root, filepath.FromSlash(nestedDirectory))
	nestedModule, nestedModuleErr := loadModule(nestedRoot)
	rootVersion, nestedContractErr := validateNestedModule(nestedModule, nestedModuleErr)
	record("nested-module-contract", nestedContractErr, nestedModulePath+" requires exact root "+rootVersion+" without replace")

	workspaceErr := validateWorkspaceExample(root)
	record("go-work-example", workspaceErr, "template uses exactly root and reference/control-plane")
	record("retired-control-plane-lite", validateRetiredBaseline(root), "control-plane-lite contains only a non-executable README tombstone")
	record("control-plane-baseline-source-parity", validateHistoricalMigrationParity(root), "migration commit e5cdf16 preserves the pre-migration command, handler, and wire tests except for the module import path")
	record("frozen-p05-closure", validateFrozenP05Closure(root), "P05 closes at 0bbf501 with tree 550cb12 and bootstrap parent 1bc642d")
	record("root-nested-import-boundary", validateRootImportBoundary(root), "root module has no imports of the nested Reference Control Plane")
	record("ci-workspace-policy", validateWorkflow(root), "strict CI structure preserves full history and runs the exact P05 Make target unconditionally with minimal permissions")

	record("negative-third-module", negativeThirdModuleProbe(), "a third go.mod is rejected")
	record("negative-permanent-replace", negativeReplaceProbe(), "a permanent replace directive is rejected")
	record("negative-version-mismatch", negativeVersionProbe(), "a non-pseudo or mismatched root version is rejected")
	record("negative-module-archive-paths", negativeModuleArchiveProbe(), "nested vendor directories, unsafe paths, and case-fold collisions are rejected or excluded")
	record("negative-transition-records", negativeTransitionRecordProbe(root), "duplicate, trailing, omitted-path, broken pin-chain, and corrupt proxy transition inputs are rejected")
	record("negative-git-nul-records", negativeNULPathProbe(), "unterminated and unsafe NUL-delimited Git paths are rejected")

	rootProxy, rootProxyErr := rootPrefetchProxy()
	record("root-prefetch-proxy-policy", rootProxyErr, "root dependencies use one credential-free HTTPS or file proxy with no direct fallback")
	trackedInputs, trackedInputsErr := trackedRepositoryInputs(root)
	record("tracked-input-closure", trackedInputsErr, fmt.Sprintf("all %d tracked repository files are bound into the report", len(trackedInputs)))
	record("clean-source-tree", sourceTreeClean(root), "P05 acceptance runs from a clean tracked and untracked source tree")

	temporaryRoot, temporaryErr := os.MkdirTemp("", "arop-go-proxy-")
	if temporaryErr != nil {
		record("temporary-proxy-bootstrap", temporaryErr, "")
		record("root-gowork-off-tests", errors.New("isolated caches were not created"), "")
		record("nested-local-proxy-tests", errors.New("temporary proxy was not created"), "")
		record("temporary-proxy-cleanup", errors.New("temporary proxy was not created"), "")
		writeReport(root, checks, nil, nil, nil, rootProxy, trackedInputs, replayEvidence{})
		os.Exit(1)
	}

	proxyDirectory := filepath.Join(temporaryRoot, "proxy")
	rootModuleCache := filepath.Join(temporaryRoot, "root-modcache")
	rootBuildCache := filepath.Join(temporaryRoot, "root-buildcache")
	rootTemp := filepath.Join(temporaryRoot, "root-tmp")
	nestedModuleCache := filepath.Join(temporaryRoot, "nested-modcache")
	nestedBuildCache := filepath.Join(temporaryRoot, "nested-buildcache")
	nestedTemp := filepath.Join(temporaryRoot, "nested-tmp")
	for _, directory := range []string{rootModuleCache, rootBuildCache, rootTemp, nestedModuleCache, nestedBuildCache, nestedTemp} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			temporaryErr = errors.Join(temporaryErr, err)
		}
	}
	rootEnvironment := map[string]string{"GOCACHE": rootBuildCache, "GOMODCACHE": rootModuleCache, "GOTMPDIR": rootTemp}
	rootPrefetchEnvironment := cloneMap(rootEnvironment)
	rootPrefetchEnvironment["GOPROXY"] = rootProxy
	var rootPrefetch []byte
	rootPrefetchErr := rootProxyErr
	if rootPrefetchErr == nil {
		rootPrefetch, rootPrefetchErr = runGoOnline(root, rootPrefetchEnvironment, "mod", "download")
	}
	rootVerify, rootVerifyErr := runGo(root, rootEnvironment, "mod", "verify")
	rootTest, rootTestErr := runGo(root, rootEnvironment, "test", "-count=1", "./...")
	rootCombinedOutput := bytes.Join([][]byte{rootPrefetch, rootVerify, rootTest}, []byte("\n"))
	record("root-gowork-off-tests", errors.Join(temporaryErr, rootProxyErr, rootPrefetchErr, rootVerifyErr, rootTestErr), "root dependencies were prefetched through one explicit proxy into an isolated cache, verified, then root tests passed with GOWORK=off and GOPROXY=off")

	replay := replayEvidence{}
	frozenErr := verifyFrozenBootstrap(root, proxyDirectory, nestedModuleCache, nestedTemp, &replay)
	record("frozen-p05-bootstrap", frozenErr, "P05 bootstrap commit, pseudo-version, module archive h1, and go.mod h1 match Git objects")

	bootstrap, bootstrapErr := resolveBootstrapCommit(root, rootVersion)
	if bootstrapErr == nil {
		bootstrapErr = writeProxy(root, proxyDirectory, bootstrap)
	}
	if bootstrapErr == nil {
		bootstrapErr = validateCurrentPin(bootstrap)
	}
	record("temporary-proxy-bootstrap", bootstrapErr, fmt.Sprintf("%s reconstructed from commit %s; later unconsumed root drift is permitted", rootVersion, bootstrap.Commit))

	proxyURL := (&url.URL{Scheme: "file", Path: proxyDirectory}).String() + ",off"
	nestedEnvironment := map[string]string{
		"GOCACHE":    nestedBuildCache,
		"GOMODCACHE": nestedModuleCache,
		"GOTMPDIR":   nestedTemp,
		"GONOSUMDB":  "*",
		"GOSUMDB":    "off",
	}
	proxyEnvironment := cloneMap(nestedEnvironment)
	proxyEnvironment["GOPROXY"] = proxyURL
	dependencyEnvironment := cloneMap(nestedEnvironment)
	dependencyEnvironment["GOPROXY"] = rootProxy
	var downloadOutput, nestedVerifyOutput, nestedListOutput, nestedDepsOutput, nestedTestOutput []byte
	var nestedErr error
	transitionErr := errors.New("transition verification did not run")
	transitionRan := false
	nestedSumPath := filepath.Join(nestedRoot, "go.sum")
	nestedSumBefore, nestedSumReadErr := os.ReadFile(nestedSumPath)
	if nestedSumReadErr != nil {
		nestedErr = fmt.Errorf("nested go.sum must be generated with --bootstrap-go-sum before acceptance: %w", nestedSumReadErr)
	} else if nestedContractErr == nil {
		nestedErr = validateNestedRootSums(nestedSumBefore, rootVersion)
	}
	if bootstrapErr == nil && nestedContractErr == nil && nestedErr == nil {
		downloadOutput, nestedErr = runGo(nestedRoot, proxyEnvironment, "mod", "download", "-json", rootModulePath+"@"+rootVersion)
		if nestedErr == nil {
			nestedErr = validateDownload(downloadOutput, rootModulePath, rootVersion, bootstrap.GoMod, temporaryRoot)
		}
		if nestedErr == nil {
			var graphOutput []byte
			graphOutput, nestedErr = runGoOnline(nestedRoot, dependencyEnvironment, "list", "-m", "-json", "all")
			downloadOutput = bytes.Join([][]byte{downloadOutput, graphOutput}, []byte("\n"))
		}
		if nestedErr == nil {
			nestedDepsOutput, nestedErr = runGoJSON(nestedRoot, dependencyEnvironment, "list", "-deps", "-test", "-json", "./...")
		}
		if nestedErr == nil {
			nestedVerifyOutput, nestedErr = runGo(nestedRoot, nestedEnvironment, "mod", "verify")
		}
		if nestedErr == nil {
			nestedListOutput, nestedErr = runGo(nestedRoot, nestedEnvironment, "list", "-m", "-json", rootModulePath)
		}
		if nestedErr == nil {
			nestedErr = validateListedModule(nestedListOutput, rootModulePath, rootVersion, root, temporaryRoot)
		}
		if nestedErr == nil {
			transitionRan = true
			replay, transitionErr = verifyReplayTransitions(root, bootstrap, nestedDepsOutput, replay)
			nestedErr = transitionErr
		}
		if nestedErr == nil {
			nestedTestOutput, nestedErr = runGo(nestedRoot, nestedEnvironment, "test", "-count=1", "-v", "./...")
		}
		if nestedErr == nil {
			nestedErr = validateHealthTestOutput(nestedTestOutput)
		}
	} else {
		nestedErr = errors.Join(nestedErr, bootstrapErr, nestedContractErr, errors.New("nested tests require a valid non-placeholder pseudo-version bootstrap"))
	}
	if !transitionRan && nestedErr != nil {
		transitionErr = errors.Join(transitionErr, nestedErr)
	}
	nestedSumAfter, nestedSumAfterErr := os.ReadFile(nestedSumPath)
	if nestedSumReadErr == nil && (nestedSumAfterErr != nil || !bytes.Equal(nestedSumBefore, nestedSumAfter)) {
		nestedErr = errors.Join(nestedErr, errors.New("default acceptance changed reference/control-plane/go.sum; regenerate it with --bootstrap-go-sum and commit it first"))
	}
	nestedCombinedOutput := bytes.Join([][]byte{nestedVerifyOutput, nestedListOutput, nestedDepsOutput, nestedTestOutput}, []byte("\n"))
	record("nested-local-proxy-tests", nestedErr, "nested module downloaded exact root bytes from the temporary proxy, then go mod verify, go list, and tests passed readonly with GOWORK=off/GOPROXY=off in isolated caches")
	record("p05-transition-ledger", transitionErr, fmt.Sprintf("%d consumed pinned-root files are bound as diagnostics and P05-owned mutations are covered by exact P06/P08/P09 transition records", len(replay.ConsumedPaths)))

	cleanupErr := removeAllWritable(temporaryRoot)
	if cleanupErr == nil {
		if _, statErr := os.Stat(temporaryRoot); !os.IsNotExist(statErr) {
			cleanupErr = fmt.Errorf("temporary proxy still exists after cleanup: %s", temporaryRoot)
		}
	}
	record("temporary-proxy-cleanup", cleanupErr, "temporary proxy and isolated caches were removed")

	writeReport(root, checks, rootCombinedOutput, downloadOutput, nestedCombinedOutput, rootProxy, trackedInputs, replay)
	for _, check := range checks {
		if !check.Passed {
			os.Exit(1)
		}
	}
}

type bootstrapCommit struct {
	Commit  string
	Version string
	Time    time.Time
	GoMod   []byte
}

func discoverModuleFiles(root string) ([]string, error) {
	modules := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			switch relative {
			case ".git", ".worktrees", "build", "coverage", "dist", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(relative) != "go.mod" {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("go.mod path is a symlink: %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("go.mod path is not a regular file: %s", relative)
		}
		modules = append(modules, relative)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover repository modules: %w", err)
	}
	sort.Strings(modules)
	return modules, nil
}

func requireExactModules(modules []string) error {
	want := []string{"go.mod", nestedDirectory + "/go.mod"}
	if !equalStrings(modules, want) {
		return fmt.Errorf("go.mod set = %v, want %v", modules, want)
	}
	return nil
}

func tracked(root, path string) (bool, error) {
	command := controlledinput.GitCommand(root, "ls-files", "--error-unmatch", "--", path)
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check tracked %s: %w", path, err)
}

func loadModule(directory string) (moduleFile, error) {
	var result moduleFile
	output, err := runGoWithProxy(directory, goEnvironment(nil, true), "mod", "edit", "-json")
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return result, fmt.Errorf("decode go.mod: %w", err)
	}
	return result, nil
}

func validateRootModule(module moduleFile, loadErr error) error {
	if loadErr != nil {
		return loadErr
	}
	if module.Module.Path != rootModulePath {
		return fmt.Errorf("root module path = %q, want %q", module.Module.Path, rootModulePath)
	}
	if len(module.Replace) != 0 {
		return errors.New("root go.mod contains a replace directive")
	}
	for _, requirement := range module.Require {
		if requirement.Path == nestedModulePath || strings.HasPrefix(requirement.Path, nestedModulePath+"/") {
			return errors.New("root go.mod requires the nested Reference Control Plane")
		}
	}
	return nil
}

func validateNestedModule(module moduleFile, loadErr error) (string, error) {
	if loadErr != nil {
		return "", loadErr
	}
	if module.Module.Path != nestedModulePath {
		return "", fmt.Errorf("nested module path = %q, want %q", module.Module.Path, nestedModulePath)
	}
	if len(module.Replace) != 0 {
		return "", errors.New("nested go.mod contains a replace directive")
	}
	versions := []string{}
	for _, requirement := range module.Require {
		if requirement.Path == rootModulePath {
			versions = append(versions, requirement.Version)
		}
	}
	if len(versions) != 1 {
		return "", fmt.Errorf("nested go.mod must require root module exactly once; found %d", len(versions))
	}
	if _, _, err := parsePseudoVersion(versions[0]); err != nil {
		return versions[0], err
	}
	return versions[0], nil
}

func validateWorkspaceExample(root string) error {
	temporary, err := os.MkdirTemp("", "arop-go-work-example-")
	if err != nil {
		return err
	}
	defer removeAllWritable(temporary)
	data, err := os.ReadFile(filepath.Join(root, "go.work.example"))
	if err != nil {
		return err
	}
	path := filepath.Join(temporary, "go.work")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	output, err := runGoWithProxy(root, goEnvironment(nil, true), "work", "edit", "-json", path)
	if err != nil {
		return err
	}
	var workspace workspaceFile
	if err := json.Unmarshal(output, &workspace); err != nil {
		return fmt.Errorf("decode go.work.example: %w", err)
	}
	uses := []string{}
	for _, use := range workspace.Use {
		uses = append(uses, filepath.ToSlash(use.DiskPath))
	}
	sort.Strings(uses)
	if !equalStrings(uses, []string{".", "./reference/control-plane"}) {
		return fmt.Errorf("workspace use set = %v", uses)
	}
	if len(workspace.Replace) != 0 {
		return errors.New("go.work.example must not contain replace directives")
	}
	return nil
}

func validateRetiredBaseline(root string) error {
	retiredRoot := filepath.Join(root, "reference", "control-plane-lite")
	files := []string{}
	err := filepath.WalkDir(retiredRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(retiredRoot, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return err
	}
	if !equalStrings(files, []string{"README.md"}) {
		return fmt.Errorf("retired baseline files = %v, want README.md only", files)
	}
	readme, err := os.ReadFile(filepath.Join(retiredRoot, "README.md"))
	if err != nil {
		return err
	}
	text := string(readme)
	for _, marker := range []string{"Retired Control Plane Lite baseline", "../control-plane", "no Go", "no Go\nmodule"} {
		if !strings.Contains(text, marker) {
			return fmt.Errorf("retired baseline README is missing marker %q", marker)
		}
	}
	return nil
}

func validateHistoricalMigrationParity(root string) error {
	files := []struct {
		migrated  string
		baseline  string
		normalize func([]byte) ([]byte, error)
	}{
		{
			migrated: nestedDirectory + "/internal/server/server.go",
			baseline: "reference/control-plane-lite/internal/server/server.go",
		},
		{
			migrated: nestedDirectory + "/internal/server/server_test.go",
			baseline: "reference/control-plane-lite/internal/server/server_test.go",
		},
		{
			migrated: nestedDirectory + "/cmd/aropd/main.go",
			baseline: "reference/control-plane-lite/cmd/aropd/main.go",
			normalize: func(data []byte) ([]byte, error) {
				current := []byte(nestedModulePath + "/internal/server")
				baseline := []byte(rootModulePath + "/reference/control-plane-lite/internal/server")
				if bytes.Count(data, current) != 1 {
					return nil, errors.New("migrated aropd import path is not present exactly once")
				}
				return bytes.Replace(data, current, baseline, 1), nil
			},
		},
	}
	parent, err := gitText(root, "rev-parse", migrationCommit+"^")
	if err != nil {
		return fmt.Errorf("resolve migration parent: %w", err)
	}
	renames, err := gitBytes(root, "diff-tree", "-r", "-M100%", "--name-status", "-z", parent, migrationCommit, "--")
	if err != nil {
		return fmt.Errorf("inspect migration renames: %w", err)
	}
	for oldPath, newPath := range map[string]string{
		"reference/control-plane-lite/internal/server/server.go":      nestedDirectory + "/internal/server/server.go",
		"reference/control-plane-lite/internal/server/server_test.go": nestedDirectory + "/internal/server/server_test.go",
	} {
		if !hasExactRename(renames, oldPath, newPath) {
			return fmt.Errorf("migration does not contain an R100 rename %s -> %s", oldPath, newPath)
		}
	}
	for _, file := range files {
		data, err := gitBytes(root, "show", migrationCommit+":"+file.migrated)
		if err != nil {
			return fmt.Errorf("read migrated Git object %s: %w", file.migrated, err)
		}
		if file.normalize != nil {
			data, err = file.normalize(data)
			if err != nil {
				return fmt.Errorf("normalize %s: %w", file.migrated, err)
			}
		}
		want, err := gitBytes(root, "show", parent+":"+file.baseline)
		if err != nil {
			return fmt.Errorf("read pre-migration Git object %s: %w", file.baseline, err)
		}
		if !bytes.Equal(data, want) {
			return fmt.Errorf("migration object %s does not preserve %s", file.migrated, file.baseline)
		}
	}
	return nil
}

func hasExactRename(output []byte, oldPath, newPath string) bool {
	if len(output) == 0 || output[len(output)-1] != 0 {
		return false
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	for index := 0; index < len(fields); {
		status := string(fields[index])
		index++
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if index+1 >= len(fields) {
				return false
			}
			oldValue, newValue := string(fields[index]), string(fields[index+1])
			index += 2
			if status == "R100" && oldValue == oldPath && newValue == newPath {
				return true
			}
		} else {
			if index >= len(fields) {
				return false
			}
			index++
		}
	}
	return false
}

func validateFrozenP05Closure(root string) error {
	for _, commit := range []string{migrationCommit, p05Bootstrap, p05Commit, p08Commit, p09Commit, currentPin} {
		if command := controlledinput.GitCommand(root, "merge-base", "--is-ancestor", commit, "HEAD"); command.Run() != nil {
			return fmt.Errorf("required historical commit %s is not an ancestor of HEAD", commit)
		}
	}
	tree, err := gitText(root, "show", "-s", "--format=%T", p05Commit)
	if err != nil || tree != p05Tree {
		return fmt.Errorf("P05 closure tree=%q want=%s: %w", tree, p05Tree, err)
	}
	parent, err := gitText(root, "rev-parse", p05Commit+"^")
	if err != nil || parent != p05Bootstrap {
		return fmt.Errorf("P05 closure parent=%q want=%s: %w", parent, p05Bootstrap, err)
	}
	moduleData, err := gitBytes(root, "show", p05Commit+":"+nestedDirectory+"/go.mod")
	if err != nil || !bytes.Contains(moduleData, []byte(p05Version)) {
		return errors.Join(err, errors.New("P05 closure does not pin the frozen bootstrap pseudo-version"))
	}
	sumData, err := gitBytes(root, "show", p05Commit+":"+nestedDirectory+"/go.sum")
	if err != nil || !bytes.Contains(sumData, []byte(p05Version+" "+p05ZipH1)) || !bytes.Contains(sumData, []byte(p05Version+"/go.mod "+p05GoModH1)) {
		return errors.Join(err, errors.New("P05 closure does not bind the frozen bootstrap h1 values"))
	}
	return nil
}

func validateRootImportBoundary(root string) error {
	problems := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == ".git" || relative == "build" || relative == "node_modules" || relative == nestedDirectory {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(relative, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse root Go source %s: %w", relative, err)
		}
		for _, imported := range parsed.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return fmt.Errorf("parse import in %s: %w", relative, err)
			}
			if value == nestedModulePath || strings.HasPrefix(value, nestedModulePath+"/") {
				problems = append(problems, relative+" imports "+value)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validateWorkflow(root string) error {
	value, _, err := structuredfile.LoadAny(filepath.Join(root, ".github", "workflows", "go-workspace.yml"))
	if err != nil {
		return err
	}
	workflow, err := exactObject(value, "workflow", "name", "on", "permissions", "jobs")
	if err != nil {
		return err
	}
	if workflow["name"] != "go-workspace" {
		return fmt.Errorf("workflow name = %v, want go-workspace", workflow["name"])
	}
	triggers, err := exactObject(workflow["on"], "workflow.on", "pull_request", "push")
	if err != nil || triggers["pull_request"] != nil {
		return errors.Join(err, errors.New("workflow pull_request trigger must be unconditional"))
	}
	push, err := exactObject(triggers["push"], "workflow.on.push", "branches")
	if err != nil {
		return err
	}
	if !equalAnyStrings(push["branches"], []string{"main"}) {
		return fmt.Errorf("workflow push branches = %v, want [main]", push["branches"])
	}
	permissions, err := exactObject(workflow["permissions"], "workflow.permissions", "contents")
	if err != nil || permissions["contents"] != "read" {
		return errors.Join(err, errors.New("workflow permissions must be exactly contents: read"))
	}
	jobs, err := exactObject(workflow["jobs"], "workflow.jobs", "modules")
	if err != nil {
		return err
	}
	job, err := exactObject(jobs["modules"], "workflow.jobs.modules", "runs-on", "steps")
	if err != nil || job["runs-on"] != "ubuntu-latest" {
		return errors.Join(err, errors.New("modules job must run on ubuntu-latest without job-level conditions"))
	}
	steps, ok := job["steps"].([]any)
	if !ok || len(steps) != 4 {
		return fmt.Errorf("modules job steps = %T/%d, want exactly four", job["steps"], len(steps))
	}
	checkout, err := exactObject(steps[0], "checkout step", "uses", "with")
	if err != nil || checkout["uses"] != "actions/checkout@v4" {
		return errors.Join(err, errors.New("checkout step must use actions/checkout@v4 without conditions"))
	}
	checkoutWith, err := exactObject(checkout["with"], "checkout.with", "fetch-depth")
	if err != nil || fmt.Sprint(checkoutWith["fetch-depth"]) != "0" {
		return errors.Join(err, errors.New("checkout fetch-depth must be 0"))
	}
	setupGo, err := exactObject(steps[1], "setup-go step", "uses", "with")
	if err != nil || setupGo["uses"] != "actions/setup-go@v5" {
		return errors.Join(err, errors.New("setup-go step must use actions/setup-go@v5 without conditions"))
	}
	setupGoWith, err := exactObject(setupGo["with"], "setup-go.with", "go-version-file", "cache", "cache-dependency-path")
	if err != nil || setupGoWith["go-version-file"] != "go.mod" || setupGoWith["cache"] != true || !equalLineSet(setupGoWith["cache-dependency-path"], []string{"go.sum", nestedDirectory + "/go.sum"}) {
		return errors.Join(err, errors.New("setup-go inputs must bind go.mod and both go.sum files"))
	}
	setupNode, err := exactObject(steps[2], "setup-node step", "uses", "with")
	if err != nil || setupNode["uses"] != "actions/setup-node@v4" {
		return errors.Join(err, errors.New("setup-node step must use actions/setup-node@v4 without conditions"))
	}
	setupNodeWith, err := exactObject(setupNode["with"], "setup-node.with", "node-version", "cache")
	if err != nil || fmt.Sprint(setupNodeWith["node-version"]) != "22" || setupNodeWith["cache"] != "npm" {
		return errors.Join(err, errors.New("setup-node inputs must be Node 22 with npm cache"))
	}
	verify, err := exactObject(steps[3], "verification step", "name", "run")
	if err != nil || verify["name"] != "Verify the two-module bootstrap" || verify["run"] != "make test-go-workspace" {
		return errors.Join(err, errors.New("verification step must unconditionally run exactly make test-go-workspace"))
	}
	return nil
}

func exactObject(value any, label string, keys ...string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	want := append([]string(nil), keys...)
	sort.Strings(want)
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	if !equalStrings(got, want) {
		return nil, fmt.Errorf("%s keys = %v, want %v", label, got, want)
	}
	return object, nil
}

func equalAnyStrings(value any, want []string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for index := range want {
		if items[index] != want[index] {
			return false
		}
	}
	return true
}

func equalLineSet(value any, want []string) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	got := strings.Fields(text)
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	return equalStrings(got, want)
}

func validateHealthTestOutput(output []byte) error {
	text := string(output)
	for _, test := range []string{"TestHealthEndpoints", "TestHealthRejectsUnsupportedMethod"} {
		if !strings.Contains(text, "=== RUN   "+test) && !strings.Contains(text, "=== PAUSE "+test) {
			return fmt.Errorf("migrated health wire test %s did not execute", test)
		}
	}
	return nil
}

func parsePseudoVersion(version string) (time.Time, string, error) {
	if version == placeholder {
		return time.Time{}, "", errors.New("nested root requirement still uses P05_BOOTSTRAP_PLACEHOLDER; replace it after creating the migration commit")
	}
	match := pseudoVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return time.Time{}, "", fmt.Errorf("root version %q is not an exact v0 pseudo-version", version)
	}
	timestamp, err := time.Parse("20060102150405", match[1])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("parse pseudo-version timestamp: %w", err)
	}
	return timestamp.UTC(), match[2], nil
}

func resolveBootstrapCommit(root, version string) (bootstrapCommit, error) {
	timestamp, short, err := parsePseudoVersion(version)
	if err != nil {
		return bootstrapCommit{}, err
	}
	commit, err := gitText(root, "rev-parse", short+"^{commit}")
	if err != nil {
		return bootstrapCommit{}, fmt.Errorf("resolve pseudo-version commit %s: %w", short, err)
	}
	if len(commit) != 40 || !strings.HasPrefix(commit, short) {
		return bootstrapCommit{}, fmt.Errorf("resolved commit %q does not match pseudo-version suffix %s", commit, short)
	}
	if command := controlledinput.GitCommand(root, "merge-base", "--is-ancestor", commit, "HEAD"); command.Run() != nil {
		return bootstrapCommit{}, fmt.Errorf("pseudo-version commit %s is not an ancestor of HEAD", commit)
	}
	commitTimeText, err := gitText(root, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return bootstrapCommit{}, fmt.Errorf("read pseudo-version commit time: %w", err)
	}
	commitTime, err := time.Parse(time.RFC3339, commitTimeText)
	if err != nil {
		return bootstrapCommit{}, fmt.Errorf("parse commit time %q: %w", commitTimeText, err)
	}
	if !commitTime.UTC().Equal(timestamp) {
		return bootstrapCommit{}, fmt.Errorf("pseudo-version timestamp %s does not match commit UTC time %s", timestamp.Format(time.RFC3339), commitTime.UTC().Format(time.RFC3339))
	}
	goMod, err := gitBytes(root, "show", commit+":go.mod")
	if err != nil {
		return bootstrapCommit{}, fmt.Errorf("read root go.mod at %s: %w", commit, err)
	}
	return bootstrapCommit{Commit: commit, Version: version, Time: commitTime.UTC(), GoMod: goMod}, nil
}

func verifyRootHasNotDrifted(root, commit string) error {
	pathspec := ":(exclude)" + nestedDirectory + "/**"
	output, err := gitBytes(root, "diff", "--name-only", commit, "HEAD", "--", ".", pathspec)
	if err != nil {
		return fmt.Errorf("compare bootstrap commit to HEAD: %w", err)
	}
	if changed := strings.TrimSpace(string(output)); changed != "" {
		return fmt.Errorf("root module changed after bootstrap commit (only %s may differ): %s", nestedDirectory, strings.ReplaceAll(changed, "\n", ", "))
	}
	for _, args := range [][]string{
		{"diff", "--quiet", "HEAD", "--", ".", pathspec},
		{"diff", "--cached", "--quiet", "HEAD", "--", ".", pathspec},
	} {
		command := controlledinput.GitCommand(root, args...)
		if err := command.Run(); err != nil {
			return errors.New("root module has uncommitted drift outside reference/control-plane")
		}
	}
	return nil
}

func writeProxy(root, proxyRoot string, bootstrap bootstrapCommit) error {
	directory := filepath.Join(proxyRoot, filepath.FromSlash(rootModulePath), "@v")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	info, err := json.Marshal(struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{bootstrap.Version, bootstrap.Time})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, bootstrap.Version+".info"), append(info, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, bootstrap.Version+".mod"), bootstrap.GoMod, 0o644); err != nil {
		return err
	}
	return writeModuleZip(root, filepath.Join(directory, bootstrap.Version+".zip"), bootstrap)
}

func writeModuleZip(root, destination string, bootstrap bootstrapCommit) error {
	entries, err := gitTree(root, bootstrap.Commit)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	closeWith := func(base error) error {
		if closeErr := archive.Close(); base == nil {
			base = closeErr
		}
		if closeErr := file.Close(); base == nil {
			base = closeErr
		}
		return base
	}
	prefix := rootModulePath + "@" + bootstrap.Version + "/"
	const maxModuleZipBytes int64 = 500 << 20
	const maxSpecialFileBytes int64 = 16 << 20
	var totalBytes int64
	caseFolded := map[string]string{}
	for _, entry := range entries {
		if excludedFromModuleZip(entry.Path) {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
			return closeWith(fmt.Errorf("unsupported Git tree entry %s %s at %s", entry.Mode, entry.Type, entry.Path))
		}
		if err := validateModuleArchivePath(entry.Path); err != nil {
			return closeWith(err)
		}
		if err := recordArchivePath(entry.Path, caseFolded); err != nil {
			return closeWith(err)
		}
		data, err := gitBytes(root, "cat-file", "blob", entry.Object)
		if err != nil {
			return closeWith(fmt.Errorf("read blob %s: %w", entry.Path, err))
		}
		totalBytes += int64(len(data))
		if totalBytes > maxModuleZipBytes {
			return closeWith(fmt.Errorf("root module archive exceeds %d bytes", maxModuleZipBytes))
		}
		base := strings.ToLower(filepath.Base(entry.Path))
		if (base == "go.mod" || base == "license") && int64(len(data)) > maxSpecialFileBytes {
			return closeWith(fmt.Errorf("module archive %s exceeds %d bytes", entry.Path, maxSpecialFileBytes))
		}
		header := &zip.FileHeader{Name: prefix + entry.Path, Method: zip.Deflate}
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		if entry.Mode == "100755" {
			header.SetMode(0o755)
		} else {
			header.SetMode(0o644)
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return closeWith(err)
		}
		if _, err := writer.Write(data); err != nil {
			return closeWith(err)
		}
	}
	return closeWith(nil)
}

func validateModuleArchivePath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || len(path) > 1024 {
		return fmt.Errorf("invalid module archive path %q", path)
	}
	if strings.Contains(path, `\`) {
		return fmt.Errorf("module archive path contains backslash: %q", path)
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." || strings.HasSuffix(element, ".") || strings.HasSuffix(element, " ") {
			return fmt.Errorf("invalid module archive path element %q in %q", element, path)
		}
		stem := strings.ToUpper(strings.SplitN(element, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return fmt.Errorf("module archive path uses reserved Windows name %q", element)
		}
		for _, character := range element {
			if character < 0x20 || strings.ContainsRune(`:*?"<>|`, character) {
				return fmt.Errorf("invalid character in module archive path %q", path)
			}
		}
	}
	return nil
}

func excludedFromModuleZip(path string) bool {
	if path == nestedDirectory || strings.HasPrefix(path, nestedDirectory+"/") {
		return true
	}
	for _, element := range strings.Split(path, "/") {
		if element == "vendor" {
			return true
		}
	}
	return false
}

func foldPath(path string) string {
	var folded strings.Builder
	for _, character := range path {
		minimum := character
		for next := unicode.SimpleFold(character); next != character; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

func recordArchivePath(path string, seen map[string]string) error {
	elements := strings.Split(path, "/")
	for index := range elements {
		prefix := strings.Join(elements[:index+1], "/")
		folded := foldPath(prefix)
		if prior := seen[folded]; prior != "" && prior != prefix {
			return fmt.Errorf("case-insensitive module zip path collision: %s and %s", prior, prefix)
		}
		seen[folded] = prefix
	}
	return nil
}

func gitTree(root, commit string) ([]treeEntry, error) {
	output, err := gitBytes(root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("list bootstrap tree: %w", err)
	}
	return parseTreeEntries(output)
}

func parseTreeEntries(output []byte) ([]treeEntry, error) {
	if len(output) != 0 && output[len(output)-1] != 0 {
		return nil, errors.New("git ls-tree output is not NUL terminated")
	}
	entries := []treeEntry{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		parts := bytes.SplitN(raw, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed git ls-tree record %q", raw)
		}
		metadata := strings.Fields(string(parts[0]))
		if len(metadata) != 3 {
			return nil, fmt.Errorf("malformed git ls-tree metadata %q", parts[0])
		}
		entries = append(entries, treeEntry{Mode: metadata[0], Type: metadata[1], Object: metadata[2], Path: string(parts[1])})
	}
	return entries, nil
}

func validateDownload(data []byte, module, version string, wantGoMod []byte, temporaryRoot string) error {
	var result downloadResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode go mod download result: %w", err)
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	if result.Path != module || result.Version != version {
		return fmt.Errorf("download resolved %s@%s, want %s@%s", result.Path, result.Version, module, version)
	}
	if module == rootModulePath {
		wantSum, wantGoModSum := "", ""
		switch version {
		case p05Version:
			wantSum, wantGoModSum = p05ZipH1, p05GoModH1
		case currentVersion:
			wantSum, wantGoModSum = currentZipH1, currentGoModH1
		}
		if wantSum != "" && (result.Sum != wantSum || result.GoModSum != wantGoModSum) {
			return fmt.Errorf("download h1 mismatch: sum=%s go.mod=%s want %s %s", result.Sum, result.GoModSum, wantSum, wantGoModSum)
		}
	}
	for name, path := range map[string]string{"info": result.Info, "go.mod": result.GoMod, "zip": result.Zip, "dir": result.Dir} {
		if path == "" {
			return fmt.Errorf("download result has empty %s path", name)
		}
		relative, err := filepath.Rel(temporaryRoot, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("download %s escaped isolated cache: %s", name, path)
		}
	}
	gotGoMod, err := os.ReadFile(result.GoMod)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotGoMod, wantGoMod) {
		return errors.New("downloaded root go.mod differs from bootstrap commit")
	}
	return nil
}

func loadArtifactManifest(root string) (artifactManifest, error) {
	var manifest artifactManifest
	value, _, err := structuredfile.LoadAny(filepath.Join(root, "spec", "artifact-manifest.yaml"))
	if err != nil {
		return manifest, fmt.Errorf("load artifact manifest: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return manifest, errors.New("artifact manifest is not an object")
	}
	version, ok := object["schema_version"].(int)
	if !ok && fmt.Sprint(object["schema_version"]) == "1" {
		version, ok = 1, true
	}
	items, itemsOK := object["artifacts"].([]any)
	if !ok || !itemsOK {
		return manifest, errors.New("artifact manifest header is invalid")
	}
	manifest.SchemaVersion = version
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return manifest, errors.New("artifact entry is not an object")
		}
		artifact := struct {
			ID         string `yaml:"id"`
			Path       string `yaml:"path"`
			Status     string `yaml:"status"`
			OwnerPhase string `yaml:"owner_phase"`
			PathRole   string `yaml:"path_role"`
		}{ID: fmt.Sprint(entry["id"]), Path: fmt.Sprint(entry["path"]), Status: fmt.Sprint(entry["status"]), OwnerPhase: fmt.Sprint(entry["owner_phase"]), PathRole: fmt.Sprint(entry["path_role"])}
		manifest.Artifacts = append(manifest.Artifacts, artifact)
	}
	if manifest.SchemaVersion != 1 || len(manifest.Artifacts) == 0 {
		return manifest, errors.New("artifact manifest is empty or has an unsupported schema version")
	}
	seen := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == "" || artifact.Path == "" || seen[artifact.ID] {
			return manifest, fmt.Errorf("artifact manifest has empty/duplicate identity %q", artifact.ID)
		}
		seen[artifact.ID] = true
	}
	return manifest, nil
}

func owningArtifact(path string, manifest artifactManifest) string {
	bestID, bestPath := "", ""
	for _, artifact := range manifest.Artifacts {
		if artifact.PathRole != "concrete" {
			continue
		}
		candidate := filepath.ToSlash(filepath.Clean(artifact.Path))
		if path != candidate && !strings.HasPrefix(path, candidate+"/") {
			continue
		}
		if len(candidate) > len(bestPath) {
			bestID, bestPath = artifact.ID, candidate
		}
	}
	return bestID
}

func transitionArtifacts(paths []string, manifest artifactManifest) ([]string, error) {
	ids := map[string]bool{}
	for _, path := range paths {
		id := owningArtifact(path, manifest)
		if id == "" && path == nestedDirectory+"/go.sum" {
			id = "nested-control-plane-go-module"
		}
		if id == "" && path == nestedDirectory+"/cmd/aropd/main_test.go" {
			id = "reference-control-plane-server"
		}
		if id == "" {
			return nil, fmt.Errorf("transition path %s has no artifact owner", path)
		}
		ids[id] = true
	}
	return sortedBoolKeys(ids), nil
}

func p05OwnedPaths(manifest artifactManifest) ([]string, error) {
	paths := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase != "P05" || artifact.PathRole != "concrete" {
			continue
		}
		if artifact.Status != "present" && artifact.Status != "planned" {
			return nil, fmt.Errorf("P05 artifact %s has unsupported status %s", artifact.ID, artifact.Status)
		}
		path := filepath.ToSlash(filepath.Clean(artifact.Path))
		paths[path] = true
		if artifact.ID == "nested-control-plane-go-module" {
			paths[nestedDirectory+"/go.sum"] = true
		}
	}
	result := sortedBoolKeys(paths)
	if len(result) == 0 {
		return nil, errors.New("manifest produced no P05-owned paths")
	}
	return result, nil
}

func discoverTransitionRecords(root string) ([]string, error) {
	output, err := gitBytes(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*-p05-transition.json")
	if err != nil {
		return nil, fmt.Errorf("discover transition carriers: %w", err)
	}
	return parseNULPaths(output)
}

func validateExactSet(got, want []string, label string) error {
	if !equalStrings(got, want) {
		return fmt.Errorf("%s=%v want=%v", label, got, want)
	}
	return nil
}

func changedPaths(root, from, to string, pathspecs []string) ([]string, error) {
	args := []string{"diff", "--no-renames", "--name-only", "-z", from, to, "--"}
	args = append(args, pathspecs...)
	output, err := gitBytes(root, args...)
	if err != nil {
		return nil, fmt.Errorf("discover transition %s..%s: %w", from, to, err)
	}
	return parseNULPaths(output)
}

func discoverTouchHistory(root, baseline, result string, pathspecs []string) ([]transitionTouch, error) {
	args := []string{"log", "--format=%H", "--reverse", baseline + ".." + result, "--"}
	args = append(args, pathspecs...)
	output, err := gitBytes(root, args...)
	if err != nil {
		return nil, fmt.Errorf("discover touch commits: %w", err)
	}
	commits := strings.Fields(string(output))
	touches := make([]transitionTouch, 0, len(commits))
	for _, commit := range commits {
		parent, err := gitText(root, "rev-parse", commit+"^")
		if err != nil {
			return nil, err
		}
		paths, err := changedPaths(root, parent, commit, pathspecs)
		if err != nil {
			return nil, err
		}
		if len(paths) != 0 {
			touches = append(touches, transitionTouch{Commit: commit, ChangedPaths: paths})
		}
	}
	return touches, nil
}

func recordedOwnedTouches(records []transitionRecord, owned map[string]bool) []transitionTouch {
	touches := []transitionTouch{}
	for _, record := range records {
		for _, hop := range record.History {
			paths := []string{}
			for _, path := range hop.ChangedPaths {
				if owned[path] {
					paths = append(paths, path)
				}
			}
			sort.Strings(paths)
			if len(paths) != 0 {
				touches = append(touches, transitionTouch{Commit: hop.Commit, ChangedPaths: paths})
			}
		}
	}
	return touches
}

func equalTouches(got, want []transitionTouch) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index].Commit != want[index].Commit || !equalStrings(got[index].ChangedPaths, want[index].ChangedPaths) {
			return false
		}
	}
	return true
}

func validateP05TouchLedger(discovered, expected []transitionTouch, owned map[string]bool, checkerPath, head string, checkerDirty bool, parentOf func(string) (string, error)) (string, error) {
	ledgerTouches := []transitionTouch{}
	checkerTouches := []transitionTouch{}
	for _, touch := range discovered {
		ownedPaths := []string{}
		for _, path := range touch.ChangedPaths {
			if owned[path] {
				ownedPaths = append(ownedPaths, path)
			}
		}
		sort.Strings(ownedPaths)
		if len(ownedPaths) == 0 {
			continue
		}
		containsChecker := false
		for _, path := range ownedPaths {
			containsChecker = containsChecker || path == checkerPath
		}
		ownedTouch := transitionTouch{Commit: touch.Commit, ChangedPaths: ownedPaths}
		if containsChecker {
			if len(ownedPaths) != 1 {
				return "", fmt.Errorf("checker correction commit %s also touched P05-owned paths %v", touch.Commit, ownedPaths)
			}
			checkerTouches = append(checkerTouches, ownedTouch)
			continue
		}
		ledgerTouches = append(ledgerTouches, ownedTouch)
	}
	if !equalTouches(ledgerTouches, expected) {
		return "", fmt.Errorf("P05-owned touch history=%v covered=%v", ledgerTouches, expected)
	}
	switch len(checkerTouches) {
	case 0:
		if head != correctionBase || !checkerDirty {
			return "", fmt.Errorf("checker has no bounded correction commit: HEAD=%s dirty=%t", head, checkerDirty)
		}
		return "", nil
	case 2:
		parent, err := parentOf(checkerTouches[0].Commit)
		if err != nil {
			return "", err
		}
		p12Parent, err := parentOf(checkerTouches[1].Commit)
		if err != nil {
			return "", err
		}
		if parent != correctionBase || p12Parent != p12CorrectionBase || checkerDirty {
			return "", fmt.Errorf("checker corrections=%v parents=%s,%s dirty=%t", checkerTouches, parent, p12Parent, checkerDirty)
		}
		return checkerTouches[1].Commit, nil
	default:
		return "", fmt.Errorf("checker has %d correction touches after %s, want exactly one", len(checkerTouches), p05Commit)
	}
}

func pathDiffersFromHEAD(root, path string) (bool, error) {
	command := controlledinput.GitCommand(root, "diff", "--quiet", "HEAD", "--", path)
	err := command.Run()
	if err == nil {
		return false, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("compare %s with HEAD: %w", path, err)
}

func parseNULPaths(output []byte) ([]string, error) {
	if len(output) == 0 {
		return []string{}, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git path output is not NUL terminated")
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, raw := range bytes.Split(output[:len(output)-1], []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "" || strings.ContainsRune(path, 0) || filepath.IsAbs(filepath.FromSlash(path)) || path == ".." || strings.HasPrefix(path, "../") || filepath.ToSlash(filepath.Clean(path)) != path {
			return nil, fmt.Errorf("unsafe Git path %q", path)
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate Git path %q", path)
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func loadTransitionRecord(root, path string) (transitionRecord, []byte, error) {
	absolute := filepath.Join(root, filepath.FromSlash(path))
	info, err := os.Lstat(absolute)
	if err != nil {
		return transitionRecord{}, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return transitionRecord{}, nil, fmt.Errorf("transition carrier %s is not a regular non-symlink file", path)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return transitionRecord{}, nil, err
	}
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return transitionRecord{}, nil, fmt.Errorf("strict parse transition record: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record transitionRecord
	if err := decoder.Decode(&record); err != nil {
		return transitionRecord{}, nil, fmt.Errorf("strict decode transition record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return transitionRecord{}, nil, errors.New("transition record contains a trailing JSON value")
		}
		return transitionRecord{}, nil, fmt.Errorf("transition record trailing content: %w", err)
	}
	if err := validateTransitionRecordShape(record); err != nil {
		return transitionRecord{}, nil, err
	}
	return record, data, nil
}

func validateTransitionCarrier(path, phase, artifactID string, manifest artifactManifest) error {
	bestID, bestPhase, bestPath := "", "", ""
	for _, artifact := range manifest.Artifacts {
		candidate := filepath.ToSlash(filepath.Clean(artifact.Path))
		if path != candidate && !strings.HasPrefix(path, candidate+"/") {
			continue
		}
		if len(candidate) > len(bestPath) {
			bestID, bestPhase, bestPath = artifact.ID, artifact.OwnerPhase, candidate
		}
	}
	if bestID != artifactID || bestPhase != phase {
		return fmt.Errorf("transition carrier %s maps to artifact=%s owner_phase=%s, want %s/%s", path, bestID, bestPhase, artifactID, phase)
	}
	return nil
}

func validateAffectedArtifactOwners(ids []string, want map[string]string, manifest artifactManifest) error {
	actual := map[string]string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.PathRole == "concrete" {
			actual[artifact.ID] = artifact.OwnerPhase
		}
	}
	if len(ids) != len(want) {
		return fmt.Errorf("affected artifact owner count=%d want=%d", len(ids), len(want))
	}
	for _, id := range ids {
		phase, ok := want[id]
		if !ok || actual[id] != phase {
			return fmt.Errorf("affected artifact %s owner_phase=%s want=%s", id, actual[id], phase)
		}
	}
	return nil
}

func validateTransitionRecordShape(record transitionRecord) error {
	if record.SchemaVersion != 1 || record.TransitionID == "" || record.Status != "validated" || record.FromPhase != "P05" || record.ToPhase == "" || len(record.BaselineCommit) != 40 || len(record.ResultCommit) != 40 || record.Reason == "" {
		return errors.New("transition record identity/commit shape is invalid")
	}
	if record.Policy != (transitionPolicy{"first-introduction-and-accountability", false, "git-object-replay"}) {
		return errors.New("transition record policy mismatch")
	}
	for label, values := range map[string][]string{"changed_paths": record.ChangedPaths, "affected_artifacts": record.AffectedArtifacts} {
		if !sort.StringsAreSorted(values) {
			return fmt.Errorf("%s must be sorted", label)
		}
		for index, value := range values {
			if value == "" || index > 0 && values[index-1] == value {
				return fmt.Errorf("%s contains empty/duplicate value", label)
			}
		}
	}
	if record.RootPin != nil {
		for _, pin := range []rootPin{record.RootPin.Before, record.RootPin.After} {
			if len(pin.Commit) != 40 || pin.Version == "" || !strings.HasPrefix(pin.ZipH1, "h1:") || !strings.HasPrefix(pin.GoModH1, "h1:") {
				return errors.New("transition pin is incomplete")
			}
		}
	}
	if len(record.History) == 0 || len(record.Acceptance) != 2 || len(record.Constraints) == 0 {
		return errors.New("transition history/acceptance/constraints are incomplete")
	}
	return nil
}

func compareTransitionRecord(got, want transitionRecord) error {
	gotJSON, err := json.Marshal(got)
	if err != nil {
		return err
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		return fmt.Errorf("record=%s want=%s", gotJSON, wantJSON)
	}
	return nil
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, present := range values {
		if present {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func evidenceValue(kind string, data []byte) report.RuntimeEvidence {
	return report.RuntimeEvidence{Kind: kind, SHA256: report.Hash(data), Bytes: int64(len(data))}
}

func validateListedModule(data []byte, module, version, repositoryRoot, temporaryRoot string) error {
	var listed listedModule
	if err := json.Unmarshal(data, &listed); err != nil {
		return fmt.Errorf("decode go list -m result: %w", err)
	}
	if listed.Path != module || listed.Version != version {
		return fmt.Errorf("go list resolved %s@%s, want %s@%s", listed.Path, listed.Version, module, version)
	}
	if listed.Replace != nil {
		return errors.New("go list reported a replacement for the root module")
	}
	if listed.Dir == "" || listed.GoMod == "" {
		return errors.New("go list did not report cached module paths")
	}
	for label, path := range map[string]string{"module directory": listed.Dir, "go.mod": listed.GoMod} {
		if !inside(temporaryRoot, path) {
			return fmt.Errorf("listed %s is not in the isolated cache: %s", label, path)
		}
		if inside(repositoryRoot, path) {
			return fmt.Errorf("listed %s unexpectedly points inside the repository: %s", label, path)
		}
	}
	return nil
}

func verifyFrozenBootstrap(root, proxyRoot, moduleCache, tempDirectory string, replay *replayEvidence) error {
	bootstrap, err := resolveBootstrapCommit(root, p05Version)
	if err != nil {
		return err
	}
	if bootstrap.Commit != p05Bootstrap {
		return fmt.Errorf("P05 pseudo-version resolved %s want %s", bootstrap.Commit, p05Bootstrap)
	}
	if err := writeProxy(root, proxyRoot, bootstrap); err != nil {
		return err
	}
	scratch := filepath.Join(filepath.Dir(tempDirectory), "p05-download")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "go.mod"), []byte("module example.invalid/p05-replay\n\ngo 1.24.0\n"), 0o600); err != nil {
		return err
	}
	proxyURL := (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",off"
	output, err := runGoWithProxy(scratch, goEnvironment(map[string]string{
		"GOMODCACHE": moduleCache,
		"GOTMPDIR":   tempDirectory,
		"GOPROXY":    proxyURL,
		"GONOSUMDB":  "*",
		"GOSUMDB":    "off",
	}, false), "mod", "download", "-json", rootModulePath+"@"+p05Version)
	if err != nil {
		return err
	}
	var downloaded downloadResult
	if err := json.Unmarshal(output, &downloaded); err != nil {
		return fmt.Errorf("decode frozen P05 download: %w", err)
	}
	if downloaded.Path != rootModulePath || downloaded.Version != p05Version || downloaded.Sum != p05ZipH1 || downloaded.GoModSum != p05GoModH1 {
		return fmt.Errorf("frozen P05 module digest mismatch: path=%s version=%s sum=%s gomod=%s", downloaded.Path, downloaded.Version, downloaded.Sum, downloaded.GoModSum)
	}
	zipData, err := os.ReadFile(downloaded.Zip)
	if err != nil {
		return err
	}
	for kind, value := range map[string]string{
		"p05-closure-commit":      p05Commit,
		"p05-closure-tree":        p05Tree,
		"p05-bootstrap-commit":    p05Bootstrap,
		"p05-bootstrap-version":   p05Version,
		"p05-bootstrap-zip-h1":    p05ZipH1,
		"p05-bootstrap-go-mod-h1": p05GoModH1,
	} {
		replay.Items = append(replay.Items, evidenceValue(kind, []byte(value)))
	}
	replay.Items = append(replay.Items, evidenceValue("p05-bootstrap-module-archive", zipData))
	return nil
}

func validateCurrentPin(bootstrap bootstrapCommit) error {
	if bootstrap.Commit != currentPin || bootstrap.Version != currentVersion {
		return fmt.Errorf("current pin is %s@%s want %s@%s", bootstrap.Commit, bootstrap.Version, currentPin, currentVersion)
	}
	return nil
}

func verifyReplayTransitions(root string, bootstrap bootstrapCommit, goListOutput []byte, replay replayEvidence) (replayEvidence, error) {
	paths, artifacts, err := discoverPinnedConsumedClosure(root, bootstrap, goListOutput)
	if err != nil {
		return replay, err
	}
	replay.ConsumedPaths = paths
	replay.AffectedArtifacts = artifacts
	replay.Items = append(replay.Items,
		evidenceValue("pinned-root-consumed-closure", []byte(strings.Join(paths, "\x00"))),
		evidenceValue("pinned-root-consumed-artifacts", []byte(strings.Join(artifacts, "\x00"))),
	)

	p05Pin := rootPin{p05Bootstrap, p05Version, p05ZipH1, p05GoModH1}
	activePin := rootPin{currentPin, currentVersion, currentZipH1, currentGoModH1}
	p06Commit := "a2bfb31bde3fe874e3a9ef215f1dbdd934d1b092"
	p09Result := "9112737522a72a72b688a8ab01c4ffeaf7a61f37"
	p06, err := expectedTransition(root, "P06-P05-MODULE-TRANSITION-001", "P06", p05Commit, p06Commit,
		[]string{"go.mod", "go.sum"}, []string{p06Commit}, []string{"root-go-module"}, nil,
		"P06 updates root module metadata while retaining the frozen P05 nested pin.", "make test-protocol-foundation", "build/reports/P06/report.json")
	if err != nil {
		return replay, err
	}
	p08, err := expectedTransition(root, "P08-P05-MODULE-TRANSITION-001", "P08", currentPin, p08Commit,
		[]string{nestedDirectory + "/cmd/aropd/main.go", nestedDirectory + "/cmd/aropd/main_test.go", nestedDirectory + "/go.mod", nestedDirectory + "/go.sum"}, []string{p08Commit}, []string{"nested-control-plane-go-module", "reference-control-plane-server"}, &pinTransition{p05Pin, activePin},
		"P08 extends the migrated P05 server baseline and repins the nested module to the exact root snapshot consumed by the platform foundation.", "make test-control-plane-platform", "build/reports/P08/report.json")
	if err != nil {
		return replay, err
	}
	p09, err := expectedTransition(root, "P09-P05-MODULE-TRANSITION-001", "P09", p08Commit, p09Result,
		[]string{nestedDirectory + "/cmd/aropd/main.go", nestedDirectory + "/cmd/aropd/main_test.go", nestedDirectory + "/go.mod", nestedDirectory + "/go.sum"}, []string{"c279071e71ec62e076ebf0d678346de89089c325", p09Result}, []string{"nested-control-plane-go-module", "reference-control-plane-server"}, nil,
		"P09 extends the P05-migrated server composition while retaining the P08 root pin.", "make test-storage-migrations", "build/reports/P09/report.json")
	if err != nil {
		return replay, err
	}
	p12Result := "24f62398736e6cac99eb8d6e6f011879dc7c05a4"
	p12Baseline := "75ba8694c47f4500725113bbf5554dde4c901d03"
	p12, err := expectedTransition(root, "P12-P05-MODULE-TRANSITION-001", "P12", p12Baseline, p12Result,
		[]string{nestedDirectory + "/go.mod", nestedDirectory + "/go.sum"}, []string{"0262c7d5b55b6ef02f8c6b002b7381232c508580", p12Result}, []string{"nested-control-plane-go-module"}, &pinTransition{activePinBeforeP12(), activePin},
		"P12 repins the nested Control Plane to the clean committed root snapshot that contains the accepted P11 generated publication model consumed by production composition.", "make test-publication-service", "build/reports/P12/report.json")
	if err != nil {
		return replay, err
	}
	expected := []transitionRecord{p06, p08, p09, p12}
	manifest, err := loadArtifactManifest(root)
	if err != nil {
		return replay, err
	}
	discoveredRecords, err := discoverTransitionRecords(root)
	if err != nil {
		return replay, err
	}
	if err := validateExactSet(discoveredRecords, transitionPaths, "transition record set"); err != nil {
		return replay, err
	}
	expectedCarrierArtifacts := []string{"conformance-harness-base", "control-plane-platform-foundation", "migration-engine-fixture-versions", "arop-cli-publication-command"}
	expectedAffectedOwners := []map[string]string{
		{"root-go-module": "P05"},
		{"nested-control-plane-go-module": "P05", "reference-control-plane-server": "P08"},
		{"nested-control-plane-go-module": "P05"},
		{"nested-control-plane-go-module": "P05", "reference-control-plane-server": "P08"},
	}
	for index, path := range transitionPaths {
		if err := validateTransitionCarrier(path, expected[index].ToPhase, expectedCarrierArtifacts[index], manifest); err != nil {
			return replay, err
		}
		record, data, err := loadTransitionRecord(root, path)
		if err != nil {
			return replay, err
		}
		if err := compareTransitionRecord(record, expected[index]); err != nil {
			return replay, fmt.Errorf("%s: %w", path, err)
		}
		_ = data // tracked carrier bytes are bound by static report inputs, not runtime evidence.
		derivedArtifacts, err := transitionArtifacts(record.ChangedPaths, manifest)
		if err != nil || !equalStrings(derivedArtifacts, record.AffectedArtifacts) {
			return replay, errors.Join(err, fmt.Errorf("%s affected_artifacts=%v want independently discovered %v", path, record.AffectedArtifacts, derivedArtifacts))
		}
		if err := validateAffectedArtifactOwners(record.AffectedArtifacts, expectedAffectedOwners[index], manifest); err != nil {
			return replay, fmt.Errorf("%s: %w", path, err)
		}
		replay.RecordedTransitionPaths = append(replay.RecordedTransitionPaths, record.ChangedPaths...)
	}
	p05Owned, err := p05OwnedPaths(manifest)
	if err != nil {
		return replay, err
	}
	discoveredTouches, err := discoverTouchHistory(root, p05Commit, "HEAD", p05Owned)
	if err != nil {
		return replay, err
	}
	ownedSet := map[string]bool{}
	for _, path := range p05Owned {
		ownedSet[path] = true
	}
	const checkerPath = "internal/tooling/cmd/arop-go-proxy-bootstrap/main.go"
	expectedTouches := recordedOwnedTouches(expected, ownedSet)
	checkerDirty, err := pathDiffersFromHEAD(root, checkerPath)
	if err != nil {
		return replay, err
	}
	head, err := gitText(root, "rev-parse", "HEAD")
	if err != nil {
		return replay, err
	}
	replay.CorrectionCommit, err = validateP05TouchLedger(discoveredTouches, expectedTouches, ownedSet, checkerPath, head, checkerDirty, func(commit string) (string, error) {
		return gitText(root, "rev-parse", commit+"^")
	})
	if err != nil {
		return replay, err
	}
	validatedHistory, err := json.Marshal(expected)
	if err != nil {
		return replay, err
	}
	replay.Items = append(replay.Items, evidenceValue("validated-transition-history", validatedHistory))
	if replay.CorrectionCommit != "" {
		replay.Items = append(replay.Items, evidenceValue("checker-correction-commit", []byte(replay.CorrectionCommit)))
	}
	if bootstrap.Commit != currentPin {
		return replay, errors.New("transition ledger did not finish at the active root pin")
	}
	replay.Items = append(replay.Items,
		evidenceValue("active-root-pin-chain", []byte(p05Version+"\n"+activePinBeforeP12().Version+"\n"+currentVersion+"\n")),
		evidenceValue("recorded-transition-paths", []byte(strings.Join(replay.RecordedTransitionPaths, "\x00"))),
	)
	return replay, nil
}

func activePinBeforeP12() rootPin {
	return rootPin{"1195e393bc0629b792a09e4401f3ac91e6af2f64", "v0.0.0-20260923092055-1195e393bc06", "h1:3tJrUIY49T4KBh43mBrY/+wzMcZ5J7bxbF3odles/Ns=", "h1:N4IdtBpzQjKJuhxiIlhJsGn/zIiC1jgKPB9TZKRBmZk="}
}

func expectedTransition(root, id, toPhase, baseline, result string, pathspecs, commits, artifacts []string, pin *pinTransition, reason, command, reportPath string) (transitionRecord, error) {
	paths, err := changedPaths(root, baseline, result, pathspecs)
	if err != nil {
		return transitionRecord{}, err
	}
	history, err := transitionHistory(root, baseline, result, pathspecs, commits)
	if err != nil {
		return transitionRecord{}, err
	}
	sort.Strings(artifacts)
	return transitionRecord{
		SchemaVersion: 1, TransitionID: id, Status: "validated",
		Policy:    transitionPolicy{"first-introduction-and-accountability", false, "git-object-replay"},
		FromPhase: "P05", ToPhase: toPhase, BaselineCommit: baseline, ResultCommit: result, Reason: reason,
		ChangedPaths: paths, AffectedArtifacts: artifacts, History: history, RootPin: pin,
		Acceptance:  []transitionAcceptance{{"P05", "make test-go-workspace", "build/reports/P05/report.json"}, {toPhase, command, reportPath}},
		Constraints: []string{"frozen-p05-archive-remains-immutable", "no-unrecorded-p05-owned-drift", "runtime-inputs-remain-empty"},
	}, nil
}

func transitionHistory(root, baseline, result string, pathspecs, wantCommits []string) ([]transitionHop, error) {
	args := []string{"log", "--format=%H", "--reverse", baseline + ".." + result, "--"}
	args = append(args, pathspecs...)
	output, err := gitBytes(root, args...)
	if err != nil {
		return nil, err
	}
	got := strings.Fields(string(output))
	if !equalStrings(got, wantCommits) {
		return nil, fmt.Errorf("transition touching commits=%v want=%v", got, wantCommits)
	}
	history := make([]transitionHop, 0, len(got))
	for _, commit := range got {
		parent, err := gitText(root, "rev-parse", commit+"^")
		if err != nil {
			return nil, err
		}
		paths, err := changedPaths(root, parent, commit, pathspecs)
		if err != nil {
			return nil, err
		}
		before, after := []pathDigest{}, []pathDigest{}
		for _, path := range paths {
			beforeRecord, beforeExists, err := gitObjectRecord(root, parent, path)
			if err != nil {
				return nil, err
			}
			afterRecord, afterExists, err := gitObjectRecord(root, commit, path)
			if err != nil {
				return nil, err
			}
			if beforeExists {
				before = append(before, beforeRecord)
			}
			if afterExists {
				after = append(after, afterRecord)
			}
		}
		history = append(history, transitionHop{commit, paths, before, after})
	}
	return history, nil
}

func gitObjectRecord(root, commit, path string) (pathDigest, bool, error) {
	output, err := gitBytes(root, "ls-tree", "-z", "--full-tree", commit, "--", path)
	if err != nil {
		return pathDigest{}, false, err
	}
	if len(output) == 0 {
		return pathDigest{}, false, nil
	}
	entries, err := parseTreeEntries(output)
	if err != nil || len(entries) != 1 || entries[0].Path != path {
		return pathDigest{}, false, errors.Join(err, fmt.Errorf("Git object lookup for %s returned %d entries", path, len(entries)))
	}
	entry := entries[0]
	data, err := gitBytes(root, "cat-file", entry.Type, entry.Object)
	if err != nil {
		return pathDigest{}, false, err
	}
	return pathDigest{Path: path, Type: entry.Type, Mode: entry.Mode, SHA256: report.Hash(data)}, true, nil
}

func discoverPinnedConsumedClosure(root string, bootstrap bootstrapCommit, output []byte) ([]string, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	pathSet := map[string]bool{"go.mod": true}
	cachePaths := map[string]string{}
	for {
		var pkg listedPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("decode go list dependency: %w", err)
		}
		baseImport := strings.SplitN(pkg.ImportPath, " [", 2)[0]
		if baseImport != rootModulePath && !strings.HasPrefix(baseImport, rootModulePath+"/") || strings.HasPrefix(baseImport, nestedModulePath) {
			continue
		}
		directory := strings.TrimPrefix(baseImport, rootModulePath)
		directory = strings.TrimPrefix(directory, "/")
		files := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SysoFiles, pkg.EmbedFiles}
		for _, group := range files {
			for _, name := range group {
				clean := filepath.ToSlash(filepath.Clean(name))
				if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(filepath.FromSlash(clean)) {
					return nil, nil, fmt.Errorf("go list returned unsafe source %q", name)
				}
				path := filepath.ToSlash(filepath.Join(directory, clean))
				pathSet[path] = true
				cachePaths[path] = filepath.Join(pkg.Dir, filepath.FromSlash(clean))
			}
		}
	}
	entries, err := gitTree(root, bootstrap.Commit)
	if err != nil {
		return nil, nil, err
	}
	objects := map[string]string{}
	for _, entry := range entries {
		if entry.Type == "blob" {
			objects[entry.Path] = entry.Object
		}
	}
	paths := sortedBoolKeys(pathSet)
	for _, path := range paths {
		object := objects[path]
		if object == "" {
			return nil, nil, fmt.Errorf("consumed pinned source %s is absent from Git tree %s", path, bootstrap.Commit)
		}
		var actual []byte
		if path == "go.mod" {
			actual = bootstrap.GoMod
		} else {
			actual, err = os.ReadFile(cachePaths[path])
			if err != nil {
				return nil, nil, fmt.Errorf("read consumed cached source %s: %w", path, err)
			}
		}
		want, err := gitBytes(root, "cat-file", "blob", object)
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(actual, want) {
			return nil, nil, fmt.Errorf("consumed cached source %s differs from pinned Git object", path)
		}
	}
	manifest, err := loadArtifactManifest(root)
	if err != nil {
		return nil, nil, err
	}
	artifactSet := map[string]bool{}
	for _, path := range paths {
		id := owningArtifact(path, manifest)
		if id == "" {
			return nil, nil, fmt.Errorf("consumed pinned source %s has no artifact-manifest owner", path)
		}
		artifactSet[id] = true
	}
	return paths, sortedBoolKeys(artifactSet), nil
}

func prepareNestedGoSum(root string) error {
	nestedRoot := filepath.Join(root, filepath.FromSlash(nestedDirectory))
	nestedModule, loadErr := loadModule(nestedRoot)
	version, err := validateNestedModule(nestedModule, loadErr)
	if err != nil {
		return err
	}
	bootstrap, err := resolveBootstrapCommit(root, version)
	if err != nil {
		return err
	}
	if err := validateCurrentPin(bootstrap); err != nil {
		return err
	}
	temporaryRoot, err := os.MkdirTemp("", "arop-go-sum-bootstrap-")
	if err != nil {
		return err
	}
	defer removeAllWritable(temporaryRoot)
	proxyRoot := filepath.Join(temporaryRoot, "proxy")
	moduleCache := filepath.Join(temporaryRoot, "modcache")
	buildCache := filepath.Join(temporaryRoot, "buildcache")
	tempDirectory := filepath.Join(temporaryRoot, "tmp")
	for _, directory := range []string{moduleCache, buildCache, tempDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
	}
	if err := writeProxy(root, proxyRoot, bootstrap); err != nil {
		return err
	}
	goModPath := filepath.Join(nestedRoot, "go.mod")
	goModBefore, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	goSumPath := filepath.Join(nestedRoot, "go.sum")
	oldSum, oldSumErr := os.ReadFile(goSumPath)
	prepared := false
	defer func() {
		if prepared {
			return
		}
		_ = os.WriteFile(goModPath, goModBefore, 0o644)
		if oldSumErr == nil {
			_ = os.WriteFile(goSumPath, oldSum, 0o644)
		} else {
			_ = os.Remove(goSumPath)
		}
	}()
	filteredSum := filterRootModuleSums(oldSum)
	if err := os.WriteFile(goSumPath, filteredSum, 0o644); err != nil {
		return fmt.Errorf("prepare nested go.sum: %w", err)
	}
	proxyURL := (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",off"
	environment := goEnvironment(map[string]string{
		"GOCACHE":    buildCache,
		"GOMODCACHE": moduleCache,
		"GOTMPDIR":   tempDirectory,
		"GONOSUMDB":  "*",
		"GOPROXY":    proxyURL,
		"GOSUMDB":    "off",
	}, false)
	environment["GOFLAGS"] = "-mod=mod"
	if _, err := runGoWithProxy(nestedRoot, environment, "mod", "download", rootModulePath+"@"+version); err != nil {
		return err
	}
	goModAfter, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(goModBefore, goModAfter) {
		return errors.New("go sum bootstrap unexpectedly changed nested go.mod")
	}
	newSum, err := os.ReadFile(goSumPath)
	if err != nil {
		return fmt.Errorf("go sum bootstrap did not create nested go.sum: %w", err)
	}
	if err := validateNestedRootSums(newSum, version); err != nil {
		return err
	}
	prepared = true
	return nil
}

func filterRootModuleSums(data []byte) []byte {
	kept := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, rootModulePath+" ") {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return nil
	}
	return []byte(strings.Join(kept, "\n") + "\n")
}

func validateNestedRootSums(data []byte, version string) error {
	want := map[string]bool{
		rootModulePath + " " + version:             false,
		rootModulePath + " " + version + "/go.mod": false,
	}
	rootLines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, rootModulePath+" ") {
			continue
		}
		rootLines++
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "h1:") {
			return fmt.Errorf("malformed root module checksum line %q", line)
		}
		key := fields[0] + " " + fields[1]
		if _, ok := want[key]; !ok {
			return fmt.Errorf("nested go.sum contains stale or unexpected root module checksum %q", key)
		}
		if want[key] {
			return fmt.Errorf("nested go.sum contains duplicate root module checksum %q", key)
		}
		want[key] = true
	}
	if rootLines != len(want) {
		return fmt.Errorf("nested go.sum root checksum count = %d, want %d for %s", rootLines, len(want), version)
	}
	for key, found := range want {
		if !found {
			return fmt.Errorf("nested go.sum is missing %s", key)
		}
	}
	return nil
}

func inside(root, path string) bool {
	rootAbsolute, rootErr := filepath.Abs(root)
	pathAbsolute, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbsolute, pathAbsolute)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func trackedRepositoryInputs(root string) ([]string, error) {
	output, err := gitBytes(root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list tracked report inputs: %w", err)
	}
	paths := []string{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		relative := filepath.ToSlash(string(raw))
		if relative == "" || filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || relative == ".." || strings.HasPrefix(relative, "../") {
			return nil, fmt.Errorf("unsafe tracked report input path %q", relative)
		}
		current := root
		for _, component := range strings.Split(relative, "/") {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if err != nil {
				return nil, fmt.Errorf("inspect tracked report input %s: %w", relative, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("tracked report input path contains symlink: %s", relative)
			}
		}
		info, err := os.Stat(current)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("tracked report input is not a regular file: %s", relative)
		}
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errors.New("tracked report input closure is empty")
	}
	return paths, nil
}

func sourceTreeClean(root string) error {
	output, err := gitBytes(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect source tree cleanliness: %w", err)
	}
	if dirty := strings.TrimSpace(string(output)); dirty != "" {
		return fmt.Errorf("source tree is dirty: %s", strings.ReplaceAll(dirty, "\n", "; "))
	}
	return nil
}

func removeAllWritable(path string) error {
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}

func negativeThirdModuleProbe() error {
	if requireExactModules([]string{"go.mod", "other/go.mod", nestedDirectory + "/go.mod"}) == nil {
		return errors.New("third module negative probe was accepted")
	}
	return nil
}

func negativeReplaceProbe() error {
	module := moduleFile{}
	module.Module.Path = nestedModulePath
	module.Require = append(module.Require, struct {
		Path     string `json:"Path"`
		Version  string `json:"Version"`
		Indirect bool   `json:"Indirect"`
	}{Path: rootModulePath, Version: "v0.0.0-20260923000000-123456789abc"})
	module.Replace = append(module.Replace, struct {
		Old struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		} `json:"Old"`
		New struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		} `json:"New"`
	}{})
	if _, err := validateNestedModule(module, nil); err == nil {
		return errors.New("replace directive negative probe was accepted")
	}
	return nil
}

func negativeVersionProbe() error {
	module := moduleFile{}
	module.Module.Path = nestedModulePath
	module.Require = append(module.Require, struct {
		Path     string `json:"Path"`
		Version  string `json:"Version"`
		Indirect bool   `json:"Indirect"`
	}{Path: rootModulePath, Version: "v0.0.0"})
	if _, err := validateNestedModule(module, nil); err == nil {
		return errors.New("non-pseudo root version negative probe was accepted")
	}
	if _, _, err := parsePseudoVersion(placeholder); err == nil {
		return errors.New("bootstrap placeholder negative probe was accepted")
	}
	return nil
}

func negativeModuleArchiveProbe() error {
	for _, path := range []string{"vendor/x.go", "foo/vendor/x.go", "foo/bar/vendor/data.json"} {
		if !excludedFromModuleZip(path) {
			return fmt.Errorf("module zip would include vendor path %q", path)
		}
	}
	for _, path := range []string{"../escape", `foo\\bar`, "foo/./bar", "foo/CON"} {
		if validateModuleArchivePath(path) == nil {
			return fmt.Errorf("unsafe module archive path %q was accepted", path)
		}
	}
	seen := map[string]string{}
	if err := recordArchivePath("Readme.md", seen); err != nil {
		return err
	}
	if err := recordArchivePath("README.md", seen); err == nil {
		return errors.New("case-fold module archive collision was accepted")
	}
	return nil
}

func negativeTransitionRecordProbe(root string) error {
	pin := rootPin{p05Bootstrap, p05Version, p05ZipH1, p05GoModH1}
	want := transitionRecord{
		SchemaVersion: 1, TransitionID: "probe", Status: "validated",
		Policy:    transitionPolicy{"first-introduction-and-accountability", false, "git-object-replay"},
		FromPhase: "P05", ToPhase: "P06", BaselineCommit: p05Commit, ResultCommit: p05Bootstrap, Reason: "probe",
		ChangedPaths: []string{"go.mod"}, AffectedArtifacts: []string{"root-go-module"},
		History:     []transitionHop{{p05Bootstrap, []string{"go.mod"}, []pathDigest{{"go.mod", "blob", "100644", report.Hash(nil)}}, []pathDigest{{"go.mod", "blob", "100644", report.Hash([]byte("after"))}}}},
		RootPin:     &pinTransition{pin, pin},
		Acceptance:  []transitionAcceptance{{"P05", "make test-go-workspace", "build/reports/P05/report.json"}, {"P06", "make test-protocol-foundation", "build/reports/P06/report.json"}},
		Constraints: []string{"probe"},
	}
	if err := validateTransitionRecordShape(want); err != nil {
		return fmt.Errorf("valid transition probe failed: %w", err)
	}
	mutations := []transitionRecord{want, want, want, want, want}
	mutations[0].ChangedPaths = nil
	mutations[1].ChangedPaths = []string{"go.mod", "go.sum"}
	mutations[2].AffectedArtifacts = []string{"wrong-owner"}
	changedPin := *mutations[3].RootPin
	changedPin.After.Commit = currentPin
	mutations[3].RootPin = &changedPin
	mutations[4].ToPhase = "P99"
	modeMutation := want
	modeMutation.History = append([]transitionHop(nil), want.History...)
	modeMutation.History[0].Before = append([]pathDigest(nil), want.History[0].Before...)
	modeMutation.History[0].Before[0].Mode = "120000"
	mutations = append(mutations, modeMutation)
	for index, mutation := range mutations {
		if compareTransitionRecord(mutation, want) == nil {
			return fmt.Errorf("transition mutation %d was accepted", index)
		}
	}
	if _, err := structuredfile.Parse([]byte(`{"schema_version":1,"schema_version":1}`), ".json"); err == nil {
		return errors.New("duplicate-key transition record was accepted")
	}
	missingAcceptance := want
	missingAcceptance.Acceptance = nil
	if validateTransitionRecordShape(missingAcceptance) == nil {
		return errors.New("transition without acceptance was accepted")
	}
	missingConstraint := want
	missingConstraint.Constraints = nil
	if validateTransitionRecordShape(missingConstraint) == nil {
		return errors.New("transition without constraints was accepted")
	}
	temporary, err := os.MkdirTemp("", "arop-transition-negative-")
	if err != nil {
		return err
	}
	defer removeAllWritable(temporary)
	validJSON, err := json.Marshal(want)
	if err != nil {
		return err
	}
	unknownJSON := append(append([]byte{}, validJSON[:len(validJSON)-1]...), []byte(`,"unknown":true}`)...)
	for name, data := range map[string][]byte{"unknown.json": unknownJSON, "trailing.json": append(append([]byte{}, validJSON...), []byte(` {}`)...)} {
		if err := os.WriteFile(filepath.Join(temporary, name), data, 0o600); err != nil {
			return err
		}
		if _, _, err := loadTransitionRecord(temporary, name); err == nil {
			return fmt.Errorf("%s transition loader negative was accepted", name)
		}
	}
	if validateExactSet([]string{"a.json", "extra.json"}, []string{"a.json"}, "probe record set") == nil {
		return errors.New("extra transition record was accepted")
	}
	if validateExactSet([]string{}, []string{"a.json"}, "probe record set") == nil {
		return errors.New("missing transition record was accepted")
	}
	checkerPath := "internal/tooling/cmd/arop-go-proxy-bootstrap/main.go"
	owned := map[string]bool{checkerPath: true, "go.mod": true, "go.sum": true}
	parentOfCorrection := func(string) (string, error) { return correctionBase, nil }
	if _, err := validateP05TouchLedger(
		[]transitionTouch{{"later", []string{"sdk/go/protocol/core/state.go"}}}, nil,
		owned, checkerPath, correctionBase, true, parentOfCorrection,
	); err != nil {
		return fmt.Errorf("legal non-P05-owned root evolution failed touch-ledger validation: %w", err)
	}
	validTouch := transitionTouch{"one", []string{"go.mod"}}
	touchNegatives := []struct {
		name             string
		discovered, want []transitionTouch
	}{
		{"changed-reverted-missing-record", []transitionTouch{validTouch, {"two", []string{"go.mod"}}}, []transitionTouch{validTouch}},
		{"extra-touch-commit", []transitionTouch{validTouch, {"two", []string{"go.sum"}}}, []transitionTouch{validTouch}},
		{"missing-touch-commit", []transitionTouch{validTouch}, []transitionTouch{validTouch, {"two", []string{"go.sum"}}}},
		{"wrong-touch-path", []transitionTouch{validTouch}, []transitionTouch{{"one", []string{"go.sum"}}}},
	}
	for _, probe := range touchNegatives {
		if _, err := validateP05TouchLedger(probe.discovered, probe.want, owned, checkerPath, correctionBase, true, parentOfCorrection); err == nil {
			return fmt.Errorf("%s touch-ledger negative was accepted", probe.name)
		}
	}
	if _, err := validateP05TouchLedger(
		[]transitionTouch{{"correction-one", []string{checkerPath}}, {"correction-two", []string{checkerPath}}}, nil,
		owned, checkerPath, "correction-two", false, parentOfCorrection,
	); err == nil {
		return errors.New("multiple checker correction commits were accepted")
	}
	if _, err := validateP05TouchLedger(
		[]transitionTouch{{"correction-mixed", []string{checkerPath, "go.mod"}}}, nil,
		owned, checkerPath, "correction-mixed", false, parentOfCorrection,
	); err == nil {
		return errors.New("checker correction mixed with another P05-owned path was accepted")
	}
	badDownload, _ := json.Marshal(downloadResult{Path: rootModulePath, Version: currentVersion, Sum: "h1:forged", GoModSum: currentGoModH1})
	if err := validateDownload(badDownload, rootModulePath, currentVersion, nil, string(filepath.Separator)); err == nil || !strings.Contains(err.Error(), "h1 mismatch") {
		return errors.New("forged module h1 negative was accepted")
	}
	badGoModDownload, _ := json.Marshal(downloadResult{Path: rootModulePath, Version: currentVersion, Sum: currentZipH1, GoModSum: "h1:forged"})
	if err := validateDownload(badGoModDownload, rootModulePath, currentVersion, nil, string(filepath.Separator)); err == nil || !strings.Contains(err.Error(), "h1 mismatch") {
		return errors.New("forged go.mod h1 negative was accepted")
	}
	errorDownload, _ := json.Marshal(downloadResult{Error: "proxy failure"})
	if err := validateDownload(errorDownload, rootModulePath, currentVersion, nil, string(filepath.Separator)); err == nil || !strings.Contains(err.Error(), "proxy failure") {
		return errors.New("proxy failure download negative was accepted")
	}
	if err := validateHealthTestOutput([]byte("PASS\n")); err == nil {
		return errors.New("missing nested health tests were accepted")
	}
	wrongCarrier := artifactManifest{SchemaVersion: 1}
	wrongCarrier.Artifacts = append(wrongCarrier.Artifacts, struct {
		ID         string `yaml:"id"`
		Path       string `yaml:"path"`
		Status     string `yaml:"status"`
		OwnerPhase string `yaml:"owner_phase"`
		PathRole   string `yaml:"path_role"`
	}{"wrong", "carrier", "present", "P99", "concrete"})
	if err := validateTransitionCarrier("carrier/record.json", "P06", "expected", wrongCarrier); err == nil {
		return errors.New("wrong transition carrier owner was accepted")
	}
	wrongAffected := artifactManifest{SchemaVersion: 1}
	wrongAffected.Artifacts = append(wrongAffected.Artifacts, struct {
		ID         string `yaml:"id"`
		Path       string `yaml:"path"`
		Status     string `yaml:"status"`
		OwnerPhase string `yaml:"owner_phase"`
		PathRole   string `yaml:"path_role"`
	}{"root-go-module", "go.mod", "present", "P99", "concrete"})
	if err := validateAffectedArtifactOwners([]string{"root-go-module"}, map[string]string{"root-go-module": "P05"}, wrongAffected); err == nil {
		return errors.New("wrong affected artifact owner phase was accepted")
	}
	bootstrap, err := resolveBootstrapCommit(root, p05Version)
	if err != nil {
		return err
	}
	proxyRoot := filepath.Join(temporary, "corrupt-proxy")
	if err := writeProxy(root, proxyRoot, bootstrap); err != nil {
		return err
	}
	zipPath := filepath.Join(proxyRoot, filepath.FromSlash(rootModulePath), "@v", p05Version+".zip")
	if err := os.WriteFile(zipPath, []byte("corrupt module archive"), 0o600); err != nil {
		return err
	}
	scratch := filepath.Join(temporary, "corrupt-download")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(temporary, "corrupt-tmp"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "go.mod"), []byte("module example.invalid/corrupt-probe\n\ngo 1.24.0\n"), 0o600); err != nil {
		return err
	}
	proxyURL := (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",off"
	corruptOutput, corruptErr := runGoWithProxy(scratch, goEnvironment(map[string]string{
		"GOMODCACHE": filepath.Join(temporary, "corrupt-cache"),
		"GOTMPDIR":   filepath.Join(temporary, "corrupt-tmp"),
		"GOPROXY":    proxyURL,
		"GONOSUMDB":  "*",
		"GOSUMDB":    "off",
	}, false), "mod", "download", "-json", rootModulePath+"@"+p05Version)
	if corruptErr == nil {
		if err := validateDownload(corruptOutput, rootModulePath, p05Version, bootstrap.GoMod, temporary); err == nil {
			return errors.New("corrupt isolated proxy archive was accepted")
		}
	}
	broken := filepath.Join(temporary, "broken-test")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(broken, "go.mod"), []byte("module example.invalid/broken-test\n\ngo 1.24.0\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(broken, "broken_test.go"), []byte("package broken\nfunc TestBroken(\n"), 0o600); err != nil {
		return err
	}
	if _, err := runGoWithProxy(broken, goEnvironment(map[string]string{
		"GOMODCACHE": filepath.Join(temporary, "broken-cache"),
		"GOTMPDIR":   filepath.Join(temporary, "corrupt-tmp"),
		"GOPROXY":    "off",
	}, false), "test", "./..."); err == nil {
		return errors.New("failing isolated go test was accepted")
	}
	return nil
}

func negativeNULPathProbe() error {
	if _, err := parseNULPaths([]byte("go.mod")); err == nil {
		return errors.New("unterminated Git path output was accepted")
	}
	if _, err := parseNULPaths([]byte("../escape\x00")); err == nil {
		return errors.New("unsafe Git path output was accepted")
	}
	paths, err := parseNULPaths([]byte("b\x00a\x00"))
	if err != nil || !equalStrings(paths, []string{"a", "b"}) {
		return errors.Join(err, errors.New("valid NUL Git path output was not deterministically sorted"))
	}
	return nil
}

func runGo(directory string, additions map[string]string, args ...string) ([]byte, error) {
	return runGoWithProxy(directory, goEnvironment(additions, true), args...)
}

func runGoOnline(directory string, additions map[string]string, args ...string) ([]byte, error) {
	return runGoWithProxy(directory, goEnvironment(additions, false), args...)
}

func rootPrefetchProxy() (string, error) {
	value := os.Getenv("AROP_GO_PROXY")
	if value == "" {
		value = "https://proxy.golang.org"
	}
	if strings.Contains(value, ",") || value == "direct" || value == "off" {
		return "", errors.New("AROP_GO_PROXY must name one explicit proxy without direct/off fallback")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "file") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" && parsed.Scheme != "file" {
		return "", fmt.Errorf("AROP_GO_PROXY is not an allowed credential-free https/file proxy: %q", value)
	}
	return value, nil
}

func goEnvironment(additions map[string]string, offline bool) map[string]string {
	base := map[string]string{
		"CGO_ENABLED":             "0",
		"GODEBUG":                 "",
		"GOENV":                   "off",
		"GOEXPERIMENT":            "",
		"GOFLAGS":                 "-mod=readonly",
		"GOTOOLCHAIN":             "local",
		"GOWORK":                  "off",
		"NODE_OPTIONS":            "",
		"NODE_PATH":               "",
		"NPM_CONFIG_NODE_OPTIONS": "",
	}
	if offline {
		base["GOPROXY"] = "off"
	}
	for key, value := range additions {
		base[key] = value
	}
	return base
}

func runGoWithProxy(directory string, overrides map[string]string, args ...string) ([]byte, error) {
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	command := exec.Command(goBinary, args...)
	command.Dir = directory
	command.Env = cleanEnvironment(os.Environ(), overrides)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runGoJSON(directory string, overrides map[string]string, args ...string) ([]byte, error) {
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	command := exec.Command(goBinary, args...)
	command.Dir = directory
	command.Env = cleanEnvironment(os.Environ(), goEnvironment(overrides, false))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return output, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func cleanEnvironment(inherited []string, overrides map[string]string) []string {
	blocked := map[string]bool{}
	for key := range overrides {
		blocked[strings.ToUpper(key)] = true
	}
	for _, key := range []string{"GOENV", "GOFLAGS", "GOWORK", "GOCACHEPROG", "GOTMPDIR", "GOROOT"} {
		blocked[key] = true
	}
	result := make([]string, 0, len(inherited)+len(overrides))
	for _, item := range inherited {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		upperKey := strings.ToUpper(key)
		if !blocked[upperKey] && !strings.HasPrefix(upperKey, "GO") && !strings.HasPrefix(upperKey, "GIT_") {
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

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func writeReport(root string, checks []report.Check, rootLog, downloadLog, nestedLog []byte, rootProxy string, trackedInputs []string, replay replayEvidence) {
	temporaryProxyRemoved := false
	for _, check := range checks {
		if check.Name == "temporary-proxy-cleanup" {
			temporaryProxyRemoved = check.Passed
		}
	}
	evidence := []report.RuntimeEvidence{
		{Kind: "nested-go-test-log", SHA256: report.Hash(nestedLog), Bytes: int64(len(nestedLog))},
		{Kind: "root-go-test-log", SHA256: report.Hash(rootLog), Bytes: int64(len(rootLog))},
		{Kind: "root-module-download-log", SHA256: report.Hash(downloadLog), Bytes: int64(len(downloadLog))},
	}
	evidence = append(evidence, replay.Items...)
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	if parsed, err := url.Parse(rootProxy); err == nil && parsed.Scheme != "" {
		evidence = append(evidence, report.RuntimeEvidence{
			Kind:   "root-prefetch-proxy-config-" + parsed.Scheme,
			SHA256: report.Hash([]byte(rootProxy)),
			Bytes:  int64(len(rootProxy)),
		})
	}
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-go-proxy-bootstrap"
	}
	inputs := trackedInputs
	if len(inputs) == 0 {
		inputs = []string{
			".github/workflows/go-workspace.yml",
			".gitignore",
			"Makefile",
			"go.mod",
			"go.sum",
			"go.work.example",
			"internal/tooling/cmd/arop-go-proxy-bootstrap/main.go",
			"internal/tooling/controlledinput/manifest.go",
			"internal/tooling/report/verifier.go",
			"internal/tooling/report/writer.go",
			"internal/tooling/schema/validator.go",
			"internal/tooling/structuredfile/files.go",
			"reference/control-plane/README.md",
			"reference/control-plane/cmd/aropd/main.go",
			"reference/control-plane/go.mod",
			"reference/control-plane/internal/server/server.go",
			"reference/control-plane/internal/server/server_test.go",
			"reference/control-plane-lite/README.md",
			"docs/DEVELOPMENT_PLAN.md",
			"docs/IMPLEMENTATION_BLUEPRINT.md",
			"spec/artifact-manifest.yaml",
			"spec/requirements.yaml",
			"spec/schemas/check-report.schema.json",
		}
	}
	if len(trackedInputs) == 0 {
		if _, err := os.Stat(filepath.Join(root, nestedDirectory, "go.sum")); err == nil {
			inputs = append(inputs, nestedDirectory+"/go.sum")
		}
	}
	result, err := report.Write(report.WriteOptions{
		Root:            root,
		Directory:       "build/reports/P05",
		Suite:           "arop-go-workspace",
		Class:           "arop.go-workspace",
		Command:         command,
		CheckerPath:     "internal/tooling/cmd/arop-go-proxy-bootstrap/main.go",
		InputPaths:      inputs,
		RuntimeEvidence: evidence,
		Checks:          checks,
		Summary: map[string]any{
			"checker_correction_commit": replay.CorrectionCommit,
			"recorded_transition_paths": replay.RecordedTransitionPaths,
			"consumed_root_artifacts":   replay.AffectedArtifacts,
			"consumed_root_closure":     replay.ConsumedPaths,
			"expected_go_modules":       2,
			"nested_module":             nestedModulePath,
			"root_module":               rootModulePath,
			"temporary_proxy_removed":   temporaryProxyRemoved,
		},
		AuditNote: "P05 reconstructs the root module from the commit named by the nested pseudo-version; it never labels current worktree bytes as an older module version.",
	})
	fatal(err)
	verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P05/report.json"})
	fatal(verifyErr)
	if mode != "current-worktree" || verified.Success != result.Success {
		fatal(fmt.Errorf("P05 report self-verification returned mode=%s success=%t", mode, verified.Success))
	}
	if result.Success {
		fmt.Printf("AROP Go workspace check passed: %d checks; temporary proxy removed.\n", len(result.Checks))
		return
	}
	fmt.Fprintln(os.Stderr, "AROP Go workspace check failed; see build/reports/P05/report.json")
}

func gitBytes(root string, args ...string) ([]byte, error) {
	command := controlledinput.GitCommand(root, args...)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func gitText(root string, args ...string) (string, error) {
	output, err := gitBytes(root, args...)
	return strings.TrimSpace(string(output)), err
}

func detail(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
