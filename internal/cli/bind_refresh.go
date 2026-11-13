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
	"strings"
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

// Loaded hosts can delay even a healthy candidate's startup by several seconds.
const terminalConsumerProbeTimeout = 10 * time.Second
const terminalConsumerProbeRetry = 60 * time.Second

var errConsumerProbeUnavailable = errors.New("consumer protocol unavailable")

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
	return terminalRefreshBlockReason(st, generation, birth, machineLabel(), os.Getpid(), now) == ""
}

type terminalRefresh struct {
	lastStatus   string
	failedStatus string
	path         string
	current      terminalExecutable
	seen         terminalExecutable
	since        time.Time
	failed       terminalExecutable
	stable       bool
	nextSafety   time.Time
	probed       terminalExecutable
	// Retain cooldowns across path changes and suppress repeated transient logs.
	probeRetries map[terminalExecutable]time.Time
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
	// Pin the launch alias before ready: an installer may retarget it as soon
	// as startup is acknowledged. Loaded-image inspection and hashing still
	// wait until poll, after ready and a complete delivery cycle.
	r := &terminalRefresh{exec: syscall.Exec}
	r.initializePath()
	return r
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
	if len(os.Args) > 0 {
		if alias, ok := admittedLaunchAlias(os.Args[0], r.path); ok {
			r.path = alias
		}
	}
}

// admittedLaunchAlias admits argv0 as the launch alias of executable only when
// it is absolute, currently resolves to the same file, and passes the same
// custody policy as an installed hook command: the alias is owned by this UID
// or root, and no directory above it or above its image is writable by other
// users, foreign-owned or inside a source checkout. Bare or relative names are
// never searched in PATH, and an alias of another image is refused. The
// detached consumer spawn and refresh share this one admission rule; a refused
// alias keeps the direct image.
func admittedLaunchAlias(argv0, executable string) (string, bool) {
	if argv0 == "" || executable == "" || !filepath.IsAbs(argv0) {
		return "", false
	}
	launch, err := filepath.EvalSymlinks(argv0)
	if err != nil {
		return "", false
	}
	actual, err := filepath.EvalSymlinks(executable)
	if err != nil || launch != actual {
		return "", false
	}
	info, err := os.Lstat(argv0)
	if err != nil {
		return "", false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 && st.Uid != uint32(os.Getuid()) {
		return "", false
	}
	if launchAliasCustodyParents(argv0, actual) != nil {
		return "", false
	}
	return argv0, true
}

// launchAliasCustodyParents is the installed-command ancestry policy. It is a
// package-private seam only so timing tests whose test binary lives in shared
// temporary ancestry can isolate refresh timing; production never changes it.
var launchAliasCustodyParents = hookExecutableParents

func (r *terminalRefresh) poll(ctx context.Context, a *App, s *session, generation string, lock *os.File) {
	if ctx.Err() != nil {
		return
	}
	diagnostic := "unavailable"
	defer func() { r.recordStatus(s, generation, diagnostic) }()
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
	if metadata == r.current.metadata() {
		diagnostic = "current"
		r.seen = terminalExecutable{}
		return
	}
	if metadata == r.failed.metadata() {
		diagnostic = r.failedStatus
		r.seen = terminalExecutable{}
		return
	}
	diagnostic = "changed_stabilizing"
	// Retain the last candidate failure through fresh stabilization attempts.
	// An installer can alternate readable and incomplete images on every poll;
	// only a mature check (or the current image) should replace that cause.
	if r.lastStatus == "changed_candidate_unavailable" {
		diagnostic = r.lastStatus
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
			diagnostic = "changed_candidate_unavailable"
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
			diagnostic = "changed_candidate_unavailable"
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
		diagnostic = terminalRefreshCooldownStatus(r.lastStatus)
		return
	}
	if now.Before(r.probeRetries[stamp]) {
		diagnostic = "changed_probe_unavailable"
		return
	}
	r.nextSafety = now.Add(30 * time.Second)
	if err := s.checkConnection(); err != nil {
		diagnostic = "unavailable"
		return
	}
	var sessionID string
	var safe bool
	diagnostic = "changed_safety_unavailable"
	if r.safety == nil {
		sessionID, safe, diagnostic, err = terminalRefreshAuthorityReason(ctx, a, s, generation)
	} else {
		sessionID, safe, err = r.safety(ctx, a, s, generation)
	}
	if err != nil || !safe || ctx.Err() != nil {
		return
	}
	diagnostic = "changed_ready"
	env := detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), sessionID)
	if r.probed != stamp {
		probe := r.probe
		if probe == nil {
			probe = probeTerminalConsumer
		}
		if err := probe(ctx, resolved, env); err != nil {
			if errors.Is(err, errConsumerProbeUnavailable) {
				diagnostic = "changed_probe_unavailable"
				if r.probeRetries == nil {
					r.probeRetries = make(map[terminalExecutable]time.Time)
				}
				if _, logged := r.probeRetries[stamp]; !logged {
					fmt.Fprintln(a.Stderr, "binding re-exec deferred: protocol probe unavailable")
				}
				// Start cooldown after completion so safety/probe latency cannot
				// shorten the interval between actual subprocess starts.
				retryAt := time.Now()
				if r.now != nil {
					retryAt = r.now()
				}
				r.probeRetries[stamp] = retryAt.Add(terminalConsumerProbeRetry)
				return
			}
			r.failed = stamp
			diagnostic = "changed_protocol_incompatible"
			r.failedStatus = diagnostic
			fmt.Fprintln(a.Stderr, "binding re-exec refused: protocol incompatible")
			return
		}
		r.probed = stamp
	}
	// Control has been released. This is called only by the single consumer
	// between entire cycles, never from Send or a reservation/finish callback.
	pin, err := a.pinnedArgs(s.name)
	if err != nil {
		diagnostic = "changed_pin_unavailable"
		return
	}
	args := append([]string{r.path}, pin...)
	args = append(args, "--as", s.name, "bind", "--run-generation", generation, "--consumer-lock-fd", strconv.Itoa(int(lock.Fd())))
	finalPath, err := filepath.EvalSymlinks(r.path)
	if err != nil || finalPath != resolved {
		diagnostic = "changed_candidate_unavailable"
		r.seen = terminalExecutable{}
		return
	}
	final, err := r.observeExecutable(resolved)
	if err != nil || final != stamp {
		diagnostic = "changed_candidate_unavailable"
		r.seen = terminalExecutable{}
		return
	}
	if err := s.checkConnection(); err != nil || ctx.Err() != nil {
		diagnostic = "unavailable"
		return
	}
	// Keep the SAME flock open-file description and log descriptors through
	// exec. Everything else remains CLOEXEC. On failure restore immediately.
	fd := int(lock.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		diagnostic = "changed_exec_failed"
		return
	}
	_ = r.exec(resolved, args, env)
	syscall.CloseOnExec(fd)
	r.failed = stamp
	diagnostic = "changed_exec_failed"
	r.failedStatus = diagnostic
	fmt.Fprintln(a.Stderr, "binding re-exec refused: exec failed")
}

