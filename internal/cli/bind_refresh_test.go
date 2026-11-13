//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

func refreshExecutable(t *testing.T) (string, []byte) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a2a")
	if err := os.WriteFile(path, raw, 0700); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func refreshApp(t *testing.T) (*App, *session, *env) {
	t.Helper()
	e := newEnv(t)
	e.agent("alice")
	e.extra[EnvSessionID] = "refresh-session"
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: time.Now, Stderr: io.Discard, Stdout: io.Discard, HTTP: e.hs.srv.Client()}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "refresh-gen", SessionID: "refresh-session", Host: machineLabel(), DaemonPID: os.Getpid(), DaemonBirth: birth, Interval: time.Second}
		st.Lease = &config.Lease{Holder: "refresh-session", Host: machineLabel(), PID: os.Getpid(), Expires: time.Now().Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, s, e
}

func matureRefresh(t *testing.T, r *terminalRefresh) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(r.path)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := observeTerminalExecutable(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if r.seen != stamp {
		r.stable = false
		r.nextSafety = time.Time{}
	}
	r.seen, r.since = stamp, time.Now().Add(-2*time.Second)
}

func refreshReservation(path string) config.Notification {
	now := time.Now().UTC()
	return config.Notification{Version: 1, NotBefore: now.Add(config.NotificationEvery),
		Reservation: &config.NotificationReservation{Token: strings.Repeat("a", 32), Path: path,
			Session: "refresh-session", Generation: "refresh-gen", At: now, Previous: now}}
}

// The production gate refuses every live/uncertain journal, reservation and
// lost-authority variant; historical completed/refused-send fields are safe.
// The exec spy also proves both lock scopes and preservation of all state
// except the new refresh diagnostic; mailbox bytes remain unchanged.
func TestTerminalRefreshSafePoint(t *testing.T) {
	isolateBindTest(t)
	cases := []struct {
		name   string
		change func(*config.State)
		safe   bool
	}{
		{"settled", func(st *config.State) {}, true},
		{"historical", func(st *config.State) {
			b := st.Terminal
			b.NoticeCount = 1
			b.AttemptAt = time.Now()
			b.Retried = true
			b.NotificationToken = "settled-token"
			b.Reservations = []time.Time{time.Now()}
		}, true},
		{"attempt", func(st *config.State) { st.Terminal.AttemptIDs = []string{"pending"} }, false},
		{"typed", func(st *config.State) { st.Terminal.Typed = true }, false},
		{"notice", func(st *config.State) { st.Terminal.Notice = tmuxwake.Notice(1) }, false},
		{"reservation", func(st *config.State) {
			st.Notification = refreshReservation("terminal")
		}, false},
		{"hook-reservation", func(st *config.State) {
			st.Notification = refreshReservation("hook")
		}, false},
		{"stopped", func(st *config.State) { st.Terminal.Stopped = true }, false},
		{"generation", func(st *config.State) { st.Terminal.Generation = "next" }, false},
		{"birth", func(st *config.State) { st.Terminal.DaemonBirth = "other" }, false},
		{"pid", func(st *config.State) { st.Terminal.DaemonPID++ }, false},
		{"host", func(st *config.State) { st.Terminal.Host = "other" }, false},
		{"lease", func(st *config.State) { st.Lease.Holder = "other" }, false},
		{"expired", func(st *config.State) { st.Lease.Expires = time.Now().Add(-time.Second) }, false},
		{"unknown-ledger", func(st *config.State) { st.Notification.Version = 42 }, false},
		{"malformed-ledger", func(st *config.State) { st.Notification.Version = 1 }, false},
		{"missing-lease", func(st *config.State) { st.Lease = nil }, false},
		{"missing-binding", func(st *config.State) { st.Terminal = nil }, false},
	}
	path, _ := refreshExecutable(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, s, e := refreshApp(t)
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { tc.change(st); return nil })
			if err != nil {
				t.Fatal(err)
			}
			before := e.state("alice")
			if tc.name == "reservation" || tc.name == "hook-reservation" {
				ledger := before.Notification
				if err := ledger.Prepare(time.Now(), time.Time{}); err != nil {
					t.Fatal("setup: reservation control was malformed")
				}
			}
			mail := []byte("synthetic-mailbox-unchanged\n")
			if err := os.WriteFile(s.paths.MailboxFile(s.name), mail, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			r := &terminalRefresh{path: path, exec: func(_ string, args, env []string) error {
				calls++
				if err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now(), func() error { return nil }); err != nil {
					t.Fatal("exec held control lock")
				}
				if err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil }); !errors.Is(err, config.ErrLockTimeout) {
					t.Fatal("exec lost consumer lock")
				}
				if !slices.Contains(args, "--run-generation") || !slices.Contains(args, "refresh-gen") || !slices.Contains(args, "--consumer-lock-fd") {
					t.Fatal("invalid resumed arguments")
				}
				if !slices.Equal(env, detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), "refresh-session")) {
					t.Fatal("re-exec environment widened")
				}
				return syscall.ENOEXEC
			}}
			matureRefresh(t, r)
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error { r.poll(context.Background(), a, s, "refresh-gen", lock); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if calls != map[bool]int{true: 1, false: 0}[tc.safe] {
				t.Fatalf("unsafe re-exec gate: calls=%d safe=%t", calls, tc.safe)
			}
			if !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(e.state("alice"))) || !bytes.Equal(mail, []byte(readFile(t, s.paths.MailboxFile(s.name)))) {
				t.Fatal("re-exec changed binding, lease or mailbox")
			}
		})
	}
}

