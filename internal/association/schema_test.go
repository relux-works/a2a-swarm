package association

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// A test-only evaluator for the closed vocabulary used by the two checked-in
// schemas. Unsupported keywords fail schema loading, so schema changes cannot
// silently weaken these checks. It has no external dependency or network refs.
type testSchema struct{ root map[string]any }

func loadTestSchema(t *testing.T, name string) testSchema {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name + ".schema.json")
	if e != nil {
		t.Fatal(e)
	}
	var root map[string]any
	if e = json.Unmarshal(b, &root); e != nil {
		t.Fatal(e)
	}
	if e = checkSchemaVocabulary(root); e != nil {
		t.Fatal(e)
	}
	return testSchema{root}
}

func checkSchemaVocabulary(s map[string]any) error {
	for k, v := range s {
		switch k {
		case "$schema", "$comment", "$ref", "type", "const", "enum", "required", "additionalProperties", "minimum", "maximum", "minLength", "maxLength", "pattern":
		case "$defs", "properties":
			for _, child := range v.(map[string]any) {
				if e := checkSchemaVocabulary(child.(map[string]any)); e != nil {
					return e
				}
			}
		case "anyOf", "oneOf", "allOf":
			for _, child := range v.([]any) {
				if e := checkSchemaVocabulary(child.(map[string]any)); e != nil {
					return e
				}
			}
		case "if", "then", "else":
			if e := checkSchemaVocabulary(v.(map[string]any)); e != nil {
				return e
			}
		default:
			return fmt.Errorf("unsupported schema keyword %s", k)
		}
	}
	return nil
}

func (s testSchema) validate(b []byte) error {
	var v any
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	return s.node(s.root, v)
}

func (s testSchema) node(n map[string]any, v any) error {
	fail := func() error { return fmt.Errorf("schema constraint failed") }
	if ref, ok := n["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/$defs/") {
			return fmt.Errorf("nonlocal schema ref")
		}
		child, ok := s.root["$defs"].(map[string]any)[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			return fmt.Errorf("missing schema ref")
		}
		if e := s.node(child, v); e != nil {
			return e
		}
	}
	if c, ok := n["const"]; ok && !reflect.DeepEqual(v, c) {
		return fail()
	}
	if choices, ok := n["enum"].([]any); ok {
		found := false
		for _, c := range choices {
			found = found || reflect.DeepEqual(v, c)
		}
		if !found {
			return fail()
		}
	}
	if typ, ok := n["type"].(string); ok {
		valid := false
		switch typ {
		case "null":
			valid = v == nil
		case "object":
			_, valid = v.(map[string]any)
		case "string":
			_, valid = v.(string)
		case "boolean":
			_, valid = v.(bool)
		case "integer":
			f, ok := v.(float64)
			valid = ok && f == float64(int64(f))
		default:
			return fmt.Errorf("unsupported schema type %s", typ)
		}
		if !valid {
			return fail()
		}
	}
	if m, ok := v.(map[string]any); ok {
		if required, ok := n["required"].([]any); ok {
			for _, k := range required {
				if _, ok := m[k.(string)]; !ok {
					return fail()
				}
			}
		}
		props, _ := n["properties"].(map[string]any)
		for k, x := range m {
			if child, ok := props[k].(map[string]any); ok {
				if e := s.node(child, x); e != nil {
					return fmt.Errorf("property %s: %w", k, e)
				}
			} else if n["additionalProperties"] == false {
				return fail()
			}
		}
	}
	if x, ok := v.(string); ok {
		length := float64(utf8.RuneCountInString(x))
		if min, ok := n["minLength"].(float64); ok && length < min {
			return fail()
		}
		if max, ok := n["maxLength"].(float64); ok && length > max {
			return fail()
		}
		if p, ok := n["pattern"].(string); ok {
			matched, e := regexp.MatchString(p, x)
			if e != nil {
				return e
			}
			if !matched {
				return fail()
			}
		}
	}
	if x, ok := v.(float64); ok {
		if min, ok := n["minimum"].(float64); ok && x < min {
			return fail()
		}
		if max, ok := n["maximum"].(float64); ok && x > max {
			return fail()
		}
	}
	for _, kind := range []string{"anyOf", "oneOf", "allOf"} {
		if children, ok := n[kind].([]any); ok {
			matched := 0
			for _, c := range children {
				if s.node(c.(map[string]any), v) == nil {
					matched++
				}
			}
			if kind == "anyOf" && matched == 0 || kind == "oneOf" && matched != 1 || kind == "allOf" && matched != len(children) {
				return fail()
			}
		}
	}
	if cond, ok := n["if"].(map[string]any); ok {
		branch := "else"
		if s.node(cond, v) == nil {
			branch = "then"
		}
		if child, ok := n[branch].(map[string]any); ok {
			return s.node(child, v)
		}
	}
	return nil
}

func checkPublicSchema(t *testing.T, b []byte, code int) {
	t.Helper()
	if e := loadTestSchema(t, "public-output").validate(b); e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if e := json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	want, ok := map[string]int{"match": 0, "mismatch": 1, "unavailable": 2}[m["result"].(string)]
	if !ok || code != want {
		t.Fatalf("schema result/exit mapping: %d want %d", code, want)
	}
}

func TestAssociationSchemaCausalControls(t *testing.T) {
	// These are schema negatives, independent of implementation struct decoding.
	private := loadTestSchema(t, "private-input")
	public := loadTestSchema(t, "public-output")
	if e := private.validate([]byte(`{}`)); e == nil {
		t.Fatal("private required fields ignored")
	}
	if e := public.validate([]byte(`{}`)); e == nil {
		t.Fatal("public required fields ignored")
	}
	ms := int64(1800000000000)
	b, code := encode(refusal("input_untrusted", &ms))
	checkPublicSchema(t, b, code)
	for _, kind := range []string{"extra", "enum", "nullability", "result-reason", "time", "integer", "digest", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			var m map[string]any
			if e := json.Unmarshal(b, &m); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "extra":
				m["raw_session"] = "SYNTHETIC_PRIVATE"
			case "enum":
				m["reason"] = "invented_reason"
			case "nullability":
				m["stored"] = map[string]any{}
			case "result-reason":
				m["result"] = "match"
			case "time":
				m["reason"] = "clock_unavailable"
			case "integer":
				m["observed_at_ms"] = 1.5
			case "digest":
				m["launch_intent_sha256"] = strings.Repeat("a", 64)
			case "stopped":
				m["reason"] = "binding_stopped"
				m["launch_intent_sha256"] = strings.Repeat("a", 64)
				m["stored"] = map[string]any{"generation": strings.Repeat("a", 32), "consumer_pid": 41001, "consumer_birth_sha256": strings.Repeat("b", 64), "owner_pid": 42001, "owner_birth_sha256": strings.Repeat("c", 64), "runtime": "codex", "stopped": true, "stopped_reason": nil}
			}
			bad, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			if e = public.validate(bad); e == nil {
				t.Fatal("schema accepted planted invalid output")
			}
		})
	}
	// A new unhandled assertion keyword must be an explicit test failure.
	if e := checkSchemaVocabulary(map[string]any{"not": map[string]any{}}); e == nil {
		t.Fatal("unknown schema vocabulary ignored")
	}
}
