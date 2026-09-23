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
)

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
	Path    string `json:"Path"`
	Version string `json:"Version"`
	Info    string `json:"Info"`
	GoMod   string `json:"GoMod"`
	Zip     string `json:"Zip"`
	Dir     string `json:"Dir"`
	Error   string `json:"Error"`
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
	record("root-nested-import-boundary", validateRootImportBoundary(root), "root module has no imports of the nested Reference Control Plane")
	record("ci-workspace-policy", validateWorkflow(root), "CI preserves full history and runs only the exact read-only P05 Make target with minimal permissions")

	record("negative-third-module", negativeThirdModuleProbe(), "a third go.mod is rejected")
	record("negative-permanent-replace", negativeReplaceProbe(), "a permanent replace directive is rejected")
	record("negative-version-mismatch", negativeVersionProbe(), "a non-pseudo or mismatched root version is rejected")

	temporaryRoot, temporaryErr := os.MkdirTemp("", "arop-go-proxy-")
	if temporaryErr != nil {
		record("temporary-proxy-bootstrap", temporaryErr, "")
		record("root-gowork-off-tests", errors.New("isolated caches were not created"), "")
		record("nested-local-proxy-tests", errors.New("temporary proxy was not created"), "")
		record("temporary-proxy-cleanup", errors.New("temporary proxy was not created"), "")
		writeReport(root, checks, nil, nil, nil)
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
	rootProxy, rootProxyErr := rootPrefetchProxy()
	rootEnvironment := map[string]string{"GOCACHE": rootBuildCache, "GOMODCACHE": rootModuleCache, "GOTMPDIR": rootTemp}
	rootPrefetchEnvironment := cloneMap(rootEnvironment)
	rootPrefetchEnvironment["GOPROXY"] = rootProxy
	rootPrefetch, rootPrefetchErr := runGoOnline(root, rootPrefetchEnvironment, "mod", "download")
	rootVerify, rootVerifyErr := runGo(root, rootEnvironment, "mod", "verify")
	rootTest, rootTestErr := runGo(root, rootEnvironment, "test", "-count=1", "./...")
	rootCombinedOutput := bytes.Join([][]byte{rootPrefetch, rootVerify, rootTest}, []byte("\n"))
	record("root-gowork-off-tests", errors.Join(temporaryErr, rootProxyErr, rootPrefetchErr, rootVerifyErr, rootTestErr), "root dependencies were prefetched through one explicit proxy into an isolated cache, verified, then root tests passed with GOWORK=off and GOPROXY=off")

	bootstrap, bootstrapErr := resolveBootstrapCommit(root, rootVersion)
	if bootstrapErr == nil {
		bootstrapErr = writeProxy(root, proxyDirectory, bootstrap)
	}
	if bootstrapErr == nil {
		bootstrapErr = verifyRootHasNotDrifted(root, bootstrap.Commit)
	}
	record("temporary-proxy-bootstrap", bootstrapErr, fmt.Sprintf("%s reconstructed from commit %s", rootVersion, bootstrap.Commit))

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
	var downloadOutput, nestedVerifyOutput, nestedListOutput, nestedTestOutput []byte
	var nestedErr error
	nestedSumPath := filepath.Join(nestedRoot, "go.sum")
	nestedSumBefore, nestedSumReadErr := os.ReadFile(nestedSumPath)
	if nestedSumReadErr != nil {
		nestedErr = fmt.Errorf("nested go.sum must be generated with --bootstrap-go-sum before acceptance: %w", nestedSumReadErr)
	}
	if bootstrapErr == nil && nestedContractErr == nil && nestedErr == nil {
		downloadOutput, nestedErr = runGo(nestedRoot, proxyEnvironment, "mod", "download", "-json", rootModulePath+"@"+rootVersion)
		if nestedErr == nil {
			nestedErr = validateDownload(downloadOutput, rootModulePath, rootVersion, bootstrap.GoMod, temporaryRoot)
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
			nestedTestOutput, nestedErr = runGo(nestedRoot, nestedEnvironment, "test", "-count=1", "-v", "./...")
		}
		if nestedErr == nil {
			nestedErr = validateHealthTestOutput(nestedTestOutput)
		}
	} else {
		nestedErr = errors.New("nested tests require a valid non-placeholder pseudo-version bootstrap")
	}
	nestedSumAfter, nestedSumAfterErr := os.ReadFile(nestedSumPath)
	if nestedSumReadErr == nil && (nestedSumAfterErr != nil || !bytes.Equal(nestedSumBefore, nestedSumAfter)) {
		nestedErr = errors.Join(nestedErr, errors.New("default acceptance changed reference/control-plane/go.sum; regenerate it with --bootstrap-go-sum and commit it first"))
	}
	nestedCombinedOutput := bytes.Join([][]byte{nestedVerifyOutput, nestedListOutput, nestedTestOutput}, []byte("\n"))
	record("nested-local-proxy-tests", nestedErr, "nested module downloaded exact root bytes from the temporary proxy, then go mod verify, go list, and tests passed readonly with GOWORK=off/GOPROXY=off in isolated caches")

	cleanupErr := removeAllWritable(temporaryRoot)
	if cleanupErr == nil {
		if _, statErr := os.Stat(temporaryRoot); !os.IsNotExist(statErr) {
			cleanupErr = fmt.Errorf("temporary proxy still exists after cleanup: %s", temporaryRoot)
		}
	}
	record("temporary-proxy-cleanup", cleanupErr, "temporary proxy and isolated caches were removed")

	writeReport(root, checks, rootCombinedOutput, downloadOutput, nestedCombinedOutput)
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
	command := controlledinput.GitCommand(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}
	modules := []string{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path != "" && filepath.Base(path) == "go.mod" {
			modules = append(modules, path)
		}
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
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "go-workspace.yml"))
	if err != nil {
		return err
	}
	text := string(data)
	for _, required := range []string{"permissions:\n  contents: read", "fetch-depth: 0", "run: make test-go-workspace", "node-version: 22"} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("go-workspace workflow is missing %q", required)
		}
	}
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"secrets.", "id-token:", "packages:", "publish", "release", "go.work\n", "go work "} {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("go-workspace workflow contains forbidden capability marker %q", forbidden)
		}
	}
	if strings.Count(text, "run: make test-go-workspace") != 1 {
		return errors.New("go-workspace workflow must invoke the exact Make target once")
	}
	return nil
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
		if entry.Path == nestedDirectory || strings.HasPrefix(entry.Path, nestedDirectory+"/") {
			continue
		}
		if entry.Path == "vendor" || strings.HasPrefix(entry.Path, "vendor/") {
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
		for _, character := range element {
			if character < 0x20 || strings.ContainsRune(`:*?"<>|`, character) {
				return fmt.Errorf("invalid character in module archive path %q", path)
			}
		}
	}
	return nil
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
	if err := verifyRootHasNotDrifted(root, bootstrap.Commit); err != nil {
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
	if !bytes.Contains(newSum, []byte(rootModulePath+" "+version+" h1:")) || !bytes.Contains(newSum, []byte(rootModulePath+" "+version+"/go.mod h1:")) {
		return errors.New("generated nested go.sum does not bind both the root module zip and go.mod")
	}
	prepared = true
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
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "file") || parsed.User != nil || parsed.Host == "" && parsed.Scheme != "file" {
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

func writeReport(root string, checks []report.Check, rootLog, downloadLog, nestedLog []byte) {
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
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-go-proxy-bootstrap"
	}
	inputs := []string{
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
	if _, err := os.Stat(filepath.Join(root, nestedDirectory, "go.sum")); err == nil {
		inputs = append(inputs, nestedDirectory+"/go.sum")
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
			"expected_go_modules":     2,
			"nested_module":           nestedModulePath,
			"root_module":             rootModulePath,
			"temporary_proxy_removed": temporaryProxyRemoved,
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
