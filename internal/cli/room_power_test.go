package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves the command uses one selected caller GET, prints the same authority
// facts in JSON/text, and never changes local files or credential storage.
func TestRoomPowerReadOnlyCommand(t *testing.T) {
	for _, asJSON := range []bool{true, false} {
		for _, label := range []string{"alpha", "beta"} {
			t.Run(label+string(rune('0'+map[bool]int{true: 1}[asJSON])), func(t *testing.T) {
				f := newNamedFixture(t, true)
				seedMemberState(t, f)
				before := memberFiles(t, f.root)
				a := f.app()
				calls := 0
				body := `[{"type":"m.room.create","state_key":"","sender":"@founder:fixture.invalid","content":{"room_version":"12"}},{"type":"m.room.member","state_key":"@alice:` + label + `.invalid","content":{"membership":"join"}}]`
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!opaque/state" || r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles[label].URL || r.Header.Get("Authorization") != "Bearer isolated-"+label {
						t.Fatalf("wrong selected request %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
				})}
				args := []string{"--profile", label, "room", "power", "!opaque"}
				if asJSON {
					args = append(args, "--json")
				}
				if code := a.Run(context.Background(), args); code != 0 {
					t.Fatalf("exit%d: %s", code, f.err.String())
				}
				if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
					t.Fatal("reader changed state or performed extra requests")
				}
				if asJSON {
					var view roomPowerView
					if err := json.Unmarshal(f.out.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
					if view.Caller != "@alice:"+label+".invalid" || view.RoomID != "!opaque" || view.V != 1 || view.Creators[0].Origin != "create-sender" {
						t.Fatalf("wrong projection %+v", view)
					}
				} else if !strings.Contains(f.out.String(), "effective=creator-infinite") || !strings.Contains(f.out.String(), "explicit=absent") {
					t.Fatalf("lost text distinctions %s", f.out.String())
				}
			})
		}
	}
}

// Proves boundary failures print no partial result and preserve all files.
// Existing matrix strict-reader tests separately cover its complete wire matrix.
func TestRoomPowerCommandRefusals(t *testing.T) {
	for _, kind := range []string{"forbidden", "incomplete", "partial-status", "network", "duplicate-key", "duplicate-state", "redacted", "wrong-room", "unknown-version", "left-caller", "redirect", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, true)
			seedMemberState(t, f)
			before := memberFiles(t, f.root)
			a := f.app()
			calls := 0
			body := `[{"type":"m.room.create","state_key":"","sender":"@founder:fixture.invalid","content":{"room_version":"12"}},{"type":"m.room.member","state_key":"@alice:alpha.invalid","content":{"membership":"join"}}]`
			status := 200
			length := int64(-1)
			want := "malformed or unsupported policy"
			switch kind {
			case "forbidden":
				status = 403
				want = "access denied"
			case "incomplete":
				length = int64(len(body) + 1)
				want = "incomplete observation"
			case "partial-status":
				status = 206
				want = "incomplete observation"
			case "network":
				want = "incomplete observation"
			case "duplicate-key":
				body = strings.Replace(body, `"room_version":"12"`, `"room_version":"12","room_version":"11"`, 1)
			case "duplicate-state":
				body = strings.TrimSuffix(body, "]") + `,` + strings.TrimPrefix(body, "[")
			case "redacted":
				body = strings.Replace(body, `"content":`, `"unsigned":{"redacted_because":{}},"content":`, 1)
			case "wrong-room":
				body = strings.Replace(body, `"content":`, `"room_id":"!other","content":`, 1)
			case "unknown-version":
				body = strings.Replace(body, `"12"`, `"13"`, 1)
			case "left-caller":
				body = strings.Replace(body, `"join"`, `"leave"`, 1)
			case "redirect":
				status = 307
				want = "incomplete observation"
			case "canceled":
				want = "incomplete observation"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if kind == "network" {
					return nil, errors.New("synthetic connection unavailable")
				}
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/state") {
					t.Fatalf("forbidden request %s %s", r.Method, r.URL)
				}
				if kind == "canceled" {
					cancel()
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: length, Request: r, Header: http.Header{"Location": []string{"https://other.invalid/state"}}}, nil
			})}
			if code := a.Run(ctx, []string{"room", "power", "!opaque", "--json"}); code == 0 || !strings.Contains(f.err.String(), want) {
				t.Fatalf("wrong refusal exit%d stderr%s", code, f.err.String())
			}
			if f.out.Len() != 0 || calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatal("failure printed partial output, followed redirect or mutated state")
			}
		})
	}
}

