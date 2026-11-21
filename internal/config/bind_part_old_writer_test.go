package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// oldImageRewrite models a State writer built before unknown top-level State
// keys were preserved: it keeps the keys it knows and writes the document
// back in place of the one it read. The provider marker and the writer seal
// are keys such an image does not know, so both are gone afterwards. edit
// changes the document as that writer's own fields. The model reproduces the
// dropped keys only, not the exact encoding of any released image.
func oldImageRewrite(t *testing.T, p Paths, name string, edit func(doc map[string]json.RawMessage)) {
	t.Helper()
	path := p.StateFile(name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc[bindPartMarkerKey]; !ok {
		t.Fatal("fixture State carries no provider marker to drop")
	}
	delete(doc, bindPartMarkerKey)
	delete(doc, "writer")
	if edit != nil {
		edit(doc)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	next := path + ".old-image"
	if err := os.WriteFile(next, append(out, '\n'), SecretFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// TestBindPartOldImageWriterIsRefused states what a current image does after
// an image that drops unknown State keys has written a marked identity's
// State, for a writer that moved the cursor and for one that changed nothing
// it knows. Claims, each with the two files compared byte for byte:
//   - every read and every update is refused with ErrBindPartUnmarked, the
//     update callback never runs, and the refusal writes nothing;
//   - a new bind does not adopt the old writer's cursor and does not restore
//     the marker over a sidecar whose values moved on: EnsureBindPart refuses
//     with ErrBindPartInvalid and writes nothing.
//
// Control: before the old writer runs, the same State loads with the
// sidecar's cursor. The test proves the refusals of this image; it does not
// prove what a released older binary writes, and it offers no repair.
func TestBindPartOldImageWriterIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(doc map[string]json.RawMessage)
	}{
		{"moved cursor", func(doc map[string]json.RawMessage) { doc["next_batch"] = json.RawMessage(`"old-image-cursor"`) }},
		{"no known field changed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, view := bindFixture(t, "legacy-cursor", false)
			owner := BindOperationOwner{Holder: "test-holder", Host: "host-1", PID: os.Getpid(), Birth: "1"}
			token, err := NewBindToken()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, nil); err != nil {
				t.Fatalf("reserve provider cursor: %v", err)
			}
			if _, err := CommitBindCursor(p, bindTestName, token, "provider-cursor"); err != nil {
				t.Fatalf("advance provider cursor: %v", err)
			}
			if st, err := LoadState(p, bindTestName); err != nil || st.NextBatch != "provider-cursor" {
				t.Fatalf("control before the old writer: cursor=%q err=%v", st.NextBatch, err)
			}

			oldImageRewrite(t, p, bindTestName, tc.edit)
			stateBefore, err := os.ReadFile(p.StateFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			sidecarBefore, err := os.ReadFile(p.BindPartFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			unchanged := func(step string) {
				t.Helper()
				state, err := os.ReadFile(p.StateFile(bindTestName))
				if err != nil || !bytes.Equal(state, stateBefore) {
					t.Fatalf("%s changed State (err=%v)", step, err)
				}
				sidecar, err := os.ReadFile(p.BindPartFile(bindTestName))
				if err != nil || !bytes.Equal(sidecar, sidecarBefore) {
					t.Fatalf("%s changed the sidecar (err=%v)", step, err)
				}
			}

			if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartUnmarked) {
				t.Fatalf("read after the old writer = %v, want ErrBindPartUnmarked", err)
			}
			unchanged("refused read")
			ran := false
			if _, err := UpdateState(p, bindTestName, func(*State) error { ran = true; return nil }); !errors.Is(err, ErrBindPartUnmarked) || ran {
				t.Fatalf("update after the old writer = %v, callback ran=%t; want ErrBindPartUnmarked and no callback", err, ran)
			}
			unchanged("refused update")
			if _, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()}); !errors.Is(err, ErrBindPartInvalid) {
				t.Fatalf("new bind after the old writer = %v, want ErrBindPartInvalid", err)
			}
			unchanged("refused bind")
		})
	}
}
