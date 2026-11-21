package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

const adminRoom = "!space:example.invalid"
const adminChild = "!child:example.invalid"
const adminMarker = "INPUT_SENTINEL_DO_NOT_ECHO"

type adminSpecimen struct {
	ID           string         `json:"id"`
	InputContent map[string]any `json:"input_content,omitempty"`
	JSON         map[string]any `json:"expected_json"`
	Text         string         `json:"expected_text"`
	Exit         int            `json:"exit"`
	Stderr       string         `json:"stderr"`
}

func adminSpecimens(t *testing.T) []adminSpecimen {
	t.Helper()
	b, err := os.ReadFile("testdata/room-admin/records.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Records []adminSpecimen `json:"records"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Records) != 34 {
		t.Fatal("incomplete specimen set")
	}
	return data.Records
}

// This independent consumer checks closed record combinations. It does not
// call production projection, validation, serialization or policy helpers.
func decodeAdminRecord(b []byte) (map[string]any, error) {
	bad := errors.New("closed record violation")
	if !utf8.Valid(b) {
		return nil, bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func() error
	value = func() error {
		tok, err := d.Token()
		if err != nil || tok == nil {
			return bad
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return bad
					}
					s, ok := key.(string)
					if !ok || seen[s] {
						return bad
					}
					seen[s] = true
					if err := value(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := value(); err != nil {
						return err
					}
				}
			default:
				return bad
			}
			if _, err := d.Token(); err != nil {
				return bad
			}
		}
		return nil
	}
	if err := value(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, bad
	}
	var m map[string]any
	d = json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&m) != nil || m == nil {
		return nil, bad
	}
	fields := []string{"type", "version", "operation", "caller", "room_id", "state_type", "state_key", "outcome", "acknowledged", "readback"}
	allowed := map[string]bool{"event_id": true, "content": true}
	for _, k := range fields {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return nil, bad
		}
	}
	for k := range m {
		if !allowed[k] {
			return nil, bad
		}
	}
	for _, k := range []string{"type", "operation", "caller", "room_id", "state_type", "state_key", "outcome", "readback"} {
		if _, ok := m[k].(string); !ok {
			return nil, bad
		}
	}
	ack, ok := m["acknowledged"].(bool)
	if !ok || !adminConsumerVersionOne(m["version"]) {
		return nil, bad
	}
	op, outcome, readback := m["operation"].(string), m["outcome"].(string), m["readback"].(string)
	child := m["type"] == "space-child"
	if child {
		if m["state_type"] != "m.space.child" || !strings.HasPrefix(m["state_key"].(string), "!") || op != "get" && op != "add" && op != "remove" {
			return nil, bad
		}
	} else if m["type"] != "room-name" || m["state_type"] != "m.room.name" || m["state_key"] != "" || op != "get" && op != "set" {
		return nil, bad
	}
	publicID := func(s string, sigil byte) bool {
		if len(s) < 2 || len(s) > 255 || s[0] != sigil || !utf8.ValidString(s) {
			return false
		}
		for _, r := range s {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				return false
			}
		}
		if sigil == '@' {
			local, server, ok := strings.Cut(s[1:], ":")
			return ok && local != "" && server != ""
		}
		return true
	}
	if !publicID(m["caller"].(string), '@') || !publicID(m["room_id"].(string), '!') || child && (!publicID(m["state_key"].(string), '!') || m["state_key"] == m["room_id"]) {
		return nil, bad
	}
	id, hasID := m["event_id"]
	if ack != hasID {
		return nil, bad
	}
	if hasID {
		s, ok := id.(string)
		// The pinned ACK predicate is deliberately opaque after the '$'. It
		// does not inherit the target/caller whitespace and control restrictions.
		if !ok || len(s) < 2 || len(s) > 255 || s[0] != '$' {
			return nil, bad
		}
	}
	content, hasContent := m["content"]
	wantContent := outcome == "PRESENT" || outcome == "EMPTY" || outcome == "VERIFIED"
	if hasContent != wantContent {
		return nil, bad
	}
	if op == "get" {
		if ack || outcome != "PRESENT" && outcome != "EMPTY" && outcome != "ABSENT" && outcome != "UNKNOWN" {
			return nil, bad
		}
		want := map[string]string{"PRESENT": "present", "EMPTY": "present", "ABSENT": "absent", "UNKNOWN": "unavailable"}[outcome]
		if readback != want {
			return nil, bad
		}
	} else {
		switch outcome {
		case "VERIFIED":
			if !ack || readback != "match" {
				return nil, bad
			}
		case "REFUSED":
			if ack || readback != "not-attempted" {
				return nil, bad
			}
		case "UNKNOWN":
			if ack {
				if readback != "absent" && readback != "mismatch" && readback != "unavailable" {
					return nil, bad
				}
			} else if readback != "not-attempted" {
				return nil, bad
			}
		default:
			return nil, bad
		}
	}
	if hasContent {
		c, ok := content.(map[string]any)
		if !ok || len(c) > 1 {
			return nil, bad
		}
		if op == "remove" {
			if len(c) != 0 {
				return nil, bad
			}
		} else if child {
			if raw, exists := c["via"]; exists {
				v, ok := raw.([]any)
				if !ok || len(v) > 16 {
					return nil, bad
				}
				n := 0
				for _, s := range v {
					str, ok := s.(string)
					if !ok || len(str) > 261 || !adminConsumerViaServer(str) {
						return nil, bad
					}
					n += len(str)
				}
				if n > 4096 {
					return nil, bad
				}
				if (outcome == "PRESENT" || outcome == "VERIFIED") && len(v) == 0 || outcome == "EMPTY" && len(v) != 0 {
					return nil, bad
				}
			} else if len(c) != 0 || outcome != "EMPTY" {
				return nil, bad
			}
		} else {
			if raw, exists := c["name"]; exists {
				s, ok := raw.(string)
				if !ok || len(s) > 255 {
					return nil, bad
				}
				for _, r := range s {
					if unicode.IsControl(r) {
						return nil, bad
					}
				}
				if (outcome == "PRESENT" || outcome == "VERIFIED") && s == "" || outcome == "EMPTY" && s != "" {
					return nil, bad
				}
			} else if len(c) != 0 || outcome != "EMPTY" {
				return nil, bad
			}
		}
	}
	return m, nil
}

