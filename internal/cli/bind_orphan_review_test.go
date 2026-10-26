package cli

import (
	"context"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Retained reviewer regression through production dispatch. Keeper o-JKabzo
// retires the exact-owned positive: both dim and exact-looking drafts refuse.
func TestReviewerLiveJournalDimHint(t *testing.T) {
	for _, dim := range []bool{false, true} {
		name := "valid"
		if dim {
			name = "dim"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			notice := tmuxwake.Notice(1)
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if !f.binding().Typed {
				t.Fatal("real dispatch did not create typed journal")
			}
			f.control(map[string]any{"dim": dim, "hint": true})
			f.dispatch()
			f.control(map[string]any{"hint": true})
			s := f.wait(func(s orphanScreen) bool { return true })
			if dim && (s.Erased != 0 || len(s.Submitted) != 0) {
				t.Fatalf("dim live journal authorized forbidden effect: %+v", s)
			}
			if !dim && (len(s.Submitted) != 0 || s.Erased != 0 || s.Text != notice) {
				t.Fatalf("valid live journal control failed: %+v", s)
			}
		})
	}
}

// A durable attempt can precede its first keystroke (death/failure between the
// reservation and Send). Recovery is a second production path to initial type.
func TestReviewerFreshRecoveryQualification(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	id := f.mail()

	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(f.dir, "type-wrapper")
	if err = os.Mkdir(wrapper, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(wrapper, "fail-once")
	if err = os.WriteFile(marker, []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os,sys\nif '-l' in sys.argv and os.path.exists(%q):\n os.unlink(%q)\n sys.exit(1)\nos.execv(%q,[%q]+sys.argv[1:])\n", marker, marker, real, real)
	if err = os.WriteFile(filepath.Join(wrapper, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.dispatch()
	f.step()
	if b := f.binding(); b.Typed || len(b.AttemptIDs) != 1 || b.AttemptIDs[0] != id || len(b.Reservations) != 1 {
		t.Fatalf("real first-type failure did not retain attempt: %+v", b)
	}
	f.control(map[string]any{"busy": true})
	f.dispatch()
	f.control(map[string]any{})
	f.now = f.now.Add(3 * time.Second)
	f.dispatch()
	f.control(map[string]any{})
	s := f.wait(func(s orphanScreen) bool { return true })
	b := f.binding()
	if s.Text != "" || len(s.Submitted) != 0 || len(b.Reservations) != 1 || b.Retried {
		t.Fatalf("first recovery idle glimpse typed/reserved: screen=%+v binding=%+v", s, b)
	}
	f.step()
	f.control(map[string]any{})
	s = f.wait(func(s orphanScreen) bool { return true })
	if len(s.Submitted) != 1 || s.Submitted[0] != tmuxwake.Notice(1) {
		t.Fatalf("stable recovery control starved: %+v", s)
	}
}

func TestReviewerOrphanGrammar(t *testing.T) {
	notice := tmuxwake.Notice(1)
	cases := map[string]string{"valid": notice, "bom": "\ufeff" + notice, "nbsp": notice + "\u00a0", "unicode": notice + "\u200b", "quote": "\"" + notice + "\"", "control": notice + "\x01", "crlf": notice + "\r\nKEEP", "oversize": notice + string(make([]byte, 1024)), "space-prefix": " " + notice, "space-suffix": notice + " ", "zero": "a2a: 0 new peer events; run a2a inbox", "leading-zero": "a2a: 01 new peer events; run a2a inbox"}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			f.control(map[string]any{"text": text})
			f.update(func(st *config.State) {
				b := st.Terminal
				b.Typed = true
				b.Notice = notice
				b.NoticeCount = 1
				b.AttemptIDs = []string{"receipted"}
			})
			f.dispatch()
			f.control(map[string]any{})
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
				t.Fatalf("invalid grammar touched: %+v", s)
			}
		})
	}
}

// Fixed safe status refusals still reach the dispatch checks: failed state
// authority or cancellation must not erase, submit, or receipt pending mail.
func TestReviewerStateFailureRefusals(t *testing.T) {
	for _, kind := range []string{"malformed", "permission-read", "permission-write", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			f.dispatch()
			path := f.e.paths().StateFile("alice")
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(path)
			defer func() { os.Chmod(dir, 0700); os.Chmod(path, 0600); os.WriteFile(path, saved, 0600) }()
			ctx := context.Background()
			switch kind {
			case "malformed":
				if err = os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
				if got := f.e.as("alice", "bind", "status").failsWith(t, ExitError, "binding state: invalid"); got.stderr != "a2a: binding state: invalid\n" || got.stdout != "" {
					t.Fatalf("unsafe state refusal: %+v", got)
				}
			case "permission-read":
				if err = os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				if got := f.e.as("alice", "bind", "status").failsWith(t, ExitError, "binding state: unavailable"); got.stderr != "a2a: binding state: unavailable\n" || got.stdout != "" {
					t.Fatalf("unsafe state refusal: %+v", got)
				}
			case "permission-write":
				if err = os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			f.now = f.now.Add(time.Second)
			err = f.a.dispatchTerminal(ctx, f.s, "orphan-gen")
			if kind != "cancelled" && err == nil {
				t.Fatal("failed state authority accepted")
			}
			os.Chmod(dir, 0700)
			os.Chmod(path, 0600)
			os.WriteFile(path, saved, 0600)
			f.control(map[string]any{})
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != "" || len(s.Submitted) != 0 || s.Erased != 0 || f.e.received("alice", id) {
				t.Fatalf("failure had forbidden effects: %+v", s)
			}
		})
	}
}

// Fault the actual post-erase journal write. The old completed erase must not
// authorize a later user draft just because it equals the old fixed notice.
// Keeper o-JKabzo retires all erase commands: the old post-erase fault is
// unreachable. Keep the original name and human/empty restart safety purposes.
func TestReviewerCompletedEraseOwnership(t *testing.T) {
	for _, neighbor := range []string{"human", "empty"} {
		t.Run(neighbor, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.e.as("alice", "inbox").ok(t)
			id := f.mail()
			f.control(map[string]any{})
			f.dispatch()
			f.control(map[string]any{})
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Erased != 0 || len(s.Submitted) != 0 {
				t.Fatalf("completed erase reauthorized later human draft: %+v", s)
			}
			clone := *f.a
			f.a = &clone
			var err error
			f.s, err = f.a.session()
			if err != nil {
				t.Fatal(err)
			}
			text := tmuxwake.Notice(1)
			if neighbor == "empty" {
				text = ""
			}
			f.control(map[string]any{"text": text})
			f.dispatch()
			if neighbor == "empty" {
				f.step()
				f.step()
			}
			f.control(map[string]any{})
			s = f.wait(func(s orphanScreen) bool { return true })
			if s.Erased != 0 || f.e.received("alice", id) || neighbor == "human" && len(s.Submitted) != 0 || neighbor == "empty" && len(s.Submitted) != 1 {
				t.Fatalf("completed erase reauthorized later human draft: %+v", s)
			}
		})
	}
}
