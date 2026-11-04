package cli

import (
	"context"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"reflect"
	"testing"
	"time"
)

// This pure carry contract checks every pinned field, including a retry cooldown,
// and forbids stale reports at session/target/manual/unbind/episode boundaries.
// The separate hook regression proves this helper is used by real replacement.
func TestTerminalNoticeEpisodeCarryBoundaries(t *testing.T) {
	for _, mode := range []string{"same", "unknown", "drained", "different-session", "different-target", "different-owner", "manual", "unbound", "cleared", "new-episode"} {
		t.Run(mode, func(t *testing.T) {
			target := tmuxwake.Target{Socket: "/synthetic/socket", SocketID: "pin", Pane: "%1", Runtime: "codex", OwnerPID: 123, OwnerBirth: "birth"}
			n := schemaNotice("true")
			report := &config.NoticeReport{Since: n.Since, Sent: true, RetryAt: time.Unix(100, 0), Outcome: "published", Message: config.LivenessAlertMessage{Room: "!system:test.local", Txn: "fixed-txn", Content: map[string]any{"body": "fixed"}}}
			old := &config.TerminalBinding{Generation: "old", SessionID: "session", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, OwnNotice: n, NoticeReport: report}
			next := &config.TerminalBinding{Generation: "new", SessionID: old.SessionID, Target: target}
			automatic := true
			switch mode {
			case "unknown":
				old.OwnNotice.PossiblyOrphaned = nil
			case "drained":
				old.Stopped = true
				old.Outcome = "cancelled"
			case "different-session":
				next.SessionID = "other"
			case "different-target":
				next.Target.Pane = "%2"
			case "different-owner":
				next.Target.OwnerBirth = "other"
			case "manual":
				automatic = false
			case "unbound":
				old.Outcome = "unbound"
			case "cleared":
				old.OwnNotice = schemaNotice("false")
			case "new-episode":
				old.OwnNotice.Since = "2026-10-20T16:00:01Z"
			}
			carryTerminalNoticeEpisode(old, next, automatic)
			want := mode == "same" || mode == "unknown" || mode == "drained"
			if want {
				if !reflect.DeepEqual(next.NoticeReport, report) || next.OwnNotice.Since != old.OwnNotice.Since || next.OwnNotice.PossiblyOrphaned != nil {
					t.Fatal("episode lost pinned report state")
				}
			} else if next.NoticeReport != nil {
				t.Fatal("report crossed an episode/binding boundary")
			}
			if next.NotificationToken != "" {
				t.Fatal("replacement gained Enter authority")
			}
		})
	}
}

// The original 24 review combinations plus changed targets retain manual
// recovery, including exact-looking drafts. Actual typing establishes journal
// provenance; every case asserts zero Enter/erase, unread mail and one original
// wake reservation. Exact known input is a positive detection/report control,
// not an automatic recovery positive. Fresh Send controls are exercised nearby.
func TestTerminalNoticeManualReviewMatrix(t *testing.T) {
	for _, layout := range []string{"codex", "claude-legacy", "claude-named"} {
		for _, variant := range []string{"exact", "partial", "suffix", "human", "busy", "unknown", "dim", "foreign-generation", "changed-target"} {
			t.Run(layout+"/"+variant, func(t *testing.T) {
				f := newOrphanDispatchFixture(t)
				system := f.e.ownReportRoom("alice")
				id := f.mail()
				initial := map[string]any{"defer": true}
				if layout != "codex" {
					dispatchRegistry(t, f, "idle")
					initial["claude"] = true
					initial["named"] = layout == "claude-named"
				}
				f.control(initial)
				f.dispatch()
				f.step()
				if !f.binding().Typed {
					t.Fatal("probe never stranded an actual typed notice")
				}
				mode := map[string]any{"text": tmuxwake.Notice(1), "hint": true}
				if layout != "codex" {
					mode["claude"] = true
					mode["named"] = layout == "claude-named"
				}
				switch variant {
				case "partial":
					mode["text"] = "a2a: 1 new peer"
				case "suffix":
					mode["text"] = tmuxwake.Notice(1) + " USER-TEXT"
				case "human":
					mode["text"] = "USER-TEXT"
				case "busy", "unknown", "dim":
					mode[variant] = true
				case "changed-target":
					f.update(func(st *config.State) { st.Terminal.Target.SocketID = "replaced" })
				case "foreign-generation":
					f.update(func(st *config.State) { st.Terminal.Generation = "other-binding" })
				}
				f.control(mode)
				if variant == "unknown" && boundTerminal(*f.binding()).AssessReadiness(context.Background()).Input.State != tmuxwake.InputUnavailable {
					t.Fatal("setup: unknown renderer was not unavailable")
				}
				for _, elapsed := range []time.Duration{10 * time.Minute, time.Second, 11 * time.Minute} {
					f.now = f.now.Add(elapsed)
					f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
					err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
					if variant == "foreign-generation" {
						if err == nil || err.Error() != "binding retired" {
							t.Fatal("stale generation did not reject with binding retired", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					state := f.wait(func(s orphanScreen) bool { return true })
					if state.Text != mode["text"] || state.Erased != 0 || len(state.Submitted) != 0 || f.e.received("alice", id) || len(f.binding().Reservations) != 1 {
						t.Fatalf("forbidden effect: %+v", state)
					}

				}
				count := len(f.e.projectionsFrom(system, "@alice:test.local"))

				if count > 1 {
					t.Fatal("duplicate incident", count)
				}
				if variant == "exact" && count != 1 {
					t.Fatal("manual recovery did not report once", count)
				}
				if variant == "foreign-generation" && count != 0 {
					t.Fatal("retired binding reported", count)
				}
			})
		}
	}
}

// The scoped observation writer retires a report on a fresh clear, and a later
// suspicion starts a new episode without inheriting the old transaction. This
// is a state-writer test; terminal classification is exercised by the matrix.
func TestTerminalNoticeReportClearedEpisode(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	b := f.binding()
	if !b.Typed {
		t.Fatal("setup: typed journal absent")
	}
	if err := f.a.observeTerminalNotice(f.s, b.Generation, *b, "nonempty"); err != nil {
		t.Fatal(err)
	}
	oldSince := f.binding().OwnNotice.Since
	if oldSince == "" {
		t.Fatal("setup: no suspicion episode")
	}
	f.update(func(st *config.State) {
		st.Terminal.NoticeReport = &config.NoticeReport{Since: oldSince, Sent: true, Message: config.LivenessAlertMessage{Txn: "old-episode"}}
	})
	b = f.binding()
	if err := f.a.observeTerminalNotice(f.s, b.Generation, *b, "empty"); err != nil {
		t.Fatal(err)
	}
	if f.binding().NoticeReport != nil || f.binding().OwnNotice.Since != "" {
		t.Fatal("clear retained incident authority")
	}
	f.now = f.now.Add(time.Second)
	b = f.binding()
	if err := f.a.observeTerminalNotice(f.s, b.Generation, *b, "nonempty"); err != nil {
		t.Fatal(err)
	}
	if n := f.binding(); n.OwnNotice.Since == "" || n.OwnNotice.Since == oldSince || n.NoticeReport != nil {
		t.Fatal("new suspicion inherited old episode/report")
	}
	screen := f.wait(func(s orphanScreen) bool { return true })
	if len(screen.Submitted) != 0 || screen.Erased != 0 || len(f.binding().Reservations) != 1 {
		t.Fatal("observation changed terminal/rate")
	}
}
