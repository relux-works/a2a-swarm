package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/inboundmedia"
)

func TestElementMediaProjectionIsInboxOnly(t *testing.T) {
	for _, consumer := range []string{"wait", "watch", "read"} {
		t.Run(consumer, func(t *testing.T) {
			e, room := nativeWorld(t)
			e.as("alice", "inbox").ok(t)
			content := elementContent("m.file")
			content["filename"] = "PEER-DECLARED-NAME-ONLY-IN-INBOX"
			id := e.hs.inject(room, "@bob:"+testServer, content)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			args := []string{"--as", "alice", consumer, "--json"}
			if consumer == "read" {
				args = append(args, room, "--event", id)
			}
			r := e.runCtx(ctx, args...)
			if !strings.Contains(r.stdout, id) {
				t.Fatalf("consumer did not output selected fixture: %+v", r)
			}
			if strings.Contains(r.stdout, `"media"`) || strings.Contains(r.stdout, "PEER-DECLARED-NAME-ONLY-IN-INBOX") {
				t.Fatalf("new peer metadata reached %s: %s", consumer, r.stdout)
			}
			records := decodeRecords(t, e.as("alice", "inbox", "--json").ok(t).stdout)
			if len(records) != 1 || records[0].Media == nil || records[0].Media.Name != "PEER-DECLARED-NAME-ONLY-IN-INBOX" {
				t.Fatalf("inbox projection missing: %+v", records)
			}
		})
	}
}

func TestEventRecordFormatControlsAreEscapedLosslessly(t *testing.T) {
	want := EventRecord{Type: "message", RoomID: "!room\U000e0001:synthetic", EventID: "$event\u2066", Sender: "@peer\u202e:synthetic"}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\U000e0001", "\u2066", "\u202e"} {
		if strings.Contains(string(raw), control) {
			t.Fatalf("literal format control: %s", raw)
		}
	}
	var got EventRecord
	if err := json.Unmarshal(raw, &got); err != nil || got.RoomID != want.RoomID || got.EventID != want.EventID || got.Sender != want.Sender {
		t.Fatalf("identifier changed: %+v %v", got, err)
	}
}

func TestElementMediaRedactedFetchCLI(t *testing.T) {
	for _, observation := range []int{1, 2} {
		t.Run(map[int]string{1: "initial", 2: "reobserve"}[observation], func(t *testing.T) {
			e, room := nativeWorld(t)
			e.as("alice", "inbox").ok(t)
			id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			e.as("alice", "inbox").ok(t)
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			original := e.hs.srv.Client().Transport
			calls := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: mediaCLITransport(func(r *http.Request) (*http.Response, error) {
					resp, err := original.RoundTrip(r)
					if err != nil || !strings.Contains(r.URL.Path, "/event/") {
						return resp, err
					}
					calls++
					if calls != observation {
						return resp, nil
					}
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					var ev map[string]any
					if err := json.Unmarshal(raw, &ev); err != nil {
						return nil, err
					}
					ev["unsigned"] = map[string]any{"redacted_because": map[string]any{}}
					ev["content"] = map[string]any{}
					raw, err = json.Marshal(ev)
					if err != nil {
						return nil, err
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			r := e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "out")).failsWith(t, ExitError, string(inboundmedia.Redacted))
			handle, _ := json.Marshal(inboundmedia.Handle{RoomID: room, EventID: id})
			if !strings.Contains(r.stderr, string(handle)) || r.stdout != "" || e.hs.CallCount(downloadRoute) != 0 || len(dirEntries(t, root)) != 0 {
				t.Fatalf("redaction contract: %+v", r)
			}
			e.configure = nil
			e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "valid")).ok(t)
		})
	}
}

// The second exact endpoint observation supplies all fields consumed by Fetch.
func TestElementMediaConsumesSecondLocatorAndSize(t *testing.T) {
	e, room := nativeWorld(t)
	id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	e.hs.mu.Lock()
	e.hs.media["second"] = fakeMedia{data: []byte("second"), contentType: "application/octet-stream"}
	e.hs.mu.Unlock()
	original := e.hs.srv.Client().Transport
	calls := 0
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: mediaCLITransport(func(r *http.Request) (*http.Response, error) {
			resp, err := original.RoundTrip(r)
			if err != nil || !strings.Contains(r.URL.Path, "/event/") {
				return resp, err
			}
			calls++
			if calls != 2 {
				return resp, nil
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			var ev map[string]any
			if err := json.Unmarshal(raw, &ev); err != nil {
				return nil, err
			}
			content := ev["content"].(map[string]any)
			content["url"] = "mxc://" + testServer + "/second"
			content["info"] = map[string]any{"size": 6}
			raw, err = json.Marshal(ev)
			if err != nil {
				return nil, err
			}
			resp.Body = io.NopCloser(strings.NewReader(string(raw)))
			resp.ContentLength = int64(len(raw))
			return resp, nil
		})}
	}
	r := e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "out")).ok(t)
	var result inboundmedia.Result
	if json.Unmarshal([]byte(r.stdout), &result) != nil || result.ActualSize != 6 || result.DeclaredSize == nil || *result.DeclaredSize != 6 || readFileStr(t, result.Path) != "second" {
		t.Fatalf("stale media fields consumed: %+v", r)
	}
}

func TestElementMediaLocalTombstoneBeforeFetch(t *testing.T) {
	e, room := nativeWorld(t)
	e.as("alice", "inbox").ok(t)
	target := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	neighbour := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	e.as("alice", "inbox").ok(t)
	if err := e.mailbox("alice").Redact([]config.Redaction{{Room: room, EventID: target}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	e.as("alice", "fetch", room, neighbour, "-o", filepath.Join(root, "valid")).ok(t)
	route := "GET /_matrix/client/v3/rooms/{room}/event/{event}"
	before := e.hs.CallCount(route)
	downloads := e.hs.CallCount(downloadRoute)
	// The server has stale unredacted content, then becomes unavailable.
	for _, unavailable := range []bool{false, true} {
		if unavailable {
			e.hs.fail(route, 500, "M_UNKNOWN", "synthetic lookup failure")
		}
		r := e.as("alice", "fetch", room, target, "-o", filepath.Join(root, "refused")).failsWith(t, ExitError, string(inboundmedia.Redacted))
		handle, _ := json.Marshal(inboundmedia.Handle{RoomID: room, EventID: target})
		if !strings.Contains(r.stderr, string(handle)) || r.stdout != "" || e.hs.CallCount(route) != before || e.hs.CallCount(downloadRoute) != downloads || len(dirEntries(t, root)) != 1 {
			t.Fatalf("local tombstone did not decide before network: %+v", r)
		}
	}
}