// Decide the exact decimal value, without rounding and without restricting
// JSON's equivalent integer spellings (1.0, 1e0, 10e-1, ...). JSON syntax has
// already been checked. Large exponents cannot equal the bounded decimal scale.
func adminConsumerVersionOne(value any) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	mantissa, exponent := n.String(), int64(0)
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(mantissa[i+1:], 10, 64)
		if err != nil {
			return false
		}
		mantissa = mantissa[:i]
	}
	integer, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(integer+fraction, "0")
	significant := strings.TrimRight(digits, "0")
	return significant == "1" && exponent == int64(len(fraction))-int64(len(digits)-len(significant))
}

// This independent allowlist preserves the retained structural DNS/IPv4 host
// alphabet and 1..5 decimal port digits, including noncanonical DNS labels and
// ports outside TCP's numeric range. Brackets require a real, unzoned IPv6 IP.
var adminConsumerServerWire = regexp.MustCompile(`^([A-Za-z0-9.-]{1,255}|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?$`)

func adminConsumerViaServer(server string) bool {
	if !adminConsumerServerWire.MatchString(server) {
		return false
	}
	if !strings.HasPrefix(server, "[") {
		return true
	}
	host := server[1:strings.IndexByte(server, ']')]
	return strings.Contains(host, ":") && net.ParseIP(host) != nil
}

type adminRequest struct{ Method, Path, Body string }

// The ledger is a contract oracle over all fixture requests, not a production
// sandbox. Source review must also inspect alternate process/network sinks.
func checkAdminEffects(got, want []adminRequest) error {
	if !reflect.DeepEqual(got, want) {
		return errors.New("request/effect contract violation")
	}
	return nil
}

func adminStatePath(child bool) string {
	typ, key := "m.room.name", ""
	if child {
		typ, key = "m.space.child", adminChild
	}
	return "/_matrix/client/v3/rooms/" + url.PathEscape(adminRoom) + "/state/" + typ + "/" + url.PathEscape(key)
}

func adminPolicy() string {
	return `[{"type":"m.room.create","state_key":"","sender":"@founder:example.invalid","content":{"type":"m.space","room_version":"12"}},{"type":"m.room.member","state_key":"@alice:alpha.invalid","content":{"membership":"join"}},{"type":"m.room.power_levels","state_key":"","content":{"users":{"@alice:alpha.invalid":100},"state_default":50}}]`
}

func adminArgs(child bool, op string) []string {
	args := []string{"room", "name", op, adminRoom}
	if child {
		args = []string{"space", "child", op, adminRoom, "--child", adminChild}
	}
	if op != "get" {
		if child {
			args = append(args, "--expect-space-id", adminRoom, "--expect-child-id", adminChild)
			if op == "add" {
				args = append(args, "--via", "example.invalid", "--via", "EXAMPLE.invalid", "--via", "example.invalid")
			}
		} else {
			args = append(args, "--expect-room-id", adminRoom, "--name", "Example")
		}
	}
	return args
}

type adminCloseBody struct {
	io.Reader
	close func()
}

func (b adminCloseBody) Close() error {
	if b.close != nil {
		b.close()
	}
	return nil
}

