// Command arop-build-container creates a deterministic scratch-based OCI image archive.
package main

import (
	"archive/tar"
	"bytes"
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
	"sort"
	"strings"
	"time"
)

const epoch = int64(315532800)

type descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}

type fileEntry struct {
	name string
	data []byte
	mode int64
}

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	outputFlag := flag.String("output", "", "output OCI tar path")
	archFlag := flag.String("goarch", "amd64", "linux architecture")
	flag.Parse()
	if flag.NArg() != 0 || *outputFlag == "" || (*archFlag != "amd64" && *archFlag != "arm64") {
		fatal(errors.New("usage: arop-build-container --root <repo> --output <image.tar> --goarch amd64|arm64"))
	}
	root, err := canonicalDirectory(*rootFlag)
	fatal(err)
	output, err := safeOutput(*outputFlag)
	fatal(err)
	scratch, err := os.MkdirTemp(filepath.Dir(output), ".arop-container-")
	fatal(err)
	defer os.RemoveAll(scratch)
	binary := filepath.Join(scratch, "aropd")
	workspace, err := writeWorkspace(scratch, root)
	fatal(err)
	build := exec.Command("go", "-C", "reference/control-plane", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", binary, "./cmd/aropd")
	build.Dir = root
	build.Env = []string{"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=" + workspace, "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + *archFlag, "GOPROXY=off", "GOSUMDB=off", "HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "TZ=UTC", "LANG=C", "LC_ALL=C"}
	if data, err := build.CombinedOutput(); err != nil {
		fatal(fmt.Errorf("build aropd: %w: %s", err, strings.TrimSpace(string(data))))
	}
	binaryData, err := os.ReadFile(binary)
	fatal(err)
	mtime := time.Unix(epoch, 0).UTC()
	layer := bytes.Buffer{}
	layerTar := tar.NewWriter(&layer)
	layerEntries := []fileEntry{{"usr/local/bin/aropd", binaryData, 0o755}}
	migrations, err := migrationEntries(root)
	fatal(err)
	layerEntries = append(layerEntries, migrations...)
	sort.Slice(layerEntries, func(i, j int) bool { return layerEntries[i].name < layerEntries[j].name })
	for _, entry := range layerEntries {
		fatal(layerTar.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), ModTime: mtime, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}))
		_, err = layerTar.Write(entry.data)
		fatal(err)
	}
	fatal(layerTar.Close())
	layerDigest := digest(layer.Bytes())
	created := mtime.Format(time.RFC3339)
	configData := mustJSON(map[string]any{"architecture": *archFlag, "os": "linux", "created": created, "config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/usr/local/bin/aropd"}, "WorkingDir": "/", "Env": []string{"PATH=/usr/local/bin"}}, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{layerDigest}}, "history": []map[string]any{{"created": created, "created_by": "arop-build-container/v1"}}})
	configDigest := digest(configData)
	manifestData := mustJSON(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: int64(len(configData))}, "layers": []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(layer.Len())}}})
	manifestDigest := digest(manifestData)
	indexData := mustJSON(map[string]any{"schemaVersion": 2, "manifests": []descriptor{{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest, Size: int64(len(manifestData)), Platform: map[string]string{"os": "linux", "architecture": *archFlag}}}})
	entries := []fileEntry{
		{"blobs/sha256/" + strings.TrimPrefix(configDigest, "sha256:"), configData, 0o644},
		{"blobs/sha256/" + strings.TrimPrefix(layerDigest, "sha256:"), layer.Bytes(), 0o644},
		{"blobs/sha256/" + strings.TrimPrefix(manifestDigest, "sha256:"), manifestData, 0o644},
		{"index.json", indexData, 0o644}, {"oci-layout", []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"), 0o644},
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	fatal(err)
	archive := tar.NewWriter(file)
	for _, entry := range entries {
		fatal(archive.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), ModTime: mtime, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}))
		_, err = archive.Write(entry.data)
		fatal(err)
	}
	fatal(archive.Close())
	fatal(file.Close())
	fmt.Println(manifestDigest)
}

func writeWorkspace(scratch, root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/go.mod"))
	if err != nil {
		return "", err
	}
	fields, version := strings.Fields(string(data)), ""
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "github.com/gmslll/agent-runtime-operations-protocol" && strings.HasPrefix(fields[index+1], "v0.0.0-") {
			version = fields[index+1]
			break
		}
	}
	if version == "" {
		return "", errors.New("nested root pin is missing")
	}
	path := filepath.Join(scratch, "go.work")
	content := fmt.Sprintf("go 1.24.0\n\nuse (\n\t%s\n\t%s\n)\n\nreplace github.com/gmslll/agent-runtime-operations-protocol %s => %s\n", root, filepath.Join(root, "reference/control-plane"), version, root)
	return path, os.WriteFile(path, []byte(content), 0o600)
}

func migrationEntries(root string) ([]fileEntry, error) {
	base := filepath.Join(root, "reference/control-plane/migrations")
	entries := []fileEntry{}
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("migration source contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("migration source is not regular")
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{name: "opt/arop/migrations/" + filepath.ToSlash(relative), data: data, mode: 0o644})
		return nil
	})
	if err != nil || len(entries) == 0 {
		return nil, errors.New("migration closure is empty or invalid")
	}
	return entries, nil
}

func mustJSON(value any) []byte { data, err := json.Marshal(value); fatal(err); return data }
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func safeOutput(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", errors.New("output already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := canonicalDirectory(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
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
		return "", errors.New("not a directory")
	}
	return value, nil
}
func fatal(err error) {
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
