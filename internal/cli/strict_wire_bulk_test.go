package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Drive real Sync -> ingest -> durable mailbox -> wait's output (doorbell).
// The identical main-source witness is preserved in the task evidence.
func TestStrictPartitionBulkSyncWake(t *testing.T) {
	for _, kind := range []string{"ordinary", "peer-duplicate-surrogate"} {
		t.Run(kind, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			before := e.state("alice").NextBatch
			id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local wake", nil))
			var syncs, changed atomic.Int32
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || !strings.HasSuffix(r.URL.Path, "/sync") {
						return resp, err
					}
					syncs.Add(1)
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "peer-duplicate-surrogate" && bytes.Contains(raw, []byte(id)) {
						old := []byte(`"body":"@alice:test.local wake"`)
						newBody := []byte(`"body":"first","body":"@alice:test.local wake\ud800"`)
						if bytes.Count(raw, old) != 1 {
							return nil, fmt.Errorf("expected exactly one event body: %s", raw)
						}
						raw = bytes.Replace(raw, old, newBody, 1)
						changed.Add(1)
					}
					resp.Body = io.NopCloser(bytes.NewReader(raw))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			r := e.as("alice", "wait", "--timeout", "2s").ok(t)
			if !strings.Contains(r.stdout, "mention #proj "+id) || e.state("alice").NextBatch == before {
				t.Fatalf("wake/cursor lost: %q", r.stdout)
			}
			en, ok := e.pendingEntry("alice", id)
			if !ok || !en.Shown || e.received("alice", id) {
				t.Fatalf("mailbox dispatch lost: %+v", en)
			}
			want := int32(0)
			if kind != "ordinary" {
				want = 1
			}
			if changed.Load() != want || syncs.Load() == 0 {
				t.Fatalf("sync witness not driven: sync=%d changed=%d", syncs.Load(), changed.Load())
			}
			// A second wait must see the same cursor and no duplicate doorbell.
			r = e.as("alice", "wait", "--timeout", "10ms").ok(t)
			if r.stdout != "timeout\n" {
				t.Fatalf("duplicate dispatch: %q", r.stdout)
			}
		})
	}
}