// Executes all 34 JSON/text specimens through real App.Run and fake HTTP. It
// proves exact observable results/effects, not live room authority or adoption.
func TestRoomAdminContractRecords(t *testing.T) {
	for _, specimen := range adminSpecimens(t) {
		for _, asJSON := range []bool{true, false} {
			t.Run(specimen.ID+"/json="+strconv.FormatBool(asJSON), func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				child := specimen.JSON["type"] == "space-child"
				op := specimen.JSON["operation"].(string)
				var got, want []adminRequest
				policyPath := "/_matrix/client/v3/rooms/" + url.PathEscape(adminRoom) + "/state"
				if child || op != "get" {
					want = append(want, adminRequest{"GET", policyPath, ""})
				}
				propertyPath := adminStatePath(child)
				stored := "{}"
				putBody := "{}"
				reversePath := "/_matrix/client/v3/rooms/" + url.PathEscape(adminChild) + "/state/m.space.parent/" + url.PathEscape(adminRoom)
				states := map[string]string{reversePath: "reverse-preserved", "/other-room": "other-preserved"}
				if op == "set" {
					putBody = `{"name":"Example"}`
				} else if op == "add" {
					putBody = `{"via":["example.invalid","EXAMPLE.invalid","example.invalid"]}`
				}
				if op == "get" {
					if specimen.InputContent != nil {
						b, _ := json.Marshal(specimen.InputContent)
						stored = string(b)
					} else if c, ok := specimen.JSON["content"]; ok {
						b, _ := json.Marshal(c)
						stored = string(b)
					} else if specimen.JSON["outcome"] == "UNKNOWN" {
						stored = `{"name":null,"via":null,"private":"` + adminMarker + `"}`
					}
					if specimen.JSON["outcome"] != "ABSENT" {
						states[propertyPath] = stored
					}
					want = append(want, adminRequest{"GET", propertyPath, ""})
				} else if !strings.Contains(specimen.ID, "guard-refused") {
					want = append(want, adminRequest{"PUT", propertyPath, putBody})
					if specimen.JSON["acknowledged"] == true {
						want = append(want, adminRequest{"GET", propertyPath, ""})
					}
				}
				initialStates := make(map[string]string, len(states))
				for path, body := range states {
					initialStates[path] = body
				}
				entered := 0
				a.beforeAdminPropertyRequest = func() {
					entered++
					if strings.Contains(specimen.ID, "guard-refused") {
						cancel()
					}
				}
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					var b []byte
					if r.Body != nil {
						b, _ = io.ReadAll(r.Body)
					}
					got = append(got, adminRequest{r.Method, r.URL.EscapedPath(), string(b)})
					if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.Header.Get("Authorization") != "Bearer isolated-alpha" || r.URL.RawQuery != "" {
						t.Fatal("wrong selected authority")
					}
					if r.URL.EscapedPath() == policyPath {
						return propertyCLIReply(r, 200, adminPolicy()), nil
					}
					if r.Method == "PUT" {
						if strings.HasSuffix(specimen.ID, "-refused") {
							return propertyCLIReply(r, 403, `{"errcode":"M_FORBIDDEN","error":"`+adminMarker+`"}`), nil
						}
						states[r.URL.EscapedPath()] = string(b)
						if strings.Contains(specimen.ID, "ack-unknown") {
							return propertyCLIReply(r, 200, `{"event_id":null,"private":"`+adminMarker+`"}`), nil
						}
						stored = string(b)
						return propertyCLIReply(r, 200, `{"event_id":"$event"}`), nil
					}
					if op == "get" && specimen.JSON["outcome"] == "ABSENT" || strings.Contains(specimen.ID, "readback-absent") {
						return propertyCLIReply(r, 404, `{"errcode":"M_NOT_FOUND"}`), nil
					}
					if op == "get" && specimen.JSON["outcome"] == "UNKNOWN" && specimen.InputContent == nil {
						return propertyCLIReply(r, 200, `{"name":null,"via":null,"private":"`+adminMarker+`"}`), nil
					}
					if strings.Contains(specimen.ID, "readback-mismatch") {
						content := map[string]any{}
						if err := json.Unmarshal([]byte(stored), &content); err != nil {
							t.Fatal("invalid stored fixture content")
						}
						content["private"] = adminMarker
						body, _ := json.Marshal(content)
						return propertyCLIReply(r, 200, string(body)), nil
					}
					if strings.Contains(specimen.ID, "readback-unavailable") {
						return nil, errors.New(adminMarker)
					}
					resp := propertyCLIReply(r, 200, stored)
					if strings.Contains(specimen.ID, "final-context") {
						resp.Body = adminCloseBody{strings.NewReader(stored), cancel}
					}
					return resp, nil
				})}
				before := memberFiles(t, f.root)
				args := adminArgs(child, op)
				if asJSON {
					args = append(args, "--json")
				}
				code := a.Run(ctx, args)
				if code != specimen.Exit || f.err.String() != specimen.Stderr {
					t.Fatalf("exit/diagnostic: code%d stderr=%q", code, f.err.String())
				}
				if entered != 1 {
					t.Fatalf("property attempts %d", entered)
				}
				if err := checkAdminEffects(got, want); err != nil {
					t.Fatalf("%v: got=%+v want=%+v", err, got, want)
				}
				if !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
					t.Fatal("local state changed")
				}
				if op == "get" && !reflect.DeepEqual(initialStates, states) {
					t.Fatal("getter changed remote state")
				}
				if states[reversePath] != "reverse-preserved" || states["/other-room"] != "other-preserved" {
					t.Fatal("reverse/other-room sentinel changed")
				}
				if op != "get" && !strings.HasSuffix(specimen.ID, "-refused") && states[propertyPath] != putBody {
					t.Fatal("post-operation selected state differs from fixture effect")
				}
				if asJSON {
					m, err := decodeAdminRecord(f.out.Bytes())
					if err != nil {
						t.Fatalf("consumer: %v: %s", err, f.out.String())
					}
					want := map[string]any{}
					for k, v := range specimen.JSON {
						want[k] = v
					}
					want["caller"] = "@alice:alpha.invalid"
					if !reflect.DeepEqual(m, want) {
						t.Fatalf("record mismatch: got%v want%v", m, want)
					}
				} else if f.out.String() != specimen.Text {
					t.Fatalf("text mismatch: %q", f.out.String())
				}
				if strings.Contains(f.out.String()+f.err.String(), adminMarker) {
					t.Fatal("diagnostic/content leaked marker")
				}
			})
		}
	}
}

