package structuredfile

import "testing"

func TestStrictJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
	}{
		{"top-level duplicate", `{"a":1,"a":2}`},
		{"nested duplicate", `{"a":{"b":1,"b":2}}`},
		{"trailing object", `{"a":1} {}`},
		{"trailing null", `{"a":1} null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data), "json"); err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
		})
	}
}

func TestStrictYAML(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
	}{
		{"top-level duplicate", "a: 1\na: 2\n"},
		{"nested duplicate", "a:\n  b: 1\n  b: 2\n"},
		{"multiple documents", "a: 1\n---\nb: 2\n"},
		{"alias", "a: &v 1\nb: *v\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data), "yaml"); err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
		})
	}
}
