package cli

import (
	"context"
	"encoding/json"
	"github.com/relux-works/a2a-swarm/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Public who reads a self-keyed synthetic Matrix record; no local status state
// is forged and no foreign credential is used. Invalid telemetry must not turn
// into extra output lines or a fresh, factual suspected-orphan assertion.
func TestReviewCR3WhoNoticeSchema(t *testing.T) {
	for _, kind := range []string{"valid", "newline-since", "invalid-reason", "missing-since"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			e.as("alice", statusArgs("working")...).ok(t)
			s, err := eApp(e).sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			r := *e.state("alice").Liveness
			v := true
			r.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &v, Reason: orphanReason, Since: "2026-10-20T16:00:00Z", ObservedAt: "2026-10-20T16:01:00Z"}
			switch kind {
			case "newline-since":
				r.OwnNotice.Since = "2026-10-20T16:00:00Z\nFORGED_SECOND_STATUS"
			case "invalid-reason":
				r.OwnNotice.Reason = "UNBOUNDED_UNVERIFIED_REASON"
			case "missing-since":
				r.OwnNotice.Since = ""
			}
			if _, err = s.client.PutState(context.Background(), room, StatusEventType, s.creds.UserID, r); err != nil {
				t.Fatal(err)
			}
			text := e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
			raw := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
			t.Logf("text=%q JSON=%s", text, raw)
			var rows []whoRow
			if err = json.Unmarshal([]byte(raw), &rows); err != nil {
				t.Fatal(err)
			}
			if kind == "valid" {
				if !strings.Contains(text, "SUSPECTED-DEAF: own notice possibly orphaned") {
					t.Fatal("valid own state absent")
				}
				return
			}
			if strings.Contains(text, "\nFORGED_SECOND_STATUS") {
				t.Fatal("untrusted notice since injected an output line")
			}
			for _, row := range rows {
				if row.Name == "alice" && row.Health == "fresh" && strings.Contains(text, "SUSPECTED-DEAF: own notice possibly orphaned") {
					t.Fatal("invalid notice facts reported as fresh suspected state")
				}
			}
		})
	}
}

// Validate the new public contract against actual production no-recovery paths,
// then examine the shipped reference used to operate those paths.
func TestReviewCR3PublishedRecoveryContract(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.e.as("alice", "inbox").ok(t)
	f.mail()
	f.control(map[string]any{})
	f.step()
	before := f.wait(func(s orphanScreen) bool { return true })
	f.step()
	f.control(map[string]any{})
	after := f.wait(func(s orphanScreen) bool { return true })
	if before.Text != after.Text || after.Erased != 0 || len(after.Submitted) != 0 {
		t.Fatal("candidate violates no-recovery prerequisite")
	}
	t.Logf("actual dispatch: erased=%d submitted=%d typed=%v", after.Erased, len(after.Submitted), f.binding().Typed)
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, obsolete := range []string{"it erases only a complete notice suffix", "unsubmitted notice and may be completed after a fresh recheck", "completing the exact journal-owned input uses the existing reservation"} {
		if strings.Contains(string(doc), obsolete) {
			t.Errorf("shipped operational reference promises retired recovery: %q", obsolete)
		}
	}
	for _, required := range []string{"SUSPECTED-DEAF", "own_notice_possibly_orphaned", "owned-notice-orphan-suspected"} {
		if !strings.Contains(string(doc), required) {
			t.Errorf("new public detect/report interface undocumented: %s", required)
		}
	}
}

// Fresh unknown after an actual suspicion must propagate to both public views
// without changing first-since, heartbeat, mail or rendering any keystroke.
func TestReviewCR3UnknownPublicRoundTrip(t *testing.T) {
	f := newRev3PaddedFixture(t, "claude")
	f.e.as("alice", "status", "init").ok(t)
	f.e.as("alice", statusArgs("working")...).ok(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.e.as("alice", "inbox").ok(t)
	id := f.mail()
	f.control(map[string]any{})
	f.step()
	requireNotice(t, f.binding().OwnNotice, "suspected")
	since := f.binding().OwnNotice.Since
	f.e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
	f.step()
	raw := f.e.as("alice", "bind", "status", "--json").ok(t).stdout
	if !strings.Contains(raw, `"own_notice_possibly_orphaned":null`) || !strings.Contains(raw, since) {
		t.Fatalf("gate did not preserve unknown last suspicion: %s", raw)
	}
	who := f.e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
	if !strings.Contains(who, "own notice unknown; last suspected since="+since) {
		t.Fatalf("unknown lost at who: %s", who)
	}
	f.e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
	f.control(map[string]any{"text": ""})
	f.dispatch()
	if len(f.binding().Reservations) != 1 {
		t.Fatal("empty first glimpse spent rate")
	}
	f.now = f.now.Add(time.Second)
	f.dispatch()
	f.step()
	f.control(map[string]any{})
	screen := f.wait(func(s orphanScreen) bool { return true })
	if len(screen.Submitted) != 1 || screen.Erased != 0 || f.e.received("alice", id) {
		t.Fatalf("unknown/empty recovery lost mail or duplicated effects: %+v", screen)
	}
}

// A synthetic sync-only outage leaves publication operational. A running
// consumer's new unknown state must not be advertised as current true by who.
func TestReviewCR3ConsumerUnknownPublication(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.e.as("alice", "status", "init").ok(t)
	f.e.as("alice", statusArgs("working")...).ok(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.e.as("alice", "inbox").ok(t)
	id := f.mail()
	f.control(map[string]any{})
	f.step()
	requireNotice(t, f.binding().OwnNotice, "suspected")
	since := f.binding().OwnNotice.Since
	f.a.Now = time.Now
	f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "synthetic sync-only interruption")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.a.runTerminal(ctx, f.s, "orphan-gen", *f.binding()); done <- err }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("consumer cleanup timeout")
		}
	}()
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "consumer unavailable; retrying" })
	local := f.e.as("alice", "bind", "status", "--json").ok(t).stdout
	remote := f.e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
	t.Logf("local=%s remote=%q", local, remote)
	if !strings.Contains(local, `"own_notice_possibly_orphaned":null`) {
		t.Fatal("consumer did not expose local unknown")
	}
	if !strings.Contains(remote, "own notice unknown; last suspected since="+since) {
		t.Error("consumer failure leaves who advertising suspected while bind reports unknown")
	}
	if f.e.received("alice", id) {
		t.Error("sync failure receipted pending mail")
	}
	// Publication endpoint remains operational during the sync-only outage.
	beforeHeartbeat := f.e.state("alice").Liveness.HeartbeatAt
	f.a.publishTerminalNotice(context.Background(), f.s, "orphan-gen")
	repaired := f.e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
	t.Logf("nearby explicit publication control=%q", repaired)
	if !strings.Contains(repaired, "own notice unknown; last suspected since="+since) || !f.e.state("alice").Liveness.HeartbeatAt.Equal(beforeHeartbeat) {
		t.Fatal("available own publisher failed unknown/heartbeat positive control")
	}
}
