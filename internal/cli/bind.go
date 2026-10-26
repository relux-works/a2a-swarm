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
	automatic := mode != bindingManual
	if len(args) > 0 && args[0] == "status" {
		return a.terminalStatus(ctx, args[1:])
	}

	fs := newFlagSet("bind")
	runtime := fs.String("runtime", "", "claude or codex")
	owner := fs.Int("owner-pid", 0, "own live runtime PID")
	sessionID := fs.String("session-id", "", "own live session (default A2A_SESSION_ID or CODEX_THREAD_ID)")
	pane := fs.String("pane", a.Getenv("TMUX_PANE"), "own exact tmux pane")
	socket := fs.String("socket", strings.Split(a.Getenv("TMUX"), ",")[0], "own tmux socket")
	interval := fs.Duration("interval", 0, "explicit idle observation and sync interval (1s..60s)")
	retryMax := fs.Duration("retry-max", 0, "explicit maximum sync-error backoff (at most 5m)")
	rate := fs.Int("max-wakes-per-hour", 0, "explicit maximum wake attempts per hour")
	run := fs.String("run-generation", "", "internal detached consumer generation")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("bind takes flags only")
	}
	if *run != "" {
		return runBoundChild(ctx, a, *run)
	}
	if *sessionID == "" {
		*sessionID = a.Getenv(EnvSessionID)
		if *sessionID == "" {
			*sessionID = a.Getenv("CODEX_THREAD_ID")
		}
	}
	if *owner <= 1 || *interval < time.Second || *interval > maxPoll || *retryMax < *interval || *retryMax > 5*time.Minute || *rate < 1 || *rate > 3600 || !claudeSessionRE.MatchString(*sessionID) {
		return usagef("bind requires --runtime, own --owner-pid, --session-id (or session env), --interval 1s..60s, --retry-max interval..5m and --max-wakes-per-hour 1..3600")
	}
	if current := a.Getenv(EnvSessionID); current != "" && current != *sessionID {
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
	s, err := a.session()
	if err != nil {
		return err
	}
	var ready *os.File
	var child *exec.Cmd
	existing := false
	err = config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
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
				return ErrBindingProcessUnknown
			}
			ownerGone := ownerErr == nil && live != b.OwnerBirth || processGone(b.OwnerPID)
			if !ownerGone {
				matches := automatic && b.SessionID == *sessionID && sameTarget(b)
				if !matches {
					return errors.New("session already bound; unbind this generation first")
				}
				daemon, daemonErr := bindingProcessBirth(ctx, b.DaemonPID)
				if daemonErr == nil && daemon != "" && daemon == b.DaemonBirth {
					existing = true
					return a.acquireLease(s, *sessionID, time.Now().Add(nativeRPCTimeout))
				}
				if daemonErr != nil && b.DaemonPID > 1 && !processGone(b.DaemonPID) {
					return ErrBindingProcessUnknown
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
				return ErrBindingProcessUnknown
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
		b := &config.TerminalBinding{Generation: hex.EncodeToString(random[:]), SessionID: *sessionID, Host: machineLabel(), OwnerPID: *owner, OwnerBirth: birth, Target: target, Interval: *interval, RetryMax: *retryMax, MaxWakes: *rate, Outcome: "starting"}
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
				b.OwnNotice = old.OwnNotice
				b.OwnNotice.PossiblyOrphaned = nil
			}
		}
		if _, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal = b; return nil }); err != nil {
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
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
		child.Env = append(os.Environ(), "A2A_CONFIG_DIR="+s.paths.ConfigRoot(), "A2A_SESSION_ID="+b.SessionID)
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
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal = b; return nil })
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

