//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

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
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	dir := fakeKeychain(e, map[string]string{s.creds.UserID: e.tokens["alice"]}, "")
	k := isolatedKeychainStore(keychainStore{paths: s.paths, probeCommand: unlockedProbe, command: securityCommand(dir), now: a.Now, cooldown: 11 * time.Second})
	a.keychain = func(config.Paths) *keychainStore { return k }
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal.RetryMax = 4 * time.Second
		st.Terminal.OwnerPID = os.Getpid() // Prevent status from inspecting a target.
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, s, e, dir, &now, k
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
				if fresh.creds.AccessToken != e.tokens["alice"] || initial.Generation != "refresh-gen" || e.state("alice").Terminal.RefreshStatus != "startup_ready" {
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

// Fake time keeps retrying just before the deadline, caps the final sleep, and
// refuses recovery at the deadline. Earlier recovery still arms the same binding.
// Timeout uses the real child route to verify its exit log and flock release.
func TestBoundStartupCredentialRetryLimit(t *testing.T) {
	if startupCredentialRetryLimit != 30*time.Minute {
		t.Fatal("startup retry limit must be 30 minutes")
	}
	for _, mode := range []string{"timeout", "recover-before", "recover-at"} {
		t.Run(mode, func(t *testing.T) {
			a, s, e, dir, now, k := startupFixture(t)
			k.probeCommand = probeFixture(t, "missing", dir)
			before := e.state("alice")
			firstRefusal := *now
			var log bytes.Buffer
			a.Stderr = &log
			var delays []time.Duration
			a.bindStartupWait = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if len(delays) > 3 {
					t.Fatal("startup retried past deadline")
				}
				if !now.Before(firstRefusal.Add(startupCredentialRetryLimit)) || e.state("alice").Terminal.RefreshStatus != "startup_companion_unavailable" {
					t.Fatal("retry did not retain the transient refusal before deadline")
				}
				if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { t.Fatal("second consumer admitted during retry"); return nil }); !errors.Is(err, config.ErrLockTimeout) {
					t.Fatalf("retry lost lock: %v", err)
				}
				switch len(delays) {
				case 1:
					*now = firstRefusal.Add(startupCredentialRetryLimit - 3*time.Second)
				case 2:
					*now = now.Add(delay)
					if mode == "recover-before" {
						k.probeCommand = unlockedProbe
					}
				case 3:
					*now = now.Add(delay)
					if mode == "recover-at" {
						k.probeCommand = unlockedProbe
					}
				}
				return nil
			}
			armed := 0
			err := config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				fd, err := unix.Dup(int(lock.Fd()))
				if err != nil {
					return err
				}
				if mode == "timeout" {
					return runBoundChild(context.Background(), a, "refresh-gen", fd)
				}
				return a.withBoundChildSession(context.Background(), "refresh-gen", fd, func(fresh *session, initial config.TerminalBinding, _ *os.File) error {
					armed++
					if fresh.creds.AccessToken != e.tokens["alice"] || initial.Generation != "refresh-gen" || !now.Before(firstRefusal.Add(startupCredentialRetryLimit)) {
						t.Fatal("recovery armed an invalid or expired startup")
					}
					return nil
				})
			})
			wantDelays := []time.Duration{time.Second, 2 * time.Second, time.Second}
			wantStatus := "startup_credentials_unavailable"
			if mode == "recover-before" {
				wantDelays = wantDelays[:2]
				wantStatus = "startup_ready"
				if err != nil || armed != 1 || len(records(t, dir)) != 1 {
					t.Fatalf("recovery: err=%v arms=%d", err, armed)
				}
			} else if !errors.Is(err, errStartupCredentialsUnavailable) || err.Error() != "startup credentials unavailable" || armed != 0 || len(records(t, dir)) != 0 || !now.Equal(firstRefusal.Add(startupCredentialRetryLimit)) {
				t.Fatalf("deadline: err=%v arms=%d elapsed=%v", err, armed, now.Sub(firstRefusal))
			}
			if !reflect.DeepEqual(delays, wantDelays) || e.state("alice").Terminal.RefreshStatus != wantStatus {
				t.Fatalf("delays=%v status=%s", delays, e.state("alice").Terminal.RefreshStatus)
			}
			if mode == "timeout" && !strings.HasSuffix(log.String(), "binding exit: startup credentials unavailable\n") {
				t.Fatalf("timeout exit log=%q", log.String())
			}
			if !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(e.state("alice"))) {
				t.Fatal("startup changed binding, lease or delivery state beyond diagnostic")
			}
			if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); err != nil {
				t.Fatalf("startup exit retained lock: %v", err)
			}
			for _, json := range []bool{false, true} {
				var out bytes.Buffer
				a.Stdout = &out
				args := []string{}
				if json {
					args = append(args, "--json")
				}
				if err := a.terminalStatus(context.Background(), args); err != nil || !strings.Contains(out.String(), wantStatus) {
					t.Fatalf("status lost startup result: %q %v", out.String(), err)
				}
			}
		})
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
		})
	}
}

// Retirement during backoff cancels arming even if credentials recover next;
// the adjacent recovery test supplies the unchanged-generation positive control.
func TestBoundStartupRetiredWhileWaiting(t *testing.T) {
	a, s, e, _, now, k := startupFixture(t)
	if err := k.locked(s.creds.UserID, func() error { return k.recordCooldown(s.creds.UserID) }); err != nil {
		t.Fatal(err)
	}
	a.bindStartupWait = func(context.Context, time.Duration) error {
		*now = now.Add(securityCooldown)
		_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.Stopped = true; return nil })
		return err
	}
	err := startupWithLock(t, a, s, func(*session, config.TerminalBinding, *os.File) error { t.Fatal("retired binding armed"); return nil })
	if err == nil || err.Error() != "binding stale consumer" || !e.state("alice").Terminal.Stopped {
		t.Fatalf("retired result=%v", err)
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
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "refresh-gen", Host: machineLabel(), DaemonPID: os.Getpid(), DaemonBirth: birth, Interval: time.Second, RetryMax: time.Second}
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
}
