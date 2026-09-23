package core

import (
	"encoding/json"
	"strings"
	"testing"
)

type testEnvelope struct {
	Name   string `json:"name"`
	Nested struct {
		Count int `json:"count"`
	} `json:"nested"`
}

func TestStrictAndForwardDecodingAreSeparate(t *testing.T) {
	document := []byte(`{"name":"agent","future":"kept","nested":{"count":1,"future_nested":true}}`)
	var strict testEnvelope
	if err := DecodeAuthoring(document, &strict); err == nil {
		t.Fatal("authoring decoder accepted unknown fields")
	}
	var forward testEnvelope
	result, err := DecodeForward(document, &forward, func(pointer string, _ any) UnknownFieldAction {
		if strings.Contains(pointer, "future") {
			return UnknownPreserve
		}
		return UnknownReject
	})
	if err != nil {
		t.Fatalf("forward decode: %v", err)
	}
	if forward.Name != "agent" || forward.Nested.Count != 1 || len(result.Preserved) != 2 {
		t.Fatalf("unexpected forward result: %#v %#v", forward, result)
	}
	if string(result.Preserved["/nested/future_nested"]) != "true" {
		t.Fatalf("nested preserved value = %s", result.Preserved["/nested/future_nested"])
	}
}

func TestForwardPreservedValuesAreJCSCanonical(t *testing.T) {
	type envelope struct {
		Known string `json:"known"`
	}
	var target envelope
	result, err := DecodeForward([]byte(`{"known":"x","future":1e0}`), &target, func(pointer string, _ any) UnknownFieldAction {
		if pointer == "/future" {
			return UnknownPreserve
		}
		return UnknownReject
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Preserved["/future"]); got != "1" {
		t.Fatalf("preserved value = %s, want RFC 8785 canonical 1", got)
	}
	for _, document := range []string{
		`{"known":"x","future":9007199254740993}`,
		`{"known":"x","future":{"nested":1e400}}`,
		`{"known":"x","future":0.10000000000000001}`,
		`{"known":"x","future":1.234567890123456789}`,
		`{"known":"x","future":9007199254740990.5}`,
	} {
		var rejected envelope
		if _, err := DecodeForward([]byte(document), &rejected, func(string, any) UnknownFieldAction { return UnknownPreserve }); err == nil {
			t.Fatalf("forward preserve silently accepted non-interoperable number: %s", document)
		}
	}
}

type customWireValue struct{ Value string }

func (value *customWireValue) UnmarshalJSON(data []byte) error {
	var wire struct {
		Wire string `json:"wire"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	value.Value = wire.Wire
	return nil
}

func TestStrictDecodeDefersToCustomJSONUnmarshalerWireShape(t *testing.T) {
	type envelope struct {
		Custom customWireValue `json:"custom"`
	}
	var target envelope
	if err := DecodeAuthoring([]byte(`{"custom":{"wire":"x"}}`), &target); err != nil {
		t.Fatalf("custom JSON wire shape was rejected before UnmarshalJSON: %v", err)
	}
	if target.Custom.Value != "x" {
		t.Fatalf("custom value=%q, want x", target.Custom.Value)
	}
	if err := DecodeAuthoring([]byte(`{"custom":{"unknown":"x"}}`), &target); err == nil {
		t.Fatal("custom unmarshaler accepted its own unknown field")
	}
	if err := DecodeAuthoring([]byte(`{"custom":null}`), &target); err == nil {
		t.Fatal("custom unmarshaler swallowed typed null")
	}
}

func TestAuthoringUsesExactJSONFieldNames(t *testing.T) {
	for _, document := range []string{
		`{"NAME":"case-folded","nested":{"count":1}}`,
		`{"name":"exact","Name":"alias","nested":{"count":1}}`,
		`{"name":"exact","nested":{"COUNT":1}}`,
	} {
		var target testEnvelope
		if err := DecodeAuthoring([]byte(document), &target); err == nil {
			t.Fatalf("DecodeAuthoring accepted case-insensitive field alias: %s", document)
		}
	}
	type embedded struct {
		Name string `json:"name"`
	}
	type envelope struct{ embedded }
	var target envelope
	if err := DecodeAuthoring([]byte(`{"name":"promoted"}`), &target); err != nil {
		t.Fatalf("DecodeAuthoring rejected exact promoted field: %v", err)
	}
}

func TestStrictJSONRejectsDuplicateAndTrailingAtEveryDepth(t *testing.T) {
	for name, document := range map[string]string{
		"top duplicate":    `{"a":1,"a":2}`,
		"nested duplicate": `{"a":{"b":1,"b":2}}`,
		"trailing object":  `{} {}`,
		"trailing null":    `{} null`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(document)); err == nil {
				t.Fatalf("ParseJSON accepted %s", document)
			}
		})
	}
	if _, err := ParseJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}); err == nil {
		t.Fatal("ParseJSON accepted invalid UTF-8")
	}
	for _, document := range []string{`{"value":"\ud800"}`, `{"value":"\udc00"}`, `{"value":"\ud800x"}`} {
		if _, err := ParseJSON([]byte(document)); err == nil {
			t.Fatalf("ParseJSON accepted unpaired surrogate escape: %s", document)
		}
	}
	value, err := ParseJSON([]byte(`{"value":"\ud83d\ude00"}`))
	if err != nil {
		t.Fatalf("ParseJSON rejected paired surrogate escape: %v", err)
	}
	if value.(map[string]any)["value"] != "😀" {
		t.Fatalf("paired surrogate decoded as %#v", value)
	}
}

func TestForwardConsumerRequiresExplicitPolicy(t *testing.T) {
	var target testEnvelope
	if _, err := DecodeForward([]byte(`{"name":"x"}`), &target, nil); err == nil {
		t.Fatal("forward decode accepted nil policy")
	}
	if _, err := DecodeForward([]byte(`{"name":"x","authorization":"future"}`), &target, func(string, any) UnknownFieldAction { return UnknownReject }); err == nil {
		t.Fatal("forward decode allowed security-sensitive unknown field")
	}
	var number any
	if err := DecodeAuthoring([]byte(`9007199254740993`), &number); err != nil {
		t.Fatal(err)
	}
	if number.(json.Number).String() != "9007199254740993" {
		t.Fatalf("number lost precision: %v", number)
	}
}

func TestForwardPolicyRecursesThroughTypedMapsAndSlices(t *testing.T) {
	type container struct {
		Items  map[string]testEnvelope   `json:"items"`
		Groups []map[string]testEnvelope `json:"groups"`
	}
	document := []byte(`{"items":{"first":{"name":"one","nested":{"count":1},"future":1}},"groups":[{"second":{"name":"two","nested":{"count":2},"future":2}}]}`)
	var target container
	result, err := DecodeForward(document, &target, func(_ string, _ any) UnknownFieldAction { return UnknownPreserve })
	if err != nil {
		t.Fatal(err)
	}
	for pointer, expected := range map[string]string{
		"/items/first/future":     "1",
		"/groups/0/second/future": "2",
	} {
		if string(result.Preserved[pointer]) != expected {
			t.Fatalf("preserved[%s]=%s, want %s", pointer, result.Preserved[pointer], expected)
		}
	}
}

func TestForwardPolicyRecursesThroughIntegerKeyMaps(t *testing.T) {
	type container struct {
		Items map[int]testEnvelope `json:"items"`
	}
	document := []byte(`{"items":{"1":{"name":"one","nested":{"count":1},"future":true}}}`)
	var target container
	result, err := DecodeForward(document, &target, func(pointer string, _ any) UnknownFieldAction {
		if pointer == "/items/1/future" {
			return UnknownPreserve
		}
		return UnknownReject
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.Items[1].Name != "one" || string(result.Preserved["/items/1/future"]) != "true" {
		t.Fatalf("unexpected integer-key map result: %#v %#v", target, result)
	}
}

func TestFixedArraysRequireExactLength(t *testing.T) {
	type envelope struct {
		Pair [2]string `json:"pair"`
	}
	for _, document := range []string{
		`{"pair":["a"]}`,
		`{"pair":["a","b","ignored"]}`,
	} {
		var authoring envelope
		if err := DecodeAuthoring([]byte(document), &authoring); err == nil {
			t.Fatalf("DecodeAuthoring accepted wrong fixed-array length: %s", document)
		}
		var forward envelope
		if _, err := DecodeForward([]byte(document), &forward, func(string, any) UnknownFieldAction { return UnknownReject }); err == nil {
			t.Fatalf("DecodeForward accepted wrong fixed-array length: %s", document)
		}
	}
	var exact envelope
	if err := DecodeAuthoring([]byte(`{"pair":["a","b"]}`), &exact); err != nil {
		t.Fatalf("DecodeAuthoring rejected exact fixed-array length: %v", err)
	}
}

func TestIntegerTargetsUseJSONSchemaMathematicalSemantics(t *testing.T) {
	type envelope struct {
		Count int `json:"count"`
	}
	for _, document := range []string{`{"count":1}`, `{"count":1.0}`, `{"count":1e0}`} {
		var target envelope
		if err := DecodeAuthoring([]byte(document), &target); err != nil {
			t.Fatalf("DecodeAuthoring rejected integral number %s: %v", document, err)
		}
		if target.Count != 1 {
			t.Fatalf("DecodeAuthoring(%s) count=%d, want 1", document, target.Count)
		}
	}
	for _, document := range []string{`{"count":1.5}`, `{"count":1e-1}`, `{"count":1e1000000}`, `{"count":1e-1000000}`} {
		var target envelope
		if err := DecodeAuthoring([]byte(document), &target); err == nil {
			t.Fatalf("DecodeAuthoring accepted non-integral number %s", document)
		}
	}
}

func TestTypedNullNeverSilentlyBecomesAZeroValue(t *testing.T) {
	type envelope struct {
		Name     string         `json:"name"`
		Optional *int           `json:"optional,omitempty"`
		Labels   map[string]any `json:"labels,omitempty"`
		Opaque   any            `json:"opaque,omitempty"`
	}
	for _, document := range []string{
		`{"name":null}`,
		`{"name":"ok","optional":null}`,
		`{"name":"ok","labels":null}`,
	} {
		var target envelope
		if err := DecodeAuthoring([]byte(document), &target); err == nil {
			t.Fatalf("DecodeAuthoring silently accepted typed null: %s", document)
		}
	}
	var opaque envelope
	if err := DecodeAuthoring([]byte(`{"name":"ok","opaque":null}`), &opaque); err != nil {
		t.Fatalf("DecodeAuthoring rejected null held by explicit interface field: %v", err)
	}
}

func TestEmbeddedFieldDominanceMatchesEncodingJSON(t *testing.T) {
	type leaf struct {
		Value string `json:"value"`
	}
	type middle struct{ leaf }
	type shallow struct{ leaf }
	type deep struct{ middle }
	type shallowerWins struct {
		shallow
		deep
	}
	var shallowTarget shallowerWins
	if err := DecodeAuthoring([]byte(`{"value":"selected"}`), &shallowTarget); err != nil {
		t.Fatalf("shallower promoted field was not selected: %v", err)
	}
	if shallowTarget.shallow.Value != "selected" || shallowTarget.deep.Value != "" {
		t.Fatalf("wrong promoted field selected: %#v", shallowTarget)
	}

	type tagged struct {
		Value string `json:"Value"`
	}
	type untagged struct{ Value string }
	type taggedWins struct {
		tagged
		untagged
	}
	var taggedTarget taggedWins
	if err := DecodeAuthoring([]byte(`{"Value":"selected"}`), &taggedTarget); err != nil {
		t.Fatalf("tagged field at equal depth was not selected: %v", err)
	}
	if taggedTarget.tagged.Value != "selected" || taggedTarget.untagged.Value != "" {
		t.Fatalf("tagged dominance selected wrong field: %#v", taggedTarget)
	}

	type first struct {
		Value string
	}
	type second struct {
		Value string
	}
	type ambiguous struct {
		first
		second
	}
	var ambiguousTarget ambiguous
	if err := DecodeAuthoring([]byte(`{"Value":"rejected"}`), &ambiguousTarget); err == nil {
		t.Fatal("DecodeAuthoring accepted a truly ambiguous promoted field")
	}
}

func TestInvalidJSONTagFallsBackToFieldName(t *testing.T) {
	type envelope struct {
		Value string `json:"bad\\name"`
	}
	var target envelope
	if err := DecodeAuthoring([]byte(`{"Value":"x"}`), &target); err != nil {
		t.Fatalf("invalid JSON tag did not fall back to the Go field name: %v", err)
	}
	if target.Value != "x" {
		t.Fatalf("Value=%q, want x", target.Value)
	}
	if err := DecodeAuthoring([]byte(`{"bad\\name":"x"}`), &target); err == nil {
		t.Fatal("invalid JSON tag was treated as a wire field name")
	}
}

func TestOptionOnlyJSONTagDoesNotGainDominance(t *testing.T) {
	type optionOnly struct {
		Value string `json:",omitempty"`
	}
	type explicit struct {
		Value string `json:"Value"`
	}
	type envelope struct {
		optionOnly
		explicit
	}
	var target envelope
	if err := DecodeAuthoring([]byte(`{"Value":"selected"}`), &target); err != nil {
		t.Fatalf("explicit field did not dominate option-only tag: %v", err)
	}
	if target.explicit.Value != "selected" || target.optionOnly.Value != "" {
		t.Fatalf("wrong option-only dominance result: %#v", target)
	}
}
