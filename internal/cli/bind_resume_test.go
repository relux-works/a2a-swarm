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
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Real Send leaves a typed draft, then later dispatch submits only that recorded
// count once, without typing/erasing, including journal reload and daemon restart.
// Adjacent refusals retain the attempt and mailbox; no provider pane is used.
func TestTerminalOwnedNoticeRecovery(t *testing.T) {
	for _, runtime := range []string{"codex", "claude", "claude-named"} {
		for _, mode := range []string{"valid", "restart", "uncertain", "more-mail", "at-budget", "foreign", "prefix", "suffix", "count", "dialog", "busy", "registry-busy", "version", "target", "generation", "session", "width", "paused", "manual", "stopped", "unrecorded", "not-typed", "retry-cap", "retry-delay", "cooldown", "budget", "transcript", "prompt-foreign", "prompt-empty", "second-prompt", "origin-row", "origin-column"} {
			if runtime == "codex" && (mode == "registry-busy" || mode == "version") {
				continue
			}
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				f := newOrphanDispatchFixture(t)
				var path string
				var registry map[string]any
				layout := func() map[string]any {
					return map[string]any{"claude": runtime != "codex", "named": runtime == "claude-named"}
				}
				if runtime != "codex" {
					path, registry = dispatchRegistry(t, f, "idle")
				}
				id := f.mail()
				initial := layout()
				initial["defer"] = true
				f.control(initial)
				if mode == "uncertain" {
					loseTerminalTypeACK(t, f)
				}
				f.dispatch()
				f.step()
				b := f.binding()
				if (mode != "uncertain" && !b.Typed) || (mode == "uncertain" && (b.Typed || b.Outcome != string(tmuxwake.SendTypeUncertain))) || !b.TypingStarted || b.NoticeOrigin == nil || b.Notice != tmuxwake.Notice(1) {
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
				case "prefix":
					render["text"] = "KEEP" + b.Notice
				case "suffix":
					render["text"] = b.Notice + "KEEP"
				case "count":
					render["text"] = tmuxwake.Notice(2)
				case "dialog":
					render["diagnostic-row"] = 23
					render["diagnostic-text"] = "Permission required"
				case "busy":
					render["busy"] = true
				case "registry-busy":
					registry["status"] = "busy"
					writeDispatchRegistry(t, path, registry)
				case "version":
					registry["version"] = "2.1.319"
					writeDispatchRegistry(t, path, registry)
				case "target":
					f.update(func(st *config.State) { st.Terminal.Target.SocketID = "changed" })
				case "generation":
					f.update(func(st *config.State) { st.Terminal.Generation = "changed" })
				case "session":
					f.update(func(st *config.State) { st.Terminal.SessionID = "changed"; st.Lease.Holder = "changed" })
				case "width":
					if out, err := exec.Command("tmux", "-S", b.Target.Socket, "resize-window", "-t", b.Target.Pane, "-x", "121", "-y", "24").CombinedOutput(); err != nil {
						t.Fatalf("resize fixture: %v %s", err, out)
					}
				case "paused":
					f.update(func(st *config.State) { st.HooksPaused = true })
				case "manual":
					f.e.as("alice", "status", "init").ok(t)
					f.e.as("alice", statusArgs("waiting-human")...).ok(t)
				case "stopped":
					f.update(func(st *config.State) { st.Terminal.Stopped = true })
				case "unrecorded":
					f.update(func(st *config.State) { st.Terminal.NoticeOrigin = nil })
				case "not-typed":
					f.update(func(st *config.State) { st.Terminal.TypingStarted = false; st.Terminal.Typed = false })
				case "retry-cap":
					f.update(func(st *config.State) { st.Terminal.Retried = true })
				case "retry-delay":
					f.update(func(st *config.State) { st.Terminal.RetryMax = time.Minute })
				case "cooldown":
					f.now = b.AttemptAt.Add(3 * time.Second)
				case "budget":
					f.update(func(st *config.State) {
						st.Terminal.MaxWakes = 1
						st.Terminal.Reservations = append(st.Terminal.Reservations, f.now)
					})
				case "origin-row":
					f.update(func(st *config.State) { st.Terminal.NoticeOrigin.ComposerRow-- })
				case "origin-column":
					f.update(func(st *config.State) { st.Terminal.NoticeOrigin.ComposerColumn++ })
				case "prompt-foreign", "prompt-empty", "second-prompt":
					glyph := "› "
					if runtime != "codex" {
						glyph = "❯ "
					}
					render["diagnostic-row"] = 12
					render["diagnostic-text"] = glyph + b.Notice
					if mode == "prompt-foreign" {
						render["text"] = "HUMAN_DRAFT"
						render["indent"] = 3
					}
					if mode == "prompt-empty" {
						render["text"] = ""
						render["cursor"] = 0
					}
					if mode == "second-prompt" {
						render["diagnostic-row"] = 23
						render["diagnostic-text"] = glyph + "HUMAN_DRAFT"
					}
				case "transcript":
					render["text"] = ""
					render["cursor"] = 0
					render["diagnostic-row"] = 9
					render["diagnostic-text"] = b.Notice
				}
				f.control(render)
				initialState := f.e.state("alice")
				initialBinding := *initialState.Terminal
				err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
				if mode == "generation" || mode == "stopped" {
					if err == nil {
						t.Fatal("retired binding accepted")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				positive := slices.Contains([]string{"valid", "restart", "uncertain", "more-mail", "at-budget"}, mode)
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
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os,sys,subprocess\nif %q in sys.argv:\n subprocess.run([%q]+sys.argv[1:],check=True)\n sys.exit(1)\nos.execv(%q,[%q]+sys.argv[1:])\n", key, real, real, real)
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
