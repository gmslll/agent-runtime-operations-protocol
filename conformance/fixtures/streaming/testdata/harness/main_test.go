package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestGitNameStatusParsingRejectsTruncationAndTracksBothRenameSides(t *testing.T) {
	t.Parallel()
	parsed, err := parseChangedSources([]byte("M\x00one\x00R100\x00old\x00new\x00"))
	if err != nil || !reflect.DeepEqual(parsed, []string{"new", "old", "one"}) {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
	for _, invalid := range [][]byte{[]byte("R100\x00old\x00"), []byte("X\x00one\x00")} {
		if _, err = parseChangedSources(invalid); err == nil {
			t.Fatalf("invalid stream accepted: %q", invalid)
		}
	}
}

func TestTransitionNegativesCoverEveryExactCollection(t *testing.T) {
	t.Parallel()
	var value transition
	value.SchemaVersion = 1
	value.WaiverID = "P22-STRUCTURED-STREAMING-TRANSITION-001"
	value.Status = "validated"
	value.Policy.OwnerPhaseSemantics = "first-introduction-and-accountability"
	value.Baseline.Rule, value.Baseline.Commit = "parent-of-unique-waiver-introduction-commit", baseline
	value.Transition.FromPhases, value.Transition.ToPhase, value.Transition.Reason = append([]string{"P01", "P02"}, phases(5, 21)...), "P22", "test"
	value.SourceClosure = []string{"a"}
	value.AffectedArtifacts = []string{"b"}
	value.Acceptance = canonicalAcceptance()
	value.Constraints = canonicalConstraints()
	discovered := discoveredTransition{sources: []string{"a"}, artifacts: []string{"b"}}
	if err := validateTransitionCandidate(value, discovered); err != nil {
		t.Fatal(err)
	}
	if err := validateTransitionNegatives(value, discovered); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	encoded = append(encoded, []byte(` {}`)...)
	if strictJSON(encoded, &transition{}) == nil {
		t.Fatal("trailing JSON accepted")
	}
	if bytes.Count(encoded, []byte(`"waiver_id"`)) != 1 {
		t.Fatal("unexpected test encoding")
	}
}
