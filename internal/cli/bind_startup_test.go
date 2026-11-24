//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Use the real inherited-flock adoption and startup continuation, without exec,
// pane access or the delivery loop. Each continuation stands for one armed run.
func startupWithLock(t *testing.T, a *App, s *session, run func(*session, config.TerminalBinding, *os.File) error) error {
	t.Helper()
	return config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		fd, err := unix.Dup(int(lock.Fd()))
		if err != nil {
			return err
		}
		return a.withBoundChildSession(context.Background(), "refresh-gen", fd, run)
	})
}

func startupFixture(t *testing.T) (*App, *session, *env, string, *time.Time, *keychainStore) {
	t.Helper()
	a, s, e := refreshApp(t)
	ownerBirth, err := bindingProcessBirth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	dir := fakeKeychain(e, map[string]string{s.creds.UserID: e.tokens["alice"]}, "")
	k := isolatedKeychainStore(keychainStore{paths: s.paths, probeCommand: unlockedProbe, command: securityCommand(dir), now: a.Now, cooldown: 11 * time.Second})
	a.keychain = func(config.Paths) *keychainStore { return k }
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal.RetryMax = 4 * time.Second
		st.Terminal.OwnerPID = os.Getpid() // Prevent status from inspecting a target.
		st.Terminal.OwnerBirth = ownerBirth
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, s, e, dir, &now, k
}

func TestBoundStartupOwnerFixture(t *testing.T) {
	if os.Getenv("A2A_TEST_BOUND_STARTUP_OWNER") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("owner fixture input: %v", err)
	}
}

func startBoundStartupOwner(t *testing.T) (int, string, func()) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestBoundStartupOwnerFixture$")
	cmd.Dir = canonicalTempDir(t)
	cmd.Env = append(os.Environ(), "A2A_TEST_BOUND_STARTUP_OWNER=1")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = input.Close()
		}
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Errorf("owner fixture pid %d did not exit", cmd.Process.Pid)
		}
	}
	t.Cleanup(stop)
	birth, err := bindingProcessBirth(context.Background(), cmd.Process.Pid)
	if err != nil {
		stop()
		t.Fatalf("owner fixture birth: %v", err)
	}
	return cmd.Process.Pid, birth, stop
}

// A resumed image keeps the real lock and generation through cooldown, locked
// and unavailable-companion refusals, then arms exactly once with fresh secrets.
// Fake time proves exponential backoff is capped; no wall-clock sleep is used.
func TestBoundReexecCredentialRecovery(t *testing.T) {
	for _, mode := range []string{"ready", "cooldown", "locked", "missing-companion"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, dir, now, k := startupFixture(t)
			want := ""
			switch mode {
			case "cooldown":
				want = "startup_keychain_cooldown"
				if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID, "security_interaction") }); err != nil {
					t.Fatal(err)
				}
			case "locked":
				want = "startup_keychain_locked"
				k.probeCommand = probeFixture(t, "locked", dir)
			case "missing-companion":
				want = "startup_companion_unavailable"
				k.probeCommand = probeFixture(t, "missing", dir)
			}
			before := e.state("alice")
			var delays []time.Duration
			a.bindStartupWait = func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if len(delays) > 4 {
					t.Fatal("credential retry did not recover")
				}
				st := e.state("alice")
				if st.Terminal.Stopped || st.Terminal.Generation != "refresh-gen" || st.Terminal.RefreshStatus != want {
					t.Fatalf("retry lost ownership/reason: %+v", st.Terminal)
				}
				if !reflect.DeepEqual(before.Lease, st.Lease) || len(records(t, dir)) != 0 {
					t.Fatal("refusal renewed lease or accessed security")
				}
				err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { t.Fatal("second consumer entered"); return nil })
				if !errors.Is(err, config.ErrLockTimeout) {
					t.Fatalf("lock not held: %v", err)
				}
				for _, json := range []bool{false, true} {
					var out bytes.Buffer
					a.Stdout = &out
					args := []string{}
					if json {
						args = append(args, "--json")
					}
					if err := a.terminalStatus(ctx, args); err != nil || !strings.Contains(out.String(), want) {
						t.Fatalf("status lost refusal: %q %v", out.String(), err)
					}
				}
				a.Stdout = io.Discard
				*now = now.Add(delay)
				k.probeCommand = unlockedProbe
				// After the first locked refusal, subsequent attempts see its marker.
				if mode == "locked" {
					want = "startup_keychain_cooldown"
				}
				return nil
			}
			armed := 0
			err := startupWithLock(t, a, s, func(fresh *session, initial config.TerminalBinding, lock *os.File) error {
				armed++
				if fresh.creds.AccessToken != e.tokens["alice"] || initial.Generation != "refresh-gen" {
					t.Fatal("recovery did not arm the same binding")
				}
				flags, err := unix.FcntlInt(lock.Fd(), unix.F_GETFD, 0)
				if err != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatal("inspection could inherit consumer lock")
				}
				return nil
			})
			if err != nil || armed != 1 {
				t.Fatalf("startup=%v armed=%d", err, armed)
			}
			var expected []time.Duration
			if mode == "cooldown" || mode == "locked" {
				expected = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
			}
			if mode == "missing-companion" {
				expected = []time.Duration{time.Second}
			}
			if !reflect.DeepEqual(delays, expected) || len(records(t, dir)) != 1 {
				t.Fatalf("delays=%v records=%d", delays, len(records(t, dir)))
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatal("continuation exit retained lock", err)
			}
		})
	}
}

