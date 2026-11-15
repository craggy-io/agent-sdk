package jschema_test

import (
	"encoding/json"
	"testing"

	"github.com/craggy-io/agent-sdk/internal/jschema"
)

func TestValidate(t *testing.T) {
	schema := json.RawMessage(`{
	  "type":"object",
	  "required":["name","count"],
	  "properties":{
	    "name":{"type":"string"},
	    "count":{"type":"integer"},
	    "tags":{"type":"array","items":{"type":"string"}},
	    "mode":{"enum":["a","b",3]},
	    "nested":{"type":"object","properties":{"x":{"type":"boolean"}},"additionalProperties":false}
	  },
	  "additionalProperties":false
	}`)
	cases := []struct {
		name string
		doc  string
		ok   bool
	}{
		{"valid", `{"name":"n","count":3,"tags":["a"],"mode":3,"nested":{"x":true}}`, true},
		{"missing required", `{"name":"n"}`, false},
		{"integer not float", `{"name":"n","count":3.5}`, false},
		{"integer accepts number type", `{"name":"n","count":3}`, true},
		{"wrong scalar type", `{"name":7,"count":3}`, false},
		{"items element type", `{"name":"n","count":1,"tags":["a",2]}`, false},
		{"enum miss", `{"name":"n","count":1,"mode":"c"}`, false},
		{"additionalProperties false top", `{"name":"n","count":1,"extra":1}`, false},
		{"additionalProperties false nested", `{"name":"n","count":1,"nested":{"y":1}}`, false},
		{"not an object", `[1,2]`, false},
		{"invalid doc json", `{"name":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := jschema.Validate(schema, json.RawMessage(tc.doc))
			if (err == nil) != tc.ok {
				t.Fatalf("Validate(%s) err=%v, want ok=%v", tc.doc, err, tc.ok)
			}
		})
	}
	if err := jschema.Validate(nil, json.RawMessage(`anything`)); err != nil {
		t.Fatalf("empty schema should accept, got %v", err)
	}
	if err := jschema.Validate(json.RawMessage(`{"type":["string","null"]}`), json.RawMessage(`null`)); err != nil {
		t.Fatalf("type union: %v", err)
	}
	if err := jschema.Validate(json.RawMessage(`{bad`), json.RawMessage(`{}`)); err == nil {
		t.Fatal("invalid schema must error")
	}
}

// §5.2: arguments double-encoded as a JSON string are decoded once.
func TestDecodeArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain object", `{"a":1}`, `{"a":1}`, true},
		{"double encoded object", `"{\"a\":1}"`, `{"a":1}`, true},
		{"double encoded array", `"[1,2]"`, `[1,2]`, true},
		{"plain string not json inside", `"hello"`, `"hello"`, false},
		{"invalid json", `{"a":`, `{"a":`, false},
		{"empty", ``, ``, false},
		{"number", `42`, `42`, true},
		{"only decoded once", `"\"{\\\"a\\\":1}\""`, `"{\"a\":1}"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := jschema.DecodeArgs(json.RawMessage(tc.in))
			if ok != tc.ok || string(got) != tc.want {
				t.Fatalf("DecodeArgs(%s) = (%s, %v), want (%s, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeStructuredArgs(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"argv":{"type":["null","array"],"items":{"type":"string"}},"overrides":{"type":["null","object"],"properties":{"network":{"type":"string"}}},"nested":{"type":"array","items":{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"}}}}},"command":{"type":"string"},"free":{}}}`)
	in := json.RawMessage(`{"argv":"[\"env\",\"\"]","overrides":"{\"network\":\"egress\"}","nested":"[{\"ids\":\"[\\\"a\\\"]\"}]","command":"[\"keep\"]","free":"{\"keep\":true}"}`)
	got := jschema.NormalizeStructuredArgs(schema, in)
	want := json.RawMessage(`{"argv":["env",""],"overrides":{"network":"egress"},"nested":[{"ids":["a"]}],"command":"[\"keep\"]","free":"{\"keep\":true}"}`)
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(t, a)) != string(mustJSON(t, b)) {
		t.Fatalf("got %s, want %s", got, want)
	}
	if err := jschema.Validate(schema, got); err != nil {
		t.Fatal(err)
	}
	bad := json.RawMessage(`{"argv":"not JSON"}`)
	if string(jschema.NormalizeStructuredArgs(schema, bad)) != string(bad) {
		t.Fatal("malformed structured string changed")
	}
	if err := jschema.Validate(schema, jschema.NormalizeStructuredArgs(schema, bad)); err == nil {
		t.Fatal("malformed structured string passed validation")
	}
	union := json.RawMessage(`{"type":"object","properties":{"value":{"anyOf":[{"type":"object"},{"type":"string"}]}}}`)
	encoded := json.RawMessage(`{"value":"{\"literal\":true}"}`)
	if string(jschema.NormalizeStructuredArgs(union, encoded)) != string(encoded) {
		t.Fatal("a field that accepts strings was coerced")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
