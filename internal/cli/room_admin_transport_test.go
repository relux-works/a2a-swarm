package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Complete getters distinguish absence, clearing and invalid observations.
// A child read proves Space type without local JOIN/version/write authority.
func TestRoomAdminGetterNeighbors(t *testing.T) {
	for _, child := range []bool{false, true} {
		field, valid := "name", `{"name":"Example","private":"`+adminMarker+`"}`
		if child {
			field, valid = "via", `{"via":["EXAMPLE.invalid"],"order":"opaque","suggested":true,"private":"`+adminMarker+`"}`
		}
		for _, tc := range []struct {
			name, body string
			status     int
			outcome    string
		}{
			{"valid-extensions", valid, 200, "PRESENT"},
			{"empty-extensions", `{"private":"` + adminMarker + `"}`, 200, "EMPTY"},
			{"null", `{"` + field + `":null}`, 200, "UNKNOWN"},
			{"wrong-type", `{"` + field + `":4}`, 200, "UNKNOWN"},
			{"nearcase", `{"` + strings.ToUpper(field) + `":"` + adminMarker + `"}`, 200, "UNKNOWN"},
			{"duplicate", `{"` + field + `":null,"` + field + `":null}`, 200, "UNKNOWN"},
			{"trailing", valid + ` {}`, 200, "UNKNOWN"},
			{"capped", `{"private":"` + strings.Repeat("x", 65536) + `"}`, 200, "UNKNOWN"},
			{"partial-status", valid, 206, "UNKNOWN"},
			{"redirect", valid, 307, "UNKNOWN"},
			{"forbidden", `{"errcode":"M_FORBIDDEN","error":"` + adminMarker + `"}`, 403, "UNKNOWN"},
			{"unauthorized", `{"errcode":"M_UNKNOWN_TOKEN","error":"` + adminMarker + `"}`, 401, "UNKNOWN"},
			{"wrong404", `{"errcode":"M_UNKNOWN","error":"` + adminMarker + `"}`, 404, "UNKNOWN"},
			{"notfound", `{"errcode":"M_NOT_FOUND"}`, 404, "ABSENT"},
			{"truncated-notfound", `{"errcode":"M_NOT_FOUND"`, 404, "UNKNOWN"},
		} {
			t.Run(fmt.Sprintf("child%t/%s", child, tc.name), func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				requests := 0
				properties := 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					if r.Method != "GET" {
						t.Fatal("getter wrote")
					}
					if strings.HasSuffix(r.URL.Path, "/state") {
						return propertyCLIReply(r, 200, `[{"type":"m.room.create","state_key":"","content":{"type":"m.space"}}]`), nil
					}
					properties++
					resp := propertyCLIReply(r, tc.status, tc.body)
					if tc.status == 307 {
						resp.Header.Set("Location", f.r.Profiles["alpha"].URL+"/forbidden-redirect")
					}
					return resp, nil
				})}
				code := a.Run(t.Context(), append(adminArgs(child, "get"), "--json"))
				wantCode := 0
				if tc.outcome == "UNKNOWN" {
					wantCode = 1
				}
				if code != wantCode || properties != 1 || requests != 1+map[bool]int{true: 1}[child] {
					t.Fatalf("getter effects/exit code%d requests%d properties%d", code, requests, properties)
				}
				m, err := decodeAdminRecord(f.out.Bytes())
				if err != nil || m["outcome"] != tc.outcome {
					t.Fatalf("getter result %s err%v", f.out.String(), err)
				}
				wantErr := ""
				if code != 0 {
					wantErr = "RULE admin_state: $.content\n"
				}
				if f.err.String() != wantErr || strings.Contains(f.out.String()+f.err.String(), adminMarker) {
					t.Fatal("diagnostic/projection boundary")
				}
			})
		}
	}
}

