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
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{{
		ID: "service", Path: "internal/example/service.go", PathRole: "concrete", OwnerPhase: "P12",
	}}}
	source := "internal/example/service_test.go"
	want := map[string]bool{"service": true}
	found, err := mapSourceArtifacts(root, manifest, []string{source}, map[string]bool{source: true}, map[string]bool{})
	if err != nil || !reflect.DeepEqual(found, want) {
		t.Fatalf("compile-discovered source mapping=%v err=%v want %v", found, err, want)
	}
	if found, err = mapSourceArtifacts(root, manifest, []string{source}, map[string]bool{}, map[string]bool{}); err == nil || found != nil {
		t.Fatalf("deleted compile-discovered source did not fail exact discovery: found=%v err=%v", found, err)
	}
	replacement := "unowned/example/service_test.go"
	if found, err = mapSourceArtifacts(root, manifest, []string{replacement}, map[string]bool{replacement: true}, map[string]bool{}); err == nil || found != nil {
		t.Fatalf("replacement compile-discovered source did not fail exact discovery: found=%v err=%v", found, err)
	}
	unknown := "notes/unowned.txt"
	if found, err = mapSourceArtifacts(root, manifest, []string{unknown}, map[string]bool{}, map[string]bool{}); err == nil || found != nil {
		t.Fatalf("unknown changed source outside compile closure did not fail closed: found=%v err=%v", found, err)
	}
}

func TestAggregateSourceMapsOnlyToCurrentConcreteFutureOwner(t *testing.T) {
	current := blueprint.Artifact{ID: "current-command", Path: "cmd/tool/internal/current", PathRole: "concrete", OwnerPhase: "P12"}
	future := blueprint.Artifact{ID: "future-command", Path: "cmd/tool/internal/future", PathRole: "concrete", OwnerPhase: "P13"}
	aggregate := blueprint.Artifact{ID: "tool-baseline", Path: "cmd/tool", PathRole: "aggregate", FutureArtifacts: []string{current.ID, future.ID}}
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{aggregate, future, current}}
	source := "cmd/tool/main.go"
	found, err := mapSourceArtifacts(t.TempDir(), manifest, []string{source}, map[string]bool{source: true}, nil)
	want := map[string]bool{current.ID: true}
	if err != nil || !reflect.DeepEqual(found, want) {
		t.Fatalf("aggregate future owner mapping=%v err=%v want %v", found, err, want)
	}
	if found, err = mapSourceArtifacts(t.TempDir(), manifest, []string{source}, nil, nil); err == nil || found != nil {
		t.Fatalf("unconsumed aggregate source did not fail closed: found=%v err=%v", found, err)
	}
}

func TestReverseDAGPropagationUsesDeterministicFixedPoint(t *testing.T) {
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{
		{ID: "phase-report-p09", Path: "build/reports/P09", ProducerPhase: "P09", DerivesFrom: []string{"middle", "waiver"}},
		{ID: "waiver", Kind: "baseline-transition-waiver", OwnerPhase: "P09", DerivesFrom: []string{"seed"}},
		{ID: "middle", OwnerPhase: "P09", DerivesFrom: []string{"seed"}},
		{ID: "seed", OwnerPhase: "P09"},
	}}
	want := map[string]bool{"seed": true, "waiver": true, "phase-report-p09": true}
	orders := [][]blueprint.Artifact{
		manifest.Artifacts,
		{manifest.Artifacts[3], manifest.Artifacts[1], manifest.Artifacts[0], manifest.Artifacts[2]},
	}
	var first map[string]bool
	for index, artifacts := range orders {
		ordered := blueprint.Manifest{Artifacts: artifacts}
		byID := map[string]blueprint.Artifact{}
		for _, artifact := range ordered.Artifacts {
			byID[artifact.ID] = artifact
		}
		found := map[string]bool{"seed": true}
		propagateAffectedArtifacts(found, ordered, byID)
		if !reflect.DeepEqual(found, want) {
			t.Fatalf("order %d fixed-point propagation=%v want %v", index, found, want)
		}
		if index == 0 {
			first = found
		} else if !reflect.DeepEqual(found, first) {
			t.Fatalf("manifest order changed propagation: first=%v next=%v", first, found)
		}
	}
}

func TestMakefileOwnershipAndReportEdgeAreDiscovered(t *testing.T) {
	service := blueprint.Artifact{
		ID:             "publication-service",
		Path:           "internal/publication",
		PathRole:       "concrete",
		OwnerPhase:     "P12",
		AcceptanceTest: "make-test-publication-service",
	}
	reportArtifact := blueprint.Artifact{
		ID:            "phase-report-p12",
		Path:          "build/reports/P12",
		PathRole:      "concrete",
		ProducerPhase: "P12",
		DerivesFrom:   []string{"publication-service"},
	}
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{service, reportArtifact}}
	found, err := mapSourceArtifacts(t.TempDir(), manifest, []string{"Makefile"}, nil, map[string]bool{"Makefile": true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]blueprint.Artifact{service.ID: service, reportArtifact.ID: reportArtifact}
	propagateAffectedArtifacts(found, manifest, byID)
	want := map[string]bool{"publication-service": true, "phase-report-p12": true}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("Makefile/report discovery=%v want %v", found, want)
	}

	withoutAcceptance := manifest
	withoutAcceptance.Artifacts = append([]blueprint.Artifact(nil), manifest.Artifacts...)
	withoutAcceptance.Artifacts[0].AcceptanceTest = "make-another-phase"
	if found, err = mapSourceArtifacts(t.TempDir(), withoutAcceptance, []string{"Makefile"}, nil, map[string]bool{"Makefile": true}); err == nil || found != nil {
		t.Fatalf("missing Makefile acceptance binding did not fail closed: found=%v err=%v", found, err)
	}

	withoutReportEdge := manifest
	withoutReportEdge.Artifacts = append([]blueprint.Artifact(nil), manifest.Artifacts...)
	withoutReportEdge.Artifacts[1].DerivesFrom = nil
	found, err = mapSourceArtifacts(t.TempDir(), withoutReportEdge, []string{"Makefile"}, nil, map[string]bool{"Makefile": true})
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]blueprint.Artifact{}
	for _, artifact := range withoutReportEdge.Artifacts {
		byID[artifact.ID] = artifact
	}
	propagateAffectedArtifacts(found, withoutReportEdge, byID)
	if err := requireArtifactLowerBound(found, []string{"publication-service", "phase-report-p12"}); err == nil {
		t.Fatal("missing phase report dependency edge did not fail the lower-bound check")
	}
}
