package specindex

import "testing"

func TestGovernanceNegativeProbes(t *testing.T) {
	t.Parallel()
	if _, duplicates := decisionDefinitions("| D-001 | a |\n| D-001 | b |\n"); len(duplicates) != 1 {
		t.Fatal("duplicate decision escaped")
	}
	if got := legacyBindingProblems(`{"event_sink":1,"cancel_url":2}\nPOST /v1/runs/{run_id}/cancel`); len(got) != 3 {
		t.Fatalf("legacy bindings escaped: %v", got)
	}
	if got := publicationV01LineProblems("publish public v0.1", "probe"); len(got) != 1 {
		t.Fatalf("positive v0.1 escaped: %v", got)
	}
	if got := publicationV01LineProblems("取消独立公共 v0.1", "probe"); len(got) != 0 {
		t.Fatalf("negative policy rejected: %v", got)
	}
}