// This regression witness fails on main: a resumed binding stays under the
// same lock/generation through a refusal well past 30 minutes, exposes degraded
// status, and arms exactly once when credentials return. Fake time and the
// retry-count bound prove exponential backoff without wall-clock sleeps.
func TestBoundStartupCredentialRetryDegradedThreshold(t *testing.T) {
	if startupCredentialRetryLimit != 30*time.Minute {
		t.Fatal("degraded status threshold must remain 30 minutes")
	}
	a, s, e, dir, now, k := startupFixture(t)
	k.probeCommand = probeFixture(t, "missing", dir)
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal.RetryMax = 5 * time.Minute
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := e.state("alice")
	firstRefusal := *now
	var log bytes.Buffer
	a.Stderr = &log
	var delays []time.Duration
	waitsPerHour := map[int]int{}
	degradedObserved := false
	a.bindStartupWait = func(_ context.Context, delay time.Duration) error {
		elapsed := now.Sub(firstRefusal)
		if elapsed >= startupCredentialRetryLimit {
			if status := e.state("alice").Terminal.RefreshStatus; status != "startup_credentials_unavailable" {
				t.Fatalf("degraded status not retained after threshold: %s", status)
			}
			if !degradedObserved {
				degradedObserved = true
				for _, json := range []bool{false, true} {
					var out bytes.Buffer
					a.Stdout = &out
					args := []string{}
					if json {
						args = append(args, "--json")
					}
					if err := a.terminalStatus(context.Background(), args); err != nil || !strings.Contains(out.String(), "startup_credentials_unavailable") {
						t.Fatalf("bind status omitted degraded startup: %q %v", out.String(), err)
					}
				}
				a.Stdout = io.Discard
			}
		} else if status := e.state("alice").Terminal.RefreshStatus; status != "startup_companion_unavailable" {
			t.Fatalf("transient refusal status changed before threshold: %s", status)
		}
		if delay <= 0 || delay > 5*time.Minute {
			t.Fatalf("retry delay outside binding policy: %s", delay)
		}
		hour := int(elapsed / time.Hour)
		waitsPerHour[hour]++
		if waitsPerHour[hour] > 24 {
			t.Fatalf("possible busy loop: %d waits in fake hour %d", waitsPerHour[hour], hour)
		}
		delays = append(delays, delay)
		if len(delays) > 24 {
			t.Fatal("retry count exceeded the fake-hour bound")
		}
		if st := e.state("alice").Terminal; st.Stopped || st.Generation != "refresh-gen" {
			t.Fatalf("retry lost binding ownership: %+v", st)
		}
		if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { t.Fatal("second consumer admitted during retry"); return nil }); !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("retry lost consumer lock: %v", err)
		}
		*now = now.Add(delay)
		if degradedObserved && now.Sub(firstRefusal) >= startupCredentialRetryLimit+5*time.Minute {
			k.probeCommand = unlockedProbe
		}
		return nil
	}
	armed := 0
	err = startupWithLock(t, a, s, func(fresh *session, initial config.TerminalBinding, lock *os.File) error {
		armed++
		if fresh.creds.AccessToken != e.tokens["alice"] || initial.Generation != "refresh-gen" || now.Sub(firstRefusal) <= startupCredentialRetryLimit {
			t.Fatal("recovery did not arm the same binding after the degraded threshold")
		}
		if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { t.Fatal("consumer lock was not held while arming"); return nil }); !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("arming lost consumer lock: %v", err)
		}
		return nil
	})
	if err != nil || armed != 1 || !degradedObserved || len(records(t, dir)) != 1 {
		t.Fatalf("recovery: err=%v arms=%d degraded=%v records=%d", err, armed, degradedObserved, len(records(t, dir)))
	}
	if strings.Count(log.String(), "binding consumer unavailable; bounded retry cause=companion_unavailable\n") != len(delays) || strings.Contains(log.String(), "binding exit: startup credentials unavailable\n") {
		t.Fatalf("startup retry diagnostics changed: waits=%d log=%q", len(delays), log.String())
	}
	after := e.state("alice")
	if after.Terminal.RefreshStatus != "startup_credentials_unavailable" {
		t.Fatalf("the fake continuation changed degraded status before the arm fence: %s", after.Terminal.RefreshStatus)
	}
	r := after.Terminal.ConsumerReport
	if r == nil || r.Since != firstRefusal.UTC().Format(time.RFC3339Nano) || r.Outcome != "unavailable: alarm destination missing" || r.Alarm || strings.Count(log.String(), "binding incident report: unavailable: alarm destination missing\n") != 1 || strings.Contains(log.String(), "binding consumer ready\n") {
		t.Fatalf("startup lost its diagnostic episode or claimed full consumer recovery: %+v log=%q", r, log.String())
	}
	after.Terminal.ConsumerReport = nil
	if !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(after)) {
		t.Fatal("startup changed binding, lease or delivery state beyond diagnostics")
	}
	if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
		t.Fatalf("startup retained consumer lock: %v", err)
	}
}

