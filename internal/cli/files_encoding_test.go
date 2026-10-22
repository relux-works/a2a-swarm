package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// canonicalMediaObject models tuwunel's object rewrite, leaving other media raw.
func canonicalMediaObject(body []byte) []byte {
	var object map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if !json.Valid(body) || dec.Decode(&object) != nil || object == nil {
		return body
	}
	result, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return result
}

func encodingWorld(t *testing.T) *env {
	t.Helper()
	e, _ := world(t)
	e.hs.mu.Lock()
	e.hs.uploadRewrite = canonicalMediaObject
	e.hs.mu.Unlock()
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	return e
}
func attachmentFields(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	ev, ok := e.hs.eventByID(id)
	if !ok {
		t.Fatal("no attachment event")
	}
	return decodeContent(t, ev)["ai.relux.a2a"].(map[string]any)["attachment"].(map[string]any)
}
func attachmentMedia(t *testing.T, e *env, att map[string]any) (string, fakeMedia) {
	t.Helper()
	m, err := matrix.ParseMXC(att["mxc"].(string))
	if err != nil {
		t.Fatal(err)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return m.MediaID, e.hs.media[m.MediaID]
}
func injectFileAttachment(e *env, fields map[string]any) string {
	return e.hs.inject("!projRoomID", "@alice:"+testServer, map[string]any{"msgtype": "m.file", "body": "fixture", "ai.relux.a2a": map[string]any{"v": 1, "type": "message", "attachment": fields}})
}
func originalFields(att map[string]any) map[string]any {
	return att["encoding"].(map[string]any)["original"].(map[string]any)
}
func fixtureGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func storeFixture(t *testing.T, e *env, body []byte) string {
	t.Helper()
	client := matrix.New(e.hs.srv.URL, e.tokens["alice"])
	client.HTTP = e.hs.srv.Client()
	mxc, err := client.Upload(context.Background(), "fixture.gz", "application/gzip", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	return mxc
}
func encodingFixture(t *testing.T) (*env, map[string]any, []byte, []byte) {
	t.Helper()
	e := encodingWorld(t)
	original := []byte(" \n{\n  \"z\": {\"b\":2, \"a\":\"\\u0061\"},\n  \"a\":\"hello\"\n}\n")
	stored := fixtureGzip(t, original)
	att := map[string]any{"name": "report.json.gz", "size": len(stored), "sha256": sumHex(stored), "mxc": storeFixture(t, e, stored), "mimetype": "application/gzip", "encoding": map[string]any{
		"version": "a2a.attachment.encoding/1", "coding": "gzip", "reason": "matrix-construct/tuwunel#666", "original": map[string]any{"name": "report.json", "size": len(original), "sha256": sumHex(original), "mimetype": "text/plain; charset=utf-8"}}}
	return e, att, original, stored
}
func fetchFixture(t *testing.T, e *env, att map[string]any) (result, string) {
	t.Helper()
	dir := t.TempDir()
	id := injectFileAttachment(e, att)
	return e.as("bob", "fetch", "proj", id, "-o", filepath.Join(dir, "result")), dir
}
func expectFetchRefusal(t *testing.T, e *env, att map[string]any, want string) {
	t.Helper()
	r, dir := fetchFixture(t, e, att)
	r.failsWith(t, ExitError, want)
	if r.stdout != "" || len(dirEntries(t, dir)) != 0 {
		t.Fatalf("refusal wrote output or left files: %+v %v", r, dirEntries(t, dir))
	}
}
func expectValidEncoding(t *testing.T, e *env, att map[string]any, body []byte) {
	t.Helper()
	r, dir := fetchFixture(t, e, att)
	r.ok(t)
	if readFileStr(t, filepath.Join(dir, "result")) != string(body) || len(dirEntries(t, dir)) != 1 {
		t.Fatal("valid control changed bytes or left temp files")
	}
}

// Sections 1/3/4/6/8: gzip survives the rewriting server, restores the original
// name/bytes, is deterministic, and remains verifiable by an old client shape.
func TestAttachmentEncodingRoundTrip(t *testing.T) {
	e := encodingWorld(t)
	original := []byte(" \n{\"z\":2, \"a\":1}\n")
	p := writeFile(t, t.TempDir(), "report.json", original)
	id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", p).ok(t).stdout)
	att := attachmentFields(t, e, id)
	enc, ok := att["encoding"].(map[string]any)
	if !ok {
		t.Fatalf("encoding is not a descriptor: %+v", att)
	}
	orig := originalFields(att)
	if len(att) != 6 || len(enc) != 4 || len(orig) != 4 || att["name"] != "report.json.gz" || att["mimetype"] != "application/gzip" || enc["version"] != "a2a.attachment.encoding/1" || enc["coding"] != "gzip" || enc["reason"] != "matrix-construct/tuwunel#666" || orig["name"] != "report.json" || orig["size"] != float64(len(original)) || orig["sha256"] != sumHex(original) || orig["mimetype"] != "text/plain; charset=utf-8" {
		t.Fatalf("wire contract: %+v", att)
	}
	_, media := attachmentMedia(t, e, att)
	if media.contentType != "application/gzip" || media.filename != "report.json.gz" || att["size"] != float64(len(media.data)) || att["sha256"] != sumHex(media.data) || media.data[3] != 0 || !bytes.Equal(media.data[4:8], make([]byte, 4)) {
		t.Fatalf("stored metadata/gzip headers: %+v", att)
	}
	ev, _ := e.hs.eventByID(id)
	content := decodeContent(t, ev)
	info := content["info"].(map[string]any)
	if content["filename"] != att["name"] || info["size"] != att["size"] || info["mimetype"] != att["mimetype"] {
		t.Fatal("Matrix file metadata is not stored-byte metadata")
	}
	// Legacy clients ignore encoding and verify the stored representation.
	var legacy struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
		MXC      string `json:"mxc"`
		MimeType string `json:"mimetype"`
	}
	raw, _ := json.Marshal(att)
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Name != "report.json.gz" || legacy.Size != int64(len(media.data)) || legacy.SHA256 != sumHex(media.data) {
		t.Fatal("old-client integrity is broken")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	e.as("bob", "fetch", "proj", id).ok(t)
	if readFileStr(t, filepath.Join(dir, "report.json")) != string(original) || !reflect.DeepEqual(dirEntries(t, dir), []string{"report.json"}) {
		t.Fatal("decoded original/name not restored")
	}
	// A decoded fetch also preserves an existing target and leaves no temp files.
	e.as("bob", "fetch", "proj", id).failsWith(t, ExitError, "already exists")
	if readFileStr(t, filepath.Join(dir, "report.json")) != string(original) || !reflect.DeepEqual(dirEntries(t, dir), []string{"report.json"}) {
		t.Fatal("refused overwrite changed target or left temp files")
	}
	id2 := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", p).ok(t).stdout)
	att2 := attachmentFields(t, e, id2)
	_, media2 := attachmentMedia(t, e, att2)
	if att2["sha256"] != att["sha256"] || !bytes.Equal(media.data, media2.data) {
		t.Fatal("nondeterministic gzip")
	}
	bare := filepath.Join(t.TempDir(), "bare.gz")
	e.as("bob", "fetch", legacy.MXC, "--sha256", legacy.SHA256, "-o", bare).ok(t)
	if readFileStr(t, bare) != string(media.data) {
		t.Fatal("bare fetch must preserve stored bytes")
	}
}

// Sections 3/8: only valid objects encode; raw controls and old unencoded
// events remain byte-exact, including BOM, arrays/scalars and invalid JSON.
func TestAttachmentEncodingRawControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		encoded bool
	}{
		{"object", []byte("{}"), true}, {"whitespace-float", []byte(strings.Repeat(" \t\r\n", 200) + "{\"x\":1.25}\n"), true},
		{"array", []byte("[ {\"z\":2, \"a\":1} ]\n"), false}, {"scalar", []byte("\"hello\"\n"), false}, {"null", []byte("null\n"), false},
		{"plain-text", []byte("hello\n"), false},
		{"bom", append([]byte{239, 187, 191}, []byte("{}\n")...), false}, {"markdown", []byte("# Text\nhello\n"), false}, {"binary", []byte{0, 1, 2, 255, 0}, false},
		{"invalid", []byte("{\"x\":oops}"), false}, {"trailing-json", []byte("{} {}"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := encodingWorld(t)
			p := writeFile(t, t.TempDir(), "input.dat", tc.body)
			id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", p).ok(t).stdout)
			att := attachmentFields(t, e, id)
			_, media := attachmentMedia(t, e, att)
			_, encoded := att["encoding"]
			if encoded != tc.encoded {
				t.Fatalf("encoding=%v want %v", att, tc.encoded)
			}
			if !encoded && (len(att) != 5 || !bytes.Equal(media.data, tc.body)) {
				t.Fatal("raw bytes or legacy shape changed")
			}
			expectValidEncoding(t, e, att, tc.body)
		})
	}
}

