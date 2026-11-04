// Package jschema is a small JSON Schema validator supporting the subset the
// loop needs (§5.2, §4.3 complete): type, required, properties, items, enum,
// additionalProperties:false. It also decodes double-encoded tool arguments.
package jschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// DecodeArgs returns usable tool arguments (§5.2). If raw is a JSON string
// whose contents are themselves valid JSON (double-encoded arguments), it is
// decoded exactly once. ok is false when raw is not valid JSON at all, or is
// a JSON string whose contents are not valid JSON.
func DecodeArgs(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return raw, false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return raw, false
		}
		inner := bytes.TrimSpace([]byte(s))
		if len(inner) == 0 || !json.Valid(inner) {
			return raw, false
		}
		return json.RawMessage(inner), true
	}
	return json.RawMessage(trimmed), true
}

func decodeUseNumber(raw json.RawMessage) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Validate checks doc against schema. A nil/empty schema accepts everything.
func Validate(schema, doc json.RawMessage) error {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	sv, err := decodeUseNumber(schema)
	if err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	dv, err := decodeUseNumber(doc)
	if err != nil {
		return fmt.Errorf("invalid document: %w", err)
	}
	return validateValue(sv, dv, "$")
}

func validateValue(schema, doc any, at string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		if b, isBool := schema.(bool); isBool {
			if !b {
				return fmt.Errorf("%s: not allowed", at)
			}
			return nil
		}
		return nil // non-object schema: nothing to check in our subset
	}
	if enum, ok := s["enum"].([]any); ok {
		if !enumContains(enum, doc) {
			return fmt.Errorf("%s: value %s not in enum", at, canonical(doc))
		}
	}
	if t, ok := s["type"]; ok {
		if err := checkType(t, doc, at); err != nil {
			return err
		}
	}
	switch doc := doc.(type) {
	case map[string]any:
		return validateObject(s, doc, at)
	case []any:
		if items, ok := s["items"]; ok {
			for i, el := range doc {
				if err := validateValue(items, el, at+"["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateObject(s map[string]any, doc map[string]any, at string) error {
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := doc[name]; !present {
				return fmt.Errorf("%s: missing required property %q", at, name)
			}
		}
	}
	props, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic first-error
	for _, k := range keys {
		if ps, ok := props[k]; ok {
			if err := validateValue(ps, doc[k], at+"."+k); err != nil {
				return err
			}
			continue
		}
		switch ap := s["additionalProperties"].(type) {
		case bool:
			if !ap {
				return fmt.Errorf("%s: additional property %q not allowed", at, k)
			}
		case map[string]any:
			if err := validateValue(ap, doc[k], at+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeOf(v any) string {
	switch n := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func checkType(t, doc any, at string) error {
	got := typeOf(doc)
	matches := func(want string) bool {
		return want == got || (want == "number" && got == "integer")
	}
	switch t := t.(type) {
	case string:
		if !matches(t) {
			return fmt.Errorf("%s: expected type %s, got %s", at, t, got)
		}
	case []any:
		for _, w := range t {
			if s, ok := w.(string); ok && matches(s) {
				return nil
			}
		}
		return fmt.Errorf("%s: expected one of %v, got %s", at, t, got)
	}
	return nil
}

func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func enumContains(enum []any, doc any) bool {
	want := canonical(doc)
	for _, e := range enum {
		if canonical(e) == want {
			return true
		}
	}
	return false
}
