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
)

const readRoomPayload = `{"event_id":"$selected","room_id":"!selected:fixture","sender":"@peer:fixture","type":"m.room.message","origin_server_ts":1234,"content":{"msgtype":"m.text","body":"same room"}}`

// Both real App.Run consumers cross the selected HTTP boundary. A wrong room
// must never be printed under the selected label, including a payload returned
// successfully from the requested path. Strict verification keeps its distinct
// absent-room observation and opaque failure diagnostic.
func TestReadRoomBindingConsumers(t *testing.T) {
	cases := []struct {
		name, body                string
		displayOK, verificationOK bool
	}{
		{"same-room", readRoomPayload, true, true},
		{"foreign-room", strings.Replace(readRoomPayload, "!selected:fixture", "!foreign:fixture", 1), false, false},
		{"missing-room", strings.Replace(readRoomPayload, `"room_id":"!selected:fixture",`, "", 1), false, true},
		{"null-room", strings.Replace(readRoomPayload, `"!selected:fixture"`, `null`, 1), false, false},
		{"number-room", strings.Replace(readRoomPayload, `"!selected:fixture"`, `42`, 1), false, false},
		{"array-room", strings.Replace(readRoomPayload, `"!selected:fixture"`, `[]`, 1), false, false},
		{"empty-room", strings.Replace(readRoomPayload, "!selected:fixture", "", 1), false, false},
		{"room-alias-value", strings.Replace(readRoomPayload, "!selected:fixture", "#selected:fixture", 1), false, false},
		{"room-case-key", strings.Replace(readRoomPayload, `"room_id"`, `"ROOM_ID"`, 1), false, false},
		{"room-case-shadow", strings.Replace(readRoomPayload, `"room_id":`, `"Room_ID":"!foreign:fixture","room_id":`, 1), false, false},
		{"room-duplicate", strings.Replace(readRoomPayload, `"room_id":`, `"room_id":"!foreign:fixture","room_id":`, 1), false, false},
		{"room-escaped-duplicate", strings.Replace(readRoomPayload, `"room_id":`, `"room_\u0069d":"!foreign:fixture","room_id":`, 1), false, false},
		{"room-repaired-unicode", strings.Replace(readRoomPayload, "!selected:fixture", `!selected\ud800:fixture`, 1), false, false},
		{"wrong-event", strings.Replace(readRoomPayload, "$selected", "$other", 1), false, false},
		{"missing-event", strings.Replace(readRoomPayload, `"event_id":"$selected",`, "", 1), false, false},
		{"event-case-key", strings.Replace(readRoomPayload, `"event_id"`, `"Event_ID"`, 1), false, false},
		{"event-duplicate", strings.Replace(readRoomPayload, `"event_id":`, `"event_id":"$other","event_id":`, 1), false, false},
		{"trailing-value", readRoomPayload + `{}`, false, false},
		{"peer-content-compatibility", strings.Replace(readRoomPayload, `"body":"same room"`, `"body":"first","body":"same room\ud800"`, 1), true, false},
		{"redacted-content", strings.TrimSuffix(readRoomPayload, "}") + `,"unsigned":{"redacted_because":{}}}`, true, false},
	}
	for _, tc := range cases {
		for _, verification := range []bool{false, true} {
			mode := "display"
			if verification {
				mode = "verification"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				before := memberFiles(t, f.root)
				calls := 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!selected:fixture/event/$selected" || r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
						t.Fatalf("wrong HTTP selection: %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(tc.body)), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})}
				args := []string{"--profile", "alpha", "--as", "alice", "read", "!selected:fixture", "--event", "$selected", "--json"}
				if verification {
					args = append(args, "--verification-input")
				}
				code := a.Run(context.Background(), args)
				wantOK := tc.displayOK
				if verification {
					wantOK = tc.verificationOK
				}
				if (code == ExitOK) != wantOK {
					t.Fatalf("exit=%d wantOK=%v stdout=%q stderr=%q", code, wantOK, f.out.String(), f.err.String())
				}
				if !wantOK && f.out.Len() != 0 {
					t.Fatalf("refusal printed event: %q", f.out.String())
				}
				if tc.name == "foreign-room" && !verification {
					if code != ExitUsage || !strings.Contains(f.err.String(), "!selected:fixture") || !strings.Contains(f.err.String(), "!foreign:fixture") {
						t.Fatalf("missing selected/returned room usage error: %d %q", code, f.err.String())
					}
				}
				if wantOK && !verification {
					var record map[string]any
					if err := json.Unmarshal(f.out.Bytes(), &record); err != nil {
						t.Fatal(err)
					}
					if record["event_id"] != "$selected" || record["room_id"] != "!selected:fixture" || !strings.Contains(f.out.String(), "same room") {
						t.Fatalf("selection lost: %s", f.out.String())
					}
				}
				if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
					t.Fatal("read changed files, secrets, or request count")
				}
			})
		}
	}
}

