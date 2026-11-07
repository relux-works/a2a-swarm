package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// descendant inspects PID/PPID only, never command arguments or environment.
func descendant(ctx context.Context, pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		b, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
		if err != nil {
			return false
		}
		next, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if next == pid {
			return false
		}
		pid = next
	}
	return false
}

// consumerProtocol must be bumped by anyone changing the inherited-FD launch
// protocol or the persisted consumer state contract.
const consumerProtocol = "a2a-bind-consumer/1\n"

// Manual bind and SessionStart share one bounded wake policy.
const (
	defaultBindInterval = 2 * time.Second
	defaultBindRetryMax = 30 * time.Second
	defaultBindMaxWakes = 30
)

// Native session metadata is a selector, never binding/lease authority. Keep
// explicit A2A and Codex precedence, and never fall through a malformed value.
func (a *App) terminalSessionID(runtime string) string {
	if id := a.Getenv(EnvSessionID); id != "" {
		return id
	}
	if id := a.Getenv("CODEX_THREAD_ID"); id != "" {
		return id
	}
	if runtime == "claude" {
		return a.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	return ""
}

// The stage is a closed diagnostic token, not an error rendering. Lock
// contention can be recognized through wrapping at any of these boundaries.
func bindingRetryCause(stage string, err error) string {
	if errors.Is(err, config.ErrLockTimeout) {
		return "lock"
	}
	switch stage {
	case "sync", "state", "lease", "connection", "dispatch":
		return stage
	default:
		return "state"
	}
}

func cmdBind(ctx context.Context, a *App, args []string) error {
	return a.bindTerminal(ctx, args, bindingManual)
}

type bindingMode int

const (
	bindingManual bindingMode = iota
	bindingSessionStart
	bindingRecovery
)

func (a *App) bindTerminal(ctx context.Context, args []string, mode bindingMode) error {
	// Handle this exact hidden invocation before session/state/lease access.
	if len(args) == 1 && args[0] == "--consumer-protocol" {
		_, err := io.WriteString(a.Stdout, consumerProtocol)
		return err
	}
	automatic := mode != bindingManual
	if len(args) > 0 && args[0] == "status" {
		return a.terminalStatus(ctx, args[1:])
	}

	fs := newFlagSet("bind")
	runtime := fs.String("runtime", "", "claude or codex")
	owner := fs.Int("owner-pid", 0, "own live runtime PID")
	sessionID := fs.String("session-id", "", "own live session (default A2A_SESSION_ID, CODEX_THREAD_ID, or native Claude session)")
	pane := fs.String("pane", a.Getenv("TMUX_PANE"), "own exact tmux pane")
	socket := fs.String("socket", strings.Split(a.Getenv("TMUX"), ",")[0], "own tmux socket")
	interval := fs.Duration("interval", defaultBindInterval, "idle observation and sync interval (1s..60s)")
	retryMax := fs.Duration("retry-max", defaultBindRetryMax, "maximum sync-error backoff (at most 5m)")
	rate := fs.Int("max-wakes-per-hour", defaultBindMaxWakes, "maximum wake attempts per hour")
	run := fs.String("run-generation", "", "internal detached consumer generation")
	lockFD := fs.Int("consumer-lock-fd", 0, "internal inherited consumer lock")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("bind takes flags only")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := refuseHostConsumer(s); err != nil {
		return err
	}
	if *run != "" {
		return runBoundChild(ctx, a, *run, *lockFD)
	}
	if *lockFD != 0 {
		return usagef("consumer lock requires detached generation")
	}
	if *sessionID == "" {
		*sessionID = a.terminalSessionID(*runtime)
	}
	if *owner <= 1 || *interval < time.Second || *interval > maxPoll || *retryMax < *interval || *retryMax > 5*time.Minute || *rate < 1 || *rate > 3600 || !claudeSessionRE.MatchString(*sessionID) {
		return usagef("bind requires --runtime, own --owner-pid, --session-id (or session env), --interval 1s..60s, --retry-max interval..5m and --max-wakes-per-hour 1..3600")
	}
	current := a.Getenv(EnvSessionID)
	if current == "" && *runtime == "claude" && a.Getenv("CODEX_THREAD_ID") == "" {
		current = a.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	if current != "" && current != *sessionID {
		return usagef("bind refuses another live session")
	}
	if current := a.Getenv("CODEX_THREAD_ID"); current != "" && (*runtime != "codex" || current != *sessionID) {
		return usagef("bind refuses another live Codex session")
	}
	if *pane != a.Getenv("TMUX_PANE") || *socket != strings.Split(a.Getenv("TMUX"), ",")[0] || *pane == "" || *socket == "" {
		return usagef("bind requires this invocation's own tmux pane/socket")
	}
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	target, err := tmuxwake.Pin(probe, *socket, *pane, *runtime)
	if err != nil {
		return err
	}
	if !descendant(probe, os.Getppid(), *owner) || !descendant(probe, *owner, target.PanePID) {
		return errors.New("bind owner is not this invocation's runtime in this pane")
	}
	birth, err := tmuxwake.Birth(probe, *owner)
	if err != nil {
		return err
	}
	target.OwnerPID = *owner
	target.OwnerBirth = birth
	var ready *os.File
	var child *exec.Cmd
	existing := false
	err = config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.HostEnrollment != nil {
			return config.ErrHostEnrollment
		}
		if st.HooksPaused || manualWakeGate(st) != "" {
			return errors.New("binding refused: manual gate")
		}
		if err := a.guardBindingProcesses(ctx, s.name, s.paths.ConfigRoot()); err != nil {
			return err
		}
		if st.Runtime != nil && !st.Runtime.Retired {
			return errors.New("native binding exists; use its verified transport, do not start terminal fallback")
		}
		registryRoot := a.ownRegistryRoot(ctx, target, *sessionID)
		sameTarget := func(b *config.TerminalBinding) bool {
			return ownTerminalTarget(b, target)
		}
		if b := st.Terminal; mode == bindingSessionStart && b != nil && b.SessionID != *sessionID && sameTarget(b) {
			st, err = a.handoverTerminalSession(s, b, target, *sessionID)
			if err != nil {
				return err
			}
		}
		if automatic && st.Terminal != nil && st.Terminal.Stopped && st.Terminal.SessionID == *sessionID && st.Terminal.Outcome == "unbound" {
			return errors.New("auto-bind suppressed: this session was explicitly stopped")
		}
		if b := st.Terminal; b != nil && !b.Stopped {
			if b.Host != machineLabel() {
				return errors.New("binding refused: prior binding belongs to another host")
			}
			live, ownerErr := bindingProcessBirth(ctx, b.OwnerPID)
			if ownerErr != nil && !processGone(b.OwnerPID) {
				return bindingInspectionFailure("prior-owner-birth", ownerErr)
			}
			ownerGone := ownerErr == nil && live != b.OwnerBirth || processGone(b.OwnerPID)
			if !ownerGone {
				matches := automatic && b.SessionID == *sessionID && sameTarget(b)
				if !matches {
					return errors.New("session already bound; unbind this generation first")
				}
				daemon, daemonErr := bindingProcessBirth(ctx, b.DaemonPID)
				if daemonErr == nil && daemon != "" && daemon == b.DaemonBirth {
					if err := a.associateTerminalRoot(s, *b, registryRoot); err != nil {
						return err
					}
					existing = true
					return a.acquireLease(s, *sessionID, time.Now().Add(nativeRPCTimeout))
				}
				if daemonErr != nil && b.DaemonPID > 1 && !processGone(b.DaemonPID) {
					return bindingInspectionFailure("current-consumer-birth", daemonErr)
				}
				// No live consumer remains: SessionStart recovers automatically,
				// carrying the same-session attempt fence and hourly ledger.
			}
			if automatic && ownerGone {
				if err := waitPriorBindingConsumer(ctx, b); err != nil {
					return err
				}
				// Preserve the consumer's final journal writes made during wait.
				st, err = config.LoadState(s.paths, s.name)
				if err != nil {
					return err
				}
				if st.Terminal != nil && st.Terminal.Generation != b.Generation {
					return errors.New("binding changed while prior consumer stopped")
				}
				if st.HooksPaused || manualWakeGate(st) != "" {
					return errors.New("binding refused: manual gate")
				}
			}
		}
		if b := st.Terminal; b != nil && b.DaemonPID > 0 {
			live, err := bindingProcessBirth(ctx, b.DaemonPID)
			if err != nil && !processGone(b.DaemonPID) {
				return bindingInspectionFailure("retiring-consumer-birth", err)
			}
			if live != "" && live == b.DaemonBirth {
				return errors.New("prior bound consumer still stopping")
			}
		}
		var random [16]byte
		if err = a.acquireLease(s, *sessionID, time.Now().Add(nativeRPCTimeout)); err != nil {
			return err
		}
		if _, err = rand.Read(random[:]); err != nil {
			return err
		}
		b := &config.TerminalBinding{Generation: hex.EncodeToString(random[:]), SessionID: *sessionID, Host: machineLabel(), OwnerPID: *owner, OwnerBirth: birth, Target: target, ClaudeRegistryRoot: registryRoot, Interval: *interval, RetryMax: *retryMax, MaxWakes: *rate, Outcome: "starting"}
		if old := st.Terminal; old != nil {
			b.Reservations = slices.Clone(old.Reservations)
			if old.SessionID == b.SessionID {
				b.Rung = slices.Clone(old.Rung)
				b.AttemptIDs = slices.Clone(old.AttemptIDs)
				b.Notice = old.Notice
				b.NoticeCount = old.NoticeCount
				b.AttemptAt = old.AttemptAt
				b.Retried = old.Retried
				b.Typed = old.Typed && ownTerminalTarget(old, target)
			}
		}
		if err = persistTerminalReplacement(s, b, automatic); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		var writer *os.File
		ready, writer, err = os.Pipe()
		if err != nil {
			return err
		}
		// The daemon log contains fixed outcomes only, never screens or bodies.
		logPath := filepath.Join(s.paths.Base, "state", s.name+".binding.log")
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0600)
		if errors.Is(err, os.ErrExist) {
			info, e := os.Lstat(logPath)
			if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return errors.New("binding log must be a regular 0600 file")
			}
			log, err = os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0600)
		}
		if err != nil {
			ready.Close()
			writer.Close()
			return err
		}
		defer log.Close()
		pin, err := a.pinnedArgs(s.name)
		if err != nil {
			return err
		}
		child = exec.Command(exe, append(pin, "--as", s.name, "bind", "--run-generation", b.Generation)...)
		child.Env = detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), b.SessionID)
		child.ExtraFiles = []*os.File{writer}
		child.Stdout = log
		child.Stderr = log
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = child.Start(); err != nil {
			ready.Close()
			writer.Close()
			return err
		}
		writer.Close()
		b.DaemonPID = child.Process.Pid
		b.DaemonBirth, err = bindingProcessBirth(ctx, b.DaemonPID)
		if err == nil {
			err = persistTerminalConsumer(s, b)
		}
		if err != nil {
			child.Process.Signal(syscall.SIGTERM)
		}
		go child.Wait()
		return err
	})
	if err != nil {
		return err
	}
	if existing {
		return nil
	}
	defer ready.Close()
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = ready.SetReadDeadline(deadline); err != nil {
		return err
	}
	buf, err := io.ReadAll(io.LimitReader(ready, 128))
	if err != nil || string(buf) != "ready\n" {
		if child != nil {
			child.Process.Signal(syscall.SIGTERM)
		}
		return errors.New("binding did not arm; inspect own binding status/log before retrying")
	}
	a.printf("%s bound pid=%d; persistent terminal fallback; unbind stops it\n", s.name, child.Process.Pid)
	return nil
}