// Wire ACK plants prove no readback without a strict original 200 event ID;
// readback differences retain that ACK. No response or hidden retry is assumed.
func TestRoomAdminACKNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		ack          string
		readback     string
		want, read   string
		acknowledged bool
	}{
		{"verified", 200, `{"event_id":"$event"}`, `{"name":"Example"}`, "VERIFIED", "match", true},
		{"extra-ack-opaque", 200, `{"event_id":"$event","private":"` + adminMarker + `"}`, `{"name":"Example"}`, "VERIFIED", "match", true},
		{"extra-content", 200, `{"event_id":"$event"}`, `{"name":"Example","private":"` + adminMarker + `"}`, "UNKNOWN", "mismatch", true},
		{"bad-content-type", 200, `{"event_id":"$event"}`, `{"name":null}`, "UNKNOWN", "mismatch", true},
		{"duplicate-ack", 200, `{"event_id":"$event","event_id":"$other"}`, "", "UNKNOWN", "not-attempted", false},
		{"wrong-ack-type", 200, `{"event_id":7}`, "", "UNKNOWN", "not-attempted", false},
		{"empty-id", 200, `{"event_id":""}`, "", "UNKNOWN", "not-attempted", false},
		{"wrong-id", 200, `{"event_id":"not-event"}`, "", "UNKNOWN", "not-attempted", false},
		{"non200", 201, `{"event_id":"$event"}`, "", "UNKNOWN", "not-attempted", false},
		{"redirect", 307, `{"event_id":"$event"}`, "", "UNKNOWN", "not-attempted", false},
		{"lost", 0, "", "", "UNKNOWN", "not-attempted", false},
		{"forbidden", 403, `{"errcode":"M_FORBIDDEN","error":"` + adminMarker + `"}`, "", "REFUSED", "not-attempted", false},
		{"unknown-token", 401, `{"errcode":"M_UNKNOWN_TOKEN","error":"` + adminMarker + `"}`, "", "REFUSED", "not-attempted", false},
		{"missing-token", 401, `{"errcode":"M_MISSING_TOKEN"}`, "", "REFUSED", "not-attempted", false},
		{"unverified-refusal", 403, `{"errcode":"M_FORBIDDEN"`, "", "UNKNOWN", "not-attempted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			puts, reads := 0, 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/state") {
					return propertyCLIReply(r, 200, adminPolicy()), nil
				}
				if r.Method == "PUT" {
					puts++
					if tc.status == 0 {
						return nil, errors.New(adminMarker)
					}
					resp := propertyCLIReply(r, tc.status, tc.ack)
					if tc.status == 307 {
						resp.Header.Set("Location", f.r.Profiles["alpha"].URL+"/forbidden-redirect")
					}
					return resp, nil
				}
				reads++
				return propertyCLIReply(r, 200, tc.readback), nil
			})}
			code := a.Run(t.Context(), append(adminArgs(false, "set"), "--json"))
			m, err := decodeAdminRecord(f.out.Bytes())
			if err != nil || m["outcome"] != tc.want || m["acknowledged"] != tc.acknowledged || m["readback"] != tc.read {
				t.Fatalf("ACK result %s consumer=%v", f.out.String(), err)
			}
			if puts != 1 || reads != map[bool]int{true: 1}[tc.acknowledged] || code != map[bool]int{false: 1}[tc.want == "VERIFIED"] {
				t.Fatalf("ACK request count/exit puts%d reads%d exit%d", puts, reads, code)
			}
			if strings.Contains(f.out.String()+f.err.String(), adminMarker) {
				t.Fatal("raw server error escaped")
			}
		})
	}
}

