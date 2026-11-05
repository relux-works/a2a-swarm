//go:build darwin || linux

package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

// File identity AND bytes matter: rename installs and in-place writes are both
// observed. Metadata participates in stability and in failed-candidate fencing.
type terminalExecutable struct {
	Image    string
	Size     int64
	Mode     os.FileMode
	UID      uint32
	Modified time.Time
	Changed  int64
	Digest   [sha256.Size]byte
}

func (s terminalExecutable) metadata() terminalExecutable {
	s.Digest = [sha256.Size]byte{}
	return s
}

func executableMetadata(info os.FileInfo) terminalExecutable {
	st := info.Sys().(*syscall.Stat_t)
	return terminalExecutable{Image: executableImage(st), Size: info.Size(), Mode: info.Mode(), UID: st.Uid, Modified: info.ModTime(), Changed: executableChanged(st)}
}

var errExecutableNotReady = errors.New("executable not ready")

func observeTerminalExecutable(path string) (terminalExecutable, error) {
	// Do not follow a newly substituted symlink or block opening a FIFO/device.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return terminalExecutable{}, errExecutableNotReady
	}
	f := os.NewFile(uintptr(fd), "binding-executable")
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return terminalExecutable{}, errExecutableNotReady
	}
	stamp := executableMetadata(before)
	if !before.Mode().IsRegular() || stamp.UID != uint32(os.Getuid()) || before.Mode().Perm()&0100 == 0 || before.Mode().Perm()&0022 != 0 || before.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || before.Size() == 0 {
		return terminalExecutable{}, errExecutableNotReady
	}
	// Parse native executable headers and all declared file ranges. A truncated
	// install must not reach exec even if it happens to pause between observations.
	if !completeTerminalExecutable(f, before.Size()) {
		return terminalExecutable{}, errExecutableNotReady
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil || n != before.Size() {
		return terminalExecutable{}, errExecutableNotReady
	}
	after, err := f.Stat()
	if err != nil || executableMetadata(after) != stamp {
		return terminalExecutable{}, errExecutableNotReady
	}
	atPath, err := os.Lstat(path)
	if err != nil || executableMetadata(atPath) != stamp {
		return terminalExecutable{}, errExecutableNotReady
	}
	copy(stamp.Digest[:], hash.Sum(nil))
	return stamp, nil
}

// No outstanding terminal effect is handed to a possibly changed interpreter.
// NoticeCount/AttemptAt/Retried and hourly reservations are historical after a
// settled send; NotificationToken can also remain after a proven refusal.
func terminalRefreshSafe(st config.State, generation, birth string, now time.Time) bool {
	ledger := st.Notification
	if ledger.Prepare(now, time.Time{}) != nil {
		return false
	}
	b := st.Terminal
	return st.HostEnrollment == nil && (st.Runtime == nil || st.Runtime.Retired) &&
		b != nil && !b.Stopped && b.Generation == generation && b.Host == machineLabel() &&
		b.DaemonPID == os.Getpid() && b.DaemonBirth == birth &&
		len(b.AttemptIDs) == 0 && !b.Typed && b.Notice == "" &&
		st.Notification.Reservation == nil &&
		st.Lease != nil && st.Lease.Holder == b.SessionID && st.Lease.Host == b.Host && st.Lease.Expires.After(now)
}

type terminalRefresh struct {
	path       string
	current    terminalExecutable
	seen       terminalExecutable
	since      time.Time
	failed     terminalExecutable
	stable     bool
	nextSafety time.Time
	probed     terminalExecutable
	// Optional seams count expensive work and advance monotonic time in tests.
	now    func() time.Time
	safety func(context.Context, *App, *session, string) (string, bool, error)
	probe  func(context.Context, string, []string) error
	// Tests may count full observations while retaining the real filesystem checks.
	observe func(string) (terminalExecutable, error)
	exec    func(string, []string, []string) error
}

func (r *terminalRefresh) observeExecutable(path string) (terminalExecutable, error) {
	if r.observe != nil {
		return r.observe(path)
	}
	return observeTerminalExecutable(path)
}

func newTerminalRefresh(context.Context) *terminalRefresh {
	// No filesystem or process observation until poll, after ready and a cycle.
	return &terminalRefresh{exec: syscall.Exec}
}

func (r *terminalRefresh) initializePath() {
	path, err := os.Executable()
	if err != nil {
		return
	}
	r.path, err = filepath.Abs(path)
	if err != nil {
		r.path = ""
		return
	}
	// Executable may already have resolved the launch symlink (notably on
	// Linux). Retain an absolute argv[0] alias of that same executable, including
	// across our own exec, so subsequent symlink retargets remain observable.
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) {
		launch, launchErr := filepath.EvalSymlinks(os.Args[0])
		actual, actualErr := filepath.EvalSymlinks(r.path)
		if launchErr == nil && actualErr == nil && launch == actual {
			r.path = os.Args[0]
		}
	}
}