func runBoundChild(ctx context.Context, a *App, generation string, inherited int) error {
	reason := "startup unavailable"
	defer func() { fmt.Fprintln(a.Stderr, "binding exit: "+reason) }()
	var ready *os.File
	if inherited == 0 {
		ready = os.NewFile(3, "binding-ready")
		if ready == nil {
			return errors.New("missing bind startup pipe")
		}
		defer ready.Close()
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	return config.WithConsumerLock(s.paths, s.name, time.Now().Add(100*time.Millisecond), inherited, func(lock *os.File) error {
		refresh := newTerminalRefresh(ctx)
		var initial config.TerminalBinding
		err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
			st, err := config.LoadState(s.paths, s.name)
			if err != nil {
				return err
			}
			b := st.Terminal
			birth, err := tmuxwake.Birth(ctx, os.Getpid())
			if err != nil || b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() || b.DaemonBirth != birth || b.Host != machineLabel() {
				reason = "stale consumer"
				return errors.New("binding stale consumer")
			}
			if err = a.acquireLease(s, b.SessionID, time.Now().Add(nativeRPCTimeout)); err != nil {
				return err
			}
			initial = *b
			if ready != nil {
				if _, err = ready.WriteString("ready\n"); err != nil {
					return err
				}
				ready.Close()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if inherited != 0 {
			fmt.Fprintln(a.Stderr, "binding re-exec: executable changed")
		}
		var runErr error
		reason, runErr = a.runTerminalWithRefresh(ctx, s, generation, initial, func() { refresh.poll(ctx, a, s, generation, lock) })
		return runErr
	})
}

func cmdUnbind(ctx context.Context, a *App, args []string) error {
	if len(args) != 0 {
		return usagef("unbind takes no arguments")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	var pid int
	var birth string
	err = config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
			b := st.Terminal
			if b == nil || b.Stopped {
				return nil
			}
			sessionID := a.terminalSessionID(b.Target.Runtime)
			if !claudeSessionRE.MatchString(sessionID) || sessionID != b.SessionID || b.Host != machineLabel() {
				return errors.New("unbind refuses another session's binding")
			}
			pid, birth = b.DaemonPID, b.DaemonBirth
			b.Stopped = true
			b.Idle = nil
			b.Outcome = "unbound"
			return nil
		})
		return err
	})
	if err != nil {
		return err
	}
	if pid > 0 {
		current, err := tmuxwake.Birth(ctx, pid)
		if err == nil && current == birth {
			proc, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			if err = proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
			deadline := time.Now().Add(5 * time.Second)
			for !processGone(pid) && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(25 * time.Millisecond):
				}
			}
			if !processGone(pid) {
				return errors.New("binding retired but process has not exited; no replacement started")
			}
		} else if err != nil && !processGone(pid) {
			return errors.New("binding process observation unavailable; no process signalled")
		}
	}
	a.printf("%s unbound; mailbox retained\n", s.name)
	return nil
}

