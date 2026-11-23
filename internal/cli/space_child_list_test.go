package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func TestSpaceChildListHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &App{Stdout: &stdout, Stderr: &stderr}
	if code := a.Run(context.Background(), []string{"help"}); code != ExitOK || stderr.Len() != 0 {
		t.Fatalf("help exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "space child get|add|remove SPACE --child CHILD | space child list SPACE [--json]") {
		t.Fatalf("list command missing from help: %s", stdout.String())
	}
}

func spaceChildEvent(id string, content map[string]any) map[string]any {
	return map[string]any{"type": "m.space.child", "state_key": id, "content": content}
}

func spaceChildStateBody(children ...map[string]any) string {
	events := []any{
		map[string]any{"type": "m.room.create", "state_key": "", "content": map[string]any{"type": "m.space"}},
		map[string]any{"type": "m.room.member", "state_key": "@alice:alpha.invalid", "content": map[string]any{"membership": "join"}},
	}
	for _, child := range children {
		events = append(events, child)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func runSpaceChildList(t *testing.T, ctx context.Context, args []string, status int, body string, alter func(*http.Response)) (*namedFixture, int, []adminRequest) {
	t.Helper()
	f := newNamedFixture(t, true)
	before := memberFiles(t, f.root)
	a := f.app()
	var requests []adminRequest
	a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
		var requestBody string
		if r.Body != nil {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			requestBody = string(b)
		}
		requests = append(requests, adminRequest{Method: r.Method, Path: r.URL.EscapedPath(), Body: requestBody})
		resp := propertyCLIReply(r, status, body)
		if alter != nil {
			alter(resp)
		}
		return resp, nil
	})}
	code := a.Run(ctx, args)
	if !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
		t.Fatal("list changed local state")
	}
	return f, code, requests
}

func assertSpaceChildListOnlyRead(t *testing.T, requests []adminRequest) {
	t.Helper()
	want := []adminRequest{{Method: http.MethodGet, Path: "/_matrix/client/v3/rooms/" + url.PathEscape(adminRoom) + "/state"}}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("unexpected requests/effects: got=%+v want=%+v", requests, want)
	}
}

func listObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		t.Fatalf("invalid object %s: %v", raw, err)
	}
	return object
}

func exactListFields(t *testing.T, object map[string]json.RawMessage, names ...string) {
	t.Helper()
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	if len(object) != len(want) {
		t.Fatalf("fields differ: got=%v want=%v", object, names)
	}
	for name := range object {
		if !want[name] {
			t.Fatalf("unexpected field %q in %v", name, object)
		}
	}
}

func rawString(t *testing.T, object map[string]json.RawMessage, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(object[key], &value); err != nil {
		t.Fatalf("field %s is not a string: %s (%v)", key, object[key], err)
	}
	return value
}

func rawRows(t *testing.T, output []byte) []map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(output, &top); err != nil {
		t.Fatalf("invalid result: %v: %s", err, output)
	}
	exactListFields(t, top, "type", "version", "operation", "caller", "room_id", "state_type", "children")
	if rawString(t, top, "type") != "space-child-list" || rawString(t, top, "operation") != "list" || rawString(t, top, "caller") != "@alice:alpha.invalid" || rawString(t, top, "state_type") != "m.space.child" || rawString(t, top, "room_id") != adminRoom {
		t.Fatalf("wrong result header: %s", output)
	}
	var version int
	if err := json.Unmarshal(top["version"], &version); err != nil || version != 1 {
		t.Fatalf("wrong result version: %s (%v)", top["version"], err)
	}
	var rawChildren []json.RawMessage
	if err := json.Unmarshal(top["children"], &rawChildren); err != nil || rawChildren == nil {
		t.Fatalf("children is not a list: %s (%v)", top["children"], err)
	}
	rows := make([]map[string]json.RawMessage, len(rawChildren))
	for i, raw := range rawChildren {
		rows[i] = listObject(t, raw)
		expected := []string{"child_id", "reason", "status"}
		if _, ok := rows[i]["via"]; ok {
			expected = append(expected, "via")
		}
		if _, ok := rows[i]["order"]; ok {
			expected = append(expected, "order")
		}
		if _, ok := rows[i]["order_invalid"]; ok {
			expected = append(expected, "order_invalid")
		}
		exactListFields(t, rows[i], expected...)
	}
	return rows
}

