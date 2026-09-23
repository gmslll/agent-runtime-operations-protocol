package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSchemaSetIsOfflineClosedAndStrict(t *testing.T) {
	const root = "https://arop.invalid/test/root.json"
	const child = "https://arop.invalid/test/child.json"
	set, err := NewSchemaSet(map[string][]byte{
		root:  []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://arop.invalid/test/root.json","$ref":"child.json"}`),
		child: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://arop.invalid/test/child.json","type":"object","required":["name"],"properties":{"name":{"type":"string"}},"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Validate(root, []byte(`{"name":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if err := set.Validate(root, []byte(`{"name":"ok","extra":true}`)); err == nil {
		t.Fatal("schema set accepted unknown authoring field")
	}
	if _, err := NewSchemaSet(map[string][]byte{root: []byte(`{"$id":"https://arop.invalid/test/root.json","$ref":"https://example.com/remote.json"}`)}); err == nil {
		t.Fatal("schema set attempted or accepted an unbundled remote reference")
	}
}

func TestSchemaSetPinsDraft202012(t *testing.T) {
	const location = "https://arop.invalid/conformance/dialect.schema.json"
	for _, schema := range []string{
		`{"$schema":"https://json-schema.org/draft/2019-09/schema","$id":"` + location + `","type":"object"}`,
		`{"$schema":true,"$id":"` + location + `","type":"object"}`,
	} {
		if _, err := NewSchemaSet(map[string][]byte{location: []byte(schema)}); err == nil {
			t.Fatalf("NewSchemaSet accepted non-2020-12 dialect: %s", schema)
		}
	}
	if _, err := NewSchemaSet(map[string][]byte{location: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","type":"object"}`)}); err != nil {
		t.Fatalf("NewSchemaSet rejected exact Draft 2020-12 dialect: %v", err)
	}
	if _, err := NewSchemaSet(map[string][]byte{location: []byte(`{"$id":"` + location + `","type":"object"}`)}); err != nil {
		t.Fatalf("NewSchemaSet rejected omitted dialect with Draft 2020-12 default: %v", err)
	}
	for _, schema := range []string{
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","$defs":{"legacy":{"$id":"legacy","$schema":"https://json-schema.org/draft/2019-09/schema","type":"string"}}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","properties":{"value":{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"string"}}}`,
	} {
		if _, err := NewSchemaSet(map[string][]byte{location: []byte(schema)}); err == nil {
			t.Fatalf("NewSchemaSet accepted nested dialect switch: %s", schema)
		}
	}
	for _, schema := range []string{
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","$ref":"#/definitions/legacy","definitions":{"legacy":{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"string"}}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","$ref":"#/x-custom/legacy","x-custom":{"legacy":{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"string"}}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","const":{"$schema":"https://json-schema.org/draft/2019-09/schema"}}`,
	} {
		if _, err := NewSchemaSet(map[string][]byte{location: []byte(schema)}); err == nil {
			t.Fatalf("NewSchemaSet accepted a hidden nested dialect switch: %s", schema)
		}
	}
}

func TestSchemaRegexHasExecutionBudget(t *testing.T) {
	const location = "https://arop.invalid/conformance/regex-budget.schema.json"
	set, err := NewSchemaSet(map[string][]byte{
		location: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","type":"string","pattern":"^(a+)+$"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := `"` + strings.Repeat("a", 32) + `!"`
	started := time.Now()
	if err := set.Validate(location, []byte(input)); err == nil {
		t.Fatal("catastrophic-backtracking input unexpectedly matched")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("schema regex exceeded execution budget: %s", elapsed)
	}

	var properties, instance strings.Builder
	properties.WriteString(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","type":"object","properties":{`)
	instance.WriteByte('{')
	for index := 0; index < 40; index++ {
		if index != 0 {
			properties.WriteByte(',')
			instance.WriteByte(',')
		}
		fmt.Fprintf(&properties, `"p%d":{"type":"string","pattern":"^(a+)+$"}`, index)
		fmt.Fprintf(&instance, `"p%d":"%s!"`, index, strings.Repeat("a", 20_000))
	}
	properties.WriteString(`},"additionalProperties":false}`)
	instance.WriteByte('}')
	many, err := NewSchemaSet(map[string][]byte{location: []byte(properties.String())})
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := many.Validate(location, []byte(instance.String())); err == nil {
		t.Fatal("multi-pattern catastrophic-backtracking input unexpectedly matched")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("schema validation multiplied the per-pattern regexp budget: %s", elapsed)
	}

	notSet, err := NewSchemaSet(map[string][]byte{
		location: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","type":"string","not":{"pattern":"^(a+)+b$|^a+$"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := notSet.Validate(location, []byte(`"`+strings.Repeat("a", 20_000)+`"`)); err == nil {
		t.Fatal("linear regexp engine failed open under not")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("linear regexp engine exceeded document budget under not: %s", elapsed)
	}

	for _, unsupported := range []string{
		`(?=a)a`, `(a)\1`, `\A`, `\z`, `\Qliteral\E`, `[[:alpha:]]`, `^.$`,
		`\-`, `\!`, `\_`, `\,`, `\:`,
	} {
		schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"` + location + `","type":"string","pattern":` + fmt.Sprintf("%q", unsupported) + `}`
		if _, err := NewSchemaSet(map[string][]byte{location: []byte(schema)}); err == nil {
			t.Fatalf("backtracking-only pattern %q was accepted", unsupported)
		}
	}
}