func runBoundChild(ctx context.Context, a *App, generation string) error {
	reason := "startup unavailable"
	defer func() { fmt.Fprintln(a.Stderr, "binding exit: "+reason) }()
	ready := os.NewFile(3, "binding-ready")
	if ready == nil {
		return terminalAt("authority", errors.New("missing bind startup pipe"))
	}
	defer ready.Close()
	s, err := a.session()
	if err != nil {
		return terminalAt("connection", err)
	}
	err = config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(100*time.Millisecond), func() error {
		var initial config.TerminalBinding
		err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
			st, err := config.LoadState(s.paths, s.name)
			if err != nil {
				return terminalAt("state", err)
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
			if _, err = ready.WriteString("ready\n"); err != nil {
				return err
			}
			ready.Close()
			return nil
		})
		if err != nil {
			return err
		}
		var runErr error
		reason, runErr = a.runTerminal(ctx, s, generation, initial)
		return runErr
	})
	return terminalAt("authority", err)
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
			sessionID := a.Getenv(EnvSessionID)
			if sessionID == "" {
				sessionID = a.Getenv("CODEX_THREAD_ID")
			}
			if sessionID != b.SessionID || b.Host != machineLabel() {
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
		return terminalAt("connection", err)
	}
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return terminalAt("connection", err)
		}
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
			return terminalAt("authority", errors.New("binding retired"))
		}
		fn(st.Terminal)
		return nil
	})
	return terminalAt("state", err)
}

// runTerminal retains its validated policy snapshot across local read errors;
// no delivery action runs until fresh authority and a lease are available.
func (a *App) runTerminal(ctx context.Context, s *session, generation string, initial config.TerminalBinding) (string, error) {
	policy := initial
	delay := policy.Interval
	qualificationInterrupted := true
	for ctx.Err() == nil {
		if err := s.checkConnection(); err != nil {
			a.recordTerminalFailure(s, generation, "connection", err)
			return "selected connection unavailable", terminalAt("connection", err)
		}
		phase := "state"
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
				a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
					b.Stopped = true
					b.Outcome = "target exited or replaced"
					d := terminalDiagnostic("authority", "target_gone", "suppressed", a.Now())
					b.Diagnostic = &d
				})
				if processGone(b.OwnerPID) || ownerErr == nil && owner != b.OwnerBirth {
					return "owner gone or replaced", nil
				}
				return "target gone or replaced", nil
			}
			if ownerErr != nil {
				phase, err = "authority", ownerErr
			} else if targetErr != nil {
				phase, err = "authority", targetErr
			} else {
				phase = "authority"
				err = a.acquireLease(s, b.SessionID, time.Now().Add(nativeRPCTimeout))
			}
		}
		if err == nil {
			syncCtx, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			var subs SubList
			phase = "delivery_start"
			st, subs, err = s.startDelivery(syncCtx, a.Now())
			if err == nil {
				if st.NextBatch == "" {
					phase = "delivery_position"
					_, err = s.takePosition(syncCtx, time.Now().Add(nativeRPCTimeout))
				} else {
					phase = "delivery_sync"
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
				phase = "qualification"
				err = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil })
				qualificationInterrupted = err != nil
			}
		}
		if err == nil {
			phase = "dispatch"
			err = a.dispatchTerminal(ctx, s, generation)
		}
		if ctx.Err() != nil {
			return "cancelled", nil
		}
		if err != nil {
			// Preserve the first failed operation even if the connection guard
			// subsequently prevents its diagnostic from reaching durable state.
			a.recordTerminalFailure(s, generation, phase, err)
			if guardErr := s.checkConnection(); guardErr != nil {
				return "selected connection unavailable", terminalAt("connection", guardErr)
			}
			qualificationInterrupted = true
			delay = min(policy.RetryMax, max(policy.Interval, delay*2))
			// A failed write is not a reason to terminate. The next cycle must load
			// fresh state and reacquire its lease before it may touch the terminal.
			a.unknownTerminalNotice(ctx, s, generation, true)
		} else {
			delay = policy.Interval
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

func (a *App) dispatchTerminal(ctx context.Context, s *session, generation string) error {
	if err := s.checkConnection(); err != nil {
		return terminalAt("connection", err)
	}
	observed := false
	phase := "dispatch_lock"
	err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		if err := s.checkConnection(); err != nil {
			return terminalAt("connection", err)
		}
		phase = "state"
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		b := st.Terminal
		phase = "authority"
		if b == nil || b.Stopped || b.Generation != generation {
			return errors.New("binding retired")
		}
		if st.HooksPaused || manualWakeGate(st) != "" {
			class := "manual_gate"
			if st.HooksPaused {
				class = "hook_pause"
			}
			return a.terminalSuppressed(s, generation, "authority", class, "suppressed: manual gate")
		}
		if st.Runtime != nil && !st.Runtime.Retired {
			return a.terminalSuppressed(s, generation, "authority", "native_binding", "suppressed: native binding")
		}
		if !ownTerminalTarget(b, b.Target) {
			return a.terminalSuppressed(s, generation, "authority", "binding_authority", "suppressed: binding target authority")
		}
		if l := st.Lease; l == nil || l.Holder != b.SessionID || l.Host != machineLabel() || !l.Expires.After(a.Now()) {
			return a.terminalSuppressed(s, generation, "authority", "lease_authority", "suppressed: lease authority")
		}
		// Mail remains available to inbox. Attempt and rung IDs prevent normal
		// duplicates; an uncertain attempt has a separate bounded recovery path.
		var ids []string
		var pendingIDs []string
		phase = "mailbox"
		_, err = s.lockedDeliver(ctx, time.Now().Add(nativeRPCTimeout), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
			for _, entry := range mb.Pending() {
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
		// Reconcile only rendered empty input. A nonempty renderer never
		// authorizes continuation or erasure, even for an exact lookalike.
		observation := b.Target.ObserveInput(ctx, b.Notice)
		observed = true
		if err := a.observeTerminalNotice(s, generation, *b, observation); err != nil {
			return err
		}
		if observation == "empty" && b.Typed && len(b.AttemptIDs) < b.NoticeCount {
			if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
				b.Typed = false
				b.AttemptIDs = nil
				b.Notice = ""
				b.NoticeCount = 0
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
			b.Idle = nil
		}
		if len(b.AttemptIDs) > 0 {
			phase = "dispatch"
			return a.recoverTerminal(ctx, s, generation, *b)
		}
		if len(ids) == 0 {
			return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
				b.Idle = nil
				b.Outcome = "armed: no new mail"
				d := terminalDiagnostic("mailbox", "no_new_mail", "idle", a.Now())
				b.Diagnostic = &d
			})
		}
		// Screen admission precedes the durable attempt fence. A busy target
		// retains IDs for the next idle observation without spending its rate.
		phase = "qualification"
		if !b.Target.Idle(ctx) {
			return a.terminalSuppressed(s, generation, "qualification", "input_unqualified", "suppressed: busy, draft or unknown input")
		}
		now := a.Now()
		if qualified, err := a.qualifyTerminalIdle(s, generation, *b); err != nil || !qualified {
			return err
		}
		reservations := slices.DeleteFunc(slices.Clone(b.Reservations), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(reservations) >= b.MaxWakes {
			return a.terminalSuppressed(s, generation, "authority", "hourly_rate", "suppressed: hourly rate")
		}
		if err = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
			b.Idle = nil
			b.AttemptIDs = ids
			b.Notice = tmuxwake.Notice(len(ids))
			b.NoticeCount = len(ids)
			b.AttemptAt = now
			b.Retried = false
			b.Typed = false
			b.Reservations = append(reservations, now)
			b.Outcome = "attempt reserved"
		}); err != nil {
			return err
		}
		if err := s.checkConnection(); err != nil {
			return terminalAt("connection", err)
		}
		phase = "dispatch"
		outcome, sendErr := b.Target.Send(ctx, len(ids), nil, nil)
		return a.finishTerminal(s, generation, outcome, sendErr, false)
	})
	err = terminalAt(phase, err)
	if err != nil {
		a.recordTerminalFailure(s, generation, phase, err)
		_ = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil })
	}
	if err != nil || !observed {
		a.unknownTerminalNotice(ctx, s, generation, false)
	} else {
		a.publishTerminalNotice(ctx, s, generation)
	}
	return err
}

