package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSavedInboxOrdinaryNumberTokensAndReceipt(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	const encoding = `{"version":"future/1","ordinary":{"label":"visible metadata","sequence":9007199254740993,"small":17,"bool":true,"nil":null},"nested":[{"timestamp_ns":1700000000000000001,"password":"synthetic_private_value"}],"numbers":[-9223372036854775808,18446744073709551615,1.2300,1E+03,-0,1e-400,1.7976931348623157e308]}`
	content := toMsg("bob", "notice", "ordinary public synthetic body")
	h := content[ContentKey].(Header)
	h.Attachment = &Attachment{Name: "ordinary.bin", Size: 1, SHA256: strings.Repeat("0", 64), MXC: "mxc://test.local/synthetic", Encoding: json.RawMessage(encoding)}
	content[ContentKey] = h
	id := e.hs.inject(room, "@bob:"+testServer, content)
	later := e.hs.inject(room, "@bob:"+testServer, ping(2))
	ev, found := e.hs.eventByID(id)
	if !found {
		t.Fatal("synthetic event missing")
	}
	incoming := eventRecord("message", "proj", room, ev, testServer, false)
	if incoming.Attachment == nil || string(incoming.Attachment.Encoding) != encoding {
		t.Fatal("input metadata changed before saving")
	}
	path := filepath.Join(outputTestDir(t), "frame")
	r := reviewRun(e, "--as", "alice", "inbox", "--out", path, "--summary", "--limit", "1")
	if r.code != ExitOK {
		t.Fatalf("save exit=%d", r.code)
	}
	records, err := readSavedInbox(path)
	if err != nil || len(records) != 1 || records[0].Attachment == nil {
		t.Fatal("complete readable saved frame missing")
	}
	decode := func(raw string) any {
		t.Helper()
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	want := decode(strings.ReplaceAll(encoding, "synthetic_private_value", "<redacted>"))
	got := decode(string(records[0].Attachment.Encoding))
	if !reflect.DeepEqual(got, want) {
		t.Error("ordinary JSON number tokens or neighboring metadata changed")
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "synthetic_private_value") {
		t.Fatal("saved file missing or secret value retained")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatal("saved file is not private and regular")
	}
	if !e.received("alice", id) || e.received("alice", later) {
		t.Fatal("save receipted a different batch")
	}
	before := len(e.hs.urls())
	show := reviewRun(e, "--as", "alice", "inbox", "show", id, "--from", path)
	if show.code != ExitOK || !strings.Contains(show.stdout, "ordinary public synthetic body") || len(e.hs.urls()) != before || e.received("alice", later) {
		t.Error("local read changed HTTP or receipt behavior")
	}
	t.Log("input_exact=true saved_readable=true secret_redacted=true selected_receipt=true private_file=true local_show=true")
}

func TestSavedInboxNumberParserAndFrameBounds(t *testing.T) {
	for _, tc := range []struct{ name, metadata string }{
		{"overflow", `{"ordinary":1e400}`},
		{"negative-overflow", `{"ordinary":[-1e400]}`},
		{"secret-overflow", `{"password":{"nested":1e400}}`},
		{"invalid-number", `{"ordinary":01}`},
		{"nonfinite", `{"ordinary":NaN}`},
		{"trailing-value", `{"ordinary":1} {}`},
		{"truncated", `{"ordinary":`},
		{"depth", strings.Repeat("[", 10001) + "1" + strings.Repeat("]", 10001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := EventRecord{Type: "message", EventID: "$synthetic", Attachment: &Attachment{Encoding: json.RawMessage(tc.metadata)}}
			records, frame, err := savedInboxFrame(&App{}, []waitItem{{rec: rec, id: rec.EventID}}, testServer)
			if err == nil || records != nil || frame != nil {
				t.Fatal("invalid metadata produced a saved frame")
			}
		})
	}
	// The same valid number metadata must still obey the complete frame cap.
	rec := EventRecord{Type: "message", EventID: "$synthetic", Attachment: &Attachment{Encoding: json.RawMessage(`{"sequence":9007199254740993}`)}}
	item := waitItem{rec: rec, id: rec.EventID, line: strings.Repeat("x", inboxSavedMax)}
	records, frame, err := savedInboxFrame(&App{}, []waitItem{item}, testServer)
	if err == nil || !strings.Contains(err.Error(), "full inbox frame exceeds") || records != nil || frame != nil {
		t.Fatal("oversized complete frame was not refused")
	}
}
