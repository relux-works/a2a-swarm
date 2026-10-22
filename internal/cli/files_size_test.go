package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"path/filepath"
	"testing"
)

// This fixture uses only legacy test helpers so the legacy cases can also run
// against the exact base checkout, independently of the encoding implementation.
func storedSizeFixture(t *testing.T, mode string, original []byte) (*env, map[string]any, []byte) {
	t.Helper()
	e, _ := world(t)
	stored := original
	if mode == "gzip" || mode == "unknown-coding" {
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		if _, err := z.Write(original); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		stored = b.Bytes()
	}
	e.hs.mu.Lock()
	e.hs.media["sizefixture"] = fakeMedia{data: stored}
	e.hs.mu.Unlock()
	a := map[string]any{"name": "fixture", "size": len(stored), "sha256": sumHex(stored), "mxc": "mxc://" + testServer + "/sizefixture", "mimetype": "application/octet-stream"}
	switch mode {
	case "future":
		a["encoding"] = map[string]any{"version": "future/2"}
	case "gzip", "unknown-coding":
		coding := "gzip"
		if mode == "unknown-coding" {
			coding = "zstd"
		}
		a["encoding"] = map[string]any{"version": "a2a.attachment.encoding/1", "coding": coding, "original": map[string]any{
			"name": "original", "size": len(original), "sha256": sumHex(original), "mimetype": "application/json"}}
	}
	want := stored
	if mode == "gzip" {
		want = original
	}
	return e, a, want
}

func sizeFetch(t *testing.T, e *env, a map[string]any) (result, string) {
	t.Helper()
	id := e.hs.inject("!projRoomID", "@alice:"+testServer, map[string]any{"msgtype": "m.file", "body": "size fixture", "ai.relux.a2a": map[string]any{"v": 1, "type": "message", "attachment": a}})
	dir := t.TempDir()
	return e.as("bob", "fetch", "proj", id, "-o", filepath.Join(dir, "out")), dir
}

func sizeControl(t *testing.T, e *env, a map[string]any, want []byte) {
	t.Helper()
	r, dir := sizeFetch(t, e, a)
	r.ok(t)
	if readFileStr(t, filepath.Join(dir, "out")) != string(want) || len(dirEntries(t, dir)) != 1 {
		t.Fatal("valid size control changed bytes or left temporary files")
	}
}

func sizeRefusal(t *testing.T, e *env, a map[string]any, cause string) {
	t.Helper()
	before := e.hs.CallCount(downloadRoute)
	r, dir := sizeFetch(t, e, a)
	r.failsWith(t, ExitError, cause)
	if e.hs.CallCount(downloadRoute) != before || r.stdout != "" || len(dirEntries(t, dir)) != 0 {
		t.Fatalf("invalid size downloaded media or wrote files: %+v, files=%v", r, dirEntries(t, dir))
	}
}

// Erratum 1: missing/null/zero/negative stored sizes refuse before download on
// every dispatch path, even with the empty-body hash; nonzero controls work.
// The legacy subtests also assert the base error, exit, read and output behavior.
func TestFetchStoredSizeRequired(t *testing.T) {
	for _, mode := range []string{"legacy", "future", "unknown-coding", "gzip"} {
		for _, bad := range []string{"missing", "null", "zero", "negative"} {
			t.Run(mode+"/"+bad, func(t *testing.T) {
				e, a, want := storedSizeFixture(t, mode, []byte("{\"a\":1}\n"))
				sizeControl(t, e, a, want)
				e.hs.mu.Lock()
				e.hs.serveAs["sizefixture"] = []byte{}
				e.hs.mu.Unlock()
				a["sha256"] = sumHex(nil)
				switch bad {
				case "missing":
					delete(a, "size")
				case "null":
					a["size"] = nil
				case "zero":
					a["size"] = 0
				case "negative":
					a["size"] = -1
				}
				sizeRefusal(t, e, a, "attachment has no valid size")
			})
		}
	}
}

// Stored size must be a JSON integer; wrong types, a fraction and int64
// overflow refuse without reads/files, paired with the same valid controls.
func TestFetchStoredSizeInteger(t *testing.T) {
	for _, mode := range []string{"legacy", "future", "unknown-coding", "gzip"} {
		for _, bad := range []struct {
			name  string
			value any
		}{{"string", "1"}, {"boolean", true}, {"fraction", json.Number("1.5")}, {"overflow", json.Number("9223372036854775808")}} {
			t.Run(mode+"/"+bad.name, func(t *testing.T) {
				e, a, want := storedSizeFixture(t, mode, []byte("{\"a\":1}\n"))
				sizeControl(t, e, a, want)
				a["size"] = bad.value
				sizeRefusal(t, e, a, "attachment")
			})
		}
	}
}

// Known v1 validates original.size presence/type before gzip or fallback;
// missing/null refuse before reads/files, while explicit zero remains valid.
func TestFetchOriginalSizeRequired(t *testing.T) {
	for _, mode := range []string{"gzip", "unknown-coding"} {
		for _, bad := range []string{"missing", "null"} {
			t.Run(mode+"/"+bad, func(t *testing.T) {
				e, a, want := storedSizeFixture(t, mode, nil)
				sizeControl(t, e, a, want)
				original := a["encoding"].(map[string]any)["original"].(map[string]any)
				if bad == "missing" {
					delete(original, "size")
				} else {
					original["size"] = nil
				}
				sizeRefusal(t, e, a, "original.size")
			})
		}
	}
}