func powerFixture(t *testing.T, version string, power string) []matrix.RoomPolicyEvent {
	t.Helper()
	raw := `[{"type":"m.room.create","state_key":"","sender":"@founder:fixture.invalid","content":{"creator":"@legacy:fixture.invalid","room_version":"` + version + `"}},` +
		`{"type":"m.room.member","state_key":"@caller:fixture.invalid","content":{"membership":"join"}}]`
	var rows []struct {
		Type    string         `json:"type"`
		Key     string         `json:"state_key"`
		Sender  string         `json:"sender"`
		Content map[string]any `json:"content"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&rows); err != nil {
		t.Fatal(err)
	}
	var events []matrix.RoomPolicyEvent
	for _, r := range rows {
		events = append(events, matrix.RoomPolicyEvent{Type: r.Type, StateKey: r.Key, Sender: r.Sender, Content: r.Content})
	}
	if power != "" {
		var p map[string]any
		d := json.NewDecoder(strings.NewReader(power))
		d.UseNumber()
		if err := d.Decode(&p); err != nil {
			t.Fatal(err)
		}
		events = append(events, matrix.RoomPolicyEvent{Type: "m.room.power_levels", Content: p})
	}
	return events
}

// Proves historical creator labels do not override actual finite power, and
// pre-v10 string levels remain accepted beside the modern integer controls.
func TestRoomPowerFiniteAndInitial(t *testing.T) {
	for _, tc := range []struct {
		version, power, creator string
		want                    int64
		initial                 bool
	}{
		{"9", `{"users_default":"-2","users":{"@legacy:fixture.invalid":"7"}}`, "@legacy:fixture.invalid", 7, false},
		{"10", `{"users_default":-2,"users":{"@legacy:fixture.invalid":7}}`, "@legacy:fixture.invalid", 7, false},
		{"11", `{"users_default":-2}`, "@founder:fixture.invalid", -2, false},
		{"1", "", "@legacy:fixture.invalid", 100, true},
		{"11", "", "@founder:fixture.invalid", 100, true},
	} {
		t.Run(tc.version+tc.power, func(t *testing.T) {
			got, err := projectRoomPower(powerFixture(t, tc.version, tc.power), "fixture.invalid", "!opaque", "@caller:fixture.invalid")
			if err != nil {
				t.Fatal(err)
			}
			if got.PowerLevelsPresent == tc.initial || got.Creators[0].MXID != tc.creator {
				t.Fatalf("wrong source: %+v", got)
			}
			found := false
			for _, u := range got.Users {
				if u.MXID == tc.creator {
					found = true
					if u.Effective.Kind != "finite" || u.Effective.Value == nil || *u.Effective.Value != tc.want {
						t.Fatalf("creator power %+v", u)
					}
				}
			}
			if !found {
				t.Fatal("creator omitted")
			}
		})
	}
}

// Proves v12 implicit authority includes left additional creators and uses a
// tagged null value, while finite defaults remain explicit for other accounts.
func TestRoomPowerV12Creators(t *testing.T) {
	events := powerFixture(t, "12", `{"users_default":-9,"users":{"@other:fixture.invalid":9223372036854775807},"kick":-1}`)
	events[0].Content["additional_creators"] = []any{"@additional:fixture.invalid"}
	events = append(events, matrix.RoomPolicyEvent{Type: "m.room.member", StateKey: "@additional:fixture.invalid", Content: map[string]any{"membership": "leave"}})
	got, err := projectRoomPower(events, "fixture.invalid", "!opaque", "@caller:fixture.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Creators) != 2 || got.Required["kick"] != -1 || got.Defaults["users_default"] != -9 {
		t.Fatalf("projection %+v", got)
	}
	ids := []string{}
	for _, u := range got.Users {
		ids = append(ids, u.MXID)
		if u.MXID == "@founder:fixture.invalid" || u.MXID == "@additional:fixture.invalid" {
			if u.Effective.Kind != "creator-infinite" || u.Effective.Value != nil || u.ExplicitLevel != nil {
				t.Fatalf("creator authority %+v", u)
			}
		}
		if u.MXID == "@additional:fixture.invalid" && (u.Membership == nil || *u.Membership != "leave") {
			t.Fatalf("lost left creator %+v", u)
		}
		if u.MXID == "@other:fixture.invalid" && (u.Effective.Value == nil || *u.Effective.Value != 9223372036854775807 || u.ExplicitLevel == nil || *u.ExplicitLevel != 9223372036854775807 || u.Membership != nil) {
			t.Fatalf("lost exact boundary or absent membership %+v", u)
		}
	}
	if !reflect.DeepEqual(ids, []string{"@additional:fixture.invalid", "@caller:fixture.invalid", "@founder:fixture.invalid", "@other:fixture.invalid"}) {
		t.Fatalf("sorted union %v", ids)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"creator-infinite","value":null`) || strings.Contains(string(b), "sole_admin") || strings.Contains(string(b), "safe_to_retire") {
		t.Fatalf("authority schema %s", b)
	}
}

