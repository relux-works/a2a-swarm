//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A copied test executable is a native candidate with a bounded, isolated probe
// entry point. No test cases or production consumers run in this subprocess.
func init() {
	if len(os.Args) != 3 || os.Args[1] != "bind" || os.Args[2] != "--consumer-protocol" {
		return
	}
	switch filepath.Base(os.Args[0]) {
	case "old":
		fmt.Fprintln(os.Stderr, "flag provided but not defined: -consumer-protocol")
		os.Exit(2)
	case "hang":
		time.Sleep(time.Minute)
	case "wrong":
		fmt.Print("a2a-bind-consumer/0\n")
	case "extra":
		fmt.Print(consumerProtocol + "extra")
	case "stderr":
		fmt.Print(consumerProtocol)
		fmt.Fprint(os.Stderr, "unexpected")
	default:
		fmt.Print(consumerProtocol)
	}
	os.Exit(0)
}

// The hidden protocol invocation emits only the fixed token without consulting
// session config, state, locks, lease or network (Getenv would fail the test).
func TestTerminalConsumerProtocol(t *testing.T) {
	var out bytes.Buffer
	a := &App{Stdout: &out, Getenv: func(string) string { t.Fatal("protocol consulted environment/session"); return "" }}
	if code := a.Run(context.Background(), []string{"bind", "--consumer-protocol"}); code != ExitOK || out.String() != consumerProtocol {
		t.Fatalf("protocol response: %q, exit=%d", out.String(), code)
	}
}

// Native old-flag, hanging, wrong, excess-output and stderr candidates never
// reach exec; a good token does. Every fingerprint is probed once, with the
// allowlisted environment, while state/lease remain unchanged on refusal.
func TestTerminalRefreshProtocolCandidates(t *testing.T) {
	_, raw := refreshExecutable(t)
	for _, kind := range []string{"old", "hang", "wrong", "extra", "stderr", "good"} {
		t.Run(kind, func(t *testing.T) {
			a, s, e := refreshApp(t)
			path := filepath.Join(t.TempDir(), kind)
			if err := os.WriteFile(path, raw, 0700); err != nil {
				t.Fatal(err)
			}
			before := e.state("alice")
			var log bytes.Buffer
			a.Stderr = &log
			probes, execs := 0, 0
			r := &terminalRefresh{path: path, current: terminalExecutable{Image: "old-image"},
				probe: func(ctx context.Context, path string, env []string) error {
					probes++
					if !slices.Equal(env, detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), "refresh-session")) {
						t.Fatal("probe environment widened")
					}
					start := time.Now()
					err := probeTerminalConsumer(ctx, path, env)
					if time.Since(start) > 4*time.Second {
						t.Fatal("probe exceeded bounded timeout/drain")
					}
					return err
				},
				exec: func(string, []string, []string) error { execs++; return syscall.ENOEXEC },
			}
			matureRefresh(t, r)
			err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				for i := 0; i < 5; i++ {
					r.poll(context.Background(), a, s, "refresh-gen", lock)
				}
				if probes != 1 {
					t.Fatalf("probe count %d", probes)
				}
				want := 0
				reason := "protocol incompatible"
				if kind == "good" {
					want = 1
					reason = "exec failed"
				}
				if execs != want || log.String() != "binding re-exec refused: "+reason+"\n" {
					t.Fatalf("execs=%d log=%q", execs, log.String())
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after := e.state("alice")
			if !reflect.DeepEqual(after, before) {
				t.Fatal("probe changed consumer authority")
			}
			// An atomic replacement, even with identical bytes, is a new fingerprint.
			next := path + ".next"
			if err := os.WriteFile(next, raw, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, path); err != nil {
				t.Fatal(err)
			}
			matureRefresh(t, r)
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error { r.poll(context.Background(), a, s, "refresh-gen", lock); return nil })
			if err != nil || probes != 2 {
				t.Fatalf("changed fingerprint: probes=%d err=%v", probes, err)
			}
		})
	}
}

// An unsafe stable candidate costs two initial full observations, then neither
// hashing nor safety checks recur within 30s. A safe state is admitted at 30s.
func TestTerminalRefreshPendingCost(t *testing.T) {
	a, s, _ := refreshApp(t)
	path, _ := refreshExecutable(t)
	now := time.Now()
	reads, checks, execs := 0, 0, 0
	safe := false
	r := &terminalRefresh{path: path, current: terminalExecutable{Image: "old-image"}, now: func() time.Time { return now },
		observe: func(path string) (terminalExecutable, error) { reads++; return observeTerminalExecutable(path) },
		safety: func(context.Context, *App, *session, string) (string, bool, error) {
			checks++
			return "refresh-session", safe, nil
		},
		probe: func(context.Context, string, []string) error { return nil },
		exec:  func(string, []string, []string) error { execs++; return syscall.ENOEXEC },
	}
	err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		now = now.Add(time.Second)
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		for i := 0; i < 29; i++ {
			now = now.Add(time.Second)
			r.poll(context.Background(), a, s, "refresh-gen", lock)
		}
		if reads != 2 || checks != 1 || execs != 0 {
			t.Fatalf("pending cost: reads=%d checks=%d execs=%d", reads, checks, execs)
		}
		safe = true
		now = now.Add(time.Second)
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		if reads != 3 || checks != 2 || execs != 1 {
			t.Fatalf("safe transition: reads=%d checks=%d execs=%d", reads, checks, execs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Even a deliberately failing observation seam is reached only after the first
// delivery cycle has synced and dispatched; construction itself does no work.
func TestTerminalRefreshLazyObservation(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	f.update(func(st *config.State) { st.HooksPaused = true })
	r := newTerminalRefresh(context.Background())
	if r.path != "" || r.current != (terminalExecutable{}) {
		t.Fatal("refresh initialized before readiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	reads := 0
	r.observe = func(string) (terminalExecutable, error) {
		reads++
		if f.binding().Outcome != "suppressed: manual gate" || f.e.hs.SyncCount("@alice:"+testServer) == 0 {
			t.Error("observation preceded first delivery cycle")
		}
		cancel()
		return terminalExecutable{}, errExecutableNotReady
	}
	_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() { r.poll(ctx, f.a, f.s, "orphan-gen", nil) })
	if err != nil || reads != 1 {
		t.Fatalf("lazy seam reads=%d err=%v", reads, err)
	}
}
