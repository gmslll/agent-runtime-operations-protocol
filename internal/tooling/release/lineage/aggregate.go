package lineage

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type AggregateOptions struct {
	Root          string
	ReleaseCommit string
	Candidates    []VerifiedCandidate
}

type Aggregate struct {
	SchemaVersion int                 `json:"schema_version"`
	ReleaseCommit string              `json:"release_commit"`
	ReleaseTree   string              `json:"release_tree"`
	Reports       []VerifiedCandidate `json:"reports"`
	SHA256        string              `json:"sha256"`
}

func BuildAggregate(options AggregateOptions) (Aggregate, error) {
	if len(options.Candidates) == 0 {
		return Aggregate{}, errors.New("release aggregate must contain reports")
	}
	release := options.ReleaseCommit
	if release == "" {
		var err error
		release, err = git(options.Root, "rev-parse", "HEAD")
		if err != nil {
			return Aggregate{}, err
		}
	}
	tree, err := git(options.Root, "rev-parse", release+"^{tree}")
	if err != nil {
		return Aggregate{}, errors.New("release commit does not exist")
	}
	reports := sortedCandidates(options.Candidates)
	seen := map[string]bool{}
	for _, item := range reports {
		if seen[item.Phase] {
			return Aggregate{}, fmt.Errorf("duplicate phase %s", item.Phase)
		}
		seen[item.Phase] = true
		if item.Strategy != StrategyCurrent && item.Strategy != StrategyIsolatedReplay && item.Strategy != StrategyTrustedCI {
			return Aggregate{}, fmt.Errorf("phase %s has unverified strategy", item.Phase)
		}
		if item.Strategy != StrategyCurrent && item.VerificationRef == "" {
			return Aggregate{}, fmt.Errorf("phase %s lacks replay or trusted provenance", item.Phase)
		}
		if item.ClaimedCommit == "" || item.ClaimedTree == "" || item.ReportSHA256 == "" || item.CheckerSHA256 == "" || item.InputsSHA256 == "" {
			return Aggregate{}, fmt.Errorf("phase %s lineage is incomplete", item.Phase)
		}
		if actual, err := git(options.Root, "rev-parse", item.ClaimedCommit+"^{tree}"); err != nil || actual != item.ClaimedTree {
			return Aggregate{}, fmt.Errorf("phase %s claimed tree mismatch", item.Phase)
		}
		command := execGit(options.Root, "merge-base", "--is-ancestor", item.ClaimedCommit, release)
		if command != nil {
			return Aggregate{}, fmt.Errorf("phase %s is not an ancestor of release commit", item.Phase)
		}
	}
	canonical := struct {
		SchemaVersion int                 `json:"schema_version"`
		ReleaseCommit string              `json:"release_commit"`
		ReleaseTree   string              `json:"release_tree"`
		Reports       []VerifiedCandidate `json:"reports"`
	}{1, release, tree, reports}
	data, err := json.Marshal(canonical)
	if err != nil {
		return Aggregate{}, err
	}
	return Aggregate{1, release, tree, reports, "sha256:" + sha(data)}, nil
}

func execGit(root string, arguments ...string) error {
	_, err := gitBytes(root, arguments...)
	return err
}

func RequiredPhases(aggregate Aggregate, phases []string) error {
	expected := append([]string{}, phases...)
	sort.Strings(expected)
	actual := make([]string, 0, len(aggregate.Reports))
	for _, item := range aggregate.Reports {
		actual = append(actual, item.Phase)
	}
	if stringList(actual) != stringList(expected) {
		return fmt.Errorf("aggregate phases are not exact: got %v want %v", actual, expected)
	}
	return nil
}

func stringList(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}