// Proves each decoder invariant has an independently rejected plant beside
// accepted specimens. These are detector checks, not CLI/network witnesses.
func TestRoomAdminDecoderPlants(t *testing.T) {
	for _, s := range adminSpecimens(t) {
		b, _ := json.Marshal(s.JSON)
		if _, err := decodeAdminRecord(b); err != nil {
			t.Fatalf("valid %s: %v", s.ID, err)
		}
	}
	base := adminSpecimens(t)[5].JSON
	plants := map[string]func(map[string]any){
		"extra": func(m map[string]any) { m["extra"] = true }, "missing": func(m map[string]any) { delete(m, "caller") }, "null": func(m map[string]any) { m["caller"] = nil },
		"type": func(m map[string]any) { m["type"] = "room-avatar" }, "version": func(m map[string]any) { m["version"] = 2 }, "operation": func(m map[string]any) { m["operation"] = "delete" },
		"false-ack": func(m map[string]any) { m["acknowledged"] = false }, "missing-id": func(m map[string]any) { delete(m, "event_id") }, "unknown-content": func(m map[string]any) { m["outcome"] = "UNKNOWN"; m["readback"] = "unavailable" },
		"missing-content": func(m map[string]any) { delete(m, "content") }, "get-verified": func(m map[string]any) { m["operation"] = "get" }, "readback": func(m map[string]any) { m["readback"] = "future" }, "outcome": func(m map[string]any) { m["outcome"] = "DONE" },
		"event-sigil":    func(m map[string]any) { m["event_id"] = "not-an-event" },
		"event-overlong": func(m map[string]any) { m["event_id"] = "$" + strings.Repeat("e", 255) },
		"name-control":   func(m map[string]any) { m["content"] = map[string]any{"name": "bad\n"} },
		"name-type":      func(m map[string]any) { m["content"] = map[string]any{"name": 7} },
		"self-child": func(m map[string]any) {
			m["type"], m["operation"], m["state_type"], m["state_key"], m["content"] = "space-child", "add", "m.space.child", m["room_id"], map[string]any{"via": []string{"example.invalid"}}
		},
	}
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			plant(m)
			b, _ := json.Marshal(m)
			if _, err := decodeAdminRecord(b); err == nil {
				t.Fatal("decoder accepted planted violation")
			}
		})
	}
	for _, b := range []string{`{"type":null}`, `{"type":"x","type":"y"}`, `[]`, string([]byte{0xff})} {
		if _, err := decodeAdminRecord([]byte(b)); err == nil {
			t.Fatal("invalid wire accepted")
		}
	}
	// JSON spellings with the exact integral value 1 remain compatible. Nearby
	// fractions must not become version 1 through binary floating-point rounding.
	wire, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		spelling string
		valid    bool
	}{
		{"1", true}, {"1.0", true}, {"1e0", true}, {"1E+0", true}, {"10e-1", true},
		{"0.1e1", true}, {"1000e-3", true}, {"1.0000000000000000", true},
		{"1e+00000", true}, {"1.000e-0", true},
		{"1.0000000000000001", false}, {"0.99999999999999999", false},
		{"1.00000000000000000000000000001", false}, {"2", false}, {`"1"`, false},
	} {
		t.Run("raw-version/"+tc.spelling, func(t *testing.T) {
			b := bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":`+tc.spelling), 1)
			if bytes.Equal(b, wire) && tc.spelling != "1" {
				t.Fatal("version plant did not change wire")
			}
			_, err := decodeAdminRecord(b)
			if (err == nil) != tc.valid {
				t.Fatalf("exact version value: spelling %s accepted=%t want=%t", tc.spelling, err == nil, tc.valid)
			}
		})
	}
	var child map[string]any
	for _, s := range adminSpecimens(t) {
		if s.JSON["type"] == "space-child" && s.JSON["operation"] == "add" && s.JSON["outcome"] == "VERIFIED" {
			child = s.JSON
			break
		}
	}
	if child == nil {
		t.Fatal("missing positive child specimen")
	}
	for _, tc := range []struct {
		server string
		valid  bool
	}{
		{"DNS.invalid", true}, {"127.0.0.1", true}, {"127.0.0.1:8448", true},
		{"[2001:db8::1]", true}, {"[2001:db8::1]:8448", true}, {"[::ffff:192.0.2.1]:0", true},
		{"server.invalid:99999", true}, {"server.invalid:00000", true},
		{".", true}, {"-host", true}, {"host-", true}, {"x..y", true},
		{strings.Repeat("h", 255) + ":12345", true},
		{"", false}, {"bad/server", false}, {"bad:port", false}, {"bad host", false},
		{"[127.0.0.1]", false}, {"[fe80::1%zone]", false}, {"[2001:db8::1]junk", false},
		{"server.invalid:", false}, {"server.invalid:123456", false}, {"server.invalid:-1", false},
		{"é.invalid", false}, {"2001:db8::1", false},
	} {
		t.Run("via-grammar/"+tc.server, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range child {
				m[k] = v
			}
			m["content"] = map[string]any{"via": []string{tc.server}}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeAdminRecord(b)
			if (err == nil) != tc.valid {
				t.Fatalf("independent via grammar: accepted=%t want=%t", err == nil, tc.valid)
			}
		})
	}
}