// The ordinary detached flag route preserves the pre-branch credential error
// and empty binding log, without waiting or touching the startup pipe/lock.
func TestBoundStartupOrdinaryCredentialLog(t *testing.T) {
	a, s, e, _, _, k := startupFixture(t)
	if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID) }); err != nil {
		t.Fatal(err)
	}
	_, want := a.session()
	if want == nil {
		t.Fatal("missing credential refusal control")
	}
	before := e.state("alice")
	var log bytes.Buffer
	a.Stderr = &log
	a.bindStartupWait = func(context.Context, time.Duration) error { t.Fatal("ordinary refusal retried"); return nil }
	err := a.bindTerminal(context.Background(), []string{"--run-generation", "refresh-gen"}, bindingManual)
	if err == nil || err.Error() != want.Error() || log.String() != "" || !reflect.DeepEqual(before, e.state("alice")) {
		t.Fatalf("ordinary refusal err=%v log=%q", err, log.String())
	}
}

// Host enrollment preserves the ordinary child's empty log; the same refusal
// still logs an exit on re-exec. Both leave enrolled state unchanged. The Host
// gate precedes descriptor adoption, so the re-exec selector needs no live FD.
func TestBoundStartupHostEnrollmentLog(t *testing.T) {
	for _, mode := range []string{"ordinary", "re-exec"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			before := enrolledHostState(t, e)
			var log bytes.Buffer
			a := &App{as: "alice", Getenv: func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return e.extra[k]
			}, Now: time.Now, Stderr: &log, Stdout: io.Discard, HTTP: e.hs.srv.Client()}
			args := []string{"--run-generation", "refresh-gen"}
			wantLog := ""
			if mode == "re-exec" {
				args = append(args, "--consumer-lock-fd", "3")
				wantLog = "binding exit: startup unavailable\n"
			}
			err := a.bindTerminal(context.Background(), args, bindingManual)
			if !errors.Is(err, config.ErrHostEnrollment) || log.String() != wantLog {
				t.Fatalf("Host refusal err=%v log=%q want=%q", err, log.String(), wantLog)
			}
			if !reflect.DeepEqual(before, e.state("alice")) {
				t.Fatal("Host refusal changed enrolled state")
			}
		})
	}
}

