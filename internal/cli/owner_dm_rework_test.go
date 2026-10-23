package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Retain R4's named regression: a skipped key must not suppress the next
// pending activation or expiry. Fresh payloads are nearby execution controls.
func TestReviewR4PendingLifecycleAfterSkippedKey(t *testing.T) {
	for _, transition := range []string{"activate", "expire"} {
		t.Run(transition, func(t *testing.T) { ownerPendingLifecycleAfterKey(t, transition, false) })
	}
}

// A refused key has the same lifecycle independence as a skipped key, while
// still suppressing that final's retry; a fresh final is the positive control.
func TestOwnerDMPendingLifecycleAfterRefusedKey(t *testing.T) {
	for _, transition := range []string{"activate", "expire"} {
		t.Run(transition, func(t *testing.T) { ownerPendingLifecycleAfterKey(t, transition, true) })
	}
}

func ownerPendingLifecycleAfterKey(t *testing.T, transition string, refused bool) {
	t.Helper()
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	payload := `{"session_id":"review-r4","prompt_id":"no-payload","hook_event_name":"Stop","stop_hook_active":false}`
	wantState := "skipped:no-payload"
	if refused {
		payload = string(ownerPayload(t, "claude", "refused-key", "never retry"))
		wantState = "refused:activation"
		e.hs.mu.Lock()
		e.hs.rooms[c.Room].members["@carol:"+testServer] = "invite"
		e.hs.mu.Unlock()
	}
	first := e.hookRT("claude", "bob", "stop", payload).ok(t)
	if first.stdout != "" || !ownerLoad(t, f).Pending {
		t.Fatal("initial pending control", first)
	}
	entries, err := f.journal(time.Now())
	if err != nil || len(entries) != 1 || entries[0].State != wantState {
		t.Fatal("missing key witness", entries, err)
	}
	key := entries[0].Key
	if refused {
		e.hs.mu.Lock()
		delete(e.hs.rooms[c.Room].members, "@carol:"+testServer)
		e.hs.mu.Unlock()
	}
	if transition == "activate" {
		ownerJoin(e, c, c.Owner)
	} else {
		old := time.Now().Add(-25 * time.Hour)
		if err := os.Chtimes(f.path("json"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		r := e.hookRT("claude", "bob", "stop", payload).ok(t)
		got := ownerLoad(t, f)
		if r.stdout != "" || got.Pending || got.Active != (transition == "activate") {
			t.Fatalf("prior key suppressed %s: %+v, %+v", transition, r, got)
		}
	}
	entries, err = f.journal(time.Now())
	if err != nil || entries[0].Key != key || entries[0].State != wantState {
		t.Fatal("retained key retried/changed", entries, err)
	}
	bodies := ownerBodies(e, c.Room)
	if transition == "activate" {
		if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "owner-dm active:") || len(entries) != 2 || entries[1].State != "sent:confirm" {
			t.Fatal("confirmation missing/repeated", bodies, entries)
		}
	} else if len(bodies) != 0 || len(entries) != 1 {
		t.Fatal("expired binding sent", bodies, entries)
	}
	control := e.hookRT("claude", "bob", "stop", string(ownerPayload(t, "claude", "fresh-control", "control"))).ok(t)
	got := ownerLoad(t, f)
	if control.stdout != "" || got.Pending || got.Active != (transition == "activate") {
		t.Fatal("fresh lifecycle control", control, got)
	}
	bodies = ownerBodies(e, c.Room)
	if transition == "activate" && (len(bodies) != 2 || bodies[1] != "control") {
		t.Fatal("fresh final control", bodies)
	}
	if transition == "expire" && len(bodies) != 0 {
		t.Fatal("fresh final sent after expiry", bodies)
	}
}

// Keeper Bj7eZRqC: state changes after chunk 1 stop subsequent chunks with
// partial/refused journal state; unchanged state delivers all three chunks.
// These deterministic fake-server schedules exercise the public Stop adapter.
func TestOwnerDMAudienceBeforeEveryChunk(t *testing.T) {
	for _, fault := range []string{"none", "third-invite", "encryption"} {
		t.Run(fault, func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			payload := string(ownerPayload(t, "claude", "between-chunks", strings.Repeat("q", 9000)))
			e.hookRT("claude", "bob", "prompt", payload).ok(t)
			before := len(ownerBodies(e, c.Room))
			base := e.hs.srv.Client().Transport
			var sends, reads atomic.Int32
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						reads.Add(1)
					}
					response, err := base.RoundTrip(r)
					if err == nil && strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") && sends.Add(1) == 1 {
						e.hs.mu.Lock()
						if fault == "third-invite" {
							e.hs.rooms[c.Room].members["@carol:"+testServer] = "invite"
						}
						if fault == "encryption" {
							e.hs.rooms[c.Room].state["m.room.encryption|"] = json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)
						}
						e.hs.mu.Unlock()
					}
					return response, err
				})}
			}
			r := e.hookRT("claude", "bob", "stop", payload).ok(t)
			entries, err := f.journal(time.Now())
			if r.stdout != "" || err != nil || len(entries) != 1 {
				t.Fatal("native output/journal", r, entries, err)
			}
			want := 3
			if fault != "none" {
				want = 1
			}
			if sends.Load() != int32(want) || len(ownerBodies(e, c.Room))-before != want {
				t.Fatal("wrong chunk count", sends.Load(), len(ownerBodies(e, c.Room))-before)
			}
			if fault == "none" {
				if entries[0].State != "sent 3/3" || reads.Load() < 3 {
					t.Fatal("unchanged control", entries, reads.Load())
				}
			} else {
				if !strings.HasPrefix(entries[0].State, "partial 1/3:refused:") || reads.Load() < 2 {
					t.Fatal("missing partial refusal", entries, reads.Load())
				}
				// Repair and replay: a retained partial key still must never resume.
				e.hs.mu.Lock()
				delete(e.hs.rooms[c.Room].members, "@carol:"+testServer)
				delete(e.hs.rooms[c.Room].state, "m.room.encryption|")
				e.hs.mu.Unlock()
				e.hookRT("claude", "bob", "stop", payload).ok(t)
				if sends.Load() != 1 {
					t.Fatal("partial key resumed")
				}
				e.hookRT("claude", "bob", "stop", string(ownerPayload(t, "claude", "fresh-after-repair", "control"))).ok(t)
				bodies := ownerBodies(e, c.Room)
				if len(bodies) != before+2 || bodies[len(bodies)-1] != "control" {
					t.Fatal("fresh repaired-room control", bodies)
				}
			}
		})
	}
}