// Invalid input must refuse at the named rule before any credential lookup,
// HTTP attempt or local-state change. Valid neighbors run through App.Run below.
func TestRoomAdminInputRefusals(t *testing.T) {
	name := adminArgs(false, "set")
	add := adminArgs(true, "add")
	replace := func(args []string, old, value string) []string {
		out := append([]string(nil), args...)
		for i, s := range out {
			if s == old {
				out[i] = value
				break
			}
		}
		return out
	}
	cases := []struct {
		name           string
		args           []string
		rule, location string
	}{
		{"unknown", append(append([]string(nil), name...), "--"+adminMarker), "admin_usage", "argv"},
		{"repeat-name", append(append([]string(nil), name...), "--name", adminMarker), "admin_usage", "argv"},
		{"repeat-json", append(append([]string(nil), name...), "--json", "--json"), "admin_usage", "argv"},
		{"json-literal", append(append([]string(nil), name...), "--json="+adminMarker), "admin_usage", "argv"},
		{"extra-operand", append(append([]string(nil), name...), adminMarker), "admin_usage", "argv"},
		{"wrong-verb", replace(name, "set", "add"), "admin_usage", "argv"},
		{"wrong-child-verb", replace(add, "add", "set"), "admin_usage", "argv"},
		{"missing-operand", []string{"room", "name", "get"}, "admin_usage", "argv"},
		{"target-space", replace(name, adminRoom, "!bad "+adminMarker), "admin_selector", "$.target"},
		{"target-control", replace(name, adminRoom, "!bad\n"+adminMarker), "admin_selector", "$.target"},
		{"bad-alias", replace(name, adminRoom, "/"+adminMarker), "admin_selector", "$.target"},
		{"child-control", replace(add, adminChild, "!bad\t"+adminMarker), "admin_selector", "$.child"},
		{"expected", replace(name, "--expect-room-id", "--expect-room-id="+adminMarker), "admin_usage", "argv"},
		{"invalid-expected", []string{"room", "name", "set", adminRoom, "--expect-room-id", adminMarker, "--name", "Example"}, "admin_selector", "$.expected"},
		{"identical-expected", []string{"space", "child", "remove", adminRoom, "--child", adminChild, "--expect-space-id", adminRoom, "--expect-child-id", adminRoom}, "admin_selector", "$.expected"},
		{"empty-name", replace(name, "Example", ""), "admin_name", "$.name"},
		{"long-name", replace(name, "Example", strings.Repeat("a", 256)), "admin_name", "$.name"},
		{"multibyte-over", replace(name, "Example", strings.Repeat("é", 128)), "admin_name", "$.name"},
		{"invalid-utf8", replace(name, "Example", string([]byte{0xff})), "admin_name", "$.name"},
		{"control-name", replace(name, "Example", "\x00"+adminMarker), "admin_name", "$.name"},
		{"bad-via", replace(add, "example.invalid", "/"+adminMarker), "admin_via", "$.via[0]"},
		{"remove-via", append(adminArgs(true, "remove"), "--via", adminMarker), "admin_usage", "argv"},
		{"get-expected", append(adminArgs(false, "get"), "--expect-room-id", adminRoom), "admin_usage", "argv"},
		{"empty-as", append(append([]string(nil), name...), "--as="), "admin_usage", "argv"},
		{"empty-as-separated", append(append([]string(nil), name...), "--as", ""), "admin_usage", "argv"},
		{"empty-as-short-separated", append(append([]string(nil), name...), "-as", ""), "admin_usage", "argv"},
		{"empty-as-child-separated", append(append([]string(nil), add...), "--as", ""), "admin_usage", "argv"},
		{"empty-as-child-short-separated", append(append([]string(nil), add...), "-as", ""), "admin_usage", "argv"},
		{"empty-as-short-equals", append(append([]string(nil), name...), "-as="), "admin_usage", "argv"},
		{"empty-profile-separated", append(append([]string(nil), name...), "--profile", ""), "admin_usage", "argv"},
		{"empty-profile-equals", append(append([]string(nil), name...), "--profile="), "admin_usage", "argv"},
		{"empty-fingerprint-separated", append(append([]string(nil), name...), "--connection-fingerprint", ""), "admin_usage", "argv"},
		{"empty-fingerprint-equals", append(append([]string(nil), name...), "--connection-fingerprint="), "admin_usage", "argv"},
		{"repeated-profile", append(append([]string(nil), name...), "--profile", "alpha", "--profile", "alpha"), "admin_usage", "argv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			stored := `{}`
			// The ambient identity has working credentials and HTTP. An invalid
			// explicit selector must not fall back to that otherwise usable identity.
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/state") {
					return propertyCLIReply(r, 200, adminPolicy()), nil
				}
				if r.Method == "PUT" {
					b, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					stored = string(b)
					return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
				}
				return propertyCLIReply(r, 200, stored), nil
			})}
			before := memberFiles(t, f.root)
			code := a.Run(t.Context(), tc.args)
			if code != ExitUsage || f.err.String() != "RULE "+tc.rule+": "+tc.location+"\n" || f.out.Len() != 0 || calls != 0 || len(f.store.reads) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
				t.Fatalf("wrong refusal code%d calls%d credentialReads%d out%q err%q", code, calls, len(f.store.reads), f.out.String(), f.err.String())
			}
		})
	}
}

