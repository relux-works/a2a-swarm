package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/inboundmedia"
)

func TestElementMediaOversizedMetadataOrder(t *testing.T) {
	e, room := nativeWorld(t)
	e.as("alice", "inbox").ok(t)
	body := `"body":"@alice:test.local ` + strings.Repeat("a", 65536) + `"`
	kind := `"msgtype":"m.file"`
	ids := []string{}
	for _, fields := range []string{kind + "," + body, body + "," + kind} {
		id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
		raw := `{` + fields + `,"url":"mxc://` + testServer + `/native"}`
		if !json.Valid([]byte(raw)) {
			t.Fatal("invalid test content")
		}
		e.hs.mu.Lock()
		for i := range e.hs.events {
			if e.hs.events[i].EventID == id {
				e.hs.events[i].Content = json.RawMessage(raw)
			}
		}
		e.hs.mu.Unlock()
		ids = append(ids, id)
	}
	plain := e.hs.inject(room, "@bob:"+testServer, msg("@alice:test.local ordinary neighbour", nil))
	records := decodeRecords(t, e.as("alice", "inbox", "--json").ok(t).stdout)
	if len(records) != 3 {
		t.Fatalf("records=%d, want both refused items and valid neighbour", len(records))
	}
	for i, id := range ids {
		r := records[i]
		if r.EventID != id || r.Media == nil || r.Media.Kind != "m.file" || r.Media.Refusal != inboundmedia.MetadataInvalid || r.Media.Handle != (inboundmedia.Handle{RoomID: room, EventID: id}) {
			t.Errorf("ordered metadata lost native refusal: %+v", r)
		}
	}
	if records[2].EventID != plain || records[2].Media != nil {
		t.Fatal("refused media changed ordinary neighbour")
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	for i, id := range ids {
		path := filepath.Join(root, []string{"kind-first", "kind-last"}[i])
		r := e.as("alice", "fetch", room, id, "-o", path)
		if r.code != ExitError || !strings.Contains(r.stderr, string(inboundmedia.MetadataInvalid)) || r.stdout != "" {
			t.Errorf("native metadata fell through or lost typed refusal: %+v", r)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("refused metadata published: %v", err)
		}
	}
	if e.hs.CallCount(downloadRoute) != 0 || len(dirEntries(t, root)) != 0 {
		t.Fatal("oversized metadata downloaded or left scratch")
	}
	good := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	e.as("alice", "fetch", room, good, "-o", filepath.Join(root, "valid-neighbour")).ok(t)
	if readFileStr(t, filepath.Join(root, "valid-neighbour")) != "valid" {
		t.Fatal("valid native neighbour changed")
	}
}