// Alias resolution is pinned once per target. Wrong resolved values/refusals
// do not enter the property stage or silently substitute another room.
func TestRoomAdminTargetAndContext(t *testing.T) {
	for _, tc := range []struct {
		name  string
		badID bool
		mode  string
	}{
		{"valid-aliases", false, ""}, {"wrong-target", false, "wrong"}, {"invalid-resolved", true, "wrong"}, {"alias-error", false, "error"}, {"context-preflight", false, "cancel"}, {"get-guard", false, "get-guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			aliases, puts, props := 0, 0, 0
			stored := ""
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/directory/room/") {
					aliases++
					if tc.mode == "error" {
						return nil, errors.New(adminMarker)
					}
					id := adminRoom
					if strings.Contains(r.URL.Path, "#child") {
						id = adminChild
					}
					// Change only the Space resolution: the child stays distinct and
					// valid, so no second invalid predicate masks a pin bypass.
					if tc.mode == "wrong" && id == adminRoom {
						id = "!other"
						if tc.badID {
							id = "!bad space"
						}
					}
					b, _ := json.Marshal(map[string]string{"room_id": id})
					return propertyCLIReply(r, 200, string(b)), nil
				}
				if strings.HasSuffix(r.URL.Path, "/state") {
					resp := propertyCLIReply(r, 200, adminPolicy())
					if tc.mode == "cancel" {
						resp.Body = adminCloseBody{strings.NewReader(adminPolicy()), cancel}
					}
					return resp, nil
				}
				if r.Method == "PUT" {
					puts++
					b, _ := io.ReadAll(r.Body)
					stored = string(b)
					return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
				}
				props++
				return propertyCLIReply(r, 200, stored), nil
			})}
			args := adminArgs(true, "add")
			args[3] = "#space:foreign.invalid"
			for i, s := range args {
				if s == "--child" {
					args[i+1] = "#child:foreign.invalid"
				}
			}
			if tc.mode == "get-guard" {
				args = adminArgs(false, "get")
				a.beforeAdminPropertyRequest = cancel
			}
			code := a.Run(ctx, append(args, "--json"))
			if tc.mode == "" {
				if code != 0 || aliases != 2 || puts != 1 || props != 1 {
					t.Fatalf("valid alias counts %d %d %d exit%d %s", aliases, puts, props, code, f.err.String())
				}
			} else {
				rule, location := "admin_target", "$.target"
				if tc.badID {
					rule = "admin_selector"
				}
				if tc.mode == "cancel" || tc.mode == "get-guard" {
					rule, location = "admin_context", "$.context"
				}
				if code != 1 || puts != 0 || props != 0 || f.out.Len() != 0 || f.err.String() != "RULE "+rule+": "+location+"\n" {
					t.Fatalf("target/context refusal exit%d puts%d props%d out%q err%q", code, puts, props, f.out.String(), f.err.String())
				}
			}
		})
	}
}

type adminBrokenOutput struct{}

func (adminBrokenOutput) Write([]byte) (int, error) { return 0, errors.New(adminMarker) }

// Session/global/output errors expose only safe rules. Output failure after
// a mutation cannot undo its one PUT or cause another send.
func TestRoomAdminSessionAndOutput(t *testing.T) {
	for _, mode := range []string{"session", "global", "output", "short-output"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			puts := 0
			stored := ""
			args := adminArgs(false, "set")
			if mode == "session" {
				a.as = adminMarker
			}
			if mode == "global" {
				args = append(args, "--connection-fingerprint="+adminMarker)
			}
			if mode == "output" {
				a.Stdout = adminBrokenOutput{}
			}
			if mode == "short-output" {
				a.Stdout = adminShortOutput{}
			}
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/state") {
					return propertyCLIReply(r, 200, adminPolicy()), nil
				}
				if r.Method == "PUT" {
					puts++
					b, _ := io.ReadAll(r.Body)
					stored = string(b)
					return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
				}
				return propertyCLIReply(r, 200, stored), nil
			})}
			code := a.Run(t.Context(), append(args, "--json"))
			rule, loc, wantCode, wantPuts := "admin_output", "$.output", 1, 1
			if mode == "session" {
				rule, loc, wantPuts = "admin_session", "$.session", 0
			}
			if mode == "global" {
				rule, loc, wantCode, wantPuts = "admin_usage", "argv", 2, 0
			}
			if code != wantCode || puts != wantPuts || f.err.String() != "RULE "+rule+": "+loc+"\n" || strings.Contains(f.out.String()+f.err.String(), adminMarker) {
				t.Fatalf("session/output boundary exit%d puts%d err%q", code, puts, f.err.String())
			}
		})
	}
}

type adminShortOutput struct{}

func (adminShortOutput) Write(b []byte) (int, error) { return len(b) - 1, nil }

