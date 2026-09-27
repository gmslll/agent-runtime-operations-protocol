package main

import "testing"

func TestCompareTransitionRejectsEveryClosureDimension(t *testing.T) {
	want := discovered{
		Artifacts:  []string{"artifact-a", "artifact-b"},
		Sources:    []string{"a.go", "b.go"},
		Acceptance: []string{"make test-a", "make test-b"},
	}
	valid := transition{
		SchemaVersion: 1, TransitionID: "P24-BASELINE-TRANSITION-001", Status: "validated",
		BaselineCommit: baseline, CarrierCommit: carrier,
		AffectedArtifacts: append([]string(nil), want.Artifacts...),
		SourceClosure:     append([]string(nil), want.Sources...),
		AcceptanceClosure: append([]string(nil), want.Acceptance...),
		Constraints:       append([]string(nil), requiredConstraints...),
	}
	if err := compareTransition(valid, want); err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}
	tests := map[string]func(*transition){
		"identity":                func(value *transition) { value.Status = "draft" },
		"artifact-omission":       func(value *transition) { value.AffectedArtifacts = value.AffectedArtifacts[:1] },
		"artifact-addition":       func(value *transition) { value.AffectedArtifacts = append(value.AffectedArtifacts, "artifact-c") },
		"artifact-substitution":   func(value *transition) { value.AffectedArtifacts[0] = "artifact-c" },
		"artifact-duplicate":      func(value *transition) { value.AffectedArtifacts[1] = value.AffectedArtifacts[0] },
		"source-omission":         func(value *transition) { value.SourceClosure = value.SourceClosure[:1] },
		"source-addition":         func(value *transition) { value.SourceClosure = append(value.SourceClosure, "c.go") },
		"source-substitution":     func(value *transition) { value.SourceClosure[0] = "c.go" },
		"source-duplicate":        func(value *transition) { value.SourceClosure[1] = value.SourceClosure[0] },
		"acceptance-omission":     func(value *transition) { value.AcceptanceClosure = value.AcceptanceClosure[:1] },
		"acceptance-addition":     func(value *transition) { value.AcceptanceClosure = append(value.AcceptanceClosure, "make test-c") },
		"acceptance-substitution": func(value *transition) { value.AcceptanceClosure[0] = "make test-c" },
		"acceptance-duplicate":    func(value *transition) { value.AcceptanceClosure[1] = value.AcceptanceClosure[0] },
		"constraint-omission":     func(value *transition) { value.Constraints = value.Constraints[:len(value.Constraints)-1] },
		"constraint-addition":     func(value *transition) { value.Constraints = append(value.Constraints, "invented") },
		"constraint-substitution": func(value *transition) { value.Constraints[0] = "invented" },
		"constraint-duplicate":    func(value *transition) { value.Constraints[1] = value.Constraints[0] },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.AffectedArtifacts = append([]string(nil), valid.AffectedArtifacts...)
			candidate.SourceClosure = append([]string(nil), valid.SourceClosure...)
			candidate.AcceptanceClosure = append([]string(nil), valid.AcceptanceClosure...)
			candidate.Constraints = append([]string(nil), valid.Constraints...)
			mutate(&candidate)
			if err := compareTransition(candidate, want); err == nil {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}
