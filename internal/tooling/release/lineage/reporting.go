package lineage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type CandidateFile struct {
	SchemaVersion  int    `json:"schema_version"`
	Phase          string `json:"phase"`
	ReportPath     string `json:"report_path"`
	Strategy       string `json:"strategy"`
	ProvenancePath string `json:"provenance_path,omitempty"`
	EnvelopePath   string `json:"envelope_path,omitempty"`
}

type Request struct {
	SchemaVersion int             `json:"schema_version"`
	ReleaseCommit string          `json:"release_commit"`
	Candidates    []CandidateFile `json:"candidates"`
}

func LoadRequest(root, path string) (Request, error) {
	rel, err := safeRelative(root, path)
	if err != nil {
		return Request{}, err
	}
	abs, err := structuredRegular(root, rel)
	if err != nil {
		return Request{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := decodeStrict(data, &request); err != nil {
		return Request{}, err
	}
	if request.SchemaVersion != 1 || len(request.Candidates) == 0 {
		return Request{}, errors.New("lineage request schema_version must be 1 and candidates non-empty")
	}
	seen := map[string]bool{}
	for _, item := range request.Candidates {
		if !phasePattern.MatchString(item.Phase) || seen[item.Phase] {
			return Request{}, errors.New("candidate phases must be unique canonical PNN values")
		}
		seen[item.Phase] = true
		if item.ReportPath == "" || (Strategy(item.Strategy) != StrategyCurrent && Strategy(item.Strategy) != StrategyIsolatedReplay && Strategy(item.Strategy) != StrategyTrustedCI) {
			return Request{}, errors.New("candidate report path or strategy is invalid")
		}
	}
	return request, nil
}

func WriteAggregate(path string, aggregate Aggregate) error {
	abs, err := filepath.Abs(path)
	if err != nil || abs == string(filepath.Separator) {
		return errors.New("aggregate output path is invalid")
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("aggregate output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(aggregate, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(abs, append(data, '\n'), 0o600)
}

func structuredRegular(root, relative string) (string, error) {
	return structuredfile.RequireInsideFile(root, filepath.Join(root, filepath.FromSlash(relative)), "lineage input")
}