// Section 8's regression control actually uploads the original object without
// gzip: the rewrite hook changes it and legacy integrity verification refuses.
func TestAttachmentEncodingRewriteControl(t *testing.T) {
	e, _, original, _ := encodingFixture(t)
	stored := canonicalMediaObject(original)
	client := matrix.New(e.hs.srv.URL, e.tokens["alice"])
	client.HTTP = e.hs.srv.Client()
	mxc, err := client.Upload(context.Background(), "report.json", "text/plain", bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	att := map[string]any{"name": "report.json", "size": len(original), "sha256": sumHex(original), "mxc": mxc, "mimetype": "text/plain"}
	_, media := attachmentMedia(t, e, att)
	if bytes.Equal(stored, original) || !bytes.Equal(media.data, stored) {
		t.Fatal("rewrite control was not applied")
	}
	expectFetchRefusal(t, e, att, "size mismatch")
	_, good, body, _ := encodingFixture(t)
	good["mxc"] = storeFixture(t, e, fixtureGzip(t, body))
	expectValidEncoding(t, e, good, body)
}

// A1.1's malformed rows refuse instead of falling back; each malformed field
// has an unchanged valid v1 gzip control, and refusal leaves no target/temp.
func TestFetchEncodingDescriptors(t *testing.T) {
	type mutation struct {
		name  string
		apply func(map[string]any)
		want  string
	}
	mutations := []mutation{
		{"null", func(a map[string]any) { a["encoding"] = nil }, "encoding"},
		{"string", func(a map[string]any) { a["encoding"] = "gzip" }, "encoding"},
		{"array", func(a map[string]any) { a["encoding"] = []any{} }, "encoding"},
		{"number", func(a map[string]any) { a["encoding"] = 1 }, "encoding"},
	}
	for _, field := range []string{"version", "coding", "reason", "original"} {
		for _, bad := range []string{"missing", "null", "empty", "number", "array"} {
			if field == "reason" && (bad == "missing" || bad == "empty") {
				continue
			}
			f, b := field, bad
			mutations = append(mutations, mutation{f + "/" + b, func(a map[string]any) {
				m := a["encoding"].(map[string]any)
				switch b {
				case "missing":
					delete(m, f)
				case "null":
					m[f] = nil
				case "empty":
					m[f] = ""
				case "number":
					m[f] = 1
				case "array":
					m[f] = []any{}
				}
			}, field})
		}
	}
	for _, field := range []string{"name", "size", "sha256", "mimetype"} {
		for _, bad := range []string{"missing", "null", "wrong-type", "invalid"} {
			if field == "mimetype" && bad == "invalid" {
				continue
			}
			f, b := field, bad
			mutations = append(mutations, mutation{"original/" + f + "/" + b, func(a map[string]any) {
				m := originalFields(a)
				switch b {
				case "missing":
					delete(m, f)
				case "null":
					m[f] = nil
				case "wrong-type":
					if f == "size" {
						m[f] = "53"
					} else {
						m[f] = 1
					}
				case "invalid":
					switch f {
					case "name":
						m[f] = "../escape"
					case "size":
						m[f] = -1
					case "sha256":
						m[f] = strings.Repeat("A", 64)
					}
				}
			}, "original." + f})
		}
	}
	mutations = append(mutations, mutation{"unknown-coding-malformed-original", func(a map[string]any) {
		a["encoding"].(map[string]any)["coding"] = "zstd"
		delete(originalFields(a), "sha256")
	}, "original.sha256"})
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			e, a, body, _ := encodingFixture(t)
			good := injectFileAttachment(e, a)
			tc.apply(a)
			expectFetchRefusal(t, e, a, tc.want)
			expectValidEncoding(t, e, attachmentFields(t, e, good), body)
		})
	}
}