func (a *App) terminalUpdate(s *session, generation string, fn func(*config.TerminalBinding)) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
			return errors.New("binding retired")
		}
		fn(st.Terminal)
		dropRetiredStrandedAlarms(st)
		return nil
	})
	return err
}

// Reset observation credit in the existing restart/interruption transaction.
// Episode fences and pending sends survive the reset.
func (a *App) resetTerminalObservations(s *session, generation string) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		if st.Terminal == nil || st.Terminal.Stopped || st.Terminal.Generation != generation {
			return errors.New("binding retired")
		}
		st.Terminal.Idle = nil
		st.MailAlarm.IdleSince = time.Time{}
		st.MailAlarm.LastObserved = time.Time{}
		return nil
	})
	return err
}

// runTerminal retains its validated policy snapshot across local read errors;
// no delivery action runs until fresh authority and a lease are available.
func (a *App) runTerminal(ctx context.Context, s *session, generation string, initial config.TerminalBinding) (string, error) {
	return a.runTerminalWithRefresh(ctx, s, generation, initial, nil)
}

func (a *App) runTerminalWithRefresh(ctx context.Context, s *session, generation string, initial config.TerminalBinding, refresh func()) (string, error) {
	policy := initial
	alarmCtx, stopAlarm := context.WithCancel(ctx)
	defer stopAlarm()
	alarmWake := make(chan struct{}, 1)
	go a.runMailAlarm(alarmCtx, s, generation, alarmWake)
	delay := policy.Interval
	qualificationInterrupted := true
	for ctx.Err() == nil {
		if err := s.checkConnection(); err != nil {
			return "selected connection unavailable", err
		}
		stage := "state"
		st, err := config.LoadState(s.paths, s.name)
		if err == nil {
			b := st.Terminal
			if b == nil {
				return "binding vanished", nil
			}
			if b.Generation != generation {
				return "generation replaced", nil
			}
			if b.Stopped {
				return "stopped", nil
			}
			policy = *b
			probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			owner, ownerErr := tmuxwake.Birth(probe, b.OwnerPID)
			_, _, _, targetErr := b.Target.Inspect(probe)
			cancel()
			if ownerErr == nil && owner != b.OwnerBirth || ownerErr != nil && processGone(b.OwnerPID) || errors.Is(targetErr, tmuxwake.ErrTargetGone) {
				a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Stopped = true; b.Outcome = "target exited or replaced" })
				if processGone(b.OwnerPID) || ownerErr == nil && owner != b.OwnerBirth {
					return "owner gone or replaced", nil
				}
				return "target gone or replaced", nil
			}
			if ownerErr != nil {
				stage = "connection"
				err = ownerErr
			} else if targetErr != nil {
				stage = "connection"
				err = targetErr
			} else {
				stage = "lease"
				err = a.acquireLease(s, b.SessionID, time.Now().Add(nativeRPCTimeout))
			}
		}
		if err == nil {
			syncCtx, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			var subs SubList
			stage = "state"
			st, subs, err = s.startDelivery(syncCtx, a.Now())
			if err == nil {
				stage = "sync"
				if st.NextBatch == "" {
					_, err = s.takePosition(syncCtx, time.Now().Add(nativeRPCTimeout))
				} else {
					_, err = s.syncNow(syncCtx, st, subs.Subs, time.Now().Add(nativeRPCTimeout))
				}
			}
			cancel()
		}
		if err == nil {
			// A failed authority read can also prevent writing its reset. Keep
			// that interruption in RAM until a fresh successful write precedes
			// dispatch, including the first cycle of a restarted consumer.
			if qualificationInterrupted {
				stage = "state"
				err = a.resetTerminalObservations(s, generation)
				qualificationInterrupted = err != nil
			}
		}
		if err == nil {
			stage = "dispatch"
			err = a.dispatchTerminal(ctx, s, generation, alarmWake)
		}
		if ctx.Err() != nil {
			return "cancelled", nil
		}
		if err != nil {
			if guardErr := s.checkConnection(); guardErr != nil {
				return "selected connection unavailable", guardErr
			}
			qualificationInterrupted = true
			delay = min(policy.RetryMax, max(policy.Interval, delay*2))
			// A failed write is not a reason to terminate. The next cycle must load
			// fresh state and reacquire its lease before it may touch the terminal.
			a.unknownTerminalNotice(ctx, s, generation, true)
			fmt.Fprintf(a.Stderr, "binding consumer unavailable; bounded retry cause=%s\n", bindingRetryCause(stage, err))
		} else {
			delay = policy.Interval
			if fresh, readErr := config.LoadState(s.paths, s.name); readErr == nil {
				delay = a.notificationWait(fresh, delay)
			}
		}
		// Dispatch (including Send, finish and all notice publication) returned;
		// no control/state lock or synchronous effect is in flight. Sync errors
		// alone must not strand an old image: refresh obtains fresh authority
		// and independently refuses any outstanding attempt or reservation.
		if refresh != nil {
			refresh()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "cancelled", nil
		case <-timer.C:
		}
	}
	return "cancelled", nil
}

