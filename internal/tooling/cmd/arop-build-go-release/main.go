// Command arop-build-go-release creates deterministic Go module source and CLI archives.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

type layout struct {
	SchemaVersion   int   `json:"schema_version" yaml:"schema_version"`
	SourceDateEpoch int64 `json:"source_date_epoch" yaml:"source_date_epoch"`
	Modules         []struct {
		ID     string `json:"id" yaml:"id"`
		Module string `json:"module" yaml:"module"`
		Path   string `json:"path" yaml:"path"`
	} `json:"modules" yaml:"modules"`
	Commands []struct {
		Name    string `json:"name" yaml:"name"`
		Package string `json:"package" yaml:"package"`
	} `json:"commands" yaml:"commands"`
	Targets []struct {
		GOOS   string `json:"goos" yaml:"goos"`
		GOARCH string `json:"goarch" yaml:"goarch"`
		Format string `json:"format" yaml:"format"`
	} `json:"targets" yaml:"targets"`
	Rules struct {
		GOWORK      string `json:"gowork" yaml:"gowork"`
		Trimpath    bool   `json:"trimpath" yaml:"trimpath"`
		CGO         bool   `json:"cgo" yaml:"cgo"`
		StableOrder bool   `json:"stable_order" yaml:"stable_order"`
		FixedMtime  bool   `json:"fixed_mtime" yaml:"fixed_mtime"`
		FixedModes  bool   `json:"fixed_modes" yaml:"fixed_modes"`
	} `json:"rules" yaml:"rules"`
}

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Version       string     `json:"version"`
	GOOS          string     `json:"goos"`
	GOARCH        string     `json:"goarch"`
	Artifacts     []artifact `json:"artifacts"`
}

type fileEntry struct {
	Name string
	Data []byte
	Mode os.FileMode
}

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	outputFlag := flag.String("output", "", "empty output directory")
	versionFlag := flag.String("version", "", "logical release version without v")
	goosFlag := flag.String("goos", "", "target GOOS")
	goarchFlag := flag.String("goarch", "", "target GOARCH")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("unexpected positional arguments"))
	}
	root, err := canonicalDirectory(*rootFlag)
	fatal(err)
	if *outputFlag == "" || !versionPattern.MatchString(*versionFlag) || *goosFlag == "" || *goarchFlag == "" {
		fatal(errors.New("--output, --version, --goos, and --goarch are required"))
	}
	output, err := prepareOutput(*outputFlag)
	fatal(err)
	var policy layout
	fatal(structuredfile.Load(filepath.Join(root, "spec/release/go-artifacts.yaml"), &policy))
	fatal(validateLayout(policy, *goosFlag, *goarchFlag))
	mtime := time.Unix(policy.SourceDateEpoch, 0).UTC()
	artifacts := []artifact{}
	for _, module := range policy.Modules {
		entries, err := moduleEntries(root, module.Path, module.Module, "v"+*versionFlag)
		fatal(err)
		name := fmt.Sprintf("%s-v%s.module.zip", module.ID, *versionFlag)
		path := filepath.Join(output, name)
		fatal(writeZip(path, entries, mtime))
		artifacts = append(artifacts, digestArtifact(path, name))
	}
	binaries := []fileEntry{}
	for _, command := range policy.Commands {
		name := command.Name
		if *goosFlag == "windows" {
			name += ".exe"
		}
		path := filepath.Join(output, ".build-"+name)
		fatal(build(root, command.Package, path, *goosFlag, *goarchFlag))
		data, err := os.ReadFile(path)
		fatal(err)
		_ = os.Remove(path)
		binaries = append(binaries, fileEntry{Name: name, Data: data, Mode: 0o755})
	}
	sort.Slice(binaries, func(i, j int) bool { return binaries[i].Name < binaries[j].Name })
	archiveName := fmt.Sprintf("arop-v%s-%s-%s", *versionFlag, *goosFlag, *goarchFlag)
	if targetFormat(policy, *goosFlag, *goarchFlag) == "zip" {
		archiveName += ".zip"
		fatal(writeZip(filepath.Join(output, archiveName), binaries, mtime))
	} else {
		archiveName += ".tar.gz"
		fatal(writeTarGzip(filepath.Join(output, archiveName), binaries, mtime))
	}
	artifacts = append(artifacts, digestArtifact(filepath.Join(output, archiveName), archiveName))
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	document := manifest{SchemaVersion: 1, Version: *versionFlag, GOOS: *goosFlag, GOARCH: *goarchFlag, Artifacts: artifacts}
	data, err := json.MarshalIndent(document, "", "  ")
	fatal(err)
	data = append(data, '\n')
	fatal(os.WriteFile(filepath.Join(output, "checksums.json"), data, 0o644))
}