// Observe every request and persisted event independently of the production
// writer and its result reconstruction. Unexpected targets remain functional
// so a substituted write reaches this wire/state assertion, not a fake error.
func adminValidInputWrite(t *testing.T, room, key, body string, args []string, pinConnection bool) map[string]any {
	t.Helper()
	f := newNamedFixture(t, true)
	a := f.app()
	if pinConnection {
		binding, err := f.r.Profiles["alpha"].Binding("alice", false)
		if err != nil {
			t.Fatal(err)
		}
		args = append(append([]string(nil), args...), "--connection-fingerprint", binding.Fingerprint())
	}
	typ := "m.room.name"
	if key != "" {
		typ = "m.space.child"
	}
	policyPath := "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/state"
	propertyPath := policyPath + "/" + url.PathEscape(typ) + "/" + url.PathEscape(key)
	otherPath := "/_matrix/client/v3/rooms/" + url.PathEscape("!other:example.invalid") + "/state/" + url.PathEscape(typ) + "/" + url.PathEscape(key)
	reversePath := "/_matrix/client/v3/rooms/" + url.PathEscape(adminChild) + "/state/m.space.parent/" + url.PathEscape(room)
	states := map[string]string{propertyPath: `{}`, otherPath: `{"preserved":"other"}`, reversePath: `{"preserved":"reverse"}`}
	wantStates := map[string]string{propertyPath: body, otherPath: states[otherPath], reversePath: states[reversePath]}
	var got []adminRequest
	want := []adminRequest{{"GET", policyPath, ""}, {"PUT", propertyPath, body}, {"GET", propertyPath, ""}}
	admissions := 0
	a.beforeAdminPropertyRequest = func() { admissions++ }
	a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
		var b []byte
		if r.Body != nil {
			var err error
			b, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
		}
		path := r.URL.EscapedPath()
		got = append(got, adminRequest{r.Method, path, string(b)})
		if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
			t.Fatal("varied input selected authority differs")
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/state") {
			return propertyCLIReply(r, 200, adminPolicy()), nil
		}
		if r.Method == "PUT" {
			states[path] = string(b)
			return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
		}
		return propertyCLIReply(r, 200, states[path]), nil
	})}
	before := memberFiles(t, f.root)
	code := a.Run(t.Context(), args)
	if err := checkAdminEffects(got, want); err != nil {
		t.Fatalf("varied input wire contract: %v: got=%+v want=%+v", err, got, want)
	}
	if !reflect.DeepEqual(states, wantStates) {
		t.Fatal("varied input stored post-state differs")
	}
	if admissions != 1 || len(f.store.reads) != 1 || len(f.store.creates) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
		t.Fatal("varied input admission/credential/local-state contract differs")
	}
	if code != ExitOK || f.err.Len() != 0 {
		t.Fatalf("valid input exit%d %s", code, f.err.String())
	}
	m, err := decodeAdminRecord(f.out.Bytes())
	if err != nil || m["outcome"] != "VERIFIED" || m["room_id"] != room || m["state_type"] != typ || m["state_key"] != key || m["event_id"] != "$ok" {
		t.Fatalf("valid input result mismatch: %s consumer=%v", f.out.String(), err)
	}
	return m
}

