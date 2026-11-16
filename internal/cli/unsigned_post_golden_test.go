package cli

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const unsignedPostGolden = "unsigned-post.golden"

// Regression witness: a post without --sign stores the same event content as
// the golden file. The golden is generated on the unchanged base commit (set
// A2A_WRITE_UNSIGNED_POST_GOLDEN=1 there; this file uses only the base test
// harness) and compared on the changed tree. The wire golden also preserves
// the exact HTTP request body bytes before the fake server re-serializes them.
// Claim: unsigned stored content and exact wire request bodies equal their unchanged-base goldens.
func TestUnsignedPostGolden(t *testing.T) {
	e, _ := world(t)
	e.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	captured := make(chan []byte, 3)
	e.configure = func(a *App) {
		client := *a.HTTP
		client.Transport = unsignedPostCapture{base: client.Transport, captured: captured}
		a.HTTP = &client
	}
	var got, wire bytes.Buffer
	for _, args := range [][]string{
		{"post", "proj", "--topic", "alpha", "--kind", "request", "--to", "alice", "plain  text "},
		{"post", "proj", "--topic", "alpha", "--re", root, "--to", "alice", "second\nline"},
		{"post", "proj", "--topic", "alpha", "broadcast"},
	} {
		id := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
		ev, ok := e.hs.eventByID(id)
		if !ok {
			t.Fatalf("event %q not stored", id)
		}
		got.Write(ev.Content)
		got.WriteByte('\n')
		select {
		case raw := <-captured:
			wire.Write(raw)
			wire.WriteByte('\n')
		default:
			t.Fatal("unsigned post wire body not captured")
		}
	}
	for name, actual := range map[string][]byte{unsignedPostGolden: got.Bytes(), "unsigned-post-wire.golden": wire.Bytes()} {
		path := filepath.Join("testdata", name)
		if os.Getenv("A2A_WRITE_UNSIGNED_POST_GOLDEN") == "1" {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, actual, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s (%d bytes)", path, len(actual))
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("golden missing: generate %s on the unchanged base commit with A2A_WRITE_UNSIGNED_POST_GOLDEN=1: %v", path, err)
		}
		if !bytes.Equal(actual, want) {
			t.Fatalf("unsigned post content changed (%s)", name)
		}
	}
}

type unsignedPostCapture struct {
	base     http.RoundTripper
	captured chan<- []byte
}

func (c unsignedPostCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/send/m.room.message/") {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(raw))
		c.captured <- raw
	}
	base := c.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