// Both a stale generation and unavailable process birth retain the original
// closed error and exit-log bytes, release the lock, and leave state unchanged.
func TestBoundStartupStaleConsumerLog(t *testing.T) {
	for _, mode := range []string{"stale", "birth-failure"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, _, _, _ := startupFixture(t)
			generation := "stale-generation"
			if mode == "birth-failure" {
				generation = "refresh-gen"
				t.Setenv("PATH", t.TempDir()) // Birth cannot start ps.
			}
			before := e.state("alice")
			var log bytes.Buffer
			a.Stderr = &log
			err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				fd, err := unix.Dup(int(lock.Fd()))
				if err != nil {
					return err
				}
				return runBoundChild(context.Background(), a, generation, fd)
			})
			if !errors.Is(err, errBindingStaleConsumer) || err.Error() != "binding stale consumer" || log.String() != "binding exit: stale consumer\n" || !reflect.DeepEqual(before, e.state("alice")) {
				t.Fatalf("stale refusal err=%v log=%q", err, log.String())
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("stale refusal retained lock: %v", err)
			}
		})
	}
}

// A resumed bind must stop when its owner PID dies or the same PID is reused;
// each refusal leaves the observed binding record untouched and releases lock.
func TestBoundStartupStopsWhenOwnerGoneOrReplaced(t *testing.T) {
	for _, mode := range []string{"dead-pid", "changed-birth"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, dir, _, k := startupFixture(t)
			k.probeCommand = probeFixture(t, "missing", dir)
			var expected config.State
			a.bindStartupWait = func(context.Context, time.Duration) error {
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					if mode == "dead-pid" {
						pid := os.Getpid() + 1_000_000
						for !processGone(pid) {
							pid++
						}
						st.Terminal.OwnerPID = pid
					} else {
						st.Terminal.OwnerBirth += "-replaced"
					}
					return nil
				})
				if err == nil {
					expected = e.state("alice")
				}
				return err
			}
			err := startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error { t.Fatal("ownerless binding armed"); return nil })
			after := e.state("alice")
			if !errors.Is(err, errBindingStaleConsumer) || !reflect.DeepEqual(expected, after) {
				t.Fatalf("owner %s refusal=%v state changed=%v", mode, err, !reflect.DeepEqual(expected, after))
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("owner %s refusal retained lock: %v", mode, err)
			}
		})
	}
}

var errStartupOwnerWaitNotInterrupted = errors.New("startup wait did not observe owner exit")

// This proves that an owner exit interrupts the pending retry wait, preserves
// the binding and lease, refuses arming, and releases the consumer lock.
func TestBoundStartupOwnerExitInterruptsBackoff(t *testing.T) {
	a, s, e, dir, _, k := startupFixture(t)
	k.probeCommand = probeFixture(t, "missing", dir)
	ownerPID, ownerBirth, stopOwner := startBoundStartupOwner(t)
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal.OwnerPID, st.Terminal.OwnerBirth = ownerPID, ownerBirth
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var expected config.State
	waited := false
	a.bindStartupWait = func(waitCtx context.Context, _ time.Duration) error {
		waited = true
		if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("consumer lock was not retained during the wait: %v", err)
		}
		expected = e.state("alice")
		stopOwner()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-timer.C:
			return errStartupOwnerWaitNotInterrupted
		}
	}
	armed := 0
	err = startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error {
		armed++
		return nil
	})
	if !waited || !errors.Is(err, errBindingStaleConsumer) || armed != 0 {
		t.Fatalf("owner exit during wait: waited=%v err=%v arms=%d", waited, err, armed)
	}
	if after := e.state("alice"); !reflect.DeepEqual(expected, after) {
		t.Fatalf("owner exit changed the binding or lease: before=%+v after=%+v", expected.Terminal, after.Terminal)
	}
	if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
		t.Fatalf("owner exit retained consumer lock: %v", err)
	}
}