// A1.1 unknown versions are opaque, while unknown codings require a complete
// v1 envelope; both fallbacks verify stored bytes before one stderr notice.
func TestFetchEncodingFallbacks(t *testing.T) {
	for _, version := range []bool{true, false} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			e, a, original, stored := encodingFixture(t)
			good := injectFileAttachment(e, a)
			if version {
				a["encoding"] = map[string]any{"version": "future/v2", "coding": nil, "original": json.Number("9223372036854775808"), "reason": []any{}}
			} else {
				a["encoding"].(map[string]any)["coding"] = "zstd"
			}
			dir := t.TempDir()
			t.Chdir(dir)
			id := injectFileAttachment(e, a)
			r := e.as("bob", "fetch", "proj", id).ok(t)
			if readFileStr(t, filepath.Join(dir, "report.json.gz")) != string(stored) || strings.Count(r.stderr, "not supported; saved the stored bytes as") != 1 || !reflect.DeepEqual(dirEntries(t, dir), []string{"report.json.gz"}) {
				t.Fatalf("fallback contract: %+v", r)
			}
			// An unknown descriptor never turns a corrupt stored body into success.
			mediaID, _ := attachmentMedia(t, e, a)
			bad := append([]byte(nil), stored...)
			bad[0] ^= 1
			e.hs.mu.Lock()
			e.hs.serveAs[mediaID] = bad
			e.hs.mu.Unlock()
			rr, outdir := fetchFixture(t, e, a)
			rr.failsWith(t, ExitError, "sha256 mismatch")
			if rr.stdout != "" || strings.Contains(rr.stderr, "saved the stored bytes") || len(dirEntries(t, outdir)) != 0 {
				t.Fatal("unverified fallback published or left files")
			}
			e.hs.mu.Lock()
			delete(e.hs.serveAs, mediaID)
			e.hs.mu.Unlock()
			expectValidEncoding(t, e, attachmentFields(t, e, good), original)
		})
	}
	// Optional reason and unknown extra keys do not make a valid v1 malformed.
	t.Run("optional-and-extra", func(t *testing.T) {
		e, a, body, _ := encodingFixture(t)
		enc := a["encoding"].(map[string]any)
		delete(enc, "reason")
		enc["future"] = []any{1}
		originalFields(a)["future"] = true
		expectValidEncoding(t, e, a, body)
	})
}

