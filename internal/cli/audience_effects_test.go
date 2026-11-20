package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// This uses the existing isolated native fixture: a private Unix socket,
// owned synthetic runtime and fake submission recorder, never a provider.
func nativeAddressVariant(t *testing.T, e *env, room, variant, target string) string {
	t.Helper()
	sender := "@bob:" + e.hs.server
	switch variant {
	case "reply":
		extra := addressMentions("@" + target + ":" + e.hs.server)
		extra["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$followed"}
		return e.hs.inject(room, sender, msg("reply quoting @"+"alice:"+e.hs.server, extra))
	case "edit":
		id := e.hs.inject(room, sender, msg("original", addressHeader(target)))
		addressSync(t, e, "alice")
		e.as("alice", "inbox").ok(t)
		extra := addressHeader("alice")
		extra["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": id}
		extra["m.new_content"] = msg("replacement", addressHeader("alice"))
		return e.hs.inject(room, sender, msg("edited", extra))
	case "redaction":
		id := e.hs.inject(room, sender, msg("body to scrub", addressHeader(target)))
		addressSync(t, e, "alice")
		e.hs.redact(id, sender)
		e.hs.mu.Lock()
		e.hs.appendLocked(room, sender, "m.room.redaction", map[string]any{"redacts": id})
		e.hs.mu.Unlock()
		return id
	case "reaction":
		id := e.hs.inject(room, sender, msg("original", addressHeader(target)))
		addressSync(t, e, "alice")
		e.as("alice", "inbox").ok(t)
		e.hs.mu.Lock()
		id = e.hs.appendLocked(room, sender, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": id, "key": "fixture"}})
		e.hs.mu.Unlock()
		return id
	case "retained-restart":
		id := "$retained-" + target
		raw := addressJSON(matrix.Event{EventID: id, Type: "m.room.message", Sender: sender, Content: addressJSON(msg("retained", addressHeader(target)))})
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = mb.Append([]config.MailEntry{{ID: id, Kind: config.MailEvent, Room: room, Rel: "to", Event: raw}}); err != nil {
			t.Fatal(err)
		}
		return id
	case "mixed":
		e.hs.inject(room, sender, msg("excluded", addressHeader("carol")))
		return e.hs.inject(room, sender, msg("mixed", addressHeader(target)))
	default:
		return e.hs.inject(room, sender, msg("ordinary", addressHeader(target)))
	}
}

// Native dispatch counts only eligible mail across originals, replies,
// relations, restart and mixed batches. Rejection must make zero native turns
// and spend no notification/hourly reservation, while its paired positive does.
func TestAudienceNativeEffectMatrix(t *testing.T) {
	for _, variant := range []string{"original", "reply", "edit", "redaction", "reaction", "retained-restart", "mixed"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%t", variant, own), func(t *testing.T) {
				e, room, n, g := runtimeWorld(t)
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.AddThread("$followed"); return nil })
				if err != nil {
					t.Fatal(err)
				}
				target := "carol"
				if own {
					target = "alice"
				}
				id := nativeAddressVariant(t, e, room, variant, target)
				addressSync(t, e, "alice")
				e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
				want := own && variant != "reaction"
				if (len(n.wakes) > 0) != want || len(n.wakes) > 1 {
					t.Fatalf("native submissions=%d want=%t", len(n.wakes), want)
				}
				st := e.state("alice")
				if !want && (st.Wake.Attempt != nil || len(st.Wake.Reservations) > 0 || len(st.Wake.Rung) > 0 || st.Notification.Reservation != nil) {
					t.Fatal("excluded native mail charged/reserved/rung")
				}
				if want && !strings.Contains(n.wakes[0], "1 new peer event") {
					t.Fatalf("notice counts excluded IDs: %q", n.wakes[0])
				}
				if e.received("alice", id) {
					t.Fatal("native wake created receipt")
				}
				if variant == "reaction" {
					runtimeMail(e, room, "positive")
					e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
					if len(n.wakes) != 1 {
						t.Fatal("ordinary positive rejected")
					}
				}
			})
		}
	}
}