func runBoundStartupWithLock(t *testing.T, a *App, s *session, ctx context.Context) error {
	t.Helper()
	return config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		fd, err := unix.Dup(int(lock.Fd()))
		if err != nil {
			return err
		}
		return runBoundChild(ctx, a, "refresh-gen", fd)
	})
}

// A resumed binding stays pinned to the owner from its first snapshot through
// credential recovery. A changed live owner cannot arm or mutate the lease;
// an unchanged owner remains a positive recovery control.
func TestBoundStartupPinsFirstOwnerAcrossRetry(t *testing.T) {
	for _, mode := range []string{"unchanged-owner", "swapped-owner"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, dir, _, k := startupFixture(t)
			k.probeCommand = probeFixture(t, "missing", dir)
			before := e.state("alice")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var replacementPID int
			var replacementBirth string
			var stopReplacement func()
			if mode == "swapped-owner" {
				replacementPID, replacementBirth, stopReplacement = startBoundStartupOwner(t)
				defer stopReplacement()
			}
			var expected config.State
			waits := 0
			a.bindStartupWait = func(context.Context, time.Duration) error {
				waits++
				if waits != 1 {
					t.Fatalf("unexpected retry count: %d", waits)
				}
				if mode == "swapped-owner" {
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.Terminal.OwnerPID = replacementPID
						st.Terminal.OwnerBirth = replacementBirth
						st.Terminal.Target.OwnerPID = replacementPID
						st.Terminal.Target.OwnerBirth = replacementBirth
						return nil
					})
					if err != nil {
						return err
					}
					expected = e.state("alice")
				}
				k.probeCommand = unlockedProbe
				return nil
			}
			armCommitted := false
			a.bindStartupArmedHook = func() {
				armCommitted = true
				cancel()
			}

			err := runBoundStartupWithLock(t, a, s, ctx)
			after := e.state("alice")
			if waits != 1 {
				t.Fatalf("waits=%d, want one credential retry", waits)
			}
			if after.Terminal.Generation != before.Terminal.Generation || after.Terminal.SessionID != before.Terminal.SessionID {
				t.Fatalf("test changed binding generation/consumer: before=%+v after=%+v", before.Terminal, after.Terminal)
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("%s retained consumer lock: %v", mode, err)
			}
			switch mode {
			case "unchanged-owner":
				if err != nil && !errors.Is(err, context.Canceled) || !armCommitted {
					t.Fatalf("unchanged owner did not arm: err=%v armed=%v", err, armCommitted)
				}
				if after.Terminal.OwnerPID != before.Terminal.OwnerPID || after.Terminal.OwnerBirth != before.Terminal.OwnerBirth || after.Terminal.RefreshStatus != "startup_ready" || reflect.DeepEqual(before.Lease, after.Lease) {
					t.Fatalf("unchanged owner failed positive recovery control: before=%+v after=%+v", before.Terminal, after.Terminal)
				}
			case "swapped-owner":
				if !errors.Is(err, errBindingStaleConsumer) || armCommitted {
					t.Fatalf("changed live owner was accepted: err=%v armed=%v", err, armCommitted)
				}
				if !reflect.DeepEqual(expected, after) || !reflect.DeepEqual(before.Lease, after.Lease) || after.Terminal.RefreshStatus == "startup_ready" {
					t.Fatalf("owner swap changed state after refusal: beforeSwap=%+v after=%+v", expected.Terminal, after.Terminal)
				}
			}
		})
	}
}