func TestSpaceChildListPositiveAndStableOutput(t *testing.T) {
	order50 := strings.Repeat("x", 50)
	secretOrder := strings.Repeat("q", 51)
	body := spaceChildStateBody(
		spaceChildEvent("!child-b", map[string]any{"via": []any{"z.example", "a.example"}, "order": "second", "topic": "TOPIC_PRIVATE_SENTINEL"}),
		spaceChildEvent("!child-a", map[string]any{"via": []any{"a.example"}}),
		spaceChildEvent("!child-c", map[string]any{"via": []any{"c.example"}, "order": ""}),
		spaceChildEvent("!child-50", map[string]any{"via": []any{"fifty.example"}, "order": order50}),
		spaceChildEvent("!child-0", map[string]any{"via": []any{"invalid-order.example"}, "order": secretOrder}),
		spaceChildEvent("!child-1", map[string]any{"via": []any{"control-order.example"}, "order": "\x19"}),
		spaceChildEvent("!removed", map[string]any{"suggested": true, "room_name": "ROOM_NAME_PRIVATE_SENTINEL"}),
	)
	wantIDs := []string{"!child-50", "!child-a", "!child-b", "!child-c", "!child-0", "!child-1"}
	for _, asJSON := range []bool{true, false} {
		t.Run(map[bool]string{true: "json", false: "text"}[asJSON], func(t *testing.T) {
			args := []string{"space", "child", "list", adminRoom}
			if asJSON {
				args = append(args, "--json")
			}
			f, code, requests := runSpaceChildList(t, t.Context(), args, http.StatusOK, body, nil)
			if code != ExitOK || f.err.String() != "" {
				t.Fatalf("exit=%d stderr=%q", code, f.err.String())
			}
			assertSpaceChildListOnlyRead(t, requests)
			if strings.Contains(f.out.String(), secretOrder) || strings.Contains(f.out.String(), "TOPIC_PRIVATE_SENTINEL") || strings.Contains(f.out.String(), "ROOM_NAME_PRIVATE_SENTINEL") {
				t.Fatal("private extension or invalid order content was echoed")
			}
			if asJSON {
				rows := rawRows(t, f.out.Bytes())
				if len(rows) != len(wantIDs) {
					t.Fatalf("rows=%d want=%d", len(rows), len(wantIDs))
				}
				for i, row := range rows {
					if got := rawString(t, row, "child_id"); got != wantIDs[i] {
						t.Fatalf("row %d child=%q want=%q", i, got, wantIDs[i])
					}
					if got := rawString(t, row, "status"); got != "valid" {
						t.Fatalf("row %d status=%q", i, got)
					}
					if got := rawString(t, row, "reason"); got != "" {
						t.Fatalf("row %d reason=%q", i, got)
					}
				}
				var invalidLength int
				if err := json.Unmarshal(rows[4]["order_invalid"], &invalidLength); err != nil || invalidLength != len(secretOrder) {
					t.Fatalf("invalid order length=%s err=%v", rows[4]["order_invalid"], err)
				}
				var controlLength int
				if err := json.Unmarshal(rows[5]["order_invalid"], &controlLength); err != nil || controlLength != 1 {
					t.Fatalf("control order length=%s err=%v", rows[5]["order_invalid"], err)
				}
				var via []string
				if err := json.Unmarshal(rows[2]["via"], &via); err != nil || !reflect.DeepEqual(via, []string{"z.example", "a.example"}) {
					t.Fatalf("via order lost: %v err=%v", via, err)
				}
				var emptyOrder string
				if err := json.Unmarshal(rows[3]["order"], &emptyOrder); err != nil || emptyOrder != "" {
					t.Fatalf("present empty order was lost: %q err=%v", emptyOrder, err)
				}
				if _, present := rows[1]["order"]; present {
					t.Fatal("unset order was serialized as present")
				}
			} else {
				want := "space-child-list v1 room=\"!space:example.invalid\" state_type=\"m.space.child\" rows=6\n" +
					"child=\"!child-50\" via=[\"fifty.example\"] order=\"" + order50 + "\" status=valid reason=\n" +
					"child=\"!child-a\" via=[\"a.example\"] order=unset status=valid reason=\n" +
					"child=\"!child-b\" via=[\"z.example\",\"a.example\"] order=\"second\" status=valid reason=\n" +
					"child=\"!child-c\" via=[\"c.example\"] order=\"\" status=valid reason=\n" +
					"child=\"!child-0\" via=[\"invalid-order.example\"] order_invalid=51 status=valid reason=\n" +
					"child=\"!child-1\" via=[\"control-order.example\"] order_invalid=1 status=valid reason=\n"
				if f.out.String() != want {
					t.Fatalf("text differs:\n%s\nwant:\n%s", f.out.String(), want)
				}
			}
		})
	}
}

