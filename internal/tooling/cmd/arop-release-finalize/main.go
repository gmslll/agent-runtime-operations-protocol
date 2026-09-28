// Command arop-release-finalize exposes read-only freeze and final delivery checks.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/finalize"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	modeFlag := flag.String("mode", "", "freeze or verify-final")
	sourceFlag := flag.String("source", "", "approved RC source commit")
	finalFlag := flag.String("final", "", "candidate final commit")
	treeFlag := flag.String("expected-tree", "", "approved final tree")
	orderFlag := flag.String("publication-order", "", "comma-separated immutable publication order")
	outputFlag := flag.String("output", "", "new JSON result path")
	flag.Parse()
	if flag.NArg() != 0 || *outputFlag == "" {
		fatal(errors.New("--mode and --output are required"))
	}
	root, err := structuredfile.FindRoot(*rootFlag)
	fatal(err)
	result := map[string]any{"schema_version": 1, "mode": *modeFlag}
	switch *modeFlag {
	case "freeze":
		freeze, err := finalize.CaptureFreeze(root, nil)
		fatal(err)
		result["source"] = freeze
	case "verify-final":
		if *sourceFlag == "" || *finalFlag == "" || *treeFlag == "" {
			fatal(errors.New("verify-final requires --source, --final and --expected-tree"))
		}
		fatal(finalize.VerifyFinalCommit(root, *sourceFlag, *finalFlag, *treeFlag))
		order := strings.Split(*orderFlag, ",")
		want := []string{"go-root", "go-nested", "python", "npm", "oci", "cli", "schema-bundle"}
		if len(order) != len(want) {
			fatal(errors.New("publication order is incomplete"))
		}
		for index := range want {
			if order[index] != want[index] {
				fatal(errors.New("root-before-nested immutable publication order is invalid"))
			}
		}
		result["source_commit"], result["final_commit"], result["expected_tree"], result["publication_order"] = *sourceFlag, *finalFlag, *treeFlag, order
	default:
		fatal(errors.New("unsupported mode"))
	}
	fatal(writeNew(*outputFlag, result))
}

func writeNew(path string, value any) error {
	abs, err := filepath.Abs(path)
	if err != nil || abs == string(filepath.Separator) {
		return errors.New("output path is invalid")
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(abs, append(data, '\n'), 0o600)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "arop-release-finalize:", err)
		os.Exit(1)
	}
}