// This proves the final control-lock and lease transaction refuses owner exit,
// PID reuse, or cancellation after the last connection check, without changing
// the lease or publishing startup_ready. The live-owner case also proves that
// startup reaches the first delivery cycle after committing readiness.
func TestBoundStartupFinalArmFence(t *testing.T) {
	for _, mode := range []string{"live-owner", "owner-exit", "owner-replaced", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, _, _, _ := startupFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ownerPID int
			var ownerBirth string
			var stopOwner func()
			if mode == "owner-exit" || mode == "owner-replaced" {
				ownerPID, ownerBirth, stopOwner = startBoundStartupOwner(t)
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					st.Terminal.OwnerPID, st.Terminal.OwnerBirth = ownerPID, ownerBirth
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := e.state("alice")
			replaced := false
			if mode == "owner-replaced" {
				a.bindStartupOwnerBirth = func(probeCtx context.Context, pid int) (string, error) {
					if pid == ownerPID && replaced {
						return ownerBirth + "-reused", nil
					}
					return bindingProcessBirth(probeCtx, pid)
				}
			}
			armHook := false
			armCommitted := false
			cycleReached := false
			a.bindStartupArmHook = func() {
				armHook = true
				switch mode {
				case "owner-exit":
					stopOwner()
				case "owner-replaced":
					replaced = true
				case "canceled":
					cancel()
				}
			}
			a.bindStartupArmedHook = func() {
				armCommitted = true
			}
			a.bindStartupCycleHook = func() {
				cycleReached = true
				if mode == "live-owner" {
					cancel()
				}
			}

			err := runBoundStartupWithLock(t, a, s, ctx)
			after := e.state("alice")
			if !armHook {
				t.Fatal("final arm hook was not reached")
			}
			switch mode {
			case "live-owner":
				if err != nil && !errors.Is(err, context.Canceled) || !armCommitted || !cycleReached {
					t.Fatalf("live owner did not enter first cycle: err=%v armed=%v cycle=%v", err, armCommitted, cycleReached)
				}
				if after.Terminal.RefreshStatus != "startup_ready" || reflect.DeepEqual(before.Lease, after.Lease) {
					t.Fatalf("live owner did not commit readiness with its lease: before=%+v after=%+v", before.Terminal, after.Terminal)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || armCommitted || cycleReached || !reflect.DeepEqual(before, after) {
					t.Fatalf("canceled arm had side effects: err=%v armed=%v cycle=%v status=%s leaseChanged=%v", err, armCommitted, cycleReached, after.Terminal.RefreshStatus, !reflect.DeepEqual(before.Lease, after.Lease))
				}
			case "owner-exit", "owner-replaced":
				if !errors.Is(err, errBindingStaleConsumer) || armCommitted || cycleReached || !reflect.DeepEqual(before.Lease, after.Lease) || after.Terminal.RefreshStatus == "startup_ready" {
					t.Fatalf("owner fence failed: err=%v armed=%v cycle=%v status=%s leaseChanged=%v", err, armCommitted, cycleReached, after.Terminal.RefreshStatus, !reflect.DeepEqual(before.Lease, after.Lease))
				}
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("%s arm retained consumer lock: %v", mode, err)
			}
		})
	}
}

// Missing items/metadata and identity mismatch remain permanent beside a valid
// control. Ordinary starts refuse a transient cooldown without retry or arming.
func TestBoundStartupPermanentAndOrdinaryRefusals(t *testing.T) {
	for _, mode := range []string{"ready", "ordinary-ready", "missing-item", "missing-metadata", "identity-mismatch", "ordinary-cooldown"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, dir, _, k := startupFixture(t)
			fragment := ""
			switch mode {
			case "missing-item":
				fragment = "keychain item missing"
				saveModel(t, dir, securityModel{Items: map[string]string{}})
			case "missing-metadata":
				fragment = "no credentials for agent"
				if err := os.Remove(s.paths.AgentFile(s.name)); err != nil {
					t.Fatal(err)
				}
			case "identity-mismatch":
				fragment = "credential user_id does not match"
				e.writeCred("alice", "@foreign:test.local", "", 0600)
			case "ordinary-cooldown":
				fragment = "cooldown active"
				if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID, "security_interaction") }); err != nil {
					t.Fatal(err)
				}
			}
			before := e.state("alice")
			a.bindStartupWait = func(context.Context, time.Duration) error { t.Fatal("refusal retried"); return nil }
			armed := 0
			run := func(*session, config.TerminalBinding, *os.File) error { armed++; return nil }
			var err error
			if mode == "ordinary-cooldown" || mode == "ordinary-ready" {
				err = a.withBoundChildSession(context.Background(), "refresh-gen", 0, run)
			} else {
				err = startupWithLock(t, a, s, run)
			}
			if mode == "ready" || mode == "ordinary-ready" {
				if err != nil || armed != 1 {
					t.Fatalf("valid control: %v arms=%d", err, armed)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), fragment) || armed != 0 {
					t.Fatalf("refusal=%v arms=%d", err, armed)
				}
				if mode != "ordinary-cooldown" && transientCredentialReason(err) != "" {
					t.Fatal("permanent refusal classified transient")
				}
				if !reflect.DeepEqual(before, e.state("alice")) {
					t.Fatal("failed startup mutated binding or lease")
				}
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("%s startup retained consumer lock: %v", mode, err)
			}
		})
	}
}

