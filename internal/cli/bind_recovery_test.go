package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Journals come from actual dispatch/type/post-type deferral. Under keeper
// o-JKabzo even exact-looking nonempty input refuses all recovery effects.
// Empty placeholders remain a separate fresh-delivery contract.
func TestTerminalLiveRecoveryOwnership(t *testing.T) {
	for _, name := range []string{"valid", "prefix", "suffix", "dim-prefix", "dim-suffix", "dim-nbsp", "dim-nbsp-prefix", "cursor", "unknown", "target", "paused"} {
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if !f.binding().Typed {
				t.Fatal("missing typed journal")
			}
			notice := tmuxwake.Notice(1)
			mode := map[string]any{"hint": true}
			switch name {
			case "prefix":
				mode["text"] = "KEEP" + notice
			case "suffix":
				mode["text"] = notice + "KEEP"
			case "dim-prefix":
				mode["dim-prefix"] = true
			case "dim-suffix":
				mode["dim"] = true
			case "dim-nbsp", "dim-nbsp-prefix":
				mode["dim-text"] = "\u00a0"
				mode["dim"] = name == "dim-nbsp"
				mode["dim-prefix"] = name == "dim-nbsp-prefix"
			case "cursor":
				mode["cursor"] = -1
			case "unknown":
				mode["unknown"] = true
			case "target":
				f.update(func(st *config.State) { st.Terminal.Target.SocketID = "replaced" })
			case "paused":
				f.e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
			}
			f.control(mode)
			before := f.wait(func(s orphanScreen) bool { return true })
			f.dispatch()
			f.control(mode)
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != before.Text || s.Erased != 0 || len(s.Submitted) != 0 || len(f.binding().Reservations) != 1 {
				t.Fatalf("changed live input authorized effects: %+v", s)
			}
			if f.e.received("alice", id) {
				t.Fatal("recovery receipted mail")
			}
		})
	}
}

// Refuse the actual first literal type command before any keystroke, leaving
// the production durable attempt/reservation. This is not a planted journal.
func failFirstTerminalType(t *testing.T, f *orphanDispatchFixture) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "failed-type")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "once")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os,sys\nif '-l' in sys.argv and os.path.exists(%q):\n os.unlink(%q)\n sys.exit(1)\nos.execv(%q,[%q]+sys.argv[1:])\n", marker, marker, real, real)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.dispatch()
	f.step()
	if b := f.binding(); b.Typed || len(b.AttemptIDs) != 1 || len(b.Reservations) != 1 {
		t.Fatalf("first literal type did not fail at the intended point: %+v", b)
	}
}

// The recovery path shares initial admission's identity and interval gate.
// Observe without sleeping/holding a lock between samples. Each invalidation
// requires two NEW samples; same-time samples spend neither retry nor rate.
func TestTerminalRecoveryIdleContinuity(t *testing.T) {
	for _, reset := range []string{"none", "busy", "unknown", "draft", "pause-pulse", "manual-pulse", "daemon", "session", "target", "no-mail"} {
		t.Run(reset, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			if reset == "manual-pulse" {
				f.e.as("alice", "status", "init").ok(t)
			}
			failFirstTerminalType(t, f)
			f.now = f.now.Add(config.NotificationEvery) // retry spacing expired; still require fresh idle samples
			f.dispatch()
			assertNoSpend := func() {
				t.Helper()
				s := f.wait(func(s orphanScreen) bool { return true })
				b := f.binding()
				if s.Text != "" || s.Erased != 0 || len(s.Submitted) != 0 || len(b.Reservations) != 1 || b.Retried {
					t.Fatalf("unqualified recovery spent type/retry/rate: %+v %+v", s, b)
				}
			}
			f.dispatch() // same timestamp is never a second qualified sample
			assertNoSpend()
			if reset != "none" {
				switch reset {
				case "busy", "unknown":
					f.control(map[string]any{reset: true})
					f.step()
					f.control(map[string]any{})
				case "draft":
					f.control(map[string]any{"text": "KEEP"})
					f.step()
					f.control(map[string]any{"text": ""})
				case "pause-pulse":
					f.e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
					f.e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
				case "manual-pulse":
					f.e.as("alice", statusArgs("waiting-human")...).ok(t)
					f.e.as("alice", statusArgs("idle")...).ok(t)
				case "daemon":
					f.update(func(st *config.State) { st.Terminal.DaemonBirth = "replacement" })
				case "session":
					f.update(func(st *config.State) { st.Terminal.SessionID = "next-session"; st.Lease.Holder = "next-session" })
				case "target":
					f.update(func(st *config.State) { st.Terminal.Target.SocketID = "replacement" })
					f.step()
					f.update(func(st *config.State) { st.Terminal.Target.SocketID, _ = tmuxwake.SocketID(st.Terminal.Target.Socket) })
				case "no-mail":
					f.e.as("alice", "inbox").ok(t)
					f.step()
					id = f.mail()
				}
				f.now = f.now.Add(time.Second)
				f.dispatch()
				assertNoSpend()
			}
			f.step() // exactly one interval after the fresh qualified sample
			f.control(map[string]any{})
			s := f.wait(func(s orphanScreen) bool { return true })
			b := f.binding()
			if len(s.Submitted) != 1 || s.Submitted[0] != tmuxwake.Notice(1) || len(b.Reservations) != 2 || f.e.received("alice", id) {
				t.Fatalf("qualified recovery starved/duplicated/lost mail: %+v %+v", s, b)
			}
			for i := 0; i < 3; i++ {
				f.step()
			}
			f.control(map[string]any{})
			if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 {
				t.Fatal("qualified recovery replayed a completed wake")
			}
		})
	}
}

// Losing the erase command acknowledgement is just as uncertain as losing the
// post-effect state write: neither permits a repeated erase after restart. The
// consumed journal does not block an empty composer or lose current mail.
// Retired erase/lost-ack commands cannot execute. Retain restart and failure
// purposes through the no-effect human/empty regression at the production entry.
func TestTerminalOrphanEraseUncertainRestart(t *testing.T) { TestReviewerCompletedEraseOwnership(t) }

// A failed observation/state write authorizes neither erase nor submission.
func TestTerminalOrphanEraseFenceFailure(t *testing.T) { TestReviewerStateFailureRefusals(t) }