// Both initial admission and empty-box recovery use the same observations.
// This records a first sample without sleeping, reserving or spending a retry.
func (a *App) qualifyTerminalIdle(s *session, generation string, b config.TerminalBinding) (bool, error) {
	now := a.Now()
	observation := config.TerminalIdle{At: now, Generation: b.Generation, SessionID: b.SessionID, Host: b.Host, DaemonPID: b.DaemonPID, DaemonBirth: b.DaemonBirth, Target: b.Target, Interval: b.Interval}
	previous := b.Idle
	qualified := false
	if previous != nil {
		identity := *previous
		identity.At = now
		qualified = identity == observation && b.Interval > 0 && !now.Before(previous.At.Add(b.Interval))
	}
	if !qualified {
		return false, a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
			b.Idle = &observation
			b.Outcome = "armed: qualifying idle"
			d := terminalDiagnostic("qualification", "idle_sample", "qualifying", a.Now())
			b.Diagnostic = &d
		})
	}
	return true, nil
}

func (a *App) finishTerminal(s *session, generation, outcome string, sendErr error, recovering bool) error {
	// Unknown transport outcomes are not evidence of a submission.
	switch outcome {
	case "wake submitted", "notice left in draft: input unavailable", "notice left in draft: changed input", "suppressed: busy, draft or unknown input", "suppressed: target or width":
	case "uncertain: type", "uncertain: interrupted", "uncertain: Enter":
		if sendErr == nil {
			sendErr = errors.New("uncertain dispatch")
		}
	default:
		outcome = "uncertain: dispatch outcome"
		sendErr = errors.New("unknown dispatch outcome")
	}
	return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
		b.Outcome = outcome
		d := terminalDiagnostic("dispatch", "submitted", "submitted", a.Now())
		switch {
		case sendErr != nil:
			d = terminalErrorDiagnostic("dispatch", sendErr, a.Now())
			d.Result = "uncertain"
		case strings.HasPrefix(outcome, "notice left in draft:"):
			d = terminalDiagnostic("dispatch", "draft_retained", "suppressed", a.Now())
		case outcome == "suppressed: busy, draft or unknown input":
			d = terminalDiagnostic("qualification", "input_unqualified", "suppressed", a.Now())
		case outcome == "suppressed: target or width":
			d = terminalDiagnostic("dispatch", "target_or_width", "suppressed", a.Now())
		}
		b.Diagnostic = &d
		if sendErr != nil {
			return
		}
		if strings.HasPrefix(outcome, "notice left in draft:") {
			b.Typed = true
			return
		}
		if strings.HasPrefix(outcome, "suppressed: busy") || outcome == "suppressed: target or width" {
			if !recovering {
				b.AttemptIDs = nil
				b.Notice = ""
			}
			return
		}
		b.Rung = append(b.Rung, b.AttemptIDs...)
		b.AttemptIDs = nil
		b.Notice = ""
		b.Typed = false
	})
}