// Stop and supersession during backoff cancel arming; the adjacent recovery
// witness proves an unchanged generation can retry through fresh credentials.
func TestBoundStartupRetiredWhileWaiting(t *testing.T) {
	for _, mode := range []string{"stopped", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, _, now, k := startupFixture(t)
			if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID) }); err != nil {
				t.Fatal(err)
			}
			var expected config.State
			a.bindStartupWait = func(context.Context, time.Duration) error {
				*now = now.Add(securityCooldown)
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					if mode == "stopped" {
						st.Terminal.Stopped = true
					} else {
						st.Terminal.Generation = "replacement-generation"
					}
					return nil
				})
				if err == nil {
					expected = e.state("alice")
				}
				return err
			}
			err := startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error { t.Fatal("retired binding armed"); return nil })
			after := e.state("alice")
			b := after.Terminal
			if err == nil || err.Error() != "binding stale consumer" || !reflect.DeepEqual(expected, after) || mode == "stopped" && !b.Stopped || mode == "superseded" && b.Generation != "replacement-generation" {
				t.Fatalf("%s result=%v binding=%+v", mode, err, b)
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("%s binding retained lock: %v", mode, err)
			}
		})
	}
}

// An invalid stored retry policy terminates a transient refusal without waiting,
// arming or changing the binding and lease state.
func TestBoundStartupRejectsInvalidRetryPolicy(t *testing.T) {
	a, s, e, dir, _, k := startupFixture(t)
	k.probeCommand = probeFixture(t, "missing", dir)
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal.Interval = 2 * time.Second
		st.Terminal.RetryMax = time.Second
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := e.state("alice")
	waits, armed := 0, 0
	a.bindStartupWait = func(context.Context, time.Duration) error { waits++; return nil }
	err = startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error { armed++; return nil })
	if err == nil || err.Error() != "binding retry policy invalid" || waits != 0 || armed != 0 || !reflect.DeepEqual(before, e.state("alice")) {
		t.Fatalf("invalid policy refusal=%v waits=%d arms=%d state changed=%v", err, waits, armed, !reflect.DeepEqual(before, e.state("alice")))
	}
	if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
		t.Fatalf("invalid policy refusal retained lock: %v", err)
	}
}