func TestSpaceChildListEmptySpace(t *testing.T) {
	body := spaceChildStateBody()
	f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, body, nil)
	if code != ExitOK || f.err.String() != "" {
		t.Fatalf("exit=%d stderr=%q", code, f.err.String())
	}
	assertSpaceChildListOnlyRead(t, requests)
	if rows := rawRows(t, f.out.Bytes()); len(rows) != 0 {
		t.Fatalf("empty Space returned %d children", len(rows))
	}
}

func TestSpaceChildListPreservesExistingViaCompatibilityForms(t *testing.T) {
	wantVia := []string{"127.0.0.1:8448", "[2001:db8::1]:8448", "EXAMPLE.invalid", "example.invalid", "example.invalid"}
	items := make([]any, len(wantVia))
	for i, server := range wantVia {
		items[i] = server
	}
	body := spaceChildStateBody(spaceChildEvent("!compatible", map[string]any{"via": items}))
	f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, body, nil)
	if code != ExitOK || f.err.String() != "" {
		t.Fatalf("exit=%d stderr=%q", code, f.err.String())
	}
	assertSpaceChildListOnlyRead(t, requests)
	rows := rawRows(t, f.out.Bytes())
	if len(rows) != 1 || rawString(t, rows[0], "status") != "valid" {
		t.Fatalf("compatible declaration was not accepted: %v", rows)
	}
	var gotVia []string
	if err := json.Unmarshal(rows[0]["via"], &gotVia); err != nil || !reflect.DeepEqual(gotVia, wantVia) {
		t.Fatalf("via compatibility/order got=%v err=%v", gotVia, err)
	}
}

func TestSpaceChildListNonStringOrderIsMarkedWithoutEcho(t *testing.T) {
	order := []any{"ORDER_PRIVATE_SENTINEL"}
	rawOrder, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	body := spaceChildStateBody(spaceChildEvent("!child", map[string]any{"via": []any{"s.example"}, "order": order}))
	f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, body, nil)
	if code != ExitOK || f.err.String() != "" || strings.Contains(f.out.String(), "ORDER_PRIVATE_SENTINEL") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, f.out.String(), f.err.String())
	}
	assertSpaceChildListOnlyRead(t, requests)
	rows := rawRows(t, f.out.Bytes())
	if len(rows) != 1 || rawString(t, rows[0], "status") != "valid" {
		t.Fatalf("valid via row was marked invalid: %v", rows)
	}
	var gotLength int
	if err := json.Unmarshal(rows[0]["order_invalid"], &gotLength); err != nil || gotLength != len(rawOrder) {
		t.Fatalf("invalid order length=%d want=%d err=%v", gotLength, len(rawOrder), err)
	}
}

