package main

import (
	"reflect"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func TestTransitionClosureAndAdversarialNegatives(t *testing.T) {
	root, err := structuredfile.FindRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTransition(root); err != nil {
		t.Fatal(err)
	}
}

func TestCompileClosureControlsSiblingOwnership(t *testing.T) {
	root := t.TempDir()
	artifact := "internal/example/service.go"
	source := "internal/example/service_test.go"
	if score := artifactPathScore(root, artifact, source, map[string]bool{}, map[string]bool{}); score != -1 {
		t.Fatalf("uncompiled sibling mapped with score %d", score)
	}
	if score := artifactPathScore(root, artifact, source, map[string]bool{source: true}, map[string]bool{}); score < 0 {
		t.Fatal("compile-discovered sibling did not map to its concrete artifact")
	}
}

func TestReverseDAGPropagationUsesDeterministicFixedPoint(t *testing.T) {
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{
		{ID: "phase-report-p09", Path: "build/reports/P09", ProducerPhase: "P09", DerivesFrom: []string{"middle", "waiver"}},
		{ID: "waiver", Kind: "baseline-transition-waiver", OwnerPhase: "P09", DerivesFrom: []string{"seed"}},
		{ID: "middle", OwnerPhase: "P09", DerivesFrom: []string{"seed"}},
		{ID: "seed", OwnerPhase: "P09"},
	}}
	byID := map[string]blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
	}
	found := map[string]bool{"seed": true}
	propagateAffectedArtifacts(found, manifest, byID)
	want := map[string]bool{"seed": true, "waiver": true, "phase-report-p09": true}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("fixed-point propagation=%v want %v", found, want)
	}
}
