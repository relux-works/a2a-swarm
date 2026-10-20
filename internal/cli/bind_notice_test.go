package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func requireNotice(t *testing.T, n config.OwnNoticeObservation, want string) {
	t.Helper()
	got := "unknown"
	if n.PossiblyOrphaned != nil {
		got = "clear"
		if *n.PossiblyOrphaned {
			got = "suspected"
		}
	}
	if got != want {
		t.Fatalf("notice detection mismatch: got=%s want=%s %+v", got, want, n)
	}
	if want == "suspected" && (n.Reason != orphanReason || n.Since == "") {
		t.Fatalf("suspected facts missing: %+v", n)
	}
}

// Real type/deferral, own inbox receipt and later mail precede detection.
// Unknown preserves the original evidence, not false or a new first-since.
func TestTerminalNoticeDetection(t *testing.T) {
	for _, neighbor := range []string{"suspected", "partial", "no-journal", "ordinary-draft", "unknown", "no-new", "untyped", "count"} {
		t.Run(neighbor, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			old := f.mail()
			if neighbor == "partial" {
				f.mail()
			}
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if !f.binding().Typed {
				t.Fatal("missing actual typed journal")
			}
			if neighbor == "partial" {
				_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
					_, err := mb.Receive([]string{old})
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				f.e.as("alice", "inbox").ok(t)
			}
			current := ""
			if neighbor != "no-new" {
				current = f.mail()
			}
			mode := map[string]any{"hint": true}
			if neighbor == "untyped" {
				f.update(func(st *config.State) { st.Terminal.Typed = false })
			}
			if neighbor == "count" {
				f.update(func(st *config.State) { st.Terminal.NoticeCount = 2 })
			}
			if neighbor == "no-journal" || neighbor == "ordinary-draft" {
				f.update(func(st *config.State) {
					st.Terminal.Typed = false
					st.Terminal.AttemptIDs = nil
					st.Terminal.Notice = ""
					st.Terminal.NoticeCount = 0
				})
			}
			if neighbor == "ordinary-draft" {
				mode["text"] = "HUMAN_DRAFT"
			}
			if neighbor == "unknown" {
				mode["unknown"] = true
			}
			f.control(mode)
			f.step()
			want := "suspected"
			if neighbor == "partial" || neighbor == "no-journal" || neighbor == "ordinary-draft" || neighbor == "untyped" {
				want = "clear"
			}
			if neighbor == "unknown" || neighbor == "count" {
				want = "unknown"
			}
			requireNotice(t, f.binding().OwnNotice, want)
			before := f.wait(func(s orphanScreen) bool { return true })
			f.step()
			f.control(mode)
			after := f.wait(func(s orphanScreen) bool { return true })
			if before.Text != after.Text || after.Erased != 0 || len(after.Submitted) != 0 || len(f.binding().Reservations) != 1 || current != "" && f.e.received("alice", current) {
				t.Fatal("detection changed draft, mail or rate")
			}
			if want == "suspected" {
				since := f.binding().OwnNotice.Since
				clone := *f.a
				f.a = &clone
				var err error
				f.s, err = f.a.session()
				if err != nil {
					t.Fatal(err)
				}
				f.control(map[string]any{"unknown": true})
				f.step()
				requireNotice(t, f.binding().OwnNotice, "unknown")
				if f.binding().OwnNotice.Since != since || f.binding().OwnNotice.Reason != orphanReason {
					t.Fatal("unknown lost last suspicion")
				}
				raw := f.e.as("alice", "bind", "status", "--json").ok(t).stdout
				if !strings.Contains(raw, `"own_notice_possibly_orphaned":null`) {
					t.Fatalf("unknown silently false: %s", raw)
				}
				f.control(mode)
				f.step()
				requireNotice(t, f.binding().OwnNotice, "suspected")
				if f.binding().OwnNotice.Since != since {
					t.Fatal("since changed without fresh clear")
				}
				f.control(map[string]any{"text": ""})
				f.dispatch()
				requireNotice(t, f.binding().OwnNotice, "clear")
				if f.binding().OwnNotice.Since != "" || f.binding().Typed || len(f.binding().Reservations) != 1 {
					t.Fatal("empty clear spent first glimpse")
				}
				f.step()
				f.step()
				f.control(map[string]any{})
				s := f.wait(func(s orphanScreen) bool { return true })
				wantWakes := 1
				if current == "" {
					wantWakes = 0
				}
				if s.Erased != 0 || len(s.Submitted) != wantWakes {
					t.Fatalf("empty resume duplicate/lost wake: %+v", s)
				}
			}
		})
	}
}

// Both public command paths read the published own record. Publication does
// not advance heartbeat, state-since, runtime, hooks, detail or product fields.
func TestTerminalNoticePublicStatusAndWho(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.e.as("alice", "status", "init").ok(t)
	f.e.as("alice", statusArgs("working")...).ok(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.e.as("alice", "inbox").ok(t)
	id := f.mail()
	before := *f.e.state("alice").Liveness
	f.control(map[string]any{})
	f.step()
	requireNotice(t, f.binding().OwnNotice, "suspected")
	r := f.e.as("alice", "bind", "status").ok(t)
	if !strings.Contains(r.stdout, "SUSPECTED-DEAF: own notice possibly orphaned") || !strings.Contains(r.stdout, "publication=published") {
		t.Fatalf("bind status missing: %s", r.stdout)
	}
	var facts config.OwnNoticeObservation
	raw := f.e.as("alice", "bind", "status", "--json").ok(t).stdout
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		t.Fatal(err)
	}
	requireNotice(t, facts, "suspected")
	raw = f.e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
	var rows []whoRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Name == "alice" && row.Record != nil {
			found = true
			requireNotice(t, row.Record.OwnNotice, "suspected")
			got := *row.Record
			got.OwnNotice = before.OwnNotice
			if !reflect.DeepEqual(got, before) {
				t.Fatalf("publication changed heartbeat/fields: %+v != %+v", got, before)
			}
		}
	}
	if !found {
		t.Fatal("own liveness not visible")
	}
	text := f.e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
	if !strings.Contains(text, "SUSPECTED-DEAF: own notice possibly orphaned") || strings.Contains(text, tmuxwake.Notice(1)) {
		t.Fatalf("who missing or leaks draft: %s", text)
	}
	if f.e.received("alice", id) {
		t.Fatal("publication receipted mail")
	}
}