// Real file observations reject truncated native images, nonexecutables,
// directories, symlinks, FIFOs, foreign ownership and disappearance before exec.
// Each refusal is paired with the same complete native positive image.
func TestTerminalRefreshFiles(t *testing.T) {
	isolateBindTest(t)
	path, raw := refreshExecutable(t)
	positive, err := observeTerminalExecutable(path)
	if err != nil {
		t.Fatalf("complete native control rejected: %v", err)
	}
	for _, name := range []string{"partial", "nonexec", "writable", "wrong-arch", "bad-format", "directory", "symlink", "fifo", "vanished", "foreign"} {
		t.Run(name, func(t *testing.T) {
			a, s, _ := refreshApp(t)
			candidate := filepath.Join(t.TempDir(), "candidate")
			switch name {
			case "partial":
				err = os.WriteFile(candidate, raw[:len(raw)/2], 0700)
			case "nonexec":
				err = os.WriteFile(candidate, raw, 0600)
			case "writable":
				err = os.WriteFile(candidate, raw, 0700)
				if err == nil {
					err = os.Chmod(candidate, 0777)
				}
			case "wrong-arch":
				bad := slices.Clone(raw)
				if runtime.GOOS == "darwin" {
					clear(bad[4:8])
				} else {
					clear(bad[18:20])
				}
				err = os.WriteFile(candidate, bad, 0700)
			case "bad-format":
				bad := slices.Clone(raw)
				copy(bad, "oops")
				err = os.WriteFile(candidate, bad, 0700)
			case "directory":
				err = os.Mkdir(candidate, 0700)
			case "symlink":
				err = os.Symlink(path, candidate)
			case "fifo":
				err = unix.Mkfifo(candidate, 0700)
			case "foreign":
				// System binaries are foreign-owned on ordinary macOS/Linux CI lanes.
				candidate = "/bin/ls"
				info, e := os.Stat(candidate)
				if e != nil {
					t.Fatal(e)
				}
				if info.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid()) {
					t.Skip("CI UID owns the system binary; foreign-owner control requires another UID")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			r := &terminalRefresh{path: candidate, current: positive, exec: func(string, []string, []string) error { calls++; return syscall.ENOEXEC }}
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				for i := 0; i < 3; i++ {
					r.since = time.Now().Add(-time.Hour)
					r.poll(context.Background(), a, s, "refresh-gen", lock)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("unready file reached exec")
			}
			if _, e := observeTerminalExecutable(candidate); !errors.Is(e, errExecutableNotReady) {
				t.Fatalf("file not rejected: %v", e)
			}
		})
	}
}

// An unchanged file and a changing/not-yet-stable observation do not exec.
// Failed exec preserves state except refresh diagnostics and CLOEXEC, logs only a fixed reason, and
// retries once only after a new fingerprint (including same-inode byte change).
func TestTerminalRefreshFailureAndStability(t *testing.T) {
	isolateBindTest(t)
	a, s, e := refreshApp(t)
	path, raw := refreshExecutable(t)
	stamp, err := observeTerminalExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	a.Stderr = &log
	calls := 0
	r := &terminalRefresh{path: path, current: stamp, exec: func(string, []string, []string) error { calls++; return errors.New("SECRET argv body") }}
	before := e.state("alice")
	err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		for i := 0; i < 3; i++ {
			matureRefresh(t, r)
			r.poll(context.Background(), a, s, "refresh-gen", lock)
		}
		if calls != 0 {
			t.Fatal("unchanged binary re-execed")
		}
		next := path + ".next"
		if err := os.WriteFile(next, raw, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		if calls != 0 {
			t.Fatal("unstable candidate re-execed")
		}
		matureRefresh(t, r)
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		for i := 0; i < 8; i++ {
			matureRefresh(t, r)
			r.poll(context.Background(), a, s, "refresh-gen", lock)
		}
		if calls != 1 {
			t.Fatal("failed exec retried unchanged file")
		}
		flags, err := unix.FcntlInt(lock.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("failed exec leaked lock descriptor")
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write([]byte("changed-content"))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		changed, err := observeTerminalExecutable(path)
		if err != nil {
			t.Fatal(err)
		}
		if changed.Image != r.failed.Image || changed.Digest == r.failed.Digest {
			t.Fatal("missing same-inode content change control")
		}
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		if calls != 1 {
			t.Fatal("changed content skipped stability")
		}
		matureRefresh(t, r)
		r.poll(context.Background(), a, s, "refresh-gen", lock)
		if calls != 2 {
			t.Fatal("changed failing file was not retried")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(e.state("alice"))) {
		t.Fatal("failed exec changed binding or lease")
	}
	if log.String() != strings.Repeat("binding re-exec refused: exec failed\n", 2) {
		t.Fatal("exec log was repeated or contained unfixed data")
	}
}

// Construction pins only the launch path; the first poll compares the loaded
// image to it, and subsequent unchanged polls cannot form an exec loop.
func TestTerminalRefreshCurrentImage(t *testing.T) {
	isolateBindTest(t)
	r := newTerminalRefresh(context.Background())
	if r.path == "" || r.current != (terminalExecutable{}) {
		t.Fatal("launch path missing or eager image inspection")
	}
	r.poll(context.Background(), nil, nil, "", nil)
	if r.path == "" {
		t.Fatal("loaded image inspection unavailable")
	}
	stamp, err := observeTerminalExecutable(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if r.current != stamp {
		t.Fatal("running image not initialized as current")
	}
	r.exec = func(string, []string, []string) error { t.Fatal("current image re-exec loop"); return nil }
	for i := 0; i < 3; i++ {
		matureRefresh(t, r)
		r.poll(context.Background(), nil, nil, "", nil)
	}
}

// Unchanged current and failed images skip every full parse/hash; changing
// valid mode metadata triggers full observations and the final pre-exec check.
func TestTerminalRefreshMetadataShortcut(t *testing.T) {
	isolateBindTest(t)
	for _, baseline := range []string{"current", "failed"} {
		t.Run(baseline, func(t *testing.T) {
			a, s, _ := refreshApp(t)
			path, _ := refreshExecutable(t)
			stamp, err := observeTerminalExecutable(path)
			if err != nil {
				t.Fatal(err)
			}
			reads, calls := 0, 0
			r := &terminalRefresh{path: path, current: stamp,
				observe: func(path string) (terminalExecutable, error) {
					reads++
					return observeTerminalExecutable(path)
				},
				exec: func(string, []string, []string) error { calls++; return syscall.ENOEXEC },
			}
			if baseline == "failed" {
				r.current = terminalExecutable{Image: "old-loaded-image"}
				r.failed = stamp
			}
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				for i := 0; i < 5; i++ {
					r.poll(context.Background(), a, s, "refresh-gen", lock)
				}
				if reads != 0 || calls != 0 {
					t.Fatalf("unchanged %s image: full reads=%d execs=%d", baseline, reads, calls)
				}
				if err := os.Chmod(path, 0500); err != nil {
					t.Fatal(err)
				}
				r.poll(context.Background(), a, s, "refresh-gen", lock)
				if reads != 1 || calls != 0 || r.seen.Mode.Perm() != 0500 || r.seen.Digest != stamp.Digest {
					t.Fatal("metadata change did not fully observe unchanged bytes before stability")
				}
				r.since = time.Now().Add(-2 * time.Second)
				r.poll(context.Background(), a, s, "refresh-gen", lock)
				if reads != 3 || calls != 1 {
					t.Fatalf("metadata change skipped full stability/final checks: reads=%d execs=%d", reads, calls)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A launch symlink retargeted between construction (before ready) and the first
// poll still reaches the exec spy (protocol success is stubbed). An unrelated
// argv[0] must not become an upgrade path.
func TestTerminalRefreshEarlyRetarget(t *testing.T) {
	isolateBindTest(t)
	// Timing only: the test binary lives in shared temporary ancestry, which
	// the alias custody policy refuses (TestAdmittedLaunchAlias and the real
	// spawn witness cover that policy). Isolate timing from custody here.
	custody := launchAliasCustodyParents
	launchAliasCustodyParents = func(...string) error { return nil }
	t.Cleanup(func() { launchAliasCustodyParents = custody })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []bool{false, true} {
		name := "launch-alias"
		if foreign {
			name = "unrelated-argv"
		}
		t.Run(name, func(t *testing.T) {
			a, s, _ := refreshApp(t)
			next, _ := refreshExecutable(t)
			launch := filepath.Join(t.TempDir(), "a2a")
			target := self
			if foreign {
				target = next
			}
			if err := os.Symlink(target, launch); err != nil {
				t.Fatal(err)
			}
			args := os.Args
			os.Args = append([]string{launch}, args[1:]...)
			t.Cleanup(func() { os.Args = args })
			r := newTerminalRefresh(context.Background())
			// No poll has run yet. Linux's os.Executable still names the old
			// resolved image, so discovering the alias now would be too late.
			link := launch + ".next"
			if err := os.Symlink(next, link); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(link, launch); err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			r.now = func() time.Time { return now }
			r.probe = func(context.Context, string, []string) error { return nil }
			calls := 0
			r.exec = func(path string, argv, _ []string) error {
				calls++
				if path != resolved || argv[0] != launch {
					t.Fatal("retarget lost resolved image or launch alias")
				}
				return syscall.ENOEXEC
			}
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				r.poll(context.Background(), a, s, "refresh-gen", lock)
				now = now.Add(time.Second)
				r.poll(context.Background(), a, s, "refresh-gen", lock)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if foreign {
				want = 0
			}
			if calls != want {
				t.Fatalf("early retarget execs=%d, want %d", calls, want)
			}
		})
	}
}

// Symlink target replacement and retargeting exec the resolved native image;
// missing, truncated and nonexecutable targets never exec or change state.
func TestTerminalRefreshSymlinkTargets(t *testing.T) {
	isolateBindTest(t)
	// Target resolution only: launch and target live in shared temporary
	// ancestry, which the alias custody policy refuses (covered by the alias
	// tests). The direct-link rule still applies here.
	custody := launchAliasCustodyParents
	launchAliasCustodyParents = func(...string) error { return nil }
	t.Cleanup(func() { launchAliasCustodyParents = custody })
	for _, change := range []string{"replace-target", "retarget", "missing", "truncated", "nonexec"} {
		t.Run(change, func(t *testing.T) {
			a, s, e := refreshApp(t)
			target, raw := refreshExecutable(t)
			stamp, err := observeTerminalExecutable(target)
			if err != nil {
				t.Fatal(err)
			}
			launch := filepath.Join(t.TempDir(), "a2a")
			if err := os.Symlink(target, launch); err != nil {
				t.Fatal(err)
			}
			next := target + ".next"
			if err := os.WriteFile(next, raw, 0700); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "replace-target":
				err = os.Rename(next, target)
			case "retarget":
				link := launch + ".next"
				if err = os.Symlink(next, link); err == nil {
					err = os.Rename(link, launch)
				}
				target = next
			case "missing":
				err = os.Remove(target)
			case "truncated":
				err = os.WriteFile(target, raw[:len(raw)/2], 0700)
			case "nonexec":
				err = os.Chmod(target, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			resolvedTarget, err := filepath.EvalSymlinks(filepath.Dir(target))
			if err != nil {
				t.Fatal(err)
			}
			resolvedTarget = filepath.Join(resolvedTarget, filepath.Base(target))
			before := e.state("alice")
			calls := 0
			r := &terminalRefresh{path: launch, current: stamp, exec: func(path string, args, _ []string) error {
				calls++
				if path != resolvedTarget || args[0] != launch {
					t.Fatal("exec did not resolve target and preserve launch path")
				}
				return syscall.ENOEXEC
			}}
			err = config.WithConsumerLock(s.paths, s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
				for i := 0; i < 3; i++ {
					r.poll(context.Background(), a, s, "refresh-gen", lock)
					r.since = time.Now().Add(-2 * time.Second)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if change == "replace-target" || change == "retarget" {
				want = 1
			}
			if calls != want || !reflect.DeepEqual(withoutRefreshDiagnostic(before), withoutRefreshDiagnostic(e.state("alice"))) {
				t.Fatalf("wrong symlink refresh effects: execs=%d want=%d", calls, want)
			}
		})
	}
}

// Repeated polls and retry cycles of a probe, pinning or candidate failure save
// state once, including readable/incomplete install flaps. A new safety cause
// saves again. Atomic file replacements count real saves; safety/observation
// seams use inert candidates and never start a probe process or exec.
func TestTerminalRefreshFailureSaves(t *testing.T) {
	for _, tc := range []struct{ failure, token string }{
		{"probe", "changed_probe_unavailable"},
		{"pin", "changed_pin_unavailable"},
		{"final-candidate", "changed_candidate_unavailable"},
		{"install-flap", "changed_candidate_unavailable"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			now := time.Now()
			s := &session{paths: config.Paths{Base: t.TempDir()}, name: "fixture"}
			st := refreshStatusFixture(now)
			st.Terminal.Host, st.Terminal.DaemonPID = machineLabel(), os.Getpid()
			if err := config.SaveState(s.paths, s.name, st); err != nil {
				t.Fatal(err)
			}
			a := &App{connectionResolved: true, Stderr: io.Discard}
			if tc.failure == "pin" {
				a.connectionError = errors.New("fixture pin unavailable")
			}
			path := filepath.Join(t.TempDir(), "inert-candidate")
			if err := os.WriteFile(path, []byte("inert"), 0700); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			stamp := executableMetadata(info)
			failure := tc.failure
			safetyChecks, observations := 0, 0
			r := &terminalRefresh{
				path: path, current: terminalExecutable{Image: "loaded-image"},
				seen: stamp, since: now.Add(-2 * time.Second), stable: true,
				now: func() time.Time { return now },
				safety: func(context.Context, *App, *session, string) (string, bool, error) {
					safetyChecks++
					return "session", failure != "safety", nil
				},
				probe: func(context.Context, string, []string) error {
					if failure == "probe" {
						return errConsumerProbeUnavailable
					}
					return nil
				},
				exec: func(string, []string, []string) error {
					t.Fatal("failure reached exec")
					return nil
				},
			}
			if failure == "install-flap" {
				r.seen, r.stable = terminalExecutable{}, false
			}
			r.observe = func(string) (terminalExecutable, error) {
				observations++
				if failure == "final-candidate" && r.stable && r.seen.Image != "" ||
					failure == "install-flap" && observations%2 == 1 {
					return terminalExecutable{}, errExecutableNotReady
				}
				return stamp, nil
			}
			lock, err := os.CreateTemp(t.TempDir(), "consumer-lock")
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			saves := 0
			poll := func() string {
				t.Helper()
				// Hold the old inode open so even immediate inode reuse cannot
				// hide the temp-file-and-rename performed by a state save.
				before, err := os.Open(s.paths.StateFile(s.name))
				if err != nil {
					t.Fatal(err)
				}
				defer before.Close()
				previous, err := before.Stat()
				if err != nil {
					t.Fatal(err)
				}
				r.poll(context.Background(), a, s, "generation", lock)
				after, err := os.Stat(s.paths.StateFile(s.name))
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(previous, after) {
					saves++
				}
				state, err := config.LoadState(s.paths, s.name)
				if err != nil {
					t.Fatal(err)
				}
				return terminalRefreshStatus(state.Terminal)
			}
			for i := 0; i < 120; i++ {
				if got := poll(); got != tc.token || saves != 1 {
					t.Fatalf("poll %d: token=%s saves=%d; want %s and one save", i, got, saves, tc.token)
				}
				now = now.Add(2 * time.Second)
			}
			if tc.failure != "install-flap" && safetyChecks < 3 {
				t.Fatalf("retry cycles not exercised: safety checks=%d", safetyChecks)
			}
			failure, a.connectionError = "safety", nil
			now = now.Add(2 * time.Minute)
			for i := 0; i < 3; i++ {
				poll()
				now = now.Add(2 * time.Second)
			}
			if got := poll(); got != "changed_safety_unavailable" || saves != 2 {
				t.Fatalf("new cause: token=%s saves=%d; want safety unavailable and two saves", got, saves)
			}
		})
	}
}

// Two changed candidate fingerprints each reach stabilization and cooldown
// diagnostics. A settled current-image neighbor clears the changed token.
// Tiny inert files and seams exercise poll without starting a probe or exec.
func TestTerminalRefreshPollDiagnostics(t *testing.T) {
	isolateBindTest(t)
	a, s, _ := refreshApp(t)
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("inert-candidate"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := &terminalRefresh{
		path: path, current: terminalExecutable{Image: "loaded-image"},
		now: func() time.Time { return now },
		observe: func(path string) (terminalExecutable, error) {
			info, err := os.Stat(path)
			if err != nil {
				return terminalExecutable{}, err
			}
			return executableMetadata(info), nil
		},
		safety: func(context.Context, *App, *session, string) (string, bool, error) {
			t.Fatal("cooldown started an authority check")
			return "", false, nil
		},
		probe: func(context.Context, string, []string) error { t.Fatal("diagnostic started a probe"); return nil },
		exec:  func(string, []string, []string) error { t.Fatal("diagnostic executed candidate"); return nil },
	}
	assertStatus := func(want string) {
		t.Helper()
		st, err := config.LoadState(s.paths, s.name)
		if err != nil || terminalRefreshStatus(st.Terminal) != want {
			t.Fatalf("poll diagnostic want=%s state=%+v err=%v", want, st.Terminal, err)
		}
	}
	for _, content := range []string{"first-replacement", "second-longer-replacement"} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
		r.poll(context.Background(), a, s, "refresh-gen", nil)
		assertStatus("changed_stabilizing")
		now = now.Add(2 * time.Second)
		r.nextSafety = now.Add(30 * time.Second)
		r.poll(context.Background(), a, s, "refresh-gen", nil)
		assertStatus("changed_cooldown")
	}
	r.current = r.seen
	r.poll(context.Background(), a, s, "refresh-gen", nil)
	assertStatus("current")
}