// Change only one evidence version in an isolated fixture. This models a
// concurrent unsupported writer after selection, not a production mutation API.
func invalidateAudienceEvidence(t *testing.T, p config.Paths, name, id string) {
	t.Helper()
	raw, err := os.ReadFile(p.MailboxFile(name))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	changed := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal("fixture mailbox JSON")
		}
		if entries := row["audiences"]; len(entries) > 0 {
			var evidence []map[string]any
			if json.Unmarshal(entries, &evidence) != nil {
				t.Fatal("fixture evidence")
			}
			for _, e := range evidence {
				if e["event_id"] == id {
					e["version"] = float64(99)
					changed = true
				}
			}
			row["audiences"] = addressJSON(evidence)
			line = string(addressJSON(row))
		}
		lines = append(lines, line)
	}
	if !changed {
		t.Fatal("control plant was not applied")
	}
	if err = os.WriteFile(p.MailboxFile(name), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// A late audience change must be detected at the native send fence, with no
// turn or new rate charge; an unchanged candidate still submits exactly once.
func TestAudienceNativeSendFence(t *testing.T) {
	for _, plant := range []bool{false, true} {
		t.Run(fmt.Sprint(plant), func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			id := runtimeMail(e, room, "fixture")
			prior := e.configure
			e.configure = func(a *App) {
				prior(a)
				if plant {
					a.afterWakeReserve = func() error { invalidateAudienceEvidence(t, e.paths(), "alice", id); return nil }
				}
			}
			r := e.as("alice", "runtime", "dispatch", "--generation", g)
			if !plant {
				r.ok(t)
				if len(n.wakes) != 1 {
					t.Fatal("positive fence rejected")
				}
				return
			}
			if r.code != ExitError || !strings.Contains(r.stderr, "ADDRESS_STATE_UNAVAILABLE") || len(n.wakes) != 0 {
				t.Fatalf("late exclusion submitted: %+v", r)
			}
			st := e.state("alice")
			if st.Wake.Attempt != nil || len(st.Wake.Reservations) != 0 || st.Notification.Reservation != nil || e.received("alice", id) {
				t.Fatal("late exclusion spent/retained reservation or receipt")
			}
		})
	}
}

// Terminal dispatch uses the actual shared selector and mailbox with a private
// fake runtime in its own tmux server. A nonrecipient never types or presses
// Enter; its positive pair types/submits exactly one count-only notice.
func TestAudienceTerminalEffectMatrix(t *testing.T) {
	for _, variant := range []string{"original", "reply", "edit", "redaction", "reaction", "retained-restart", "mixed"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%t", variant, own), func(t *testing.T) {
				f := newOrphanDispatchFixture(t)
				target := "carol"
				if own {
					target = "alice"
				}
				id := nativeAddressVariant(t, f.e, f.room, variant, target)
				addressSync(t, f.e, "alice")
				for i := 0; i < 4; i++ {
					f.step()
				}
				screen := f.wait(func(orphanScreen) bool { return true })
				want := own && variant != "reaction"
				if want {
					if len(screen.Submitted) != 1 || screen.Submitted[0] != tmuxwake.Notice(1) {
						t.Fatalf("positive terminal effects=%+v", screen)
					}
				} else {
					if screen.Text != "" || len(screen.Submitted) != 0 || screen.Erased != 0 || screen.Typed != 0 {
						t.Fatalf("excluded terminal effects=%+v", screen)
					}
					b := f.binding()
					if len(b.AttemptIDs) > 0 || len(b.Reservations) > 0 || b.TypingStarted || len(b.Rung) > 0 || len(f.e.state("alice").MailAlarm.Jobs) > 0 {
						t.Fatal("excluded terminal reserved/rung/alarmed")
					}
				}
				if f.e.received("alice", id) {
					t.Fatal("terminal wake receipted mail")
				}
				if variant == "reaction" {
					f.mail()
					for i := 0; i < 4; i++ {
						f.step()
					}
					if got := f.wait(func(orphanScreen) bool { return true }); len(got.Submitted) != 1 {
						t.Fatal("ordinary terminal positive rejected")
					}
				}
			})
		}
	}
}

// The final recovery fence revalidates retained IDs under its existing state
// lock. Stale nonrecipient IDs cannot spend Retried or another hourly slot.
func TestAudienceRetainedAttemptFence(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			target := "carol"
			if own {
				target = "alice"
			}
			id := nativeAddressVariant(t, f.e, f.room, "original", target)
			addressSync(t, f.e, "alice")
			// Qualify the actual fixture's ready empty composer before retaining a
			// notice; all ownership/readiness checks remain real and unchanged.
			r := boundTerminal(*f.binding()).ObserveReadiness(context.Background(), "")
			b := *f.binding()
			b.AttemptIDs = []string{id}
			b.Notice = tmuxwake.Notice(1)
			b.NoticeCount = 1
			b.TypingStarted = true
			b.AttemptAt = f.now.Add(-time.Minute)
			b.NoticeOrigin = &config.TerminalIdle{Generation: b.Generation, SessionID: b.SessionID, Host: b.Host, Target: b.Target, ClaudeRegistryRoot: b.ClaudeRegistryRoot, ReadinessRoute: r.Key.Route, RegistrySessionID: r.Key.RegistrySessionID, RegistryVersion: r.Key.RegistryVersion, PaneWidth: r.Key.PaneWidth}
			f.update(func(st *config.State) {
				token, err := st.Notification.Reserve("terminal", b.SessionID, b.Generation, b.AttemptAt, false)
				if err != nil {
					t.Fatal(err)
				}
				b.NotificationToken = token
				st.Terminal = &b
				st.Lease = &config.Lease{Holder: b.SessionID, Host: machineLabel(), PID: b.OwnerPID, Expires: f.now.Add(time.Hour)}
			})
			_, err := f.a.reserveTerminalRecovery(context.Background(), f.s, b.Generation, b, r.Key)
			if own {
				if err != nil {
					t.Fatal("positive retained fence", err)
				}
			} else if err == nil {
				t.Fatal("nonrecipient retained fence admitted")
			}
			if !own {
				after := f.binding()
				if after.Retried || len(after.Reservations) > 0 {
					t.Fatal("rejected recovery charged retry")
				}
			}
		})
	}
}

