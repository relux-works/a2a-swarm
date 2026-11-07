//go:build linux || darwin

package inboundmedia

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A trusted root may use a system-style alias; descendants still cannot.
func TestNativeTrustedRootAliasAndNoFollowBelow(t *testing.T) {
	f := newFixture(t, []byte("valid"), 64)
	aliases := t.TempDir()
	alias := filepath.Join(aliases, "tmp")
	if err := os.Symlink(f.root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	for _, root := range []string{alias, f.root} {
		output := filepath.Join(root, filepath.Base(root)+"-out")
		result, err := Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, output)
		if err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(result.Path); err != nil || string(data) != "valid" {
			t.Fatalf("alias bytes %q %v", data, err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.root, "child")); err != nil {
		t.Fatal(err)
	}
	before := f.downloads.Load()
	_, err := Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, filepath.Join(alias, "child", "out"))
	wantRefusal(t, err, UnsafeOutput)
	if f.downloads.Load() != before {
		t.Fatal("symlink descendant downloaded")
	}
	cleanRoot(t, outside)
}

func TestNativeMetadataFormatEscapesAndTextNeighbor(t *testing.T) {
	ev := validEvent("m.file", 5)
	ev.Sender = "@peer\u202e:synthetic"
	ev.EventID = "$event\u2066"
	room := "!room\U000e0001:synthetic"
	m := Inspect(ev, room)
	if m == nil {
		t.Fatal("missing native projection")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\u202e", "\u2066", "\U000e0001"} {
		if strings.Contains(string(raw), control) {
			t.Fatalf("literal format control in %s", raw)
		}
	}
	var decoded Metadata
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.SenderMXID != ev.Sender || decoded.Handle.EventID != ev.EventID || decoded.Handle.RoomID != room {
		t.Fatalf("opaque identifiers lost: %s %v", raw, err)
	}
	plain := nativeEvent(`{"msgtype":"m.text","body":"ordinary","key":{"custom":true}}`)
	if Inspect(plain, selection.RoomID) != nil {
		t.Fatal("ordinary text gained media refusal")
	}
}
