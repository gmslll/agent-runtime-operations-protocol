// Command arop-release-lineage verifies report provenance and writes a digest-bound aggregate.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/lineage"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	requestFlag := flag.String("request", "", "tracked lineage request JSON")
	registryFlag := flag.String("registry", "", "TUF registry bundle JSON required by trusted_ci candidates")
	outputFlag := flag.String("output", "", "new aggregate output path")
	flag.Parse()
	if flag.NArg() != 0 || *requestFlag == "" || *outputFlag == "" {
		fatal(errors.New("--request and --output are required"))
	}
	root, err := structuredfile.FindRoot(*rootFlag)
	fatal(err)
	request, err := lineage.LoadRequest(root, *requestFlag)
	fatal(err)
	needsTrust := false
	for _, item := range request.Candidates {
		needsTrust = needsTrust || lineage.Strategy(item.Strategy) == lineage.StrategyTrustedCI
	}
	var trust *releaseevidence.VerifiedTrust
	if needsTrust {
		if *registryFlag == "" {
			fatal(errors.New("--registry is required for trusted_ci candidates"))
		}
		protectedPath := os.Getenv("TRUST_ROOT")
		if protectedPath == "" {
			fatal(errors.New("TRUST_ROOT protected configuration is required for trusted_ci; no fallback is allowed"))
		}
		protectedPath, err = releaseevidence.ProtectedPathOutsideRepository(root, protectedPath)
		fatal(err)
		protected, _, err := releaseevidence.LoadProtectedState(protectedPath)
		fatal(err)
		bundle, _, err := releaseevidence.LoadRegistryBundle(root, *registryFlag)
		fatal(err)
		verified, err := releaseevidence.VerifyRegistry(bundle, protected, time.Now().UTC())
		fatal(err)
		trust = &verified
	}
	verified := make([]lineage.VerifiedCandidate, 0, len(request.Candidates))
	for _, item := range request.Candidates {
		candidate := lineage.Candidate{Phase: item.Phase, ReportPath: item.ReportPath, Strategy: lineage.Strategy(item.Strategy)}
		if candidate.Strategy == lineage.StrategyTrustedCI {
			candidate.Trust = trust
			candidate.ProvenancePayload, err = readInside(root, item.ProvenancePath)
			fatal(err)
			envelope, _, err := releaseevidence.LoadEnvelope(root, item.EnvelopePath)
			fatal(err)
			candidate.Envelope = &envelope
			candidate.Expected, err = ciBindings(root)
			fatal(err)
		}
		result, err := lineage.VerifyCandidate(root, candidate, time.Now().UTC())
		fatal(err)
		verified = append(verified, result)
	}
	aggregate, err := lineage.BuildAggregate(lineage.AggregateOptions{Root: root, ReleaseCommit: request.ReleaseCommit, Candidates: verified})
	fatal(err)
	fatal(lineage.WriteAggregate(*outputFlag, aggregate))
	fmt.Printf("verified %d report lineages for %s; aggregate %s\n", len(aggregate.Reports), aggregate.ReleaseCommit, aggregate.SHA256)
}

func ciBindings(root string) (releaseevidence.ExpectedBindings, error) {
	schema, err := digestFile(filepath.Join(root, "spec/schemas/check-report.schema.json"))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	policy, err := digestFile(filepath.Join(root, ".github/workflows/release.lock.json"))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	validator, err := digestFile(filepath.Join(root, "internal/tooling/cmd/arop-release-lineage/main.go"))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	return releaseevidence.ExpectedBindings{Kind: "ci_report_provenance", Role: "independent_reviewer", SchemaSHA256: schema, PolicySHA256: policy, ValidatorSHA256: validator}, nil
}

func readInside(root, path string) ([]byte, error) {
	if path == "" || filepath.IsAbs(path) {
		return nil, errors.New("provenance path must be repository-relative")
	}
	rel, err := structuredfile.SafeRelative(root, filepath.ToSlash(filepath.Clean(path)))
	if err != nil {
		return nil, err
	}
	abs, err := structuredfile.RequireInsideFile(root, filepath.Join(root, filepath.FromSlash(rel)), "provenance")
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "arop-release-lineage:", err)
		os.Exit(1)
	}
}