// A retained notice ages during credential backoff and queues the existing
// stranded alarm without a request. Recovery sends that same job once; the
// pre-threshold control queues nothing and no wake or receipt is manufactured.
func TestBoundStartupRetainsStrandedAlarm(t *testing.T) {
	a, s, e, _, now, k := startupFixture(t)
	room := e.ownReportRoom("alice")
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room}
		b := st.Terminal
		b.NoticeCount, b.Notice = 1, tmuxwake.Notice(1)
		b.AttemptAt = now.Add(-strandedNoticeThreshold + 2*time.Second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID) }); err != nil {
		t.Fatal(err)
	}
	waits := 0
	a.bindStartupWait = func(context.Context, time.Duration) error {
		waits++
		st := e.state("alice")
		if waits == 1 {
			if len(st.MailAlarm.Jobs) != 0 || st.Terminal.NoticeReport != nil {
				t.Fatal("alarm fired before threshold")
			}
			*now = now.Add(3 * time.Second)
		} else {
			if len(st.MailAlarm.Jobs) != 1 || st.Terminal.NoticeReport == nil || st.Terminal.NoticeReport.Sent || st.Terminal.OwnNotice.PossiblyOrphaned == nil || !*st.Terminal.OwnNotice.PossiblyOrphaned {
				t.Fatal("credential refusal silenced stranded alarm")
			}
			if len(strandedAlerts(e)) != 0 || len(st.Terminal.Reservations) != 0 {
				t.Fatal("uncredentialed startup sent or spent a wake")
			}
			*now = now.Add(securityCooldown)
		}
		return nil
	}
	err = startupWithLock(t, a, s, func(fresh *session, initial config.TerminalBinding, _ *os.File) error {
		a.sendMailAlarm(context.Background(), fresh, initial.Generation)
		a.sendMailAlarm(context.Background(), fresh, initial.Generation)
		return nil
	})
	st := e.state("alice")
	if err != nil || waits != 2 || len(strandedAlerts(e)) != 1 || !st.Terminal.NoticeReport.Sent || len(st.MailAlarm.Jobs) != 0 || len(st.Terminal.Rung) != 0 {
		t.Fatalf("alarm recovery: waits=%d alerts=%d error=%v", waits, len(strandedAlerts(e)), err)
	}
}

// A selected store's permanent revocation refusal ends resumed startup without
// retry, arming or state mutation. Its valid stored-token control arms once.
func TestBoundStartupCredentialRevocation(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "revoked"}[revoked], func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			ownerBirth, err := bindingProcessBirth(context.Background(), os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "refresh-gen", Host: machineLabel(), OwnerPID: os.Getpid(), OwnerBirth: ownerBirth, DaemonPID: os.Getpid(), DaemonBirth: birth, Interval: time.Second, RetryMax: time.Second}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := config.LoadState(s.paths, s.name)
			if err != nil {
				t.Fatal(err)
			}
			if revoked {
				a.connectionSecretStore = revokedStartupStore{f.store}
			}
			a.bindStartupWait = func(context.Context, time.Duration) error { t.Fatal("permanent revocation retried"); return nil }
			armed := 0
			err = startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error { armed++; return nil })
			if revoked {
				after, readErr := config.LoadState(s.paths, s.name)
				if !errors.Is(err, matrix.ErrAuth) || readErr != nil || armed != 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("revocation refusal=%v arms=%d read=%v", err, armed, readErr)
				}
			} else if err != nil || armed != 1 {
				t.Fatalf("valid control=%v arms=%d", err, armed)
			}
			if f.httpCalls.Load() != 0 {
				t.Fatal("startup made an unexpected authenticated request")
			}
		})
	}
}

type revokedStartupStore struct{ connectionSecretStore }

func (revokedStartupStore) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	return "", matrix.ErrAuth
}

// The real bind flag route reaches backoff with the inherited lock still owned,
// rather than returning the initial cooldown. Cancellation interrupts the wait;
// recovery/ordinary-start controls above cover the complementary outcomes.
func TestBindReexecRoutesBeforeCredentials(t *testing.T) {
	a, s, e, _, _, k := startupFixture(t)
	if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID) }); err != nil {
		t.Fatal(err)
	}
	waits := 0
	a.bindStartupWait = func(context.Context, time.Duration) error {
		waits++
		if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { t.Fatal("second consumer admitted"); return nil }); !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("missing lock: %v", err)
		}
		return context.Canceled
	}
	err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		fd, err := unix.Dup(int(lock.Fd()))
		if err != nil {
			return err
		}
		return a.bindTerminal(context.Background(), []string{"--run-generation", "refresh-gen", "--consumer-lock-fd", fmt.Sprint(fd)}, bindingManual)
	})
	if !errors.Is(err, context.Canceled) || waits != 1 || e.state("alice").Terminal.RefreshStatus != "startup_keychain_cooldown" {
		t.Fatalf("bind route refused instead of retrying: waits=%d err=%v", waits, err)
	}
	if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
		t.Fatalf("canceled startup retained consumer lock: %v", err)
	}
}
