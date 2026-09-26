package main

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
)

func TestCompileClosureControlsSiblingOwnership(t *testing.T) {
	root := t.TempDir()
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{{
		ID: "run-service", Path: "internal/run/service.go", PathRole: "concrete", OwnerPhase: "P18",
	}}}
	source := "internal/run/service_test.go"
	want := map[string]bool{"run-service": true}
	found, err := mapSourceArtifacts(root, manifest, []string{source}, map[string]bool{source: true}, nil)
	if err != nil || !reflect.DeepEqual(found, want) {
		t.Fatalf("compile-discovered source mapping=%v err=%v want=%v", found, err, want)
	}
	if found, err = mapSourceArtifacts(root, manifest, []string{source}, nil, nil); err == nil || found != nil {
		t.Fatalf("uncompiled sibling source did not fail closed: found=%v err=%v", found, err)
	}
	replacement := "unowned/run/service_test.go"
	if found, err = mapSourceArtifacts(root, manifest, []string{replacement}, map[string]bool{replacement: true}, nil); err == nil || found != nil {
		t.Fatalf("replacement source did not fail closed: found=%v err=%v", found, err)
	}
}

func TestReverseDAGPropagationUsesDeterministicFixedPoint(t *testing.T) {
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{
		{ID: "phase-report-p18", Path: "build/reports/P18", PathRole: "concrete", ProducerPhase: "P18", DerivesFrom: []string{"middle", "waiver"}},
		{ID: "waiver", Kind: "baseline-transition-waiver", PathRole: "concrete", OwnerPhase: "P18", DerivesFrom: []string{"seed"}},
		{ID: "middle", PathRole: "concrete", OwnerPhase: "P18", DerivesFrom: []string{"seed"}},
		{ID: "seed", PathRole: "concrete", OwnerPhase: "P18"},
	}}
	want := map[string]bool{"seed": true, "middle": true, "waiver": true, "phase-report-p18": true}
	orders := [][]blueprint.Artifact{
		manifest.Artifacts,
		{manifest.Artifacts[3], manifest.Artifacts[1], manifest.Artifacts[0], manifest.Artifacts[2]},
	}
	for index, artifacts := range orders {
		ordered := blueprint.Manifest{Artifacts: artifacts}
		byID := map[string]blueprint.Artifact{}
		for _, artifact := range ordered.Artifacts {
			byID[artifact.ID] = artifact
		}
		found := map[string]bool{"seed": true}
		propagateAffectedArtifacts(found, ordered, byID)
		if !reflect.DeepEqual(found, want) {
			t.Fatalf("order %d propagation=%v want=%v", index, found, want)
		}
	}
}

func TestMakefileAcceptanceAndReportEdgeAreDiscovered(t *testing.T) {
	service := blueprint.Artifact{ID: "run-service", Path: "internal/run", PathRole: "concrete", OwnerPhase: "P18", AcceptanceTest: "make-test-run-lifecycle"}
	reportArtifact := blueprint.Artifact{ID: "phase-report-p18", Path: "build/reports/P18", PathRole: "concrete", ProducerPhase: "P18", DerivesFrom: []string{service.ID}}
	manifest := blueprint.Manifest{Artifacts: []blueprint.Artifact{service, reportArtifact}}
	found, err := mapSourceArtifacts(t.TempDir(), manifest, []string{"Makefile"}, nil, map[string]bool{"Makefile": true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]blueprint.Artifact{service.ID: service, reportArtifact.ID: reportArtifact}
	propagateAffectedArtifacts(found, manifest, byID)
	want := map[string]bool{service.ID: true, reportArtifact.ID: true}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("Makefile/report discovery=%v want=%v", found, want)
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
	byID = map[string]blueprint.Artifact{service.ID: service, reportArtifact.ID: withoutReportEdge.Artifacts[1]}
	propagateAffectedArtifacts(found, withoutReportEdge, byID)
	if err := requireArtifactLowerBound(found, []string{service.ID, reportArtifact.ID}); err == nil {
		t.Fatal("missing report dependency edge did not fail the lower-bound check")
	}
}

func TestGitNameStatusParsingCoversAllTrackedChangeKinds(t *testing.T) {
	stream := bytes.Join([][]byte{
		[]byte("A"), []byte("added"),
		[]byte("M"), []byte("modified"),
		[]byte("D"), []byte("deleted"),
		[]byte("R100"), []byte("rename-old"), []byte("rename-new"),
		[]byte("C100"), []byte("copy-old"), []byte("copy-new"),
		{},
	}, []byte{0})
	found, err := parseChangedSources(stream)
	want := []string{"added", "copy-new", "copy-old", "deleted", "modified", "rename-new", "rename-old"}
	if err != nil || !reflect.DeepEqual(found, want) {
		t.Fatalf("name-status parsing=%v err=%v want=%v", found, err, want)
	}
}
