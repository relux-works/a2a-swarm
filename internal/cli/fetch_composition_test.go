package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/inboundmedia"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Each refusal has a valid legacy neighbour in both command modes. The same
// event must never download through batch after single fetch has refused it.
func TestFetchCompositionSharedMediaGates(t *testing.T) {
	for _, mode := range []string{"local-tombstone", "server-redacted", "encrypted-legacy", "oversized-dispatch", "native"} {
		t.Run(mode, func(t *testing.T) {
			e, good, data, _ := attachWorld(t)
			fields := attachmentFields(t, e, good)
			fields["name"] = "refused.bin"
			content := map[string]any{"msgtype": "m.file", "body": "fixture", "ai.relux.a2a": map[string]any{"attachment": fields}}
			if mode == "encrypted-legacy" {
				content["file"] = nil
			}
			if mode == "oversized-dispatch" {
				content["body"] = strings.Repeat("x", matrix.VerificationEntityLimit)
			}
			if mode == "native" {
				content = elementContent("m.file")
				e.hs.mu.Lock()
				e.hs.media["native"] = fakeMedia{data: []byte("valid"), contentType: "application/octet-stream"}
				e.hs.mu.Unlock()
			}
			target := e.hs.inject("!projRoomID", "@alice:"+testServer, content)
			if mode == "local-tombstone" {
				if err := e.mailbox("bob").Redact([]config.Redaction{{Room: "!projRoomID", EventID: target}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "server-redacted" {
				e.hs.redact(target, "@alice:"+testServer)
			}
			root := outputTestDir(t)
			t.Setenv("TMPDIR", root)
			before := e.hs.CallCount(downloadRoute)
			eventRoute := "GET /_matrix/client/v3/rooms/{room}/event/{event}"
			beforeEvents := e.hs.CallCount(eventRoute)
			dir := filepath.Join(root, "mixed")
			r := e.as("bob", "fetch", "proj", target, good, "-o", dir).failsWith(t, ExitError, "1 attachment(s) failed")
			want := fmt.Sprintf("%s\t-\t-\t0\tmetadata_error\n%s\tnotes.txt\t%s\t%d\tok\n", target, good, sumHex(data), len(data))
			if r.stdout != want || e.hs.CallCount(downloadRoute) != before+1 || len(dirEntries(t, dir)) != 1 || readFileStr(t, filepath.Join(dir, "notes.txt")) != string(data) {
				t.Fatal("batch bypassed media gate, changed fixed rows, or lost valid neighbour")
			}
			if mode == "local-tombstone" && e.hs.CallCount(eventRoute) != beforeEvents+1 {
				t.Fatal("batch tombstone reached event endpoint before refusal")
			}
			before = e.hs.CallCount(downloadRoute)
			out := filepath.Join(root, "single")
			if mode == "native" {
				r = e.as("bob", "fetch", "proj", target, "-o", out).ok(t)
				var native inboundmedia.Result
				if json.Unmarshal([]byte(r.stdout), &native) != nil || native.Path != out || native.HashStatus != "computed_unverified" || native.SHA256 != sumHex([]byte("valid")) || readFileStr(t, out) != "valid" || e.hs.CallCount(downloadRoute) != before+1 {
					t.Fatal("single native fetch contract changed")
				}
			} else {
				code := inboundmedia.Redacted
				if mode == "encrypted-legacy" {
					code = inboundmedia.Encrypted
				}
				if mode == "oversized-dispatch" {
					code = inboundmedia.ContextInvalid
				}
				r = e.as("bob", "fetch", "proj", target, "-o", out).failsWith(t, ExitError, string(code))
				if r.stdout != "" || e.hs.CallCount(downloadRoute) != before {
					t.Fatal("single media refusal downloaded or printed success")
				}
				if _, err := os.Lstat(out); !os.IsNotExist(err) {
					t.Fatal("single refusal left output")
				}
			}
			e.as("bob", "fetch", "proj", good, "-o", filepath.Join(root, "legacy-control")).ok(t)
			if readFileStr(t, filepath.Join(root, "legacy-control")) != string(data) {
				t.Fatal("single legacy control changed")
			}
			if mode == "local-tombstone" {
				route := "GET /_matrix/client/v3/rooms/{room}/event/{event}"
				beforeEvents := e.hs.CallCount(route)
				before = e.hs.CallCount(downloadRoute)
				e.hs.fail(route, 500, "M_UNKNOWN", "synthetic unavailable event endpoint")
				e.as("bob", "fetch", "proj", target, "-o", filepath.Join(root, "offline-refused")).failsWith(t, ExitError, string(inboundmedia.Redacted))
				if e.hs.CallCount(route) != beforeEvents || e.hs.CallCount(downloadRoute) != before {
					t.Fatal("local tombstone contacted unavailable endpoint")
				}
			}
		})
	}
}

// Single legacy fetch retains its explicit caller-selected output semantics;
// the stricter batch TSV name gate must not change these bytes or destinations.
func TestFetchCompositionUnicodeSingleOutput(t *testing.T) {
	for _, name := range []string{"x\u2028name", "x\u2029name", "x\u2027name"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			e, good, data, _ := attachWorld(t)
			fields := attachmentFields(t, e, good)
			fields["name"] = name
			id := injectFileAttachment(e, fields)
			out := filepath.Join(outputTestDir(t), "chosen.bin")
			before := e.hs.CallCount(downloadRoute)
			r := e.as("bob", "fetch", "proj", id, "-o", out).ok(t)
			if r.stdout != out+"\n" || readFileStr(t, out) != string(data) || e.hs.CallCount(downloadRoute) != before+1 {
				t.Fatal("explicit single output changed under peer Unicode filename")
			}
		})
	}
}