// Sections 4/6/8: stored integrity precedes decoding, and original mismatch,
// truncated/trailing/second-member streams and footer errors leave no files.
func TestFetchEncodingCorruption(t *testing.T) {
	for _, fault := range []string{"stored-hash", "stored-short", "stored-long", "original-hash", "decoded-long", "decoded-short", "truncated", "trailing", "second-member", "crc", "isize"} {
		t.Run(fault, func(t *testing.T) {
			e, a, original, stored := encodingFixture(t)
			good := injectFileAttachment(e, a)
			data := append([]byte(nil), stored...)
			want := ""
			switch fault {
			case "stored-hash":
				data[0] ^= 1
				want = "sha256 mismatch"
			case "stored-short":
				data = data[:len(data)-1]
				want = "size mismatch"
			case "stored-long":
				data = append(data, 1)
				want = "more than"
			case "original-hash":
				originalFields(a)["sha256"] = strings.Repeat("0", 64)
				want = "original sha256 mismatch"
			case "decoded-long":
				originalFields(a)["size"] = len(original) - 1
				want = "decoded output exceeds"
			case "decoded-short":
				originalFields(a)["size"] = len(original) + 1
				want = "original size mismatch"
			case "truncated":
				data = data[:len(data)-1]
				a["size"] = len(data)
				a["sha256"] = sumHex(data)
				want = "gzip"
			case "trailing":
				data = append(data, 0)
				a["size"] = len(data)
				a["sha256"] = sumHex(data)
				want = "trailing"
			case "second-member":
				data = append(data, fixtureGzip(t, []byte("second"))...)
				a["size"] = len(data)
				a["sha256"] = sumHex(data)
				want = "trailing"
			case "crc":
				data[len(data)-8] ^= 1
				a["sha256"] = sumHex(data)
				want = "gzip"
			case "isize":
				data[len(data)-4] ^= 1
				a["sha256"] = sumHex(data)
				want = "gzip"
			}
			mediaID, _ := attachmentMedia(t, e, a)
			e.hs.mu.Lock()
			e.hs.serveAs[mediaID] = data
			e.hs.mu.Unlock()
			expectFetchRefusal(t, e, a, want)
			e.hs.mu.Lock()
			delete(e.hs.serveAs, mediaID)
			e.hs.mu.Unlock()
			expectValidEncoding(t, e, attachmentFields(t, e, good), original)
		})
	}
}