// Keeper item 1: every event dispatch refuses declared stored L+1 before
// downloading or writing, paired with valid media inside the same server limit.
func TestFetchStoredSizeOverLimit(t *testing.T) {
	const limit = 64
	for _, mode := range []string{"legacy", "future", "unknown-coding", "gzip"} {
		t.Run(mode, func(t *testing.T) {
			e, a, want := storedSizeFixture(t, mode, []byte("hello"))
			e.hs.mu.Lock()
			e.hs.uploadMax = limit
			e.hs.mu.Unlock()
			sizeControl(t, e, a, want)
			a["size"] = limit + 1
			sizeRefusal(t, e, a, "over the server's upload limit of 64 bytes")
		})
	}
}

// Keeper item 2: actual 1-byte/L-byte media succeed for legacy and fallback;
// gzip succeeds at stored L using a real stream and its exact size as server L.
// A 1-byte gzip stream cannot contain even the fixed 10-byte gzip header.
func TestFetchStoredSizeBoundaryControls(t *testing.T) {
	for _, mode := range []string{"legacy", "future", "unknown-coding", "gzip"} {
		for _, edge := range []string{"one", "at-L"} {
			if mode == "gzip" && edge == "one" {
				continue
			}
			t.Run(mode+"/"+edge, func(t *testing.T) {
				body := []byte("x")
				if edge == "at-L" {
					body = bytes.Repeat([]byte("x"), 64)
				}
				if mode == "gzip" {
					body = []byte("{\"a\":1}\n")
				}
				e, a, want := storedSizeFixture(t, mode, body)
				if mode == "unknown-coding" {
					// An unsupported coding's stored bytes need not be gzip; its
					// valid v1 envelope still describes this body's original bytes.
					e.hs.mu.Lock()
					e.hs.media["sizefixture"] = fakeMedia{data: body}
					e.hs.mu.Unlock()
					a["size"], a["sha256"], want = len(body), sumHex(body), body
				}
				limit := 64
				if edge == "at-L" {
					limit = a["size"].(int)
				}
				if edge == "one" && a["size"] != 1 || edge == "at-L" && a["size"] != limit {
					t.Fatal("fixture does not exercise the declared stored-size boundary")
				}
				e.hs.mu.Lock()
				e.hs.uploadMax = int64(limit)
				actual := len(e.hs.media["sizefixture"].data)
				e.hs.mu.Unlock()
				if actual != a["size"].(int) {
					t.Fatal("declared boundary does not match actual stored bytes")
				}
				sizeControl(t, e, a, want)
			})
		}
	}
}

// A one-byte stored body passes the positive size range but is not gzip;
// decode refuses without files, while a valid stream at stored L succeeds.
func TestFetchGzipOneByteStoredInvalid(t *testing.T) {
	e, a, want := storedSizeFixture(t, "gzip", nil)
	e.hs.mu.Lock()
	e.hs.uploadMax = int64(a["size"].(int))
	e.hs.mu.Unlock()
	sizeControl(t, e, a, want)
	body := []byte("x")
	e.hs.mu.Lock()
	e.hs.media["sizefixture"] = fakeMedia{data: body}
	e.hs.mu.Unlock()
	a["size"], a["sha256"] = len(body), sumHex(body)
	before := e.hs.CallCount(downloadRoute)
	r, dir := sizeFetch(t, e, a)
	r.failsWith(t, ExitError, "gzip")
	if e.hs.CallCount(downloadRoute) != before+1 || r.stdout != "" || len(dirEntries(t, dir)) != 0 {
		t.Fatalf("invalid gzip did not reach decoding cleanly or left files: %+v", r)
	}
}

// Keeper item 4: bare MXC has no event size; real media succeeds at server L,
// then refuses at L+1 bytes with no stdout/target/temp, identically on base.
func TestFetchBareSizeBound(t *testing.T) {
	body := []byte("12345678")
	e, a, _ := storedSizeFixture(t, "legacy", body)
	e.hs.mu.Lock()
	e.hs.uploadMax = int64(len(body))
	e.hs.mu.Unlock()
	goodDir := t.TempDir()
	good := filepath.Join(goodDir, "out")
	e.as("bob", "fetch", a["mxc"].(string), "--sha256", sumHex(body), "-o", good).ok(t)
	if readFileStr(t, good) != string(body) || len(dirEntries(t, goodDir)) != 1 {
		t.Fatal("valid bare fetch changed media or left temporary files")
	}
	e.hs.mu.Lock()
	e.hs.uploadMax = int64(len(body) - 1)
	e.hs.mu.Unlock()
	badDir := t.TempDir()
	before := e.hs.CallCount(downloadRoute)
	r := e.as("bob", "fetch", a["mxc"].(string), "--sha256", sumHex(body), "-o", filepath.Join(badDir, "out"))
	r.failsWith(t, ExitError, "more than 7 bytes")
	if e.hs.CallCount(downloadRoute) != before+1 || r.stdout != "" || len(dirEntries(t, badDir)) != 0 {
		t.Fatalf("over-limit bare fetch did not use its download bound or left files: %+v", r)
	}
}
