package publication

import "testing"

func TestClassifyAllowedHostIsStaticAndFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input   string
		class   HostClass
		allowed bool
	}{
		{"api.example.invalid", HostPublicName, true},
		{"*.example.invalid", HostWildcardName, true},
		{"8.8.8.8", HostGlobalIPLiteral, true},
		{"127.0.0.1", HostLoopback, false},
		{"169.254.169.254", HostMetadata, false},
		{"10.0.0.1", HostPrivate, false},
		{"metadata.google.internal", HostMetadata, false},
		{"user@example.invalid", HostInvalid, false},
		{"https://example.invalid", HostInvalid, false},
		{"example.invalid:443", HostInvalid, false},
		{"localhost", HostLoopback, false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			got := ClassifyAllowedHost(test.input)
			if got.Class != test.class || got.Class.Allowed() != test.allowed {
				t.Fatalf("classify %q = %q allowed=%v", test.input, got.Class, got.Class.Allowed())
			}
		})
	}
}

func TestAllowedHostsRejectCanonicalDuplicatesAndDeniedClasses(t *testing.T) {
	t.Parallel()
	if _, err := classifyAllowedHosts([]string{"EXAMPLE.invalid", "example.invalid"}); err == nil {
		t.Fatal("canonical duplicate allowed host accepted")
	}
	if _, err := classifyAllowedHosts([]string{"api.example.invalid", "192.168.1.1"}); err == nil {
		t.Fatal("private allowed host accepted")
	}
}