// A1.2 checks both sizes, including extreme integer inputs, before downloading;
// invalid JSON integer ranges refuse rather than wrap to an allowed size.
func TestFetchEncodingCaps(t *testing.T) {
	for _, field := range []string{"stored", "original"} {
		for _, value := range []string{strconv.FormatInt(math.MaxInt64, 10), strconv.FormatInt(math.MinInt64, 10), "-1", strconv.FormatInt((24<<20)+1, 10), strconv.FormatInt((1<<40)+1, 10), "9223372036854775808", "1.5"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				e, a, body, _ := encodingFixture(t)
				good := injectFileAttachment(e, a)
				if field == "stored" {
					a["size"] = json.Number(value)
				} else {
					originalFields(a)["size"] = json.Number(value)
				}
				before := e.hs.CallCount(downloadRoute)
				want := "size"
				if field == "stored" && (value == "9223372036854775808" || value == "1.5") {
					want = "attachment"
				}
				expectFetchRefusal(t, e, a, want)
				if e.hs.CallCount(downloadRoute) != before {
					t.Fatal("bad cap downloaded media")
				}
				expectValidEncoding(t, e, attachmentFields(t, e, good), body)
			})
		}
	}
	t.Run("zero-gzip", func(t *testing.T) {
		e, a, body, _ := encodingFixture(t)
		good := injectFileAttachment(e, a)
		a["size"] = 0
		expectFetchRefusal(t, e, a, "attachment has no valid size")
		expectValidEncoding(t, e, attachmentFields(t, e, good), body)
	})
	t.Run("no-server-limit", func(t *testing.T) {
		e, a, body, _ := encodingFixture(t)
		e.hs.mu.Lock()
		e.hs.uploadMax = 0
		e.hs.mu.Unlock()
		expectFetchRefusal(t, e, a, "no upload limit")
		e.hs.mu.Lock()
		e.hs.uploadMax = 24 << 20
		e.hs.mu.Unlock()
		expectValidEncoding(t, e, a, body)
	})
}

// Section 7: JSON retains the descriptor and text/archive show original
// metadata; existing tests keep unencoded output byte-identical.
func TestFilesEncodingDisplays(t *testing.T) {
	e, a, _, _ := encodingFixture(t)
	id := injectFileAttachment(e, a)
	text := e.as("bob", "files", "proj").ok(t).stdout
	for _, part := range []string{"encoding=a2a.attachment.encoding/1/gzip", "original_name=report.json", "original_size=", "original_sha256=" + originalFields(a)["sha256"].(string)} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %s: %q", part, text)
		}
	}
	for _, args := range [][]string{{"files", "proj", "--json"}, {"read", "proj", "--event", id, "--json"}} {
		records := decodeRecords(t, e.as("bob", args...).ok(t).stdout)
		if len(records) != 1 {
			t.Fatal("missing file record")
		}
		encoded, err := json.Marshal(records[0].Attachment)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		json.Unmarshal(encoded, &fields)
		if !reflect.DeepEqual(fields["encoding"], attachmentFields(t, e, id)["encoding"]) {
			t.Fatalf("descriptor lost: %s", encoded)
		}
	}
	dir := t.TempDir()
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	archive := readFileStr(t, filepath.Join(dir, "proj", RoomFileName))
	if !strings.Contains(archive, "original_name=report.json") || !strings.Contains(archive, "original_sha256="+originalFields(a)["sha256"].(string)) {
		t.Fatalf("missing archive original metadata: %s", archive)
	}
}

// A1.2's boundary comparison accepts exactly 1024:1 and refuses one byte more;
// this tests declared-number validation, separately from gzip stream validity.
func TestAttachmentEncodingRatioBoundary(t *testing.T) {
	if err := validateGzipSizes(1024, 1024*1024, 1<<40); err != nil {
		t.Fatalf("exact ratio refused: %v", err)
	}
	if err := validateGzipSizes(1024, 1024*1024+1, 1<<40); err == nil || !strings.Contains(err.Error(), "1024:1") {
		t.Fatalf("over-ratio declaration accepted: %v", err)
	}
	if err := validateGzipSizes(1<<40, 1<<40, 1<<40); err != nil {
		t.Fatalf("clamped upper bound overflowed: %v", err)
	}
	e, a, body, stored := encodingFixture(t)
	good := injectFileAttachment(e, a)
	originalFields(a)["size"] = 1024*len(stored) + 1
	before := e.hs.CallCount(downloadRoute)
	expectFetchRefusal(t, e, a, "1024:1")
	if e.hs.CallCount(downloadRoute) != before {
		t.Fatal("over-ratio event downloaded bytes")
	}
	expectValidEncoding(t, e, attachmentFields(t, e, good), body)
}