// A slow first or second audience read consumes the same total mirror budget;
// no later chunk sends. The 250ms journal reserve records the terminal outcome;
// an unwritable reserve leaves durable pending. Fast reads are the control.
func TestOwnerDMChunkAudienceDeadline(t *testing.T) {
	for _, slowAt := range []int32{0, 1, 2, 3} {
		t.Run(map[int32]string{0: "fast-control", 1: "first-read", 2: "second-read", 3: "reserve-write-failure"}[slowAt], func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			payload := string(ownerPayload(t, "claude", "state-deadline", strings.Repeat("q", 9000)))
			e.hookRT("claude", "bob", "prompt", payload).ok(t)
			before := len(ownerBodies(e, c.Room))
			t.Cleanup(func() { _ = os.Chmod(filepath.Dir(f.path("journal")), 0700) })
			base := e.hs.srv.Client().Transport
			var reads, sends atomic.Int32
			e.configure = func(a *App) {
				a.ownerMirrorBudget = 750 * time.Millisecond
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						at := reads.Add(1)
						if at == slowAt || (slowAt == 3 && at == 2) {
							<-r.Context().Done()
							if slowAt == 3 {
								if err := os.Chmod(filepath.Dir(f.path("journal")), 0500); err != nil {
									return nil, err
								}
							}
							return nil, r.Context().Err()
						}
					}
					if strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") {
						sends.Add(1)
					}
					return base.RoundTrip(r)
				})}
			}
			start := time.Now()
			r := e.hookRT("claude", "bob", "stop", payload).ok(t)
			elapsed := time.Since(start)
			entries, err := f.journal(time.Now())
			if r.stdout != "" || elapsed > time.Second || err != nil || len(entries) != 1 {
				t.Fatal("output/budget/journal", r, elapsed, entries, err)
			}
			want := int32(3)
			if slowAt != 0 {
				want = slowAt - 1
			}
			if slowAt == 3 {
				want = 1
			}
			if sends.Load() != want || len(ownerBodies(e, c.Room))-before != int(want) {
				t.Fatal("sent past slow read", sends.Load(), want)
			}
			if slowAt == 0 && entries[0].State != "sent 3/3" {
				t.Fatal("fast control", entries)
			}
			switch slowAt {
			case 1:
				if entries[0].State != "failed:deadline" {
					t.Fatal("missing failed deadline", entries)
				}
			case 2:
				if entries[0].State != "partial 1/3:refused:deadline" {
					t.Fatal("missing partial deadline", entries)
				}
			case 3:
				if err := os.Chmod(filepath.Dir(f.path("journal")), 0700); err != nil {
					t.Fatal(err)
				}
				if entries[0].State != "pending" {
					t.Fatal("claims terminal record despite failed reserve", entries)
				}
			}
			e.configure = nil
			e.hookRT("claude", "bob", "stop", payload).ok(t)
			if len(ownerBodies(e, c.Room))-before != int(want) {
				t.Fatal("timed-out key resumed")
			}
		})
	}
}

// Port the probe's public-adapter controls for both runtimes: blocked Stop
// never mirrors, receipt-unblocked Stop mirrors once, and replay stays suppressed.
// Fake payload adapters do not establish live runtime discovery or policy enforcement.
func TestOwnerDMReworkStopBothRuntimes(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e, c, _ := ownerActiveFixture(t)
			e.sequentialSessions()
			payload := string(ownerPayload(t, runtime, "public-control", "mirror control"))
			e.hookRT(runtime, "bob", "prompt", payload).ok(t)
			e.hs.inject(c.Room, c.Owner, map[string]any{"msgtype": "m.text", "body": "pending mail control"})
			before := len(ownerBodies(e, c.Room))
			blocked := e.hookRT(runtime, "bob", "stop", payload).ok(t)
			if !strings.Contains(blocked.stdout, `"decision":"block"`) || len(ownerBodies(e, c.Room)) != before {
				t.Fatal("blocked Stop mirrored", blocked)
			}
			e.as("bob", "inbox").ok(t)
			allowed := e.hookRT(runtime, "bob", "stop", payload).ok(t)
			if allowed.stdout != "" || len(ownerBodies(e, c.Room)) != before+1 {
				t.Fatal("unblocked Stop control", allowed)
			}
			e.hookRT(runtime, "bob", "stop", payload).ok(t)
			if len(ownerBodies(e, c.Room)) != before+1 {
				t.Fatal("public replay duplicated")
			}
		})
	}
}