// HTTP failures and interruption refuse before any output or local write in
// both consumers. The successful neighbor is the same-room case above.
func TestReadRoomTransportRefusals(t *testing.T) {
	for _, verification := range []bool{false, true} {
		for _, kind := range []string{"403", "404", "interrupted", "short-body"} {
			t.Run(kind+map[bool]string{false: "/display", true: "/verification"}[verification], func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				before := memberFiles(t, f.root)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/event/$selected") {
						t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					}
					if kind == "interrupted" {
						cancel()
						return nil, context.Canceled
					}
					status, body := 200, readRoomPayload
					if kind == "403" {
						status, body = 403, `{"errcode":"M_FORBIDDEN","error":"private detail"}`
					}
					if kind == "404" {
						status, body = 404, `{"errcode":"M_NOT_FOUND","error":"private detail"}`
					}
					length := int64(len(body))
					if kind == "short-body" {
						length++
					}
					return &http.Response{StatusCode: status, Request: r, ContentLength: length, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				args := []string{"read", "!selected:fixture", "--event", "$selected", "--json"}
				if verification {
					args = append(args, "--verification-input")
				}
				code := a.Run(ctx, args)
				if code == ExitOK || f.out.Len() != 0 || strings.Contains(f.err.String(), "private detail") || calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
					t.Fatalf("failed read leaked or changed state: exit=%d calls=%d out=%q err=%q", code, calls, f.out.String(), f.err.String())
				}
			})
		}
	}
}

// A real fake-homeserver round trip proves alias resolution cannot relabel a
// foreign payload, and the unmodified same-room display is byte-for-byte equal.
// Timeline and topic reads retain their output and issue no mutation requests.
func TestReadRoomActualHTTPAndOrdinaryNeighbors(t *testing.T) {
	e, room := world(t)
	id := e.hs.inject(room, "@bob:test.local", msg("ordinary neighbor", nil))
	base := e.as("alice", "read", "proj", "--event", id, "--json").ok(t).stdout
	text := e.as("alice", "read", "proj", "--event", id).ok(t).stdout
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "read-room-topic", "Read room topic").ok(t).stdout)
	timeline := e.as("alice", "read", "proj", "--last", "1", "--json").ok(t).stdout
	topic := e.as("alice", "read", "proj", "--topic", root, "--json").ok(t).stdout
	before := memberFiles(t, e.dir)
	foreign := e.hs.addRoom("foreign", "@alice:test.local")
	changed := false
	gets := 0
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				return nil, errors.New("read attempted a mutation")
			}
			gets++
			resp, err := http.DefaultTransport.RoundTrip(r)
			if err != nil || !strings.HasSuffix(r.URL.Path, "/event/"+id) || !changed {
				return resp, err
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			body := strings.Replace(string(raw), room, foreign, 1)
			resp.Body = io.NopCloser(strings.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp, nil
		})}
	}
	if got := e.as("alice", "read", "proj", "--event", id, "--json").ok(t).stdout; got != base {
		t.Fatalf("same-room output changed: %q != %q", got, base)
	}
	if got := e.as("alice", "read", "proj", "--event", id).ok(t).stdout; got != text {
		t.Fatalf("same-room text changed: %q != %q", got, text)
	}
	changed = true
	r := e.as("alice", "read", "proj", "--event", id, "--json").failsWith(t, ExitUsage, foreign)
	if r.stdout != "" || !strings.Contains(r.stderr, room) {
		t.Fatalf("foreign event mislabeled: %+v", r)
	}
	if r := e.as("alice", "read", "proj", "--event", id).failsWith(t, ExitUsage, foreign); r.stdout != "" {
		t.Fatal("foreign text event printed")
	}
	for _, args := range [][]string{{"read", "proj", "--last", "1", "--json"}, {"read", "proj", "--topic", root, "--json"}} {
		got := e.as("alice", args...).ok(t).stdout
		want := timeline
		if strings.Contains(strings.Join(args, " "), "--topic") {
			want = topic
		}
		if got != want {
			t.Fatalf("ordinary read changed: %q != %q", got, want)
		}
	}
	if gets == 0 || !reflect.DeepEqual(before, memberFiles(t, e.dir)) {
		t.Fatal("read changed local state or did not reach HTTP")
	}
}