func (a *App) recoverTerminal(ctx context.Context, s *session, generation string, b config.TerminalBinding) error {
	if err := s.checkConnection(); err != nil {
		return terminalAt("connection", err)
	}
	input := b.Target.ObserveInput(ctx, b.Notice)
	if input != "empty" {
		return a.terminalSuppressed(s, generation, "qualification", "pending_input", "suppressed: pending attempt input")
	}
	if b.NoticeCount <= 0 || b.Notice != tmuxwake.Notice(b.NoticeCount) {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
			b.Idle = nil
			b.Outcome = "uncertain legacy attempt; retained mail requires inbox"
			d := terminalDiagnostic("dispatch", "legacy_attempt", "suppressed", a.Now())
			b.Diagnostic = &d
		})
	}
	if len(b.AttemptIDs) != b.NoticeCount {
		return a.terminalSuppressed(s, generation, "dispatch", "partial_attempt", "uncertain partial attempt; mail retained")
	}
	now := a.Now()
	if input == "empty" {
		if qualified, err := a.qualifyTerminalIdle(s, generation, b); err != nil || !qualified {
			return err
		}
		// A fresh type into an emptied box spends a new hourly reservation,
		// including a previously known-unsubmitted notice (Typed=true).
		// Empty input may follow an accepted turn; the retry remains bounded.
		if input == "empty" && (b.Retried || now.Before(b.AttemptAt.Add(2*b.Interval))) {
			return a.terminalSuppressed(s, generation, "dispatch", "retry_held", "uncertain attempt; bounded retry held")
		}
		reservations := slices.DeleteFunc(slices.Clone(b.Reservations), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
		if len(reservations) >= b.MaxWakes {
			return a.terminalSuppressed(s, generation, "authority", "hourly_rate", "suppressed: hourly rate")
		}
		if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Retried = true; b.Reservations = append(reservations, now) }); err != nil {
			return err
		}
	}
	// Fence before fresh typing. A crash cannot create unlimited empty retries.
	if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Typed = false }); err != nil {
		return err
	}
	var outcome string
	var sendErr error
	if input == "empty" {
		if err := s.checkConnection(); err != nil {
			return terminalAt("connection", err)
		}
		outcome, sendErr = b.Target.Send(ctx, b.NoticeCount, nil, nil)

	}
	return a.finishTerminal(s, generation, outcome, sendErr, true)
}