// Literal values and every varied via array must reach the exact selected
// event, with an independently encoded body and persisted state, not just echo.
func TestRoomAdminValidInputs(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 255), strings.Repeat("é", 127) + "a", " literal spaces ", "--profile", "-as", "--as", "--human", "--connection-fingerprint"} {
		t.Run(fmt.Sprintf("name-%x", []byte(name)[:min(5, len(name))]), func(t *testing.T) {
			args := []string{"room", "name", "set", "!x", "--expect-room-id", "!x", "--name", name, "--json"}
			body, err := json.Marshal(struct {
				Name string `json:"name"`
			}{name})
			if err != nil {
				t.Fatal(err)
			}
			m := adminValidInputWrite(t, "!x", "", string(body), args, false)
			if m["content"].(map[string]any)["name"] != name {
				t.Fatal("literal valid name lost")
			}
		})
	}
	for _, globals := range [][]string{nil, {"--as", "alice"}, {"-as", "alice"}, {"--as=alice"}, {"-as=alice"}, {"--profile", "alpha"}, {"--profile=alpha"}} {
		t.Run("global-neighbor/"+strings.Join(globals, "_"), func(t *testing.T) {
			args := append([]string{"room", "name", "set", "!x", "--expect-room-id", "!x", "--name", "Example", "--json"}, globals...)
			adminValidInputWrite(t, "!x", "", `{"name":"Example"}`, args, false)
		})
	}
	t.Run("global-neighbor/fingerprint", func(t *testing.T) {
		adminValidInputWrite(t, "!x", "", `{"name":"Example"}`, []string{"room", "name", "set", "!x", "--expect-room-id", "!x", "--name", "Example", "--json"}, true)
	})
	viaSets := [][]string{{"DNS.invalid", "127.0.0.1:8448", "[2001:db8::1]:8448", "DNS.invalid"}, {strings.Repeat("h", 255) + ":12345"}}
	viaSets = append(viaSets, []string{"127.0.0.1", "[::1]", "[::ffff:192.0.2.1]:0", "server.invalid:99999", "server.invalid:00000", ".", "-host", "host-", "x..y"})
	full := make([]string, 16)
	for i := range full {
		full[i] = strings.Repeat("h", 250) + ":12345"
	}
	viaSets = append(viaSets, full)
	for n, via := range viaSets {
		t.Run(fmt.Sprintf("via-%d", n), func(t *testing.T) {
			args := adminArgs(true, "remove")
			args[2] = "add"
			for _, server := range via {
				args = append(args, "--via", server)
			}
			args = append(args, "--json")
			body, err := json.Marshal(struct {
				Via []string `json:"via"`
			}{append([]string(nil), via...)})
			if err != nil {
				t.Fatal(err)
			}
			m := adminValidInputWrite(t, adminRoom, adminChild, string(body), args, false)
			b, _ := json.Marshal(m["content"].(map[string]any)["via"])
			want, _ := json.Marshal(via)
			if !bytes.Equal(b, want) {
				t.Fatal("via order/case/duplicates lost")
			}
		})
	}
}

