package core

import (
	"strings"
	"testing"
)

func TestWirePrimitives(t *testing.T) {
	valid := "01956e7b-9abc-7def-8abc-0123456789ab"
	if err := ValidateResourceID(ResourceSession, "ses_"+valid); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResourceID(ResourceSession, "boot_"+valid); err == nil {
		t.Fatal("accepted legacy boot_ prefix")
	}
	if err := ValidateResourceID(ResourceAttempt, "attempt_"+valid); err == nil {
		t.Fatal("accepted legacy attempt_ prefix")
	}
	if err := ValidateTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); err != nil {
		t.Fatal(err)
	}
	for _, valid := range []string{
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-a",
	} {
		if err := ValidateTraceParent(valid); err != nil {
			t.Fatalf("rejected future-version traceparent %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-",
	} {
		if err := ValidateTraceParent(invalid); err == nil {
			t.Fatalf("accepted traceparent %q", invalid)
		}
	}
	context := TraceContext{Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", Tracestate: strings.Repeat("界", 512)}
	if err := context.Validate(); err != nil {
		t.Fatalf("rejected 512-code-point tracestate: %v", err)
	}
	context.Tracestate += "界"
	if err := context.Validate(); err == nil {
		t.Fatal("accepted 513-code-point tracestate")
	}
	wire := WireError{Code: CodeDependencyUnavailable, Category: ErrorDependency, Message: "temporary", Retryable: true}
	if err := wire.Validate(); err != nil {
		t.Fatal(err)
	}
	wire.Message = string(make([]rune, 0))
	if err := wire.Validate(); err == nil {
		t.Fatal("accepted empty error message")
	}
	wire.Message = strings.Repeat("界", 2000)
	if err := wire.Validate(); err != nil {
		t.Fatalf("rejected 2000 Unicode code points: %v", err)
	}
	wire.Message += "界"
	if err := wire.Validate(); err == nil {
		t.Fatal("accepted 2001 Unicode code points")
	}
	wire.Message = string([]byte{0xff})
	if err := wire.Validate(); err == nil {
		t.Fatal("accepted invalid UTF-8 error message")
	}
	for _, document := range []string{
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":true,"retry_after_seconds":1.0}`,
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":true,"retry_after_seconds":1e0}`,
	} {
		var decoded WireError
		if err := DecodeAuthoring([]byte(document), &decoded); err != nil {
			t.Fatalf("wire error decoder diverged from JSON Schema integer semantics for %s: %v", document, err)
		}
		if decoded.RetryAfterSeconds == nil || *decoded.RetryAfterSeconds != 1 {
			t.Fatalf("retry_after_seconds from %s = %v, want 1", document, decoded.RetryAfterSeconds)
		}
	}
	for _, document := range []string{
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary"}`,
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":null}`,
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":true,"retry_after_seconds":null}`,
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":true,"details":null}`,
		`{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"temporary","retryable":true,"trace_id":null}`,
	} {
		var decoded WireError
		if err := DecodeAuthoring([]byte(document), &decoded); err == nil {
			t.Fatalf("wire error decoder accepted Schema-invalid absent/null field: %s", document)
		}
	}
	var trace TraceContext
	if err := DecodeAuthoring([]byte(`{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","tracestate":null}`), &trace); err == nil {
		t.Fatal("trace decoder accepted null tracestate")
	}
}

func TestIdentifierValidatorsMatchV1Families(t *testing.T) {
	validUUID := "01956e7b-9abc-7def-8abc-0123456789ab"
	for name, validate := range map[string]func(string) error{
		"agent": ValidateAgentID, "skill": ValidateSkillID, "service": ValidateServiceID, "instance": ValidateInstanceID,
	} {
		if err := validate("image.generate"); err != nil {
			t.Fatalf("%s slug: %v", name, err)
		}
	}
	for name, test := range map[string]struct {
		validate func(string) error
		valid    string
		invalid  string
	}{
		"uuidv7":     {ValidateUUIDv7, validUUID, strings.Replace(validUUID, "-7def-", "-6def-", 1)},
		"effect":     {ValidateEffectID, "eff_order:1234", "eff_x"},
		"semver":     {ValidateSemanticVersion, "1.2.3-rc.1+build", "1.2.3-01"},
		"digest":     {ValidateSHA256Digest, "sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64)},
		"capability": {ValidateCapabilityID, "streaming.resume.v1", "streaming.v0"},
		"extension":  {ValidateExtensionID, "com.example.governance.v1", "example.governance.v1"},
	} {
		if err := test.validate(test.valid); err != nil {
			t.Fatalf("%s valid: %v", name, err)
		}
		if err := test.validate(test.invalid); err == nil {
			t.Fatalf("%s accepted invalid value", name)
		}
	}
}

func TestUTF8ByteOffsetsAndDuplicateDelivery(t *testing.T) {
	accumulator, err := NewTextAccumulator("")
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		offset int
		delta  string
	}{{0, "商品"}, {6, "😀"}, {10, "é"}, {10, "é"}}
	for _, step := range steps {
		if err := accumulator.Apply(step.offset, step.delta); err != nil {
			t.Fatal(err)
		}
	}
	if accumulator.String() != "商品😀é" || accumulator.Bytes() != 13 {
		t.Fatalf("value=%q bytes=%d", accumulator.String(), accumulator.Bytes())
	}
	if err := ValidateUTF8Offset("商品", 1); err == nil {
		t.Fatal("accepted offset inside UTF-8 code point")
	}
	if err := accumulator.Apply(10, "different"); err == nil {
		t.Fatal("accepted conflicting duplicate delta")
	}
}

func TestPureStateMachinesAndRegistryFencing(t *testing.T) {
	if err := ValidateRunTransition(RunRunning, RunCancelRequested); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRunTransition(RunCancelRequested, RunRunning); err == nil {
		t.Fatal("cancel_requested incorrectly returned to running")
	}
	if err := ValidateRunTransition(RunSucceeded, RunFailed); err == nil {
		t.Fatal("terminal Run state was reversible")
	}
	if err := ValidateAttemptTransition(AttemptCreated, AttemptRunning); err == nil {
		t.Fatal("Attempt skipped assignment and acceptance")
	}
	first, err := (RegistrySession{State: RegistryUnregistered}).Register("ses_01956e7b-9abc-7def-8abc-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	second, err := first.Register("ses_01956e7c-9abc-7def-8abc-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := second.Keepalive(first.SessionID, first.Generation)
	if err == nil || err.Error() != "INSTANCE_GENERATION_FENCED" {
		t.Fatalf("old generation was not fenced: %v", err)
	}
	if unchanged != second {
		t.Fatal("fenced keepalive mutated registry session")
	}
	draining, err := second.Drain(second.SessionID, second.Generation)
	if err != nil {
		t.Fatal(err)
	}
	reregistered, err := draining.Register("ses_01956e7d-9abc-7def-8abc-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if reregistered.State != RegistryDraining || reregistered.Generation != draining.Generation+1 {
		t.Fatalf("registration cleared drain or failed to advance generation: %#v", reregistered)
	}
	if !(DiscoveryFacts{LeaseAlive: true, Healthy: true, Ready: true, Enabled: true, CapacityAvailable: true, ProtocolCompatible: true, BindingMatches: true}).Discoverable() {
		t.Fatal("fully eligible instance was not discoverable")
	}
	if (DiscoveryFacts{LeaseAlive: true, Healthy: true, Ready: true, Enabled: true, Draining: true, CapacityAvailable: true, ProtocolCompatible: true, BindingMatches: true}).Discoverable() {
		t.Fatal("draining instance was discoverable")
	}
}
