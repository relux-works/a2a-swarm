package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Real Send leaves a typed draft, then later dispatch submits only that recorded
// count once, without typing/erasing, including journal reload and daemon restart.
// Adjacent refusals retain the attempt and mailbox; no provider pane is used.
func TestTerminalOwnedNoticeRecovery(t *testing.T) {
	for _, runtime := range []string{"codex", "claude", "claude-named"} {
		modes := []string{"restart", "foreign", "dialog"}
		if runtime == "codex" {
			modes = append(modes, "valid", "more-mail", "at-budget", "width", "prompt-foreign")
		}
		for _, mode := range modes {
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				f := newOrphanDispatchFixture(t)
				layout := func() map[string]any {
					return map[string]any{"claude": runtime != "codex", "named": runtime == "claude-named"}
				}
				if runtime != "codex" {
					dispatchRegistry(t, f, "idle")
				}
				id := f.mail()
				initial := layout()
				initial["defer"] = true
				f.control(initial)
				f.dispatch()
				f.step()
				b := f.binding()
				if !b.Typed || !b.TypingStarted || b.NoticeOrigin == nil || b.Notice != tmuxwake.Notice(1) {
					t.Fatal("missing typed origin")
				}
				before := f.wait(func(s orphanScreen) bool { return s.Text == tmuxwake.Notice(1) })
				f.now = f.now.Add(config.NotificationEvery)
				render := layout()
				render["cursor"] = -5
				switch mode {
				case "restart":
					clone := *f.a
					clone.notification = nil
					f.a = &clone
					var err error
					f.s, err = f.a.session()
					if err != nil {
						t.Fatal(err)
					}
					f.update(func(st *config.State) { st.Terminal.DaemonBirth = "restarted-same-generation" })
				case "more-mail":
					f.mail() // Saved notice count remains one while pending count becomes two.
				case "at-budget":
					f.update(func(st *config.State) { st.Terminal.MaxWakes = 1 })
				case "foreign":
					render["text"] = "FOREIGN"
					// With no saved position and a footer hint, only the indented
				// foreign guard prevents selecting the transcript as recovery input.
				case "prompt-foreign":
					render["text"], render["indent"], render["hint"] = "HUMAN_DRAFT", 3, true
					render["diagnostic-row"], render["diagnostic-text"] = 12, "› "+b.Notice
					f.update(func(st *config.State) { st.Terminal.NoticePosition = nil })
				case "dialog":
					render["diagnostic-row"] = 23
					render["diagnostic-text"] = "Permission required"
				case "width":
					if out, err := exec.Command("tmux", "-S", b.Target.Socket, "resize-window", "-t", b.Target.Pane, "-x", "121", "-y", "24").CombinedOutput(); err != nil {
						t.Fatalf("resize fixture: %v %s", err, out)
					}
				}
				f.control(render)
				initialState := f.e.state("alice")
				initialBinding := *initialState.Terminal
				err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
				if err != nil {
					t.Fatal(err)
				}
				positive := slices.Contains([]string{"valid", "restart", "more-mail", "at-budget"}, mode)
				if positive {
					f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
				}
				barrier := renderWithoutText(render)
				if positive {
					barrier = layout()
				}
				f.control(barrier) // Barrier; preserve actual editor text.
				after := f.wait(func(s orphanScreen) bool { return true })
				got := f.binding()
				if after.Typed != before.Typed || after.Erased != 0 || f.e.received("alice", id) || !reflect.DeepEqual(got.Reservations, initialBinding.Reservations) {
					t.Fatal("recovery typed, erased, receipted or spent another wake")
				}
				if positive {
					if len(after.Submitted) != 1 || after.Submitted[0] != b.Notice || after.Text != "" || got.Notice != "" || len(got.AttemptIDs) != 0 || !slices.Contains(got.Rung, id) || got.NotificationToken != "" || got.Recovery == nil || got.Recovery.Reason != config.TerminalRecovered || !got.Recovery.At.Equal(f.now) || got.OwnNotice.PossiblyOrphaned == nil || *got.OwnNotice.PossiblyOrphaned || got.OwnNotice.Since != "" {
						t.Fatal("recovery did not settle once")
					}
					// Current pending extras may need a future wake, but this immediate cycle must not replay Enter.
					f.dispatch()
					f.control(barrier)
					if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 || s.Typed != before.Typed {
						t.Fatal("recovery replayed")
					}
				} else {
					want := before.Text
					if text, ok := render["text"].(string); ok {
						want = text
					}
					if len(after.Submitted) != 0 || after.Text != want || got.Recovery != nil || got.Notice != initialBinding.Notice || !slices.Equal(got.AttemptIDs, initialBinding.AttemptIDs) || got.NotificationToken != initialBinding.NotificationToken || got.Retried != initialBinding.Retried {
						t.Fatal("recovery refusal changed input or attempt")
					}
				}
			})
		}
	}
}