func TestSpaceChildListInvalidRowsRemainVisible(t *testing.T) {
	// This is the targeted control for a mutant that silently drops an invalid
	// row instead of marking it. The same test remains in the PR; no mutation
	// framework is needed.
	tooMany := make([]any, 17)
	for i := range tooMany {
		tooMany[i] = "s.example"
	}
	tooWideTotal := make([]any, 16)
	for i := range tooWideTotal {
		tooWideTotal[i] = strings.Repeat("a", 255) + ":1"
	}
	cases := []struct {
		name, reason string
		content      map[string]any
	}{
		{"ambiguous-case", "ambiguous_property_field", map[string]any{"via": []any{"s.example"}, "Via": []any{"other.example"}}},
		{"via-not-array", "malformed_child_via", map[string]any{"via": "PRIVATE_VIA_VALUE"}},
		{"via-entry-not-string", "malformed_child_server", map[string]any{"via": []any{"s.example", 3}}},
		{"zero-entries", "invalid_child_via", map[string]any{"via": []any{}}},
		{"too-many-entries", "invalid_child_via", map[string]any{"via": tooMany}},
		{"entry-too-long", "invalid_child_via", map[string]any{"via": []any{strings.Repeat("s", 262)}}},
		{"aggregate-too-long", "invalid_child_via", map[string]any{"via": tooWideTotal}},
		{"invalid-server", "invalid_child_via", map[string]any{"via": []any{"bad host"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := spaceChildStateBody(
				spaceChildEvent("!invalid", tc.content),
				spaceChildEvent("!valid", map[string]any{"via": []any{"valid.example"}}),
			)
			f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, body, nil)
			if code != ExitInvalidRows || f.err.String() != "RULE admin_child_invalid: $.children\n" {
				t.Fatalf("exit=%d stderr=%q", code, f.err.String())
			}
			assertSpaceChildListOnlyRead(t, requests)
			rows := rawRows(t, f.out.Bytes())
			if len(rows) != 2 {
				t.Fatalf("invalid declaration was dropped: rows=%d", len(rows))
			}
			if rawString(t, rows[0], "child_id") != "!invalid" || rawString(t, rows[0], "status") != "invalid" || rawString(t, rows[0], "reason") != tc.reason {
				t.Fatalf("invalid row mismatch: %v", rows[0])
			}
			if rawString(t, rows[1], "child_id") != "!valid" || rawString(t, rows[1], "status") != "valid" || rawString(t, rows[1], "reason") != "" {
				t.Fatalf("valid control mismatch: %v", rows[1])
			}
			if strings.Contains(f.out.String()+f.err.String(), "PRIVATE_VIA_VALUE") {
				t.Fatal("invalid via content escaped")
			}
		})
	}
	t.Run("text-row", func(t *testing.T) {
		body := spaceChildStateBody(
			spaceChildEvent("!invalid", map[string]any{"via": "PRIVATE_VIA_VALUE"}),
			spaceChildEvent("!valid", map[string]any{"via": []any{"valid.example"}}),
		)
		f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom}, http.StatusOK, body, nil)
		want := "space-child-list v1 room=\"!space:example.invalid\" state_type=\"m.space.child\" rows=2\n" +
			"child=\"!invalid\" via=invalid order=unset status=invalid reason=malformed_child_via\n" +
			"child=\"!valid\" via=[\"valid.example\"] order=unset status=valid reason=\n"
		if code != ExitInvalidRows || f.err.String() != "RULE admin_child_invalid: $.children\n" || f.out.String() != want {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, f.out.String(), f.err.String())
		}
		assertSpaceChildListOnlyRead(t, requests)
	})
}

func TestSpaceChildListRejectsInvalidUTF8ViaInProjection(t *testing.T) {
	rows, invalid, overLimit := projectSpaceChildList([]matrix.RoomPolicyEvent{
		{Type: "m.space.child", StateKey: "!invalid", Content: map[string]any{"via": []any{string([]byte{0xff})}}},
		{Type: "m.space.child", StateKey: "!valid", Content: map[string]any{"via": []any{"valid.example"}}},
	})
	if overLimit || !invalid || len(rows) != 2 || rows[0].Status != "invalid" || rows[0].Reason != "invalid_child_via" || rows[1].Status != "valid" {
		t.Fatalf("UTF-8 projection rows=%+v invalid=%t overLimit=%t", rows, invalid, overLimit)
	}
}

func TestValidSpaceChildOrderASCIIEdges(t *testing.T) {
	for _, order := range []string{"", " ", "~", strings.Repeat("~", 50)} {
		if !validSpaceChildOrder(order) {
			t.Fatalf("valid order rejected at boundary length %d", len(order))
		}
	}
	for _, order := range []string{"\x1f", "\x7f", "é", strings.Repeat(" ", 51)} {
		if validSpaceChildOrder(order) {
			t.Fatalf("invalid order accepted at raw length %d", len(order))
		}
	}
}