func (a *App) dispatchTerminal(ctx context.Context, s *session, generation string, alarmWakes ...chan<- struct{}) error {
	// Only the consumer supplies a wake channel for alarm observation.
	var alarmWake chan<- struct{}
	if len(alarmWakes) > 0 {
		alarmWake = alarmWakes[0]
	}
	if err := s.checkConnection(); err != nil {
		return err
	}
	observed := false
	err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation {
			return errors.New("binding retired")
		}
		heldObservation := func(outcome string) error {
			if err := a.observeTerminalNotice(s, generation, *b, "unknown"); err != nil {
				return err
			}
			observed = true
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = outcome })
		}
		if st.HooksPaused || manualWakeGate(st) != "" {
			return heldObservation("suppressed: manual gate")
		}
		if st.Runtime != nil && !st.Runtime.Retired {
			return heldObservation("suppressed: native binding")
		}
		if !ownTerminalTarget(b, b.Target) {
			return heldObservation("suppressed: binding target authority")
		}
		if l := st.Lease; l == nil || l.Holder != b.SessionID || l.Host != machineLabel() || !l.Expires.After(a.Now()) {
			return heldObservation("suppressed: lease authority")
		}
		// Mail remains available to inbox. Attempt and rung IDs prevent normal
		// duplicates; an uncertain attempt has a separate bounded recovery path.
		var ids []string
		var pendingIDs []string
		alarm := alarmSample{At: a.Now()}
		_, err = s.lockedDeliver(ctx, time.Now().Add(nativeRPCTimeout), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
			for _, entry := range mb.Pending() {
				alarm.add(entry)
				pendingIDs = append(pendingIDs, entry.ID)
				if !slices.Contains(b.Rung, entry.ID) {
					ids = append(ids, entry.ID)
				}
			}
			return nil, nil
		})
		if err != nil {
			return err
		}
		attemptIDs := slices.DeleteFunc(slices.Clone(b.AttemptIDs), func(id string) bool { return !slices.Contains(pendingIDs, id) })
		b.AttemptIDs = attemptIDs
		if err = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
			b.Rung = slices.DeleteFunc(b.Rung, func(id string) bool { return !slices.Contains(pendingIDs, id) })
			b.AttemptIDs = slices.Clone(attemptIDs)
		}); err != nil {
			return err
		}
		// Diagnostics never grant ownership; continuation below independently
		// checks the persisted generation-scoped typing origin.
		readiness := boundTerminal(*b).ObserveReadiness(ctx, b.Notice)
		observation := readinessInput(readiness)
		if alarmWake != nil {
			a.observeMailAlarm(s, generation, alarmObservation(alarm, readiness))
			select {
			case alarmWake <- struct{}{}:
			default:
			}
		}
		if err := a.observeTerminalReadiness(s, *b, readiness); err != nil {
			return err
		}
		observed = true
		if err := a.observeTerminalNotice(s, generation, *b, observation); err != nil {
			return err
		}
		// Only an aged empty observation disproves continuous presence. A redraw
		// inside the recovery window must retain lost-ACK typing authority.
		if observation == "empty" && !a.Now().Before(b.AttemptAt.Add(max(2*b.Interval, b.RetryMax))) {
			if err := a.terminalUpdate(s, generation, func(current *config.TerminalBinding) {
				current.NoticeOrigin = nil
				current.NoticePosition = nil
				current.TypingStarted = false
			}); err != nil {
				return err
			}
			b.NoticeOrigin = nil
			b.NoticePosition = nil
			b.TypingStarted = false
		}
		if observation == "empty" && b.Typed && !a.Now().Before(b.AttemptAt.Add(max(2*b.Interval, b.RetryMax))) && len(b.AttemptIDs) < b.NoticeCount {
			if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
				b.Typed = false
				b.AttemptIDs = nil
				b.Notice = ""
				b.NoticeCount = 0
				b.NoticeOrigin = nil
				b.NoticePosition = nil
				b.TypingStarted = false
				b.AttemptAt = time.Time{}
				b.Retried = false
				b.Idle = nil
			}); err != nil {
				return err
			}
			b.Typed = false
			b.AttemptIDs = nil
			b.Notice = ""
			b.NoticeCount = 0
			b.NoticeOrigin = nil
			b.NoticePosition = nil
			b.TypingStarted = false
			b.Idle = nil
		}
		if len(b.AttemptIDs) > 0 || b.NoticeOrigin != nil && b.Notice != "" {
			return a.recoverTerminal(ctx, s, generation, *b, readiness)
		}
		if len(ids) == 0 {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "armed: no new mail" })
		}
		// Screen admission precedes the durable attempt fence. A busy target
		// retains IDs for the next idle observation without spending its rate.
		if !readiness.Eligible() {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: busy, draft or unknown input" })
		}
		now := a.Now()
		if qualified, err := a.qualifyTerminalIdle(s, generation, *b, readiness); err != nil || !qualified {
			return err
		}
		reservations := slices.DeleteFunc(slices.Clone(b.Reservations), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(reservations) >= b.MaxWakes {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: hourly rate" })
		}
		var reservedAttempt config.TerminalBinding
		token, reserveErr := a.reserveNotification(ctx, s, time.Now().Add(nativeRPCTimeout), "terminal", b.SessionID, generation, false, func(st *config.State, mb *config.Mailbox, token string) (bool, error) {
			current := st.Terminal
			if current == nil || current.Stopped || current.Generation != generation || current.SessionID != b.SessionID || !ownTerminalTarget(current, b.Target) || !sameTerminalScope(*current, *b) || !sameTerminalIdle(current.Idle, b.Idle) {
				return false, errors.New("binding retired")
			}
			if st.HooksPaused || manualWakeGate(*st) != "" {
				current.Outcome = "suppressed: manual gate"
				return false, nil
			}
			if l := st.Lease; l == nil || l.Holder != b.SessionID || l.Host != machineLabel() || !l.Expires.After(a.Now()) {
				return false, errors.New("terminal lease changed")
			}
			if len(current.AttemptIDs) > 0 {
				return false, errors.New("terminal attempt changed")
			}
			pending := mb.Pending()
			ids = slices.DeleteFunc(ids, func(id string) bool {
				return !slices.ContainsFunc(pending, func(en config.MailEntry) bool { return en.ID == id }) || slices.Contains(current.Rung, id)
			})
			if len(ids) == 0 {
				current.Idle = nil
				current.Outcome = "armed: no new mail"
				return false, nil
			}
			current.NoticeOrigin = current.Idle
			current.NoticePosition = nil
			current.TypingStarted = false
			current.Idle = nil
			current.AttemptIDs = ids
			current.Notice = tmuxwake.Notice(len(ids))
			current.NoticeCount = len(ids)
			current.AttemptAt = now
			current.Retried = false
			current.Typed = false
			current.Reservations = append(reservations, now)
			current.NotificationToken = token
			current.Outcome = "attempt reserved"
			reservedAttempt = *current
			reservedAttempt.AttemptIDs = slices.Clone(current.AttemptIDs)
			return true, nil
		})
		if reserveErr != nil || token == "" {
			return reserveErr
		}
		if err := s.checkConnection(); err != nil {
			return err
		}
		outcome, sendErr := a.sendTerminalAttempt(ctx, s, reservedAttempt, readiness.Key)
		return a.finishTerminal(s, generation, outcome, sendErr, false, reservedAttempt)
	})
	if err != nil {
		_ = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil })
	}
	if err != nil || !observed {
		a.unknownTerminalNotice(ctx, s, generation, false)
	} else {
		return a.completeTerminalObservation(ctx, s, generation)
	}
	return err
}

