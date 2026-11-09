package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both archive selectors leave fresh, empty-directory and pin-only identities
// untouched. Existing legacy coverage also compares the exported bytes.
func TestArchiveDMNeverConfigured(t *testing.T) {
	for _, layout := range []string{"absent", "empty-directory", "pin-only"} {
		for _, selector := range []string{"proj", "all"} {
			t.Run(layout+"/"+selector, func(t *testing.T) {
				e, _ := world(t)
				dir := filepath.Join(e.dir, "owner-dm")
				if layout != "absent" {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if layout == "pin-only" {
					ownerPut(t, ownerFiles(e.paths(), "bob").path("pin"), ownerDMPin{Mode: "host", Owner: "@alice:" + testServer})
				}
				e.as("bob", "archive", selector, "--dir", filepath.Join(t.TempDir(), "export")).ok(t)
				items, err := os.ReadDir(dir)
				if layout == "absent" {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatal("archive provisioned DM directory", err)
					}
				} else {
					want := 0
					if layout == "pin-only" {
						want = 1
					}
					if err != nil || len(items) != want || (want == 1 && items[0].Name() != "bob.pin") {
						t.Fatal("archive provisioned DM files", items, err)
					}
				}
			})
		}
	}
}

// A first DM created while candidate inventory is fetched is still excluded.
// Give it an alias so skipping unaliased DMs cannot hide a stale exclusion gate;
// an ordinary room in the same archive must actually be exported.
func TestArchiveDMFirstCreationDuringInventory(t *testing.T) {
	e, f, who := principalFixture(t)
	e.hs.inject("!projRoomID", "@alice:"+testServer, map[string]any{"msgtype": "m.text", "body": "ordinary archive control"})
	prior := e.configure
	hit := false
	e.configure = func(a *App) {
		prior(a)
		base := a.HTTP.Transport
		a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
			if !hit && strings.HasSuffix(r.URL.Path, "/joined_rooms") {
				hit = true
				configured := e.configure
				e.configure = prior
				defer func() { e.configure = configured }()
				e.as("bob", "principal-dm", "ensure", who).ok(t)
				d := principalEntry(t, f, who)
				e.hs.mu.Lock()
				e.hs.rooms[d.Room].state["m.room.canonical_alias|"] = json.RawMessage(`{"alias":"#late-dm:test.local"}`)
				e.hs.mu.Unlock()
				e.hs.inject(d.Room, who, map[string]any{"msgtype": "m.text", "body": "must remain private"})
			}
			return base.RoundTrip(r)
		})}
	}
	dir := filepath.Join(t.TempDir(), "export")
	e.as("bob", "archive", "all", "--dir", dir).ok(t)
	if !hit {
		t.Fatal("creation injection was not reached")
	}
	if _, err := os.Stat(filepath.Join(dir, "late-dm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new DM entered archive", err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "proj", RoomFileName)), "ordinary archive control") {
		t.Fatal("positive neighbour did not export ordinary message")
	}
}

// An existing reservation is checked under the lock even if no lock file was
// present initially. A held lock times out before reading the reservation;
// clearing only the reservation then permits ordinary export.
func TestArchiveDMReservationLock(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "exclusion-only", true: "held-lock"}[held], func(t *testing.T) {
			e, _ := world(t)
			f := ownerFiles(e.paths(), "bob")
			if err := os.MkdirAll(filepath.Dir(f.base), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.path("archive-exclude"), []byte(principalArchiveReservation+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "export")
			if held {
				unlock, err := f.lock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if unlock != nil {
						unlock()
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				r := e.runCtx(ctx, "--as", "bob", "archive", "all", "--dir", dir)
				cancel()
				unlock()
				unlock = nil
				r.failsWith(t, ExitError, "owner-dm lock: context deadline exceeded")
			}
			e.as("bob", "archive", "all", "--dir", dir).failsWith(t, ExitError, "creation reservation excludes all rooms")
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed archive wrote export", err)
			}
			if _, err := os.Stat(f.path("lock")); err != nil {
				t.Fatal("reservation was not coordinated with a lock file", err)
			}
			if err := os.WriteFile(f.path("archive-exclude"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			e.as("bob", "archive", "all", "--dir", dir).ok(t)
			if _, err := os.Stat(filepath.Join(dir, "proj", RoomFileName)); err != nil {
				t.Fatal("positive neighbour did not export ordinary room", err)
			}
		})
	}
}