func renderWithoutText(render map[string]any) map[string]any {
	next := make(map[string]any, len(render))
	for k, v := range render {
		if k != "text" {
			next[k] = v
		}
	}
	return next
}

// Recovery status is a fixed token/time in both forms; malformed and absent
// diagnostics never project arbitrary screen text or break old journals.
func TestTerminalRecoveryStatus(t *testing.T) {
	isolateBindTest(t)
	for _, raw := range []string{``, `,"recovery":{"reason":"owned_notice_submitted","at":"2026-11-06T20:00:00Z"}`, `,"recovery":{"reason":"PRIVATE_SCREEN","at":"bad"}`, `,"recovery":42`} {
		t.Run(raw, func(t *testing.T) {
			e, _ := world(t)
			a, _ := notificationApp(t, e)
			var b config.TerminalBinding
			if err := json.Unmarshal([]byte(`{"generation":"g","stopped":true`+raw+`}`), &b); err != nil {
				t.Fatal(err)
			}
			if b.Generation != "g" || !b.Stopped {
				t.Fatal("diagnostic changed old binding")
			}
			encoded, err := json.Marshal(b)
			if err != nil || bytes.Contains(encoded, []byte("PRIVATE")) {
				t.Fatal("unsafe recovery state")
			}
			notificationState(t, e, func(st *config.State) { st.Terminal = &b })
			for _, jsonView := range []bool{false, true} {
				var out bytes.Buffer
				a.Stdout = &out
				var args []string
				if jsonView {
					args = []string{"--json"}
				}
				if err := a.terminalStatus(context.Background(), args); err != nil {
					t.Fatal(err)
				}
				valid := raw != "" && b.Recovery != nil && b.Recovery.Reason == config.TerminalRecovered
				if bytes.Contains(out.Bytes(), []byte("PRIVATE")) || bytes.Contains(out.Bytes(), []byte(config.TerminalRecovered)) != valid {
					t.Fatal("unsafe or missing recovery diagnostic")
				}
				if valid && !bytes.Contains(out.Bytes(), []byte("2026-11-06T20:00:00Z")) {
					t.Fatal("missing recovery time")
				}
				if jsonView {
					var view struct {
						Recovery *config.TerminalRecovery `json:"recovery"`
					}
					if json.Unmarshal(out.Bytes(), &view) != nil || (view.Recovery != nil) != valid {
						t.Fatal("wrong recovery JSON schema")
					}
					if valid && (view.Recovery.Reason != config.TerminalRecovered || !view.Recovery.At.Equal(b.Recovery.At)) {
						t.Fatal("wrong recovery JSON values")
					}
				}
			}
		})
	}
}

// Apply the fixture's literal type but lose its ACK. Subsequent capture/Enter
// commands use the same private socket; no planted product journal is needed.
func loseTerminalTypeACK(t *testing.T, f *orphanDispatchFixture) {
	loseTerminalACK(t, f, "-l")
}

func loseTerminalACK(t *testing.T, f *orphanDispatchFixture, key string) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "lost-terminal-ack")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,subprocess,time
if %q in sys.argv:
 subprocess.run([%q]+sys.argv[1:],check=True)
 if '-l' in sys.argv:
  notice=sys.argv[sys.argv.index('-l')+1].encode()
  deadline=time.monotonic()+3
  while time.monotonic()<deadline:
   shown=subprocess.check_output([%q,'-S',%q,'capture-pane','-p','-t','%%0'])
   if notice in shown:break
   time.sleep(0.005)
 sys.exit(1)
os.execv(%q,[%q]+sys.argv[1:])
`, key, real, real, f.binding().Target.Socket, real, real)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A recovery Enter applied without an ACK spends its durable retry once. Empty
// composer observations after reload cannot replay either typing or Enter.
func TestTerminalOwnedRecoveryLostEnterACK(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	id := f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	before := f.wait(func(s orphanScreen) bool { return s.Text == tmuxwake.Notice(1) })
	f.now = f.now.Add(config.NotificationEvery)
	f.control(map[string]any{})
	loseTerminalACK(t, f, "Enter")
	f.dispatch()
	f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	if b := f.binding(); !b.Retried || b.Recovery != nil || b.Outcome != string(tmuxwake.SendEnterUncertain) || len(b.Reservations) != 1 || b.Notice == "" {
		t.Fatal("lost ACK did not retain bounded uncertainty")
	}
	clone := *f.a
	clone.notification = nil
	f.a = &clone
	var err error
	f.s, err = f.a.session()
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(config.NotificationEvery)
	for i := 0; i < 3; i++ {
		f.step()
	}
	f.control(map[string]any{})
	after := f.wait(func(s orphanScreen) bool { return true })
	if after.Text != "" || len(after.Submitted) != 1 || after.Typed != before.Typed || after.Erased != 0 || len(f.binding().Reservations) != 1 || f.e.received("alice", id) {
		t.Fatal("lost recovery ACK replayed effects")
	}
}