func validateLayout(value layout, goos, goarch string) error {
	if value.SchemaVersion != 1 || value.SourceDateEpoch != 315532800 || len(value.Modules) != 2 || len(value.Commands) != 2 || value.Rules.GOWORK != "off" || !value.Rules.Trimpath || value.Rules.CGO || !value.Rules.StableOrder || !value.Rules.FixedMtime || !value.Rules.FixedModes {
		return errors.New("release layout invariants are incomplete")
	}
	if targetFormat(value, goos, goarch) == "" {
		return errors.New("target is not declared by release layout")
	}
	return nil
}

func targetFormat(value layout, goos, goarch string) string {
	for _, target := range value.Targets {
		if target.GOOS == goos && target.GOARCH == goarch {
			return target.Format
		}
	}
	return ""
}

func moduleEntries(root, modulePath, moduleName, version string) ([]fileEntry, error) {
	result := exec.Command("git", "ls-files", "-z")
	result.Dir = root
	output, err := result.Output()
	if err != nil {
		return nil, errors.New("enumerate tracked source")
	}
	prefix := ""
	if modulePath != "." {
		prefix = filepath.ToSlash(filepath.Clean(modulePath)) + "/"
	}
	entries := []fileEntry{}
	archivePrefix := moduleName + "@" + version + "/"
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "" || strings.HasPrefix(path, "build/") || strings.Contains(path, "/node_modules/") {
			continue
		}
		relative := path
		if prefix == "" {
			if strings.HasPrefix(path, "reference/control-plane/") {
				continue
			}
		} else {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			relative = strings.TrimPrefix(path, prefix)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("tracked source is not a regular file: %s", path)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		entries = append(entries, fileEntry{Name: archivePrefix + relative, Data: data, Mode: mode})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) == 0 {
		return nil, errors.New("module source closure is empty")
	}
	return entries, nil
}

func build(root, pkg, output, goos, goarch string) error {
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", output, pkg)
	command.Dir = root
	command.Env = []string{"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch, "TZ=UTC", "LANG=C", "LC_ALL=C", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w: %s", pkg, err, strings.TrimSpace(string(outputBytes)))
	}
	return nil
}

func writeZip(path string, entries []fileEntry, mtime time.Time) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: filepath.ToSlash(entry.Name), Method: zip.Deflate}
		header.SetModTime(mtime)
		header.SetMode(entry.Mode)
		part, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := part.Write(entry.Data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

func writeTarGzip(path string, entries []fileEntry, mtime time.Time) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = mtime
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: filepath.ToSlash(entry.Name), Mode: int64(entry.Mode.Perm()), Size: int64(len(entry.Data)), ModTime: mtime, Typeflag: tar.TypeReg, Uid: 0, Gid: 0, Uname: "", Gname: "", Format: tar.FormatUSTAR}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tarWriter.Write(entry.Data); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	return file.Close()
}

func digestArtifact(path, name string) artifact {
	file, err := os.Open(path)
	fatal(err)
	defer file.Close()
	hash := sha256.New()
	bytesWritten, err := io.Copy(hash, file)
	fatal(err)
	return artifact{Path: name, SHA256: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Bytes: bytesWritten}
}

func prepareOutput(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", errors.New("output already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Mkdir(absolute, 0o700); err != nil {
		return "", err
	}
	return absolute, nil
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	value, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return "", errors.New("root is not a directory")
	}
	return value, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