func (a *App) finishTerminal(s *session, generation string, result tmuxwake.SendResult, sendErr error, recovering bool, expected config.TerminalBinding) error {
	var refusal *config.TerminalSendRefusal
	token := expected.NotificationToken
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Generation != generation || b.Stopped {
			return errors.New("binding retired")
		}
		if token == "" || b.NotificationToken != token || !sameTerminalScope(*b, expected) || b.SessionID != expected.SessionID || !b.AttemptAt.Equal(expected.AttemptAt) || !slices.Equal(b.AttemptIDs, expected.AttemptIDs) {
			return config.ErrNotificationStale
		}

		outcome := result.Text()
		if sendErr != nil && result.Disposition == tmuxwake.SendSubmitted {
			outcome = string(tmuxwake.SendInterrupted)
		}
		b.Outcome = outcome
		if result.Valid() && (result.Disposition == tmuxwake.SendDraft || result.Disposition == tmuxwake.SendRefused) {
			at := result.ReasonAt
			if at.IsZero() || at.After(a.Now()) {
				at = a.Now()
			}
			refusal = &config.TerminalSendRefusal{Reason: tmuxwake.SafeSendReason(result.Reason), At: at.UTC()}
			b.SendRefusal = refusal
		}
		b.Idle = nil
		// Only the typing invocation may create this position constraint.
		// Preserve it across later-cycle refusals; a restart cannot invent it.
		if b.TypingStarted && result.NoticePosition != nil && result.Valid() && (result.Disposition == tmuxwake.SendDraft || result.Disposition == tmuxwake.SendUncertain) {
			position := *result.NoticePosition
			b.NoticePosition = &position
		}
		if sendErr != nil || !result.Valid() {
			return nil
		}
		if result.Disposition == tmuxwake.SendDraft {
			b.Typed = true
			return nil
		}
		if result.Disposition == tmuxwake.SendRefused {
			if !recovering {
				b.AttemptIDs = nil
				b.Notice = ""
				b.NoticeOrigin = nil
				b.NoticePosition = nil
				b.TypingStarted = false
			}
			if r := st.Notification.Reservation; r != nil && r.Token == token {
				return st.Notification.Cancel(token)
			}
			return nil
		}
		if result.Disposition != tmuxwake.SendSubmitted || result.Outcome != tmuxwake.SendWakeSubmitted {
			return nil
		}
		b.Rung = append(b.Rung, b.AttemptIDs...)
		b.AttemptIDs = nil
		b.Notice = ""
		b.Typed = false
		b.NotificationToken = ""
		b.NoticeOrigin = nil
		b.NoticePosition = nil
		b.TypingStarted = false
		clear := false
		b.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &clear, ObservedAt: a.Now().UTC().Format(time.RFC3339Nano)}
		b.NoticeReport = nil
		dropRetiredStrandedAlarms(st)
		if recovering && expected.NoticeOrigin != nil && expected.TypingStarted {
			b.Recovery = &config.TerminalRecovery{Reason: config.TerminalRecovered, At: a.Now().UTC()}
		}
		return a.completeNotification(s, st, token)
	})
	if err == nil && refusal != nil {
		// The journal already committed delivery effects; diagnostics cannot
		// turn this poll into a failure or mark its notice unknown.
		_ = appendSendRefusal(s, *refusal)
	}
	return err
}

