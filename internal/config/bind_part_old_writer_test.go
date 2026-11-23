package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// legacyRewrite models a State writer that knows nothing of the sidecar: it
// writes the document back in place of the one it read, without the writer
// seal. With dropMarker it is an image built before unknown top-level State
// keys were preserved, which loses the provider marker as well; without it, a
// later image that keeps unknown keys. edit changes the document as that
// writer's own fields. The model reproduces the kept and dropped keys only,
// not the exact encoding of any released image.
func legacyRewrite(t *testing.T, p Paths, name string, dropMarker bool, edit func(doc map[string]json.RawMessage)) {
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
		t.Fatal("fixture State carries no provider marker")
	}
	if dropMarker {
		delete(doc, bindPartMarkerKey)
	}
	delete(doc, "writer")
	if edit != nil {
		edit(doc)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	next := path + ".legacy"
	if err := os.WriteFile(next, append(out, '\n'), SecretFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// oldImageRewrite is legacyRewrite for an image that drops the marker.
func oldImageRewrite(t *testing.T, p Paths, name string, edit func(doc map[string]json.RawMessage)) {
	t.Helper()
	legacyRewrite(t, p, name, true, edit)
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

// TestBindPartRepairAfterOldImageWriter proves the repair for an identity
// whose marked State was written by an image that drops the marker: with the
// sidecar removed, State loads again as an unmarked document carrying the old
// writer's cursor, and the next bind migrates afresh from it - the new
// sidecar holds that cursor and State is marked again. Nothing of the removed
// sidecar's cursor survives, which is the cost of the repair: the identity
// resumes from the old writer's position.
//
// Control: before the sidecar is removed the same State is refused. The test
// does not cover a marked State whose sidecar is removed; that stays refused
// (TestBindPartRollbackKeepsPermanentTombstone).
func TestBindPartRepairAfterOldImageWriter(t *testing.T) {
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
	oldImageRewrite(t, p, bindTestName, func(doc map[string]json.RawMessage) {
		doc["next_batch"] = json.RawMessage(`"old-image-cursor"`)
	})
	if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartUnmarked) {
		t.Fatalf("control before the repair = %v, want ErrBindPartUnmarked", err)
	}

	if err := os.Remove(p.BindPartFile(bindTestName)); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(p, bindTestName)
	if err != nil || BindPartMarked(state) || state.NextBatch != "old-image-cursor" || len(state.Threads) != 1 {
		t.Fatalf("State after removing the sidecar: marked=%t cursor=%q threads=%v err=%v", BindPartMarked(state), state.NextBatch, state.Threads, err)
	}
	migrated, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
	if err != nil || !BindPartActive(migrated) {
		t.Fatalf("bind after the repair: active=%t err=%v", BindPartActive(migrated), err)
	}
	repaired, err := ReadBindPartView(p, bindTestName, migrated)
	if err != nil || repaired.NextBatch != "old-image-cursor" {
		t.Fatalf("repaired sidecar cursor=%q err=%v, want the old writer's cursor", repaired.NextBatch, err)
	}
	if again, err := LoadState(p, bindTestName); err != nil || !BindPartMarked(again) || again.NextBatch != "old-image-cursor" {
		t.Fatalf("State after the repair: marked=%t cursor=%q err=%v", BindPartMarked(again), again.NextBatch, err)
	}
}

// TestBindPartMarkerKeepingLegacyWriter states what a current image does after
// an older image that keeps the marker, but knows nothing of the sidecar, has
// written a marked identity's State.
//
//   - A write of ordinary fields only (here a followed thread) is harmless:
//     State loads, carries the new thread and the sidecar's cursor.
//   - A write of a moved field (here the cursor) is refused with
//     ErrBindPartInvalid, and the refusal changes neither file.
//   - The repair for that case has two steps. Removing the sidecar alone leaves
//     a marked State without a sidecar, refused with ErrBindPartMissing. With
//     the marker removed from State as well, State loads as an unmarked
//     document carrying the legacy writer's cursor and the next bind migrates
//     afresh from it.
//
// The legacy writer is modelled by legacyRewrite; no released image is run.
func TestBindPartMarkerKeepingLegacyWriter(t *testing.T) {
	advanced := func(t *testing.T) Paths {
		t.Helper()
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
		return p
	}

	t.Run("ordinary fields only", func(t *testing.T) {
		p := advanced(t)
		legacyRewrite(t, p, bindTestName, false, func(doc map[string]json.RawMessage) {
			doc["threads"] = json.RawMessage(`["$keep","$learned"]`)
		})
		state, err := LoadState(p, bindTestName)
		if err != nil || !BindPartActive(state) || state.NextBatch != "provider-cursor" || len(state.Threads) != 2 || state.Threads[1] != "$learned" {
			t.Fatalf("State after an ordinary legacy write: active=%t cursor=%q threads=%v err=%v", BindPartActive(state), state.NextBatch, state.Threads, err)
		}
	})

	t.Run("moved field and the two-step repair", func(t *testing.T) {
		p := advanced(t)
		legacyRewrite(t, p, bindTestName, false, func(doc map[string]json.RawMessage) {
			doc["next_batch"] = json.RawMessage(`"legacy-writer-cursor"`)
		})
		stateBefore, err := os.ReadFile(p.StateFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		sidecarBefore, err := os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartInvalid) {
			t.Fatalf("read after a legacy cursor write = %v, want ErrBindPartInvalid", err)
		}
		stateAfter, err := os.ReadFile(p.StateFile(bindTestName))
		if err != nil || !bytes.Equal(stateAfter, stateBefore) {
			t.Fatalf("refused read changed State (err=%v)", err)
		}
		sidecarAfter, err := os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil || !bytes.Equal(sidecarAfter, sidecarBefore) {
			t.Fatalf("refused read changed the sidecar (err=%v)", err)
		}

		if err := os.Remove(p.BindPartFile(bindTestName)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartMissing) {
			t.Fatalf("control: sidecar removed, marker kept = %v, want ErrBindPartMissing", err)
		}
		// The operator's second step, exactly: delete the one key and write the
		// document back. Every other key, the stale writer seal included, stays.
		raw, err := os.ReadFile(p.StateFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, bindPartMarkerKey)
		edited, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.StateFile(bindTestName), append(edited, '\n'), SecretFileMode); err != nil {
			t.Fatal(err)
		}
		state, err := LoadState(p, bindTestName)
		if err != nil || BindPartMarked(state) || state.NextBatch != "legacy-writer-cursor" {
			t.Fatalf("State after both repair steps: marked=%t cursor=%q err=%v", BindPartMarked(state), state.NextBatch, err)
		}
		migrated, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
		if err != nil || !BindPartActive(migrated) {
			t.Fatalf("bind after the repair: active=%t err=%v", BindPartActive(migrated), err)
		}
		if view, err := ReadBindPartView(p, bindTestName, migrated); err != nil || view.NextBatch != "legacy-writer-cursor" {
			t.Fatalf("repaired sidecar cursor=%q err=%v, want the legacy writer's cursor", view.NextBatch, err)
		}
	})
}

// TestBindPartMarkerKeepingLegacyWriterMovesABindingField is the cursor case
// of TestBindPartMarkerKeepingLegacyWriter for the other fields of a binding
// that the sidecar owns: its generation, its session and its stopped flag. An
// older image that keeps the marker writes one of them into State, one field
// per row and nothing else. This version then refuses the identity with
// ErrBindPartInvalid, the refusal changes neither file, and the sidecar still
// says what it said: the same generation, the same session, not stopped.
//
// Control: the same rewrite with the binding left as it is loads.
//
// The legacy writer is modelled by legacyRewrite; no released image is run.
// TestBindOlderImageStopsABinding runs a real one for the stopped flag.
func TestBindPartMarkerKeepingLegacyWriterMovesABindingField(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
	}{
		{"control: nothing moved", "", ""},
		{"generation", "generation", `"` + strings.Repeat("b", 32) + `"`},
		{"session", "session_id", `"session-of-the-older-image"`},
		{"stopped", "stopped", `true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, before := bindFixture(t, "s0", true)
			legacyRewrite(t, p, bindTestName, false, func(doc map[string]json.RawMessage) {
				if tc.key == "" {
					return
				}
				var terminal map[string]json.RawMessage
				if err := json.Unmarshal(doc["terminal"], &terminal); err != nil {
					t.Fatal(err)
				}
				if tc.key != "stopped" {
					if _, ok := terminal[tc.key]; !ok {
						t.Fatalf("premise: State's binding has no %q", tc.key)
					}
				}
				terminal[tc.key] = json.RawMessage(tc.value)
				edited, err := json.Marshal(terminal)
				if err != nil {
					t.Fatal(err)
				}
				doc["terminal"] = edited
			})
			stateBefore, err := os.ReadFile(p.StateFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			sidecarBefore, err := os.ReadFile(p.BindPartFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			_, err = LoadState(p, bindTestName)
			if tc.key == "" {
				if err != nil {
					t.Fatalf("control: a legacy rewrite that moves nothing: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrBindPartInvalid) {
				t.Fatalf("read after the older image moved the %s = %v, want ErrBindPartInvalid", tc.name, err)
			}
			if stateAfter, err := os.ReadFile(p.StateFile(bindTestName)); err != nil || !bytes.Equal(stateAfter, stateBefore) {
				t.Fatalf("the refused read changed State (err=%v)", err)
			}
			if sidecarAfter, err := os.ReadFile(p.BindPartFile(bindTestName)); err != nil || !bytes.Equal(sidecarAfter, sidecarBefore) {
				t.Fatalf("the refused read changed the sidecar (err=%v)", err)
			}
			part, _, err := readBindPart(p, bindTestName)
			if err != nil || part.Generation != before.Generation || part.SessionID != before.SessionID || part.Stopped {
				t.Fatalf("the sidecar's binding moved: generation=%q session=%q stopped=%t err=%v", part.Generation, part.SessionID, part.Stopped, err)
			}
		})
	}
}
