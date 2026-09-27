package main

import (
	"reflect"
	"testing"
)

func TestP26RuntimeInputInventoryIsExact(t *testing.T) {
	want := []string{"P08", "P10", "P12", "P13", "P18", "P19", "P20", "P21", "P22", "P23", "P24", "P25"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases=%v", phases)
	}
	seen := map[string]bool{}
	for _, phase := range phases {
		if seen[phase] {
			t.Fatalf("duplicate phase %s", phase)
		}
		seen[phase] = true
	}
}

func TestSensitiveEvidenceScanner(t *testing.T) {
	if err := scanEvidence([]byte("safe digest bytes")); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"wlt_secret", "Bearer eySecret", "postgresql://user:pass@host/db", "password=secret", "/Users/name/file", "/private/tmp/leak"} {
		if err := scanEvidence([]byte(value)); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
