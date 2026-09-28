package exporta2a

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/adapters/a2a"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
	"go.yaml.in/yaml/v3"
)

const maxManifestBytes = 4 << 20

type Options struct {
	PackageRoot  string
	ManifestPath string
	InterfaceURL string
}

type Command struct {
	Stdout io.Writer
	Report io.Writer
}

func (command Command) Execute(ctx context.Context, options Options) error {
	if command.Stdout == nil {
		return errors.New("A2A export stdout is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root := options.PackageRoot
	if root == "" {
		root = "."
	}
	if options.ManifestPath == "" || filepath.IsAbs(options.ManifestPath) || strings.Contains(filepath.ToSlash(options.ManifestPath), "../") {
		return errors.New("A2A export manifest path must be package-relative")
	}
	if err := manifest.ValidatePackageFile(root, options.ManifestPath); err != nil {
		return errors.New("A2A export manifest is invalid")
	}
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(options.ManifestPath)))
	if err != nil {
		return errors.New("A2A export manifest is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return errors.New("A2A export manifest is unavailable")
	}
	var document any
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&document); err != nil {
		return errors.New("A2A export manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("A2A export manifest is invalid")
	}
	wire, err := json.Marshal(document)
	if err != nil {
		return errors.New("A2A export manifest is invalid")
	}
	source, err := controlplane.DecodeAgentManifest(wire)
	if err != nil {
		return errors.New("A2A export manifest is invalid")
	}
	card, report, err := a2a.ExportAgentCard(source, options.InterfaceURL)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoder := json.NewEncoder(command.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(card); err != nil {
		return errors.New("write A2A Agent Card: unavailable")
	}
	if command.Report != nil {
		reportEncoder := json.NewEncoder(command.Report)
		reportEncoder.SetEscapeHTML(false)
		if err := reportEncoder.Encode(report); err != nil {
			return errors.New("write A2A mapping report: unavailable")
		}
	}
	return nil
}