func (r *terminalRefresh) poll(ctx context.Context, a *App, s *session, generation string, lock *os.File) {
	if ctx.Err() != nil {
		return
	}
	if r.path == "" {
		r.initializePath()
	}
	if r.path == "" {
		return
	}
	// Keep the launch path, but resolve it afresh so symlink retargets are upgrades.
	resolved, err := filepath.EvalSymlinks(r.path)
	if err != nil {
		r.seen = terminalExecutable{}
		return
	}
	if r.current.Image == "" {
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		image, err := bindingImage(probe, os.Getpid())
		cancel()
		if err != nil {
			return
		}
		r.current.Image = image
		if stamp, err := r.observeExecutable(resolved); err == nil && stamp.Image == image {
			r.current = stamp
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Unchanged current/failed images need no file reads, parsing or hashing.
	info, err := os.Lstat(resolved)
	if err != nil {
		r.seen = terminalExecutable{}
		return
	}
	metadata := executableMetadata(info)
	if metadata == r.current.metadata() || metadata == r.failed.metadata() {
		r.seen = terminalExecutable{}
		return
	}
	// Production throttling uses monotonic time, independent of delivery time.
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	stamp := r.seen
	if metadata != r.seen.metadata() {
		stamp, err = r.observeExecutable(resolved)
		if err != nil {
			r.seen = terminalExecutable{}
			return
		}
		r.seen, r.since = stamp, now
		r.stable, r.nextSafety = false, time.Time{}
		return
	}
	if now.Sub(r.since) < time.Second {
		return
	}
	if !r.stable {
		stamp, err = r.observeExecutable(resolved)
		if err != nil {
			r.seen = terminalExecutable{}
			return
		}
		if stamp != r.seen {
			r.seen, r.since = stamp, now
			return
		}
		r.stable = true
	}
	if now.Before(r.nextSafety) {
		return
	}
	r.nextSafety = now.Add(30 * time.Second)
	if err := s.checkConnection(); err != nil {
		return
	}
	check := r.safety
	if check == nil {
		check = terminalRefreshAuthority
	}
	sessionID, safe, err := check(ctx, a, s, generation)
	if err != nil || !safe || ctx.Err() != nil {
		return
	}
	env := detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), sessionID)
	if r.probed != stamp {
		probe := r.probe
		if probe == nil {
			probe = probeTerminalConsumer
		}
		if err := probe(ctx, resolved, env); err != nil {
			r.failed = stamp
			fmt.Fprintln(a.Stderr, "binding re-exec refused: protocol incompatible")
			return
		}
		r.probed = stamp
	}
	// Control has been released. This is called only by the single consumer
	// between entire cycles, never from Send or a reservation/finish callback.
	pin, err := a.pinnedArgs(s.name)
	if err != nil {
		return
	}
	args := append([]string{r.path}, pin...)
	args = append(args, "--as", s.name, "bind", "--run-generation", generation, "--consumer-lock-fd", strconv.Itoa(int(lock.Fd())))
	finalPath, err := filepath.EvalSymlinks(r.path)
	if err != nil || finalPath != resolved {
		r.seen = terminalExecutable{}
		return
	}
	final, err := r.observeExecutable(resolved)
	if err != nil || final != stamp {
		r.seen = terminalExecutable{}
		return
	}
	if err := s.checkConnection(); err != nil || ctx.Err() != nil {
		return
	}
	// Keep the SAME flock open-file description and log descriptors through
	// exec. Everything else remains CLOEXEC. On failure restore immediately.
	fd := int(lock.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		return
	}
	_ = r.exec(resolved, args, env)
	syscall.CloseOnExec(fd)
	r.failed = stamp
	fmt.Fprintln(a.Stderr, "binding re-exec refused: exec failed")
}

func terminalRefreshAuthority(ctx context.Context, a *App, s *session, generation string) (string, bool, error) {
	safe := false
	var sessionID string
	err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		birth, err := tmuxwake.Birth(probe, os.Getpid())
		cancel()
		if err != nil {
			return err
		}
		safe = terminalRefreshSafe(st, generation, birth, a.Now())
		if safe {
			sessionID = st.Terminal.SessionID
		}
		return nil
	})
	return sessionID, safe, err
}

// Cap memory and reject even a correct token followed by extra output.
type protocolOutput struct {
	data  []byte
	limit int
}

func (b *protocolOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, io.ErrShortBuffer
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func probeTerminalConsumer(ctx context.Context, path string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "bind", "--consumer-protocol")
	cmd.Env = env
	// No inherited lock or readiness descriptors. Kill the private probe group
	// on timeout and after exit, including children holding output pipes open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 100 * time.Millisecond
	out := &protocolOutput{limit: len(consumerProtocol)}
	stderr := &protocolOutput{limit: 0}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil || ctx.Err() != nil || string(out.data) != consumerProtocol {
		return errors.New("consumer protocol refused")
	}
	return nil
}