// Missing records and actual server rejection do not claim visibility. A
// successful retry publishes the same first-since; forged own-key identity is
// refused before touching a foreign key, and the server independently refuses it.
func TestTerminalNoticePublicationRefusals(t *testing.T) {
	for _, kind := range []string{"missing", "failed", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			if kind != "missing" {
				f.e.as("alice", "status", "init").ok(t)
				f.e.as("alice", statusArgs("working")...).ok(t)
			}
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.e.as("alice", "inbox").ok(t)
			id := f.mail()
			f.control(map[string]any{})
			if kind == "failed" {
				f.e.hs.fail("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}", 503, "M_UNKNOWN", "synthetic publication failure")
			}
			putsBefore := f.e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			oldUID := f.s.creds.UserID
			if kind == "foreign" {
				f.s.creds.UserID = "@bob:test.local"
			}
			f.step()
			requireNotice(t, f.binding().OwnNotice, "suspected")
			since := f.binding().OwnNotice.Since
			if kind == "foreign" && f.e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != putsBefore {
				t.Fatal("foreign publication attempted write")
			}
			if f.binding().NoticePublication == "published" {
				t.Fatal("failed publication claimed visibility")
			}
			if kind == "failed" {
				var remote config.AgentStatus
				room, _, err := f.s.resolveRoom(context.Background(), StatusRoom)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.s.client.GetState(context.Background(), room, StatusEventType, oldUID, &remote); err != nil {
					t.Fatal(err)
				}
				if remote.OwnNotice.PossiblyOrphaned != nil && *remote.OwnNotice.PossiblyOrphaned {
					t.Fatal("failed publication falsely visible")
				}
				f.e.hs.mu.Lock()
				delete(f.e.hs.failures, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
				f.e.hs.mu.Unlock()
			}
			if kind == "missing" {
				f.e.as("alice", "status", "init").ok(t)
				f.e.as("alice", statusArgs("working")...).ok(t)
			}
			f.s.creds.UserID = oldUID
			f.step()
			if f.binding().NoticePublication != "published" || f.binding().OwnNotice.Since != since || f.e.received("alice", id) {
				t.Fatal("publication retry lost evidence/mail")
			}
		})
	}
	TestLivenessPowerZeroAndForeignKey(t)
}

// Gate refusal is checked on genuinely empty, interval-qualified input. Each
// nearby restored-authority control delivers once without losing pending mail.
func TestTerminalFreshAuthorityGates(t *testing.T) {
	for _, kind := range []string{"foreign-lease", "missing-lease", "expired-lease", "binding-owner", "rate"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			saved := *f.binding()
			lease := *f.e.state("alice").Lease
			f.update(func(st *config.State) {
				switch kind {
				case "foreign-lease":
					st.Lease.Holder = "another-session"
				case "missing-lease":
					st.Lease = nil
				case "expired-lease":
					st.Lease.Expires = f.now
				case "binding-owner":
					st.Terminal.OwnerPID++
				case "rate":
					st.Terminal.MaxWakes = 1
					st.Terminal.Reservations = []time.Time{f.now}
				}
			})
			f.dispatch()
			f.step()
			f.control(map[string]any{})
			s := f.wait(func(s orphanScreen) bool { return true })
			if len(s.Submitted) != 0 || s.Text != "" || s.Erased != 0 {
				t.Fatalf("fresh authority gate admitted: %+v", s)
			}
			want := 0
			if kind == "rate" {
				want = 1
			}
			if len(f.binding().Reservations) != want {
				t.Fatal("fresh authority gate spent rate")
			}
			f.update(func(st *config.State) {
				st.Lease = &lease
				st.Terminal.OwnerPID = saved.OwnerPID
				st.Terminal.MaxWakes = 30
			})
			f.step()
			f.step()
			f.control(map[string]any{})
			s = f.wait(func(s orphanScreen) bool { return true })
			if len(s.Submitted) != 1 || f.e.received("alice", id) {
				t.Fatal("fresh authority positive lost/duplicated mail")
			}
		})
	}
}

// Partial receipt cannot reset an uncertain attempt's retry fence or reuse its
// original count. Rendered empty input does not prove whether it was accepted.
func TestTerminalUntypedPartialReceipt(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	old := f.mail()
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.update(func(st *config.State) { st.Terminal.Typed = false; st.Terminal.Retried = true })
	_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		_, e := mb.Receive([]string{old})
		return nil, e
	})
	if err != nil {
		t.Fatal(err)
	}
	f.control(map[string]any{"text": ""})
	f.step()
	f.step()
	f.step()
	b := f.binding()
	screen := f.wait(func(s orphanScreen) bool { return true })
	if !b.Retried || len(b.AttemptIDs) != 1 || len(b.Reservations) != 1 || len(screen.Submitted) != 0 || screen.Erased != 0 {
		t.Fatal("partial uncertain attempt lost retry fence or reused stale count")
	}
}
