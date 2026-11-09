package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/inboundmedia"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func TestFilesNativeDiscoveryAndReadRoute(t *testing.T) {
	e, room := nativeWorld(t)
	name := "../../Отчёт владельца 画像.bin"
	var ids []string
	for _, kind := range []string{"m.file", "m.image", "m.audio", "m.video"} {
		content := elementContent(kind)
		content["filename"] = name
		ids = append(ids, e.hs.inject(room, "@bob:"+testServer, content))
	}
	bad := elementContent("m.file")
	bad["info"] = map[string]any{"size": nil}
	badID := e.hs.inject(room, "@bob:"+testServer, bad)
	e.hs.inject(room, "@bob:"+testServer, msg("ordinary", nil))
	out := e.as("alice", "files", room, "--json").ok(t).stdout
	records := decodeRecords(t, out)
	if len(records) != 5 || strings.Contains(out, "mxc://") {
		t.Fatalf("native listing: %s", out)
	}
	for i, r := range records[:4] {
		if r.EventID != ids[i] || r.Media == nil || r.Media.Handle != (inboundmedia.Handle{RoomID: room, EventID: ids[i]}) || r.Media.Kind != []string{"m.file", "m.image", "m.audio", "m.video"}[i] || r.Media.Name != name || r.Media.NameStatus != "declared_untrusted" || r.Media.DeclaredSize == nil || *r.Media.DeclaredSize != 5 || r.Media.Integrity != "homeserver_only_no_trusted_hash" || r.Attachment != nil {
			t.Fatalf("native metadata: %+v", r)
		}
	}
	if records[4].EventID != badID || records[4].Media == nil || records[4].Media.Refusal != inboundmedia.MetadataInvalid {
		t.Fatal("invalid native item lost its bounded refusal")
	}
	text := e.as("alice", "files", room, "--last", "5").ok(t).stdout
	if strings.Contains(text, "mxc://") || !strings.Contains(text, `"name_status":"declared_untrusted"`) || !strings.Contains(text, `"integrity":"homeserver_only_no_trusted_hash"`) {
		t.Fatalf("native text metadata: %s", text)
	}
	last := decodeRecords(t, e.as("alice", "files", room, "--last", "1", "--json").ok(t).stdout)
	if len(last) != 1 || last[0].EventID != badID {
		t.Fatal("native listing ignored last bound")
	}
	// The documented discovery route is files --json; read's wire shape stays stable.
	read := decodeRecords(t, e.as("alice", "read", room, "--event", ids[0], "--json").ok(t).stdout)
	if len(read) != 1 || read[0].Media != nil {
		t.Fatal("read unexpectedly gained native metadata")
	}
	if e.hs.CallCount(downloadRoute) != 0 || e.received("alice", ids[0]) {
		t.Fatal("discovery downloaded bytes or receipted mail")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	chosen := filepath.Join(root, "chosen.bin")
	var result inboundmedia.Result
	fetched := e.as("alice", "fetch", room, ids[0], "-o", chosen).ok(t)
	if err := json.Unmarshal([]byte(fetched.stdout), &result); err != nil || result.Path != chosen || readFileStr(t, chosen) != "valid" || len(dirEntries(t, root)) != 1 {
		t.Fatalf("declared filename influenced path: %+v %v", result, err)
	}
}

func TestFilesNativeMixedTopicAndBounds(t *testing.T) {
	e, legacyID, _, root := attachWorld(t)
	legacy, ok := e.hs.eventByID(legacyID)
	if !ok || nativeFileMetadata(legacy, "!projRoomID") != nil {
		t.Fatal("legacy attachment reinterpreted as native")
	}
	content := elementContent("m.file")
	content["filename"] = "файл с пробелами.bin"
	content["body"] = strings.Repeat("界", inboxBodyMax+1)
	content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
	nativeID := e.hs.inject("!projRoomID", "@bob:"+testServer, content)
	outsideID := e.hs.inject("!projRoomID", "@bob:"+testServer, elementContent("m.image"))
	records := decodeRecords(t, e.as("alice", "files", "proj", "--topic", "alpha", "--json").ok(t).stdout)
	if len(records) != 2 || records[0].EventID != legacyID || records[0].Attachment == nil || records[0].Media != nil || records[1].EventID != nativeID || records[1].Media == nil || utf8.RuneCountInString(records[1].Body) != inboxBodyMax {
		t.Fatalf("mixed topic records: %+v", records)
	}
	last := decodeRecords(t, e.as("alice", "files", "proj", "--last", "1", "--json").ok(t).stdout)
	if len(last) != 1 || last[0].EventID != outsideID {
		t.Fatal("combined listing lost last bound")
	}
	// Opaque encrypted events do not prove the presence of native media.
	if nativeFileMetadata(matrix.Event{Type: "m.room.encrypted", EventID: "$opaque"}, "!projRoomID") != nil {
		t.Fatal("opaque encrypted event listed as native file")
	}
}