// A version change after the real terminal reservation but before literal
// typing must invoke no terminal effect and refund only the unstarted attempt.
func TestAudienceTerminalPreTypeFence(t *testing.T) {
	for _, plant := range []bool{false, true} {
		t.Run(fmt.Sprint(plant), func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := nativeAddressVariant(t, f.e, f.room, "original", "alice")
			addressSync(t, f.e, "alice")
			if plant {
				f.a.afterNotificationReserve = func() error { invalidateAudienceEvidence(t, f.e.paths(), "alice", id); return nil }
			}
			rejected := false
			for i := 0; i < 4; i++ {
				f.now = f.now.Add(orphanFixtureStep)
				err := f.a.dispatchTerminal(context.Background(), f.s, f.binding().Generation)
				if err != nil {
					if !plant || !errors.Is(err, errAddressChanged) {
						t.Fatal("unexpected terminal failure", err)
					}
					rejected = true
				}
			}
			screen := f.wait(func(orphanScreen) bool { return true })
			if !plant {
				if len(screen.Submitted) != 1 {
					t.Fatal("positive pre-type gate rejected")
				}
				return
			}
			b := f.binding()
			if !rejected || screen.Text != "" || len(screen.Submitted) > 0 || screen.Erased > 0 || screen.Typed > 0 || b.TypingStarted || len(b.AttemptIDs) > 0 || len(b.Reservations) > 0 || f.e.state("alice").Notification.Reservation != nil {
				t.Fatal("pre-type audience failure caused effects or charges")
			}
		})
	}
}

// A late hook exclusion cancels its shared token, emits no context/Stop block,
// and preserves the original unread mail. The unmodified positive still rings.
func TestAudienceHookSendFence(t *testing.T) {
	for _, plant := range []bool{false, true} {
		t.Run(fmt.Sprint(plant), func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "inbox").ok(t)
			id := e.hs.inject(room, "@sender:example.org", msg("fixture", addressHeader("alpha")))
			if plant {
				e.configure = func(a *App) {
					a.afterNotificationReserve = func() error { invalidateAudienceEvidence(t, e.paths(), "alpha", id); return nil }
				}
			}
			r := e.hook("alpha", "stop", `{"session_id":"audience-fixture"}`).ok(t)
			if (r.stdout != "") == plant {
				t.Fatal("hook send fence eligibility", r)
			}
			if plant {
				st := e.state("alpha")
				for _, h := range st.HookSessions {
					if len(h.Rung) > 0 || h.Blocks != 0 || h.LastBlock != 0 {
						t.Fatal("late hook exclusion changed delivery")
					}
				}
				if st.Notification.Reservation != nil || e.received("alpha", id) {
					t.Fatal("late hook exclusion retained charge or received")
				}
			}
		})
	}
}

// Addressing grants eligibility, never permission to bypass the existing rate
// hold. The same pending mail submits after the fixture releases that hold.
// This check covers the rate gate, not priority-class timing.
func TestAudienceAddressedHeldByRate(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "fixture")
	now := time.Now()
	e.now = func() time.Time { return now }
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Runtime.MaxPerHour = 1
		st.Wake.Reservations = []time.Time{now}
		st.Notification = config.Notification{Version: 1, NotBefore: now.Add(-time.Second)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 0 || len(e.state("alice").Wake.Reservations) != 1 {
		t.Fatal("audience bypassed or charged rate hold")
	}
	_, s := addressSession(t, e, "alice")
	mb, err := s.openAudienceMailbox()
	if err != nil || len(mb.Pending()) != 1 {
		t.Fatal("rate hold lost eligible pending mail", err)
	}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Wake.Reservations = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("released positive did not submit exactly once without receipt")
	}
}
