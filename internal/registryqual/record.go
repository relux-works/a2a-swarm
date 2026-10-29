// Package registryqual gathers evidence for qualifying a new Claude Code
// session-registry version: it describes the caller's own live registry
// record by keys and JSON types only, and derives a synthetic fixture whose
// identity values are fixed placeholders and whose protocol values come from
// a reviewed vocabulary. It never changes the qualified-version allowlist.
package registryqual

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const maxRecord = 64 << 10

// Field is one top-level registry key and its JSON type, never its value.
type Field struct {
	Key  string
	Type string
}

type member struct {
	key   string
	value json.RawMessage
}

// ReadRecord reads the caller's own registry record: a regular, non-symlink
// file of at most 64 KiB, bounded on the opened descriptor and the read.
func ReadRecord(path string) ([]byte, error) { return readRegular(path, "record", maxRecord) }

// members decodes one top-level JSON object in key order and refuses
// duplicate keys and trailing data.
func members(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("record is not a JSON object")
	}
	var out []member
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, errors.New("record is not valid JSON")
		}
		k := t.(string)
		if seen[k] {
			return nil, fmt.Errorf("record has duplicate key %q", k)
		}
		seen[k] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("record is not valid JSON")
		}
		out = append(out, member{k, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, errors.New("record is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("record has trailing data")
	}
	return out, nil
}

func jsonType(v json.RawMessage) string {
	switch t := bytes.TrimSpace(v); {
	case len(t) == 0:
		return "invalid"
	case t[0] == '"':
		return "string"
	case t[0] == '{':
		return "object"
	case t[0] == '[':
		var items []json.RawMessage
		if json.Unmarshal(t, &items) != nil {
			return "invalid"
		}
		if len(items) == 0 {
			return "array(empty)"
		}
		kinds := map[string]bool{}
		for _, it := range items {
			kinds[jsonType(it)] = true
		}
		var ks []string
		for k := range kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return "array<" + strings.Join(ks, "|") + ">"
	case string(t) == "true" || string(t) == "false":
		return "bool"
	case string(t) == "null":
		return "null"
	default:
		return "number"
	}
}

// Manifest lists the record's top-level keys in order with their types.
func Manifest(data []byte) ([]Field, error) {
	ms, err := members(data)
	if err != nil {
		return nil, err
	}
	out := make([]Field, len(ms))
	for i, m := range ms {
		out[i] = Field{m.key, jsonType(m.value)}
	}
	return out, nil
}

// Every key the fixture knows. Identity keys get a fixed synthetic value of
// the captured type. Protocol keys keep their captured value only when it is
// one of the reviewed values below; anything else, however short, could be a
// private token and refuses the fixture until a reviewed change admits it.
var (
	syntheticValues = map[string]string{
		"pid":                 `1234`,
		"sessionId":           `"11111111-1111-4111-8111-111111111111"`,
		"cwd":                 `"/synthetic"`,
		"startedAt":           `1793016001000`,
		"procStart":           `"Mon Oct 26 12:00:00 2026"`,
		"tmux":                `"synthetic:@0.%1"`,
		"messagingSocketPath": `"/synthetic/not-opened"`,
		"name":                `"synthetic"`,
		"nameSince":           `1793016001000`,
		"updatedAt":           `1793016002000`,
		"statusUpdatedAt":     `1793016002000`,
		"bridgeSessionId":     `"22222222-2222-4222-8222-222222222222"`,
	}
	versionRE      = regexp.MustCompile(`^"[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}"$`)
	protocolRE     = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
	reviewedValues = map[string]func(json.RawMessage) bool{
		"version":      func(v json.RawMessage) bool { return versionRE.Match(bytes.TrimSpace(v)) },
		"peerProtocol": func(v json.RawMessage) bool { return protocolRE.Match(bytes.TrimSpace(v)) },
		"peerFeatures": oneOfEach("notify_idle", "reply_across_default_dirs", "artifact_yield"),
		"kind":         oneOf("interactive"),
		"entrypoint":   oneOf("cli"),
		"pidDomain":    oneOf("darwin", "linux"),
		"nameSource":   oneOf("derived", "user"),
		"status":       oneOf("idle", "busy", "shell"),
	}
)

func oneOf(allowed ...string) func(json.RawMessage) bool {
	return func(v json.RawMessage) bool {
		var s string
		return json.Unmarshal(v, &s) == nil && slices.Contains(allowed, s)
	}
}

func oneOfEach(allowed ...string) func(json.RawMessage) bool {
	return func(v json.RawMessage) bool {
		var items []string
		if t := bytes.TrimSpace(v); len(t) == 0 || t[0] != '[' || json.Unmarshal(t, &items) != nil {
			return false // null and other non-arrays are not reviewed values
		}
		for _, it := range items {
			if !slices.Contains(allowed, it) {
				return false
			}
		}
		return true
	}
}

// Fixture returns the record with identity values replaced by synthetic ones,
// keeping key order and types. It refuses unknown keys, a non-empty
// formerNames history, a changed identity type and protocol values outside
// the reviewed vocabulary, naming the field but never its value.
func Fixture(data []byte) ([]byte, error) {
	ms, err := members(data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	var refused []string
	for i, m := range ms {
		v := m.value
		switch synth, ok := syntheticValues[m.key]; {
		case ok:
			if jsonType(json.RawMessage(synth)) != jsonType(v) {
				refused = append(refused, m.key+" (unexpected type "+jsonType(v)+")")
			}
			v = json.RawMessage(synth)
		case reviewedValues[m.key] != nil:
			if !reviewedValues[m.key](v) {
				refused = append(refused, m.key+" (value outside the reviewed vocabulary)")
			}
		case m.key == "formerNames":
			if jsonType(v) != "array(empty)" {
				refused = append(refused, "formerNames (non-empty history is not synthesised)")
			}
		default:
			refused = append(refused, m.key+" (unknown key)")
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	if len(refused) > 0 {
		return nil, fmt.Errorf("fixture refused: %s", strings.Join(refused, ", "))
	}
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return append(out.Bytes(), '\n'), nil
}