// All power cases use actual strict snapshot decoding. A valid policy below
// the concrete threshold must refuse without PUT beside an override success.
// Child rows pin the concrete m.space.child gate itself: evaluating a child
// write against m.room.name power, skipping the child refusal, clamping an
// override to state_default or requiring strictly more than the required
// level must each fail at least one row beside its allowed neighbour.
func TestRoomAdminPowerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, body, rule string
		child            bool
		op               string
	}{
		{"concrete-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.room.power_levels":0,"m.room.name":80}`, 1), "admin_power", false, ""},
		{"concrete-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.room.power_levels":0,"m.room.name":40}`, 1), "", false, ""},
		{"inherited-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events_default":0`, 1), "admin_power", false, ""},
		{"inherited-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":80},"state_default":75`, 1), "", false, ""},
		// Claim: a child override above the caller refuses add and remove.
		{"child-override-refusal-add", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":50,"events":{"m.space.child":80}`, 1), "admin_power", true, "add"},
		{"child-override-refusal-remove", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":50,"events":{"m.space.child":80}`, 1), "admin_power", true, "remove"},
		// Claim: a child override below caller and default admits add/remove.
		{"child-override-allowed-add", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.space.child":40,"m.room.name":100}`, 1), "", true, "add"},
		{"child-override-allowed-remove", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.space.child":40,"m.room.name":100}`, 1), "", true, "remove"},
		// Claim: name power does not admit a child write, in either direction.
		{"child-cross-name-low-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":50,"events":{"m.room.name":0,"m.space.child":80}`, 1), "admin_power", true, "add"},
		{"child-inherited-name-low-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.room.name":0}`, 1), "admin_power", true, "add"},
		{"child-cross-child-low-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.room.name":80,"m.space.child":0}`, 1), "", true, "add"},
		{"name-cross-child-low-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":75,"events":{"m.room.name":80,"m.space.child":0}`, 1), "admin_power", false, ""},
		// Claim: an override above state_default wins, it is not clamped down.
		{"name-override-above-default-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":60},"state_default":50,"events":{"m.room.name":80}`, 1), "admin_power", false, ""},
		// Claim: caller level exactly at the required level is admitted.
		{"name-equal-override-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":75},"state_default":50,"events":{"m.room.name":75}`, 1), "", false, ""},
		{"name-equal-inherited-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":75},"state_default":75`, 1), "", false, ""},
		{"child-equal-override-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":75},"state_default":50,"events":{"m.space.child":75}`, 1), "", true, "add"},
		{"child-equal-inherited-allowed", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":75},"state_default":75`, 1), "", true, "add"},
		// Claim: caller one below required (74 vs 75) refuses with no PUT,
		// for name and child, via override and via state_default alike.
		{"name-required-minus-one-override-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":74},"state_default":50,"events":{"m.room.name":75}`, 1), "admin_power", false, ""},
		{"name-required-minus-one-inherited-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":74},"state_default":75`, 1), "admin_power", false, ""},
		{"child-required-minus-one-override-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":74},"state_default":50,"events":{"m.space.child":75}`, 1), "admin_power", true, "add"},
		{"child-required-minus-one-inherited-refusal", strings.Replace(adminPolicy(), `"users":{"@alice:alpha.invalid":100},"state_default":50`, `"users":{"@alice:alpha.invalid":74},"state_default":75`, 1), "admin_power", true, "add"},
		// Claim: with no power_levels, state_default 50 fails a non-creator.
		{"nopower-noncreator-refusal", `[{"type":"m.room.create","state_key":"","sender":"@founder:example.invalid","content":{"type":"m.space","room_version":"12"}},{"type":"m.room.member","state_key":"@alice:alpha.invalid","content":{"membership":"join"}}]`, "admin_power", false, ""},
		{"nopower-creator-allowed", `[{"type":"m.room.create","state_key":"","sender":"@alice:alpha.invalid","content":{"type":"m.space","room_version":"12"}},{"type":"m.room.member","state_key":"@alice:alpha.invalid","content":{"membership":"join"}}]`, "", false, ""},
		{"left", strings.Replace(adminPolicy(), `"membership":"join"`, `"membership":"leave"`, 1), "admin_policy", true, ""},
		{"bad-version", strings.Replace(adminPolicy(), `"room_version":"12"`, `"room_version":"13"`, 1), "admin_policy", false, ""},
		{"numeric-string10", strings.Replace(strings.Replace(adminPolicy(), `"room_version":"12"`, `"room_version":"10","creator":"@founder:example.invalid"`, 1), `:100`, `:"100"`, 1), "admin_policy", false, ""},
		{"numeric-string9", strings.Replace(strings.Replace(adminPolicy(), `"room_version":"12"`, `"room_version":"9","creator":"@founder:example.invalid"`, 1), `:100`, `:"100"`, 1), "", false, ""},
		{"nearcase-event", strings.Replace(adminPolicy(), `"state_default":50`, `"state_default":50,"events":{"M.ROOM.NAME":0}`, 1), "admin_policy", false, ""},
		{"malformed-unrelated", strings.Replace(adminPolicy(), `"state_default":50`, `"state_default":50,"events":{"other":1.5}`, 1), "admin_policy", false, ""},
		{"nonspace", strings.Replace(adminPolicy(), `"type":"m.space"`, `"type":"ordinary"`, 1), "admin_space", true, ""},
		{"nearcase-space", strings.Replace(adminPolicy(), `"type":"m.space"`, `"Type":"m.space"`, 1), "admin_space", true, ""},
		{"duplicate", strings.TrimSuffix(adminPolicy(), "]") + `,{"type":"m.room.create","state_key":"","content":{"type":"m.space"}}]`, "admin_space", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			puts, gets := 0, 0
			stored := ""
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/state") {
					gets++
					return propertyCLIReply(r, 200, tc.body), nil
				}
				if r.Method == "PUT" {
					puts++
					b, _ := io.ReadAll(r.Body)
					stored = string(b)
					return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
				}
				return propertyCLIReply(r, 200, stored), nil
			})}
			op := tc.op
			if op == "" {
				op = "set"
				if tc.child {
					op = "add"
				}
			}
			code := a.Run(t.Context(), append(adminArgs(tc.child, op), "--json"))
			if gets != 1 {
				t.Fatal("policy not a single observation")
			}
			if tc.rule != "" {
				loc := "$.policy"
				if tc.rule == "admin_space" {
					loc = "$.space"
				}
				if code != 1 || puts != 0 || f.out.Len() != 0 || f.err.String() != "RULE "+tc.rule+": "+loc+"\n" {
					t.Fatalf("policy refusal code%d puts%d out%q err%q", code, puts, f.out.String(), f.err.String())
				}
			} else {
				if code != 0 || puts != 1 {
					t.Fatalf("valid policy code%d puts%d %s", code, puts, f.err.String())
				}
			}
		})
	}
}
