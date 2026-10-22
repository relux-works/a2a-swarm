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