func TestSpaceChildListWholeSnapshotRefusals(t *testing.T) {
	valid := spaceChildStateBody(spaceChildEvent("!child", map[string]any{"via": []any{"s.example"}}))
	nonSpace := strings.Replace(valid, `"type":"m.space"`, `"type":"m.room"`, 1)
	duplicateChild := spaceChildStateBody(
		spaceChildEvent("!same", map[string]any{"via": []any{"a.example"}}),
		spaceChildEvent("!same", map[string]any{"via": []any{"b.example"}}),
	)
	cases := []struct {
		name, body string
		status     int
		alter      func(*http.Response)
		wantDiag   string
	}{
		{"denied", `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden, nil, "RULE admin_space: $.space\n"},
		{"malformed", `not-json`, http.StatusOK, nil, "RULE admin_space: $.space\n"},
		{"incomplete", valid, http.StatusOK, func(resp *http.Response) { resp.ContentLength++ }, "RULE admin_space: $.space\n"},
		{"not-space", nonSpace, http.StatusOK, nil, "RULE admin_space: $.space\n"},
		{"duplicate-child-key", duplicateChild, http.StatusOK, nil, "RULE admin_space: $.space\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, tc.status, tc.body, tc.alter)
			if code != ExitError || f.err.String() != tc.wantDiag || f.out.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, f.out.String(), f.err.String())
			}
			assertSpaceChildListOnlyRead(t, requests)
		})
	}
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		alter := func(resp *http.Response) { resp.Body = adminCloseBody{strings.NewReader(valid), cancel} }
		f, code, requests := runSpaceChildList(t, ctx, []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, valid, alter)
		if code != ExitError || f.err.String() != "RULE admin_context: $.context\n" || f.out.Len() != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, f.out.String(), f.err.String())
		}
		assertSpaceChildListOnlyRead(t, requests)
	})
}

func TestSpaceChildListArgumentRefusals(t *testing.T) {
	base := []string{"space", "child", "list", adminRoom}
	cases := map[string][]string{
		"missing-target": {"space", "child", "list"},
		"extra-operand":  append(append([]string(nil), base...), "!extra"),
		"child-selector": append(append([]string(nil), base...), "--child", adminChild),
		"via":            append(append([]string(nil), base...), "--via", "s.example"),
		"all":            append(append([]string(nil), base...), "--all"),
		"expected-id":    append(append([]string(nil), base...), "--expect-space-id", adminRoom),
		"unknown":        append(append([]string(nil), base...), "--unexpected"),
		"repeated-json":  append(append([]string(nil), base...), "--json", "--json"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			f := newNamedFixture(t, false)
			calls := 0
			a := f.app()
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return propertyCLIReply(r, http.StatusOK, `{}`), nil
			})}
			if code := a.Run(t.Context(), args); code != ExitUsage || f.out.Len() != 0 || f.err.String() != "RULE admin_usage: argv\n" || calls != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q requests=%d", code, f.out.String(), f.err.String(), calls)
			}
		})
	}
}

func TestSpaceChildListEntryAndResponseBounds(t *testing.T) {
	childrenAtLimit := make([]map[string]any, maxSpaceChildListEntries)
	for i := range childrenAtLimit {
		childrenAtLimit[i] = spaceChildEvent(fmt.Sprintf("!child-%04d:example.invalid", i), map[string]any{"via": []any{"s.example"}})
	}
	for _, tc := range []struct {
		name, body, wantDiag string
		code, rows           int
	}{
		{"entries-at-limit", spaceChildStateBody(childrenAtLimit...), "", ExitOK, maxSpaceChildListEntries},
		{"entries-over-limit", spaceChildStateBody(append(childrenAtLimit, spaceChildEvent("!child-over:example.invalid", map[string]any{"via": []any{"s.example"}}))...), "RULE admin_child_limit: $.children\n", ExitError, 0},
		{"bytes-at-limit", spaceChildStateBodyAtBytes(matrix.VerificationEntityLimit), "", ExitOK, 0},
		{"bytes-over-limit", spaceChildStateBodyAtBytes(matrix.VerificationEntityLimit + 1), "RULE admin_space_limit: $.space\n", ExitError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, code, requests := runSpaceChildList(t, t.Context(), []string{"space", "child", "list", adminRoom, "--json"}, http.StatusOK, tc.body, nil)
			if code != tc.code || f.err.String() != tc.wantDiag {
				t.Fatalf("exit=%d stderr=%q", code, f.err.String())
			}
			assertSpaceChildListOnlyRead(t, requests)
			if tc.code != ExitOK {
				if f.out.Len() != 0 {
					t.Fatalf("over-limit output was partial: %d bytes", f.out.Len())
				}
				return
			}
			if rows := rawRows(t, f.out.Bytes()); len(rows) != tc.rows {
				t.Fatalf("rows=%d want=%d", len(rows), tc.rows)
			}
		})
	}
}

func spaceChildStateBodyAtBytes(size int) string {
	prefix := `[{"type":"m.room.create","state_key":"","content":{"type":"m.space"}},{"type":"m.room.topic","state_key":"","content":{"topic":"`
	suffix := `"}}]`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		panic("target state body too small")
	}
	return prefix + strings.Repeat("x", padding) + suffix
}