// Plants are actual extra fake-transport requests, independently rejected by
// the effect instrument beside the unchanged successful ledger. No live HTTP.
func TestRoomAdminEffectDetectorPlants(t *testing.T) {
	valid := []adminRequest{{"PUT", adminStatePath(true), `{}`}, {"GET", adminStatePath(true), ""}}
	if err := checkAdminEffects(valid, append([]adminRequest(nil), valid...)); err != nil {
		t.Fatal("positive effect control")
	}
	plants := map[string]adminRequest{
		"reverse":    {"PUT", "/_matrix/client/v3/rooms/" + url.PathEscape(adminChild) + "/state/m.space.parent/" + url.PathEscape(adminRoom), `{}`},
		"other-room": {"PUT", "/_matrix/client/v3/rooms/%21other/state/m.space.child/%21child", `{}`},
		"retry":      valid[0], "message": {"PUT", "/_matrix/client/v3/rooms/%21other/send/m.room.message/txn", `{}`},
		"raw-fallback": {"POST", "/_matrix/client/v3/createRoom", `{}`},
	}
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			ledger := append([]adminRequest(nil), valid...)
			client := &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				ledger = append(ledger, adminRequest{r.Method, r.URL.EscapedPath(), string(b)})
				return propertyCLIReply(r, 200, `{}`), nil
			})}
			r, err := http.NewRequestWithContext(context.Background(), plant.Method, "https://fixture.invalid"+plant.Path, strings.NewReader(plant.Body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(ledger) != 3 || reflect.DeepEqual(ledger, valid) || checkAdminEffects(ledger, valid) == nil {
				t.Fatal("effect detector missed actual planted request")
			}
		})
	}
}

func checkAdminDiagnostic(stdout, stderr string, code int, rule, location string) error {
	if strings.Contains(stdout+stderr, adminMarker) {
		return errors.New("diagnostic marker escaped")
	}
	want := ""
	if code != 0 {
		want = "RULE " + rule + ": " + location + "\n"
	}
	if stderr != want {
		return errors.New("diagnostic rule mismatch")
	}
	return nil
}

// Exercises native helper/server failures at the real App.Run boundary and a
// raw-error plant in the diagnostic detector, not just credential scrubbing.
func TestRoomAdminDiagnosticDetector(t *testing.T) {
	for _, mode := range []string{"valid", "argument", "session", "server"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			args := append(adminArgs(false, "get"), "--json")
			wantCode, rule, loc := 0, "", ""
			if mode == "argument" {
				args = append(args, "--"+adminMarker)
				wantCode, rule, loc = 2, "admin_usage", "argv"
			}
			if mode == "session" {
				a.as = adminMarker
				wantCode, rule, loc = 1, "admin_session", "$.session"
			}
			if mode == "server" {
				wantCode, rule, loc = 1, "admin_state", "$.content"
			}
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if mode == "server" {
					return propertyCLIReply(r, 403, `{"errcode":"M_FORBIDDEN","error":"`+adminMarker+`"}`), nil
				}
				return propertyCLIReply(r, 200, `{"name":"Example","private":"`+adminMarker+`"}`), nil
			})}
			code := a.Run(t.Context(), args)
			if err := checkAdminDiagnostic(f.out.String(), f.err.String(), code, rule, loc); err != nil {
				t.Fatal(err)
			}
			if code != wantCode {
				t.Fatalf("wrong exit %d", code)
			}
			if err := checkAdminDiagnostic(f.out.String(), f.err.String()+adminMarker, code, rule, loc); err == nil || err.Error() != "diagnostic marker escaped" {
				t.Fatal("raw diagnostic plant not specifically detected")
			}
		})
	}
}

// Cancellation during an unverified ACK does not invent a readback or erase
// uncertainty. The request may have applied, but no event ID was established.
func TestRoomAdminUnacknowledgedContext(t *testing.T) {
	f := newNamedFixture(t, true)
	a := f.app()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	puts, gets := 0, 0
	a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/state") {
			return propertyCLIReply(r, 200, adminPolicy()), nil
		}
		if r.Method == "PUT" {
			puts++
			resp := propertyCLIReply(r, 200, `{"event_id":"$uncertain"}`)
			resp.Body = adminCloseBody{strings.NewReader(`{"event_id":"$uncertain"}`), cancel}
			return resp, nil
		}
		gets++
		return propertyCLIReply(r, 200, `{}`), nil
	})}
	code := a.Run(ctx, append(adminArgs(false, "set"), "--json"))
	m, err := decodeAdminRecord(f.out.Bytes())
	if code != 1 || err != nil || m["outcome"] != "UNKNOWN" || m["acknowledged"] != false || m["readback"] != "not-attempted" || puts != 1 || gets != 0 || f.err.String() != "RULE admin_context: $.context\n" {
		t.Fatalf("unacknowledged cancellation: %s %s consumer=%v", f.out.String(), f.err.String(), err)
	}
}
