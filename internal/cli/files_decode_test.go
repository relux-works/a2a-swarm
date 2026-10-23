package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const mediaConfigRoute = "GET /_matrix/client/v1/media/config"

func rawAttachmentEvent(e *env, header json.RawMessage) string {
	return e.hs.inject("!projRoomID", "@alice:"+testServer, map[string]any{"msgtype": "m.file", "body": "decode fixture", "ai.relux.a2a": header})
}

func rawFetch(t *testing.T, e *env, id string) (result, string) {
	t.Helper()
	dir := t.TempDir()
	return e.as("bob", "fetch", "proj", id, "-o", filepath.Join(dir, "out")), dir
}

func rawFetchControl(t *testing.T, e *env, header json.RawMessage, want []byte) {
	t.Helper()
	r, dir := rawFetch(t, e, rawAttachmentEvent(e, header))
	r.ok(t)
	if readFileStr(t, filepath.Join(dir, "out")) != string(want) || len(dirEntries(t, dir)) != 1 {
		t.Fatal("selected-value control changed bytes or left temporary files")
	}
}

func rawFetchRefusal(t *testing.T, e *env, id, cause string) {
	t.Helper()
	configs, downloads := e.hs.CallCount(mediaConfigRoute), e.hs.CallCount(downloadRoute)
	r, dir := rawFetch(t, e, id)
	if r.code != ExitError || !strings.Contains(r.stderr, cause) || r.stdout != "" || len(dirEntries(t, dir)) != 0 || e.hs.CallCount(mediaConfigRoute) != configs || e.hs.CallCount(downloadRoute) != downloads {
		t.Fatalf("malformed selected value: exit=%d configs=%d downloads=%d files=%v stdout=%q stderr=%q", r.code, e.hs.CallCount(mediaConfigRoute)-configs, e.hs.CallCount(downloadRoute)-downloads, dirEntries(t, dir), r.stdout, r.stderr)
	}
}

func jsonBytes(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wrapAttachment(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(`{"v":1,"type":"message","attachment":` + string(raw) + `}`)
}

// R2's permanent regression: an object/null/object sequence must validate the
// attachment actually consumed, not an earlier size; all four paths refuse
// before config/download/files. Ordinary and selected-valid controls succeed.
func TestReviewR2StoredSizeDecodeAgreement(t *testing.T) {
	for _, mode := range []string{"legacy", "future", "unknown-coding", "gzip"} {
		t.Run(mode, func(t *testing.T) {
			e, a, want := storedSizeFixture(t, mode, []byte("{\"a\":1}\n"))
			sizeControl(t, e, a, want)
			first := jsonBytes(t, a)
			raw := json.RawMessage(`{"v":1,"type":"message","attachment":` + string(first) + `,"attachment":null,"attachment":` + string(first) + `}`)
			rawFetchControl(t, e, raw, want)
			delete(a, "size")
			a["sha256"] = sumHex(nil)
			last := jsonBytes(t, a)
			raw = json.RawMessage(`{"v":1,"type":"message","attachment":` + string(first) + `,"attachment":null,"attachment":` + string(last) + `}`)
			e.hs.mu.Lock()
			e.hs.serveAs["sizefixture"] = []byte{}
			e.hs.mu.Unlock()
			id := rawAttachmentEvent(e, raw)
			ev, ok := e.hs.eventByID(id)
			if !ok {
				t.Fatal("no fixture event")
			}
			selected, ok := attachmentOf(ev)
			if !ok || selected.Size != 0 {
				t.Fatal("fixture did not preserve the repeated-key unsized attachment")
			}
			rawFetchRefusal(t, e, id, "size")
		})
	}
}

// Repeated version/coding/original.size keys use the selected raw value for
// both validation and consumption. Invalid final values refuse before config
// or download; a final valid value still decodes or falls back byte-exactly.
func TestFetchEncodingDuplicateSelection(t *testing.T) {
	for _, mode := range []string{"future", "unknown-coding", "gzip"} {
		for _, field := range []string{"version", "coding", "original.size"} {
			if mode == "future" && field != "version" {
				continue // Unknown versions leave all other fields opaque.
			}
			for _, bad := range []string{"null", "{}", "1.5"} {
				t.Run(mode+"/"+field+"/"+bad, func(t *testing.T) {
					var original []byte
					if mode == "future" {
						original = []byte("future body")
					}
					e, a, want := storedSizeFixture(t, mode, original)
					sizeControl(t, e, a, want)
					enc := a["encoding"].(map[string]any)
					key, value := field, enc[field]
					if field == "original.size" {
						key, value = "size", enc["original"].(map[string]any)["size"]
					}
					prefix := `"` + key + `":`
					member := prefix + string(jsonBytes(t, value))
					encoded := string(jsonBytes(t, enc))
					if strings.Count(encoded, member) != 1 {
						t.Fatal("duplicate-member fixture is ambiguous")
					}
					a["encoding"] = json.RawMessage(strings.Replace(encoded, member, prefix+bad+","+member, 1))
					rawFetchControl(t, e, wrapAttachment(jsonBytes(t, a)), want)
					a["encoding"] = json.RawMessage(strings.Replace(encoded, member, member+","+prefix+"null,"+prefix+bad, 1))
					cause := field
					rawFetchRefusal(t, e, rawAttachmentEvent(e, wrapAttachment(jsonBytes(t, a))), cause)
				})
			}
		}
	}
}

// Rebinding an encoding/original object through null must not resurrect a
// missing version/size from the earlier object. Final complete objects work;
// final incomplete objects refuse before config/download or any file writes.
func TestFetchEncodingParentReplacement(t *testing.T) {
	for _, mode := range []string{"future", "unknown-coding", "gzip"} {
		for _, parent := range []string{"encoding", "original"} {
			if mode == "future" && parent == "original" {
				continue
			}
			t.Run(mode+"/"+parent, func(t *testing.T) {
				var original []byte
				if mode == "future" {
					original = []byte("future body")
				}
				e, a, want := storedSizeFixture(t, mode, original)
				sizeControl(t, e, a, want)
				container := a
				missing := "version"
				cause := "version"
				if parent == "original" {
					container = a["encoding"].(map[string]any)
					missing, cause = "size", "original.size"
				}
				value := container[parent].(map[string]any)
				first := string(jsonBytes(t, value))
				member := `"` + parent + `":` + first
				encoded := string(jsonBytes(t, a))
				if strings.Count(encoded, member) != 1 {
					t.Fatal("parent-replacement fixture is ambiguous")
				}
				prefix := `"` + parent + `":`
				control := strings.Replace(encoded, member, prefix+"null,"+member, 1)
				rawFetchControl(t, e, wrapAttachment(json.RawMessage(control)), want)
				delete(value, missing)
				last := string(jsonBytes(t, value))
				bad := strings.Replace(encoded, member, member+","+prefix+"null,"+prefix+last, 1)
				rawFetchRefusal(t, e, rawAttachmentEvent(e, wrapAttachment(json.RawMessage(bad))), cause)
			})
		}
	}
}