func terminalRefreshAuthority(ctx context.Context, a *App, s *session, generation string) (string, bool, error) {
	name, safe, _, err := terminalRefreshAuthorityReason(ctx, a, s, generation)
	return name, safe, err
}

func terminalRefreshAuthorityReason(ctx context.Context, a *App, s *session, generation string) (string, bool, string, error) {
	safe := false
	reason := "changed_safety_unavailable"
	var sessionID string
	err := a.withRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if err := st.RefuseMonitorConsumer(); err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		birth, err := tmuxwake.Birth(probe, os.Getpid())
		cancel()
		if err != nil {
			return err
		}
		safe = terminalRefreshSafe(st, generation, birth, a.Now())
		reason = terminalRefreshBlockReason(st, generation, birth, machineLabel(), os.Getpid(), a.Now())
		if safe {
			sessionID = st.Terminal.SessionID
		}
		return nil
	})
	return sessionID, safe, reason, err
}

// Diagnostics are best effort and bounded: no extra wait in the consumer loop.
func (r *terminalRefresh) recordStatus(s *session, generation, token string) {
	token = safeTerminalRefreshStatus(token)
	if s == nil || token == r.lastStatus || s.checkConnection() != nil {
		return
	}
	unchanged := false
	_, err := s.app.updateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
		if b := st.Terminal; b != nil && !b.Stopped && b.Generation == generation && b.Host == machineLabel() && b.DaemonPID == os.Getpid() && b.RefreshStatus == token && s.checkConnection() == nil {
			unchanged = true
			return errors.New("refresh diagnostic unchanged")
		}
		if s.checkConnection() != nil || !recordTerminalRefreshStatus(st, generation, machineLabel(), os.Getpid(), token) {
			return errors.New("refresh diagnostic unchanged or retired")
		}
		return nil
	})
	if err == nil || unchanged {
		r.lastStatus = token
	}
}

// Cap memory and reject even a correct token followed by extra output.
type protocolOutput struct {
	data   []byte
	limit  int
	excess bool
}

func (b *protocolOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		b.excess = true
		return 0, io.ErrShortBuffer
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func probeTerminalConsumer(ctx context.Context, path string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, terminalConsumerProbeTimeout)
	defer cancel()
	// Negotiation deliberately bypasses credentials. Loading them here would
	// expose secrets to a candidate child and run Keychain access/marker effects;
	// the inherited-lock startup path handles transient readiness refusals.
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
	if err := cmd.Start(); err != nil {
		return errConsumerProbeUnavailable
	}
	err := cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	// Explicit bad output is a refusal even if the process subsequently hangs.
	// A signal caused by our deadline is not a candidate's nonzero exit.
	if out.excess || stderr.excess || !strings.HasPrefix(consumerProtocol, string(out.data)) ||
		(cmd.ProcessState != nil && cmd.ProcessState.ExitCode() > 0) {
		return errors.New("consumer protocol refused")
	}
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return errConsumerProbeUnavailable
	}
	if err != nil || string(out.data) != consumerProtocol {
		return errors.New("consumer protocol refused")
	}
	return nil
}
