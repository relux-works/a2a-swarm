//go:build linux || darwin

package inboundmedia

import (
	"context"
	"encoding/json"
	"fmt"
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
	canonical, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce macOS's system-ancestor spelling even on Linux. This is
	// distinct from both the configured TMPDIR alias and its canonical path.
	ancestorAlias := filepath.Join(aliases, "system")
	if err := os.Symlink(filepath.Dir(canonical), ancestorAlias); err != nil {
		t.Fatal(err)
	}
	systemSpelling := filepath.Join(ancestorAlias, filepath.Base(canonical))
	t.Setenv("TMPDIR", alias)
	for i, root := range []string{alias, f.root, canonical, systemSpelling} {
		output := filepath.Join(root, fmt.Sprintf("chosen-%d", i))
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
	for _, root := range []string{alias, f.root, canonical, systemSpelling} {
		_, err := Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, filepath.Join(root, "child", "out"))
		wantRefusal(t, err, UnsafeOutput)
	}
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

func TestNativeDeclaredNameConsistency(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		refusal      Code
		status       string
	}{
		{"same", `"filename":"report.pdf","org.matrix.msc1767.file":{"name":"report.pdf"}`, "", "declared_untrusted"},
		{"body", `"body":"report.pdf","org.matrix.msc1767.file":{"name":"report.pdf"}`, "", "declared_untrusted"},
		{"conflict", `"filename":"report.pdf","org.matrix.msc1767.file":{"name":"evil.exe"}`, MetadataInvalid, "declared_untrusted"},
		{"null", `"filename":null,"body":"report.pdf"`, "", "invalid"},
		{"malformed", `"filename":7,"body":"report.pdf"`, "", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := nativeEvent(`{"msgtype":"m.file","url":"mxc://remote.invalid/media-id",` + tc.fields + `}`)
			m := Inspect(ev, selection.RoomID)
			if m == nil || m.Refusal != tc.refusal || m.NameStatus != tc.status {
				t.Fatalf("name contract: %+v", m)
			}
			if tc.status == "invalid" && (m.Name != "" || m.NameSource != "unknown") {
				t.Fatalf("invalid filename retained fallback: %+v", m)
			}
		})
	}
}

func TestNativeStateEventRefusedBeforeDownload(t *testing.T) {
	f := newFixture(t, []byte("valid"), 64)
	ev := validEvent("m.file", 5)
	key := ""
	ev.StateKey = &key
	if m := Inspect(ev, selection.RoomID); m == nil || m.Refusal != ContextInvalid {
		t.Fatalf("state-event projection: %+v", m)
	}
	_, err := f.fetch(ev, "state")
	wantRefusal(t, err, ContextInvalid)
	if f.configs.Load() != 0 || f.downloads.Load() != 0 {
		t.Fatal("state event reached preflight or download")
	}
	cleanRoot(t, f.root)
}