// Proves the default room version, minimum integer and tuple-key identity are
// preserved; delimiter-shaped extension keys must not produce false duplicates.
func TestRoomPowerInitialAndTupleNeighbors(t *testing.T) {
	e := powerFixture(t, "1", `{"users_default":-9223372036854775808}`)
	delete(e[0].Content, "room_version")
	e = append(e, matrix.RoomPolicyEvent{Type: "custom|field", StateKey: "key"}, matrix.RoomPolicyEvent{Type: "custom", StateKey: "field|key"})
	got, err := projectRoomPower(e, "fixture.invalid", "!opaque", "@caller:fixture.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if got.RoomVersion != "1" || got.Defaults["users_default"] != -9223372036854775808 {
		t.Fatalf("lost initial version/exact minimum %+v", got)
	}
}

// Proves malformed/unsupported or incomplete policy returns no partial result;
// the positive controls above exercise the same supported versions and fields.
func TestRoomPowerPolicyRefusals(t *testing.T) {
	for _, name := range []string{"version", "version-leading-zero", "version-type", "missing-create", "duplicate-create", "duplicate-state", "left-caller", "missing-caller", "bad-membership", "bad-member-id", "bad-creator", "additional-type", "additional-id", "duplicate-creator", "v12-explicit-creator", "v10-string", "fraction", "exponent", "overflow", "null-number", "null-users", "bad-user-id", "bad-event-value", "case-shadow"} {
		t.Run(name, func(t *testing.T) {
			e := powerFixture(t, "12", `{"users":{"@other:fixture.invalid":8}}`)
			switch name {
			case "version":
				e[0].Content["room_version"] = "13"
			case "version-leading-zero":
				e[0].Content["room_version"] = "012"
			case "version-type":
				e[0].Content["room_version"] = json.Number("12")
			case "missing-create":
				e = e[1:]
			case "duplicate-create":
				e = append(e, e[0])
			case "duplicate-state":
				e = append(e, matrix.RoomPolicyEvent{Type: "custom", StateKey: "x"}, matrix.RoomPolicyEvent{Type: "custom", StateKey: "x"})
			case "left-caller":
				e[1].Content["membership"] = "leave"
			case "missing-caller":
				e = append(e[:1], e[2:]...)
			case "bad-membership":
				e[1].Content["membership"] = "unknown"
			case "bad-member-id":
				e[1].StateKey = "caller"
			case "bad-creator":
				e[0].Sender = "founder"
			case "additional-type":
				e[0].Content["additional_creators"] = nil
			case "additional-id":
				e[0].Content["additional_creators"] = []any{"short"}
			case "duplicate-creator":
				e[0].Content["additional_creators"] = []any{e[0].Sender}
			case "v12-explicit-creator":
				e[2].Content["users"] = map[string]any{e[0].Sender: json.Number("100")}
			case "v10-string":
				e[0].Content["room_version"] = "10"
				e[2].Content["kick"] = "50"
			case "fraction":
				e[2].Content["kick"] = json.Number("1.5")
			case "exponent":
				e[2].Content["kick"] = json.Number("1e2")
			case "overflow":
				e[2].Content["kick"] = json.Number("9223372036854775808")
			case "null-number":
				e[2].Content["kick"] = nil
			case "null-users":
				e[2].Content["users"] = nil
			case "bad-user-id":
				e[2].Content["users"] = map[string]any{"short": json.Number("1")}
			case "bad-event-value":
				e[2].Content["events"] = map[string]any{"custom": true}
			case "case-shadow":
				e[2].Content["Users_default"] = json.Number("100")
			}
			got, err := projectRoomPower(e, "fixture.invalid", "!opaque", "@caller:fixture.invalid")
			if !errors.Is(err, errRoomPowerPolicy) || got != nil {
				t.Fatalf("expected policy refusal/no rows: got=%+v err=%v", got, err)
			}
		})
	}
}
