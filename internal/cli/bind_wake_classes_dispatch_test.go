//go:build linux || darwin

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// wakeClassMail injects one message from sender and models the consumer's
// ingestion into alice's mailbox, like runtimeMail.
func wakeClassMail(e *env, room, sender string, content map[string]any) string {
	e.t.Helper()
	id := e.hs.inject(room, sender, content)
	a := NewApp()
	a.as = "alice"
	a.HTTP = e.hs.srv.Client()
	a.ctx = context.Background()
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}
	if e.now != nil {
		a.Now = e.now
	}
	s, err := a.session()
	if err != nil {
		e.t.Fatal(err)
	}
	st, subs, err := s.startDelivery(context.Background(), a.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err = s.syncNow(context.Background(), st, subs.Subs, time.Now().Add(time.Second)); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// Proves the dispatch timeline end to end with a fake terminal. Standard
// mode (no recorded policy) is the golden control: plain batch-class mail
// wakes at once under the single cap. In economy the same mail waits for
// the batch interval, addressed peer mail wakes and carries it, pause and
// lease gates still block owner mail, a flooded peer class is capped with
// one recorded outcome, and owner mail still wakes in that hour carrying
// the capped peer entry (never dropped, never rung twice).
func TestWakeClassDispatchTimeline(t *testing.T) {
	e, a, s, fake, now, room := quiescentMuteWorld(t, false)
	e.as("alice", "host", "principals", "set", wakeTestPrincipal).ok(t)
	cycle := func() {
		t.Helper()
		if err := a.dispatchTerminal(context.Background(), s, "scope-terminal"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
	}
	untilTypes := func(want int) {
		t.Helper()
		for i := 0; i < 25 && fake.types < want; i++ {
			cycle()
		}
		if fake.types != want {
			t.Fatalf("types = %d, want %d (outcome %q)", fake.types, want, e.state("alice").Terminal.Outcome)
		}
	}
	advance := func(d time.Duration) { *now = now.Add(d) }

	// Golden standard timeline: no policy recorded, plain mail wakes.
	first := runtimeMail(e, room, "standard one")
	untilTypes(1)
	st := e.state("alice")
	if st.WakeClasses != nil || !slices.Contains(st.Terminal.Rung, first) || len(st.Terminal.Reservations) != 1 {
		t.Fatalf("standard wake diverged: policy=%+v rung=%v reservations=%v", st.WakeClasses, st.Terminal.Rung, st.Terminal.Reservations)
	}

	// Economy from here, started now.
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error {
		cur.WakeClasses = wakePolicy(*now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batchID := runtimeMail(e, room, "batch two")
	advance(31 * time.Second) // common spacing over, batch interval not
	for i := 0; i < 3; i++ {
		cycle()
	}
	st = e.state("alice")
	if fake.types != 1 || st.Terminal.Outcome != "suppressed: batch interval" || slices.Contains(st.Terminal.Rung, batchID) || e.received("alice", batchID) {
		t.Fatalf("batch mail not held: types=%d outcome=%q rung=%v", fake.types, st.Terminal.Outcome, st.Terminal.Rung)
	}
	peerID := wakeClassMail(e, room, "@bob:test.local", toMsg("bob", "question", "peer question"))
	untilTypes(2)
	st = e.state("alice")
	if !slices.Contains(st.Terminal.Rung, batchID) || !slices.Contains(st.Terminal.Rung, peerID) || len(st.WakeClasses.PeerReservations) != 1 || len(st.WakeClasses.OwnerReservations) != 0 {
		t.Fatalf("peer wake did not carry both: rung=%v policy=%+v", st.Terminal.Rung, st.WakeClasses)
	}
	advance(31 * time.Second)
	cycle()
	if fake.types != 2 {
		t.Fatalf("rung mail woke again: types=%d", fake.types)
	}

	// Pause and lease gates still block owner mail; then it wakes (control).
	ownerID := wakeClassMail(e, room, wakeTestPrincipal, msg("owner instruction", nil))
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error { cur.HooksPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	cycle()
	if fake.types != 2 || e.state("alice").Terminal.Outcome != "suppressed: manual gate" {
		t.Fatalf("pause did not block owner wake: types=%d outcome=%q", fake.types, e.state("alice").Terminal.Outcome)
	}
	lease := e.state("alice").Lease
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error { cur.HooksPaused = false; cur.Lease = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	cycle()
	if fake.types != 2 || e.state("alice").Terminal.Outcome != "suppressed: lease authority" {
		t.Fatalf("lease loss did not block owner wake: types=%d outcome=%q", fake.types, e.state("alice").Terminal.Outcome)
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error { cur.Lease = lease; return nil }); err != nil {
		t.Fatal(err)
	}
	untilTypes(3)
	st = e.state("alice")
	if !slices.Contains(st.Terminal.Rung, ownerID) || len(st.WakeClasses.OwnerReservations) != 1 {
		t.Fatalf("owner wake after gates: rung=%v policy=%+v", st.Terminal.Rung, st.WakeClasses)
	}

	// Flooded peer class: capped, one episode, owner still wakes carrying it.
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error {
		cur.WakeClasses.PeerReservations = wakeFill(20, *now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cappedID := wakeClassMail(e, room, "@bob:test.local", toMsg("bob", "request", "peer flood"))
	advance(31 * time.Second)
	for i := 0; i < 3; i++ {
		cycle()
	}
	st = e.state("alice")
	if fake.types != 3 || st.Terminal.Outcome != "suppressed: peer hourly cap" || !st.WakeClasses.PeerSuppressed || slices.Contains(st.Terminal.Rung, cappedID) || e.received("alice", cappedID) {
		t.Fatalf("peer cap not held: types=%d outcome=%q policy=%+v", fake.types, st.Terminal.Outcome, st.WakeClasses)
	}
	owner2 := wakeClassMail(e, room, wakeTestPrincipal, msg("owner again", nil))
	untilTypes(4)
	st = e.state("alice")
	if !slices.Contains(st.Terminal.Rung, cappedID) || !slices.Contains(st.Terminal.Rung, owner2) || len(st.WakeClasses.OwnerReservations) != 2 || st.WakeClasses.PeerSuppressed {
		t.Fatalf("owner wake beside peer cap: rung=%v policy=%+v", st.Terminal.Rung, st.WakeClasses)
	}
	advance(31 * time.Second)
	cycle()
	if fake.types != 4 || fake.enters != 4 {
		t.Fatalf("duplicate delivery after capped entry woke: types=%d enters=%d", fake.types, fake.enters)
	}
}

// Proves a persisted unrecognized policy mode neither falls through to the
// legacy send path nor appears as standard in bind status.
func TestReviewUnknownWakePolicyFailsClosed(t *testing.T) {
	e, a, s, fake, now, room := quiescentMuteWorld(t, false)
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.WakeClasses = &config.WakeClassPolicy{Mode: "future-mode", StartedAt: *now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	id := runtimeMail(e, room, "ordinary pending mail")
	for i := 0; i < 25 && fake.types == 0; i++ {
		if err := a.dispatchTerminal(context.Background(), s, "scope-terminal"); err != nil {
			t.Fatal(err)
		}
		if fake.types == 0 {
			*now = now.Add(2 * time.Second)
		}
	}
	st := e.state("alice")
	status := wakeClassStatusOf(s, st, *now)
	statusOutput := e.as("alice", "bind", "status").ok(t).stdout
	if fake.types != 0 || slices.Contains(st.Terminal.Rung, id) || len(st.Terminal.Reservations) != 0 || st.Terminal.Outcome != "suppressed: invalid wake mode" || status.Mode != "invalid" || !strings.Contains(statusOutput, "mode=invalid") {
		t.Fatalf("unknown policy was not held and reported invalid: types=%d rung=%v reservations=%v outcome=%q status=%+v output=%q", fake.types, st.Terminal.Rung, st.Terminal.Reservations, st.Terminal.Outcome, status, statusOutput)
	}
}

// Proves the allowlist retains existing economy behavior when owner,
// addressed peer, and timer-due batch entries are pending together.
func TestReviewEconomyOwnerPeerAndDueBatchStillWake(t *testing.T) {
	e, a, s, fake, now, room := quiescentMuteWorld(t, false)
	e.as("alice", "host", "principals", "set", wakeTestPrincipal).ok(t)
	policy := wakePolicy(*now)
	policy.StartedAt = now.Add(-defaultBatchInterval)
	if !nextBatchDue(policy).Equal(*now) {
		t.Fatalf("positive-control batch timer is not due: %s", nextBatchDue(policy))
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.WakeClasses = policy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batchID := runtimeMail(e, room, "ordinary batch mail")
	peerID := wakeClassMail(e, room, "@bob:test.local", toMsg("bob", "question", "addressed peer mail"))
	ownerID := wakeClassMail(e, room, wakeTestPrincipal, msg("owner mail", nil))
	for i := 0; i < 25 && fake.types == 0; i++ {
		if err := a.dispatchTerminal(context.Background(), s, "scope-terminal"); err != nil {
			t.Fatal(err)
		}
		if fake.types == 0 {
			*now = now.Add(2 * time.Second)
		}
	}
	st := e.state("alice")
	for _, id := range []string{batchID, peerID, ownerID} {
		if !slices.Contains(st.Terminal.Rung, id) {
			t.Errorf("eligible mail %s was not delivered; rung=%v", id, st.Terminal.Rung)
		}
	}
	if fake.types != 1 || len(st.WakeClasses.OwnerReservations) != 1 || len(st.WakeClasses.PeerReservations) != 0 || !st.WakeClasses.LastBatchWake.Equal(*now) {
		t.Fatalf("valid economy classes changed: types=%d policy=%+v", fake.types, st.WakeClasses)
	}
}