func (a *App) recoverTerminal(ctx context.Context, s *session, generation string, b config.TerminalBinding, readiness tmuxwake.ReadinessAssessment) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	input := readinessInput(readiness)
	if input != "empty" && b.NoticeOrigin != nil && b.TypingStarted {
		return a.resumeTerminalNotice(ctx, s, generation, b)
	}
	if input != "empty" {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: pending attempt input" })
	}
	if b.NoticeCount <= 0 || b.Notice != tmuxwake.Notice(b.NoticeCount) {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
			b.Idle = nil
			b.Outcome = "uncertain legacy attempt; retained mail requires inbox"
		})
	}
	if len(b.AttemptIDs) != b.NoticeCount {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "uncertain partial attempt; mail retained" })
	}
	now := a.Now()
	if b.NoticeOrigin != nil && b.TypingStarted && now.Before(b.AttemptAt.Add(max(2*b.Interval, b.RetryMax))) {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: awaiting notice render" })
	}
	if input == "empty" {
		if qualified, err := a.qualifyTerminalIdle(s, generation, b, readiness); err != nil || !qualified {
			return err
		}
		// A fresh type into an emptied box spends a new hourly reservation,
		// including a previously known-unsubmitted notice (Typed=true).
		// Empty input may follow an accepted turn; the retry remains bounded.
		if input == "empty" && (b.Retried || now.Before(b.AttemptAt.Add(2*b.Interval))) {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "uncertain attempt; bounded retry held" })
		}
		reservations := slices.DeleteFunc(slices.Clone(b.Reservations), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(reservations) >= b.MaxWakes {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: hourly rate" })
		}
	}
	var reservedAttempt config.TerminalBinding
	token, err := a.reserveNotification(ctx, s, time.Now().Add(nativeRPCTimeout), "terminal", b.SessionID, generation, false, func(st *config.State, _ *config.Mailbox, token string) (bool, error) {
		current := st.Terminal
		if current == nil || current.Stopped || current.Generation != generation || current.SessionID != b.SessionID || !current.AttemptAt.Equal(b.AttemptAt) || !slices.Equal(current.AttemptIDs, b.AttemptIDs) || current.NotificationToken != b.NotificationToken {
			return false, errors.New("terminal recovery attempt changed")
		}
		if st.HooksPaused || manualWakeGate(*st) != "" {
			current.Outcome = "suppressed: manual gate"
			return false, nil
		}
		if l := st.Lease; l == nil || l.Holder != b.SessionID || l.Host != machineLabel() || !l.Expires.After(a.Now()) {
			return false, errors.New("terminal lease changed")
		}
		if !ownTerminalTarget(current, b.Target) || !sameTerminalScope(*current, b) || !sameTerminalIdle(current.Idle, b.Idle) {
			return false, errors.New("terminal target changed")
		}
		reservations := slices.DeleteFunc(slices.Clone(current.Reservations), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(reservations) >= current.MaxWakes {
			current.Outcome = "suppressed: hourly rate"
			return false, nil
		}
		current.NoticeOrigin = current.Idle
		current.NoticePosition = nil
		current.TypingStarted = false
		current.Retried = true
		current.Reservations = append(reservations, now)
		current.Idle = nil
		current.Typed = false
		current.NotificationToken = token
		reservedAttempt = *current
		reservedAttempt.AttemptIDs = slices.Clone(current.AttemptIDs)
		return true, nil
	})
	if err != nil || token == "" {
		return err
	}
	var outcome tmuxwake.SendResult
	var sendErr error
	if input == "empty" {
		if err := s.checkConnection(); err != nil {
			return err
		}
		outcome, sendErr = a.sendTerminalAttempt(ctx, s, reservedAttempt, readiness.Key)

	}
	return a.finishTerminal(s, generation, outcome, sendErr, true, reservedAttempt)
}