// A1.2 clamps even an extreme server limit to 2^40 before checking either size;
// the unchanged small attachment is a valid end-to-end control at that limit.
func TestFetchEncodingLimitClamp(t *testing.T) {
	for _, field := range []string{"stored", "original"} {
		t.Run(field, func(t *testing.T) {
			e, a, body, _ := encodingFixture(t)
			good := injectFileAttachment(e, a)
			e.hs.mu.Lock()
			e.hs.uploadMax = math.MaxInt64
			e.hs.mu.Unlock()
			if field == "stored" {
				a["size"] = int64(1<<40) + 1
			} else {
				originalFields(a)["size"] = int64(1<<40) + 1
			}
			before := e.hs.CallCount(downloadRoute)
			expectFetchRefusal(t, e, a, "size")
			if e.hs.CallCount(downloadRoute) != before {
				t.Fatal("unclamped size downloaded media")
			}
			expectValidEncoding(t, e, attachmentFields(t, e, good), body)
		})
	}
}

// A1.1 permits zero original size and empty string mimetype/reason; the gzip
// representation is nonempty, as required for stored size by Erratum 1.
func TestFetchEncodingZeroSizes(t *testing.T) {
	e, a, _, _ := encodingFixture(t)
	stored := fixtureGzip(t, nil)
	a["mxc"] = storeFixture(t, e, stored)
	a["size"] = len(stored)
	a["sha256"] = sumHex(stored)
	orig := originalFields(a)
	orig["size"] = 0
	orig["sha256"] = sumHex(nil)
	orig["mimetype"] = ""
	a["encoding"].(map[string]any)["reason"] = ""
	expectValidEncoding(t, e, a, nil)
}

// Sections 5/A1.2: attach refuses original/stored/ratio excess before upload or
// message publication, never raw-falling-back; a smaller object still works.
func TestAttachEncodingCaps(t *testing.T) {
	for _, fault := range []string{"original", "stored", "ratio", "clamp"} {
		t.Run(fault, func(t *testing.T) {
			e := encodingWorld(t)
			body := []byte("{}")
			if fault == "ratio" {
				body = append([]byte("{\"x\":\""), bytes.Repeat([]byte("x"), 8<<20)...)
				body = append(body, []byte("\"}")...)
				compressed := fixtureGzip(t, body)
				if int64(len(body)) <= 1024*int64(len(compressed)) {
					t.Fatal("ratio witness no longer exceeds cap; requalify for this Go compressor")
				}
			}
			path := writeFile(t, t.TempDir(), "input.json", body)
			e.hs.mu.Lock()
			initialEvents := len(e.hs.events)
			switch fault {
			case "original":
				e.hs.uploadMax = 1
			case "stored":
				e.hs.uploadMax = 3
			case "clamp":
				e.hs.uploadMax = math.MaxInt64
			}
			e.hs.mu.Unlock()
			if fault == "clamp" {
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate((1 << 40) + 1)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			want := "upload limit"
			if fault == "stored" {
				want = "stored size"
			}
			if fault == "ratio" {
				want = "1024:1"
			}
			r := e.as("alice", "attach", "proj", "--topic", "alpha", path).failsWith(t, ExitError, want)
			e.hs.mu.Lock()
			mediaCount := len(e.hs.media)
			eventCount := len(e.hs.events)
			e.hs.uploadMax = 24 << 20
			e.hs.mu.Unlock()
			if r.stdout != "" || mediaCount != 0 || eventCount != initialEvents {
				t.Fatal("attach refusal uploaded media or published a message")
			}
			path = writeFile(t, t.TempDir(), "good.json", []byte("{\"x\":1}"))
			id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", path).ok(t).stdout)
			if _, ok := attachmentFields(t, e, id)["encoding"].(map[string]any); !ok {
				t.Fatal("control fell back to raw")
			}
		})
	}
}
