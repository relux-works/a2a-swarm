package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Existing typed-payload regression checks normalize only their allowed
// transition, then exclude the additive writer seal and private disk-input
// snapshot. Opaque round-trip/seal coverage has separate config tests. Keep
// byte-identical refusal checks separate; this helper is for successful writes.
func typedStateWithoutWriter(st config.State) config.State {
	st.Writer = nil
	raw, err := json.Marshal(st)
	if err != nil {
		panic(err)
	}
	var out config.State
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func cliMonitorRecord() config.Monitor {
	return config.Monitor{Version: 1, Kind: "monitor", Runtime: "muse", Generation: strings.Repeat("a", 32), SessionID: "monitor-session", Host: machineLabel(), OwnerPID: 123, OwnerBirth: "fixture-owner", ConsumerPID: os.Getpid(), ConsumerBirth: "fixture-consumer", Transport: "stdout", Phase: "streaming", HeartbeatEvery: time.Second, HeartbeatAt: time.Now().UTC()}
}
func cliMonitorFixture(t *testing.T) (*env, *App, *session) {
	t.Helper()
	e := newEnv(t)
	var err error
	e.dir, err = filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.agent("alice")
	e.extra[EnvSessionID] = "monitor-session"
	a := &App{as: "alice", Stdout: io.Discard, Stderr: io.Discard, Now: time.Now, HTTP: e.hs.srv.Client(), Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Lease = &config.Lease{Holder: "monitor-session", Host: machineLabel(), PID: 123, Expires: time.Now().Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, a, s
}

// Exercise actual CLI manual, automatic, recovery and detached entrypoints,
// before process/socket probes, consumer launch, lease changes or raw output.
func TestMonitorCLIExclusionBeforeEffects(t *testing.T) {
	for _, phase := range []string{"starting", "streaming", "stopping", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			e, a, s := cliMonitorFixture(t)
			var output bytes.Buffer
			a.Stdout = &output
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			e.extra["TMUX"] = "/nonexistent-monitor-test,0,0"
			e.extra["TMUX_PANE"] = "%1"
			m := cliMonitorRecord()
			m.Phase = phase
			err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, m, func(*config.MonitorGeneration) error {
				before, err := os.ReadFile(s.paths.StateFile(s.name))
				if err != nil {
					t.Fatal(err)
				}
				a.nativeDial = func(context.Context, string) (nativeRuntime, error) {
					t.Fatal("refusal reached native provider")
					return nil, nil
				}
				for name, run := range map[string]func() error{
					"manual": func() error { return a.bindTerminal(context.Background(), nil, bindingManual) },
					"automatic": func() error {
						return a.autoBindHook(context.Background(), "codex", hookInput{SessionID: "monitor-session"}, bindingSessionStart)
					},
					"recovery": func() error {
						return a.healHookBinding(context.Background(), "codex", hookInput{SessionID: "monitor-session"})
					},
					"native-manual": func() error {
						return cmdRuntimeBind(context.Background(), a, []string{"--codex-thread", "monitor-session", "--endpoint", "unix:///nonexistent-monitor-test.sock", "--owner-pid", "123", "--heartbeat", "1s", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none"})
					},
					"native-recovery": func() error { return a.dispatchNative(context.Background(), s, "stale-native") },
					"raw-wait":        func() error { return cmdWait(context.Background(), a, []string{"--timeout", "1ms"}) },
					"raw-watch":       func() error { return cmdWatch(ctx, a, nil) },
					"detached-recovery": func() error {
						return a.withBoundChildStartup(context.Background(), s, "stale-terminal", 0, func(*session, config.TerminalBinding, *os.File) error { t.Fatal("consumer launched"); return nil })
					},
				} {
					t.Run(name, func(t *testing.T) {
						if err := run(); !errors.Is(err, config.ErrMonitorConflict) {
							t.Fatal("consumer did not refuse monitor", err)
						}
						after, _ := os.ReadFile(s.paths.StateFile(s.name))
						if !bytes.Equal(before, after) {
							t.Fatal("refusal mutated state/lease")
						}
						if output.Len() != 0 {
							t.Fatal("foreign consumer reached output")
						}
					})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Proves: real raw, terminal and native routes refuse a live monitor, while only its exact generation emits through the sink.
func TestMonitorOnlySinkAdmission(t *testing.T) {
	_, a, s := cliMonitorFixture(t)
	writes := 0
	var retained *monitorSink
	if err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, cliMonitorRecord(), func(g *config.MonitorGeneration) error {
		sink, err := admitMonitorSink(g, cliMonitorRecord().Generation, func() error { writes++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		retained = sink
		// Each name executes its real consumer route. The second-generation
		// case presents a wrong ID on an actually admitted live handle.
		for _, path := range []string{"raw-watch", "terminal", "native", "second-generation"} {
			t.Run(path, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				var err error
				want := config.ErrMonitorConflict
				switch path {
				case "raw-watch":
					err = cmdWatch(ctx, a, nil)
				case "terminal":
					err = a.bindTerminal(ctx, nil, bindingManual)
				case "native":
					err = a.dispatchNative(ctx, s, "stale-native")
				case "second-generation":
					var foreign *monitorSink
					foreign, err = admitMonitorSink(g, strings.Repeat("b", 32), func() error { writes++; return nil })
					if foreign != nil {
						t.Fatal("wrong generation got sink")
					}
					want = config.ErrMonitorGeneration
				}
				if !errors.Is(err, want) || writes != 0 {
					t.Fatal("foreign route reached sink", err)
				}
			})
		}
		if err := sink.announce(); err != nil || writes != 1 {
			t.Fatal("valid monitor refused", err)
		}
		if _, err := admitMonitorSink(g, strings.Repeat("b", 32), func() error { t.Fatal("second generation emitted"); return nil }); !errors.Is(err, config.ErrMonitorGeneration) {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(s.paths.StateFile(s.name))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			t.Fatal("fixture")
		}
		fields["next_batch"] = json.RawMessage(`"old-writer"`)
		raw, _ = json.Marshal(fields)
		if os.WriteFile(s.paths.StateFile(s.name), raw, 0600) != nil {
			t.Fatal("fixture old write")
		}
		if err := sink.announce(); !errors.Is(err, config.ErrIncompatibleWriter) || writes != 1 {
			t.Fatal("mismatch reached sink", err)
		}
		after, _ := os.ReadFile(s.paths.StateFile(s.name))
		if !bytes.Equal(raw, after) {
			t.Fatal("sink invalidation wrote state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := retained.announce(); !errors.Is(err, config.ErrMonitorGeneration) || writes != 1 {
		t.Fatal("retired sink emitted", err)
	}
}

// lockedDeliver is a production ordinary writer with external/mailbox effects.
// The shared precheck must precede its callback, but durability is only claimed
// after the final successful save. Tests do not turn receipts into monitor work.
func TestMonitorLockedDeliverFenceOrdering(t *testing.T) {
	_, _, s := cliMonitorFixture(t)
	if err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, cliMonitorRecord(), func(*config.MonitorGeneration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	fields["next_batch"] = json.RawMessage(`"old-writer"`)
	raw, _ = json.Marshal(fields)
	if os.WriteFile(s.paths.StateFile(s.name), raw, 0600) != nil {
		t.Fatal("old fixture")
	}
	called := false
	var first time.Time
	_, err = s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(st *config.State, mb *config.Mailbox) ([]string, error) {
		called = true
		if st.Writer == nil || st.Writer.ForeignSeen.IsZero() {
			t.Fatal("lockedDeliver effect preceded input check")
		}
		first = st.Writer.ForeignSeen
		current, _ := os.ReadFile(s.paths.StateFile(s.name))
		if !bytes.Equal(raw, current) {
			t.Fatal("precheck claimed premature persistence")
		}
		if len(mb.Pending()) != 0 {
			t.Fatal("unexpected mailbox fixture")
		}
		return nil, nil
	})
	if err != nil || !called {
		t.Fatal(err)
	}
	saved, err := config.LoadState(s.paths, s.name)
	if err != nil || !saved.Writer.ForeignSeen.Equal(first) {
		t.Fatal("successful write did not persist first fence", err)
	}
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.SetHookBlocks("ordinary-hook", 1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	after, err := config.LoadState(s.paths, s.name)
	if err != nil || !reflect.DeepEqual(saved.Monitor, after.Monitor) || !after.Writer.ForeignSeen.Equal(first) {
		t.Fatal("ordinary hook lost fence/history", err)
	}
}

// Proves: the production lease route borrows without renewal, refuses another
// owner without writes, and records foreign history before its successful save.
// This covers cooperative persistence, not a live external runtime lease.
func TestMonitorProductionLeaseFence(t *testing.T) {
	_, a, s := cliMonitorFixture(t)
	if err := a.acquireLease(s, "monitor-session", time.Now().Add(time.Second)); err != nil {
		t.Fatal("ordinary lease neighbour", err)
	}
	beforeLease, err := config.LoadState(s.paths, s.name)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, cliMonitorRecord(), func(g *config.MonitorGeneration) error {
		before, err := os.ReadFile(s.paths.StateFile(s.name))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.acquireLease(s, "foreign-session", time.Now().Add(time.Second)); !errors.Is(err, config.ErrMonitorConflict) {
			t.Fatal("foreign lease route", err)
		}
		after, err := os.ReadFile(s.paths.StateFile(s.name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("refused lease changed State", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(before, &fields); err != nil {
			t.Fatal(err)
		}
		fields["next_batch"] = json.RawMessage(`"old-lease-route"`)
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.paths.StateFile(s.name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.acquireLease(s, "monitor-session", time.Now().Add(time.Second)); err != nil {
			t.Fatal("same-owner borrow", err)
		}
		saved, err := config.LoadState(s.paths, s.name)
		if err != nil || !reflect.DeepEqual(saved.Lease, beforeLease.Lease) || saved.Writer == nil || saved.Writer.Protocol != 2 || saved.Writer.ForeignSeen.IsZero() {
			t.Fatal("lease route renewed or lost fence", err)
		}
		if err := g.CheckRenewal(cliMonitorRecord().Generation); !errors.Is(err, config.ErrIncompatibleWriter) {
			t.Fatal("production route laundered foreign write", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
