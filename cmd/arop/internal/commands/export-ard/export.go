package exportard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/ard"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
	"go.yaml.in/yaml/v3"
)

const maxManifestBytes = 4 << 20

type Options struct{ PackageRoot, ManifestPath, Publisher, AgentCardURL string }
type Command struct{ Stdout, Report io.Writer }

func (command Command) Execute(ctx context.Context, options Options) error {
	if command.Stdout == nil {
		return errors.New("ARD export stdout is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root := options.PackageRoot
	if root == "" {
		root = "."
	}
	if options.ManifestPath == "" || filepath.IsAbs(options.ManifestPath) || strings.Contains(filepath.ToSlash(options.ManifestPath), "../") {
		return errors.New("ARD export manifest path must be package-relative")
	}
	if err := manifest.ValidatePackageFile(root, options.ManifestPath); err != nil {
		return errors.New("ARD export manifest is invalid")
	}
	digest, err := manifest.DigestPackageFile(root, options.ManifestPath)
	if err != nil {
		return errors.New("ARD export manifest is invalid")
	}
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(options.ManifestPath)))
	if err != nil {
		return errors.New("ARD export manifest is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return errors.New("ARD export manifest is unavailable")
	}
	var document any
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&document); err != nil {
		return errors.New("ARD export manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("ARD export manifest is invalid")
	}
	wire, err := json.Marshal(document)
	if err != nil {
		return errors.New("ARD export manifest is invalid")
	}
	source, err := controlplane.DecodeAgentManifest(wire)
	if err != nil {
		return errors.New("ARD export manifest is invalid")
	}
	entry, report, err := ard.Export(source, digest, options.Publisher, options.AgentCardURL)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := ard.EncodeEntry(entry)
	if err != nil {
		return errors.New("encode ARD entry: unavailable")
	}
	if _, err := command.Stdout.Write(append(encoded, '\n')); err != nil {
		return errors.New("write ARD entry: unavailable")
	}
	if command.Report != nil {
		encoder := json.NewEncoder(command.Report)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			return errors.New("write ARD loss report: unavailable")
		}
	}
	return nil
}
