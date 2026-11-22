package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestCursorReservationThatStaysIsReleasedByItsOwner proves that a process
// never loses the token of a cursor reservation it may have left in place.
// Such a reservation refuses every other token while its owner lives, the
// owner's own later tokens included, so a lost token stops the delivery of
// that identity until the process exits.
//
// Each row leaves a reservation in place in its own way, with mail waiting on
// the server:
//
//   - "reserve": the directory sync of the reservation fails and the write
//     that takes it back cannot replace the file. The config layer names the
//     token beside its error. This is one whole sync step of the bound
//     consumer, and it answers "write outcome is uncertain".
//   - "release": a reservation was taken, and the write that releases it
//     cannot replace the file.
//   - "persist": a reservation was taken, and the State cannot be read when
//     the batch is to be written, nor by the release that follows.
//
// Then, with the disk healthy again, the next step of the same process must
// deliver the mail and move the cursor. It is run twice: by the same session
// through the bound consumer's sync step, and by a new session of the process
// through the write-time reservation that inbox and the hooks use.
//
// Premise, every row: the reservation is in place after the failure. Another
// live owner is refused as held, and the cursor has not moved.
//
// Controls, every row: afterwards nothing is reserved (another owner reserves
// at once), and the process remembers no leftover for this State file.
//
// Regression witness: the command path dropped the token on every error of
// the reservation, and kept one after a failed release only when the lock was
// not had in time. The next step was then refused as held.
//
// The write failures are planted in the writer's two seams; no crash and no
// real unhealthy directory is involved. The two sessions are in one process:
// what another process does with a reservation of a live owner is the config
// layer's contract (it is refused as held) and is not shown here.
func TestCursorReservationThatStaysIsReleasedByItsOwner(t *testing.T) {
	for _, fault := range []string{"reserve", "release", "persist"} {
		for _, next := range []string{"the same session", "a new session of the process"} {
			t.Run(fault+"/"+next, func(t *testing.T) {
				e, a, s, generation, room := markedBindDeliveryFixture(t, false)
				ctx := context.Background()
				stateFile := e.paths().StateFile("alice")
				start, subs, err := s.startBindDelivery(ctx, a.Now())
				if err != nil || !start.cached {
					t.Fatalf("premise: the stable binding's cached view: cached=%t err=%v", start.cached, err)
				}
				id := e.hs.inject(room, "@bob:"+testServer, msg("@alice:"+testServer+" waiting mail", nil))
				deadline := time.Now().Add(5 * time.Second)

				switch fault {
				case "reserve":
					renames, syncs := 0, 0
					restore := config.PlantBindPartWriteFaults(func() error {
						if renames++; renames == 2 {
							return os.ErrPermission
						}
						return nil
					}, func() error {
						if syncs++; syncs == 1 {
							return errors.New("planted directory-sync failure")
						}
						return nil
					})
					t.Cleanup(restore)
					_, err = s.syncIdleBindNow(ctx, start.view, subs.Subs, deadline)
					restore()
					if !errors.Is(err, config.ErrBindPartWrite) {
						t.Fatalf("the sync step whose reservation could not be taken back: %v, want ErrBindPartWrite", err)
					}
					if renames != 2 || syncs != 1 {
						t.Fatalf("premise: the step made %d replacements and %d directory syncs, want 2 and 1", renames, syncs)
					}
				case "release":
					token, err := s.beginBindCursorView(ctx, start.view, "delivery", deadline)
					if err != nil {
						t.Fatalf("premise: the reservation: %v", err)
					}
					restore := config.PlantBindPartWriteFaults(func() error { return os.ErrPermission }, nil)
					t.Cleanup(restore)
					err = s.abortBindCursor(token)
					restore()
					if !errors.Is(err, os.ErrPermission) {
						t.Fatalf("the release that cannot replace the file: %v, want the planted failure", err)
					}
				case "persist":
					token, err := s.beginBindCursorView(ctx, start.view, "delivery", deadline)
					if err != nil {
						t.Fatalf("premise: the reservation: %v", err)
					}
					state, err := os.ReadFile(stateFile)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(stateFile, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
					_, err = s.persistReserved(ctx, batch{}, start.view.NextBatch, "s-never-written", deadline, token)
					if writeErr := os.WriteFile(stateFile, state, 0o600); writeErr != nil {
						t.Fatal(writeErr)
					}
					if err == nil || cursorTakenElsewhere(err) {
						t.Fatalf("the write with an unreadable State: %v, want a failure that is not \"taken elsewhere\"", err)
					}
				}

				other := config.BindOperationOwner{Holder: "another-owner", Host: machineLabel(), PID: os.Getpid(), Birth: "another-start"}
				otherToken := strings.Repeat("9", 32)
				alive := func(config.BindOperationOwner) bool { return false }
				if _, err := config.BeginBindCursor(e.paths(), "alice", otherToken, "bind", start.view.NextBatch, generation, other, alive); !errors.Is(err, config.ErrBindPartHeld) {
					t.Fatalf("premise: the reservation is not in place after the failure: another owner got %v, want ErrBindPartHeld", err)
				}
				if e.mailbox("alice").Has(id) {
					t.Fatal("premise: the mail was delivered by the step that failed")
				}

				if next == "the same session" {
					_, err = s.syncIdleBindNowOrSyncNow(ctx, a)
				} else {
					fresh, session := bindIdleSession(t, e, "alice")
					_, err = session.syncIdleBindNowOrSyncNow(ctx, fresh)
				}
				if err != nil {
					t.Fatalf("the next step of the same process: %v", err)
				}
				if !e.mailbox("alice").Has(id) {
					t.Fatal("the next step did not deliver the waiting mail")
				}
				view, err := config.ReadBindPartView(e.paths(), "alice", e.state("alice"))
				if err != nil || view.NextBatch == start.view.NextBatch || view.Generation != generation || view.Stopped {
					t.Fatalf("the cursor after the next step: %q, before %q; binding %+v err=%v", view.NextBatch, start.view.NextBatch, view, err)
				}

				if _, err := config.BeginBindCursor(e.paths(), "alice", otherToken, "bind", view.NextBatch, generation, other, alive); err != nil {
					t.Fatalf("control: a reservation is left behind: %v", err)
				}
				if err := config.AbortBindCursor(e.paths(), "alice", otherToken); err != nil {
					t.Fatal(err)
				}
				if left := leftoverCursors.of(s.paths.StateFile(s.name)); len(left) != 0 {
					t.Fatalf("control: the process still remembers %d leftover reservations", len(left))
				}
			})
		}
	}
}

// TestLeftoverCursorsKeepEveryToken proves the set the leftovers are kept in:
// a second token for one State file does not replace the first, a token is
// kept once, a State file's tokens are its own, and a token leaves only when
// it is dropped.
//
// Regression witness: one slot per State file, in which a later token
// replaced an earlier one.
func TestLeftoverCursorsKeepEveryToken(t *testing.T) {
	var set leftoverCursorSet
	if got := set.of("a"); len(got) != 0 {
		t.Fatalf("an empty set answers %v", got)
	}
	set.keep("a", "first")
	set.keep("a", "second")
	set.keep("a", "first")
	set.keep("b", "third")
	if got := set.of("a"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("the tokens of a: %v, want first and second", got)
	}
	if got := set.of("b"); len(got) != 1 || got[0] != "third" {
		t.Fatalf("the tokens of b: %v, want third", got)
	}
	set.drop("a", "absent")
	set.drop("a", "first")
	if got := set.of("a"); len(got) != 1 || got[0] != "second" {
		t.Fatalf("after dropping first: %v, want second", got)
	}
	set.drop("a", "second")
	if got := set.of("a"); len(got) != 0 {
		t.Fatalf("after dropping both: %v", got)
	}
	if got := set.of("b"); len(got) != 1 {
		t.Fatalf("dropping the tokens of a changed b: %v", got)
	}
}
