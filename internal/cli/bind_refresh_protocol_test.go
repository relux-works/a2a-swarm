//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
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
		time.Sleep(5 * time.Minute)
	case "slow":
		// The first probe times out; the same executable answers on the next run.
		marker := os.Args[0] + ".attempted"
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				os.Exit(2)
			}
			time.Sleep(5 * time.Minute)
		}
		fmt.Print(consumerProtocol)
	case "nonzero":
		fmt.Print(consumerProtocol)
		os.Exit(1)
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
	isolateBindTest(t)
	var out bytes.Buffer
	a := &App{Stdout: &out, Getenv: func(string) string { t.Fatal("protocol consulted environment/session"); return "" }}
	if code := a.Run(context.Background(), []string{"bind", "--consumer-protocol"}); code != ExitOK || out.String() != consumerProtocol {
		t.Fatalf("protocol response: %q, exit=%d", out.String(), code)
	}
}

// Native old-flag, nonzero, wrong, excess-output and stderr candidates never
// reach exec and stay fenced across minutes; a good token does. Each is probed
// once with the allowlisted environment, leaving state/lease unchanged except
// the refresh diagnostic, whose token identifies protocol refusal or exec failure.
func TestTerminalRefreshProtocolCandidates(t *testing.T) {
	isolateBindTest(t)
	_, raw := refreshExecutable(t)
	for _, kind := range []string{"old", "nonzero", "wrong", "extra", "stderr", "good"} {
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
			now := time.Now()
			r := &terminalRefresh{path: path, current: terminalExecutable{Image: "old-image"}, now: func() time.Time { return now },
				safety: func(context.Context, *App, *session, string) (string, bool, error) {
					return "refresh-session", true, nil
				},
				probe: func(ctx context.Context, path string, env []string) error {
					probes++
					if !slices.Equal(env, detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), "refresh-session")) {
						t.Fatal("probe environment widened")
					}
					// Success has the production 10s budget, with no tighter elapsed assertion.
					return probeTerminalConsumer(ctx, path, env)
				},
				exec: func(string, []string, []string) error { execs++; return syscall.ENOEXEC },
			}
			matureRefresh(t, r)
			r.since = now.Add(-time.Second)
			err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				for i := 0; i < 5; i++ {
					r.poll(context.Background(), a, s, "refresh-gen", lock)
					now = now.Add(time.Minute)
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
			if !reflect.DeepEqual(withoutRefreshDiagnostic(after), withoutRefreshDiagnostic(before)) {
				t.Fatal("probe changed consumer authority")
			}
			wantStatus := "changed_protocol_incompatible"
			if kind == "good" {
				wantStatus = "changed_exec_failed"
			}
			if got := after.Terminal.RefreshStatus; got != wantStatus {
				t.Fatalf("refresh diagnostic=%q want=%q", got, wantStatus)
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
			r.since = now.Add(-time.Second)
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error { r.poll(context.Background(), a, s, "refresh-gen", lock); return nil })
			if err != nil || probes != 2 {
				t.Fatalf("changed fingerprint: probes=%d err=%v", probes, err)
			}
		})
	}
}

// A real slow candidate times out without being fenced, then execs on a later
// poll of the identical fingerprint. A real hanging candidate never execs over
// three attempts. The clock seam proves no retry at any poll before 60s and one
// fixed transient log across retries; a failed Start has the same retry policy.
// Consumer authority is unchanged; only refresh diagnostics may be written.
func TestTerminalRefreshProtocolRetry(t *testing.T) {
	isolateBindTest(t)
	_, raw := refreshExecutable(t)
	for _, kind := range []string{"slow", "hang", "start"} {
		t.Run(kind, func(t *testing.T) {
			a, s, e := refreshApp(t)
			before := e.state("alice")
			path := filepath.Join(t.TempDir(), kind)
			if err := os.WriteFile(path, raw, 0700); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			a.Stderr = &log
			now := time.Now()
			probes, execs := 0, 0
			r := &terminalRefresh{path: path, current: terminalExecutable{Image: "old-image"}, now: func() time.Time { return now },
				safety: func(context.Context, *App, *session, string) (string, bool, error) {
					return "refresh-session", true, nil
				},
				probe: func(ctx context.Context, path string, env []string) error {
					probes++
					if kind == "start" {
						path += ".missing"
					}
					started := time.Now()
					err := probeTerminalConsumer(ctx, path, env)
					if kind != "slow" || probes == 1 {
						if !errors.Is(err, errConsumerProbeUnavailable) {
							t.Fatalf("probe result: %v", err)
						}
						// The child sleeps for five minutes: this loose watchdog catches
						// failure to cancel/drain, allowing 50s of loaded-runner slack.
						if kind != "start" && (time.Since(started) < terminalConsumerProbeTimeout || time.Since(started) > terminalConsumerProbeTimeout+50*time.Second) {
							t.Fatal("probe did not honor timeout/drain watchdog")
						}
					}
					return err
				},
				exec: func(string, []string, []string) error { execs++; return syscall.ENOEXEC },
			}
			matureRefresh(t, r)
			r.since = now.Add(-time.Second)
			err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				r.poll(context.Background(), a, s, "refresh-gen", lock)
				if probes != 1 || execs != 0 || r.failed != (terminalExecutable{}) {
					t.Fatalf("first attempt: probes=%d execs=%d failed=%+v", probes, execs, r.failed)
				}
				for attempt := 2; attempt <= 3; attempt++ {
					for second := 1; second < 60; second++ {
						now = now.Add(time.Second)
						r.poll(context.Background(), a, s, "refresh-gen", lock)
						if probes != attempt-1 || execs != 0 {
							t.Fatalf("early retry at second %d: probes=%d execs=%d", second, probes, execs)
						}
					}
					now = now.Add(time.Second)
					r.poll(context.Background(), a, s, "refresh-gen", lock)
					if probes != attempt {
						t.Fatalf("retry count: %d", probes)
					}
					if kind == "slow" {
						if execs != 1 {
							t.Fatalf("healthy retry did not exec: %d", execs)
						}
						break
					}
					if execs != 0 || r.failed != (terminalExecutable{}) {
						t.Fatal("unanswered probe executed or fenced")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := "binding re-exec deferred: protocol probe unavailable\n"
			if kind == "slow" {
				want += "binding re-exec refused: exec failed\n"
			}
			if log.String() != want {
				t.Fatalf("retry logs: %q", log.String())
			}
			if !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(e.state("alice"))) {
				t.Fatal("probe changed consumer authority")
			}
		})
	}
}

// An unsafe stable candidate costs two initial full observations, then neither
// hashing nor safety checks recur within 30s. A safe state is admitted at 30s.
func TestTerminalRefreshPendingCost(t *testing.T) {
	isolateBindTest(t)
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
// delivery cycle has synced and dispatched; construction only pins the path.
func TestTerminalRefreshLazyObservation(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	f.update(func(st *config.State) { st.HooksPaused = true })
	r := newTerminalRefresh(context.Background())
	if r.path == "" || r.current != (terminalExecutable{}) {
		t.Fatal("launch path missing or eager image inspection")
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
