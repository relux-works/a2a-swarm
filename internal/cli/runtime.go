package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
)

const nativeRPCTimeout = 10 * time.Second

var errOwnerAbsent = errors.New("own launcher process absent")

type nativeRuntime interface {
	Read(context.Context, string, bool) (codexwake.Snapshot, error)
	TurnStatus(context.Context, string, string) (string, error)
	Reconcile(context.Context, string, string, time.Time) (string, error)
	Wake(context.Context, string, string) (string, error)
	Info() (string, string)
	Close()
}

func (a *App) dialNative(ctx context.Context, endpoint string) (nativeRuntime, error) {
	if a.nativeDial != nil {
		return a.nativeDial(ctx, endpoint)
	}
	return codexwake.Dial(ctx, endpoint)
}

// Process birth and uid prevent a recycled PID from inheriting a generation.
// This command exposes no arguments, environment, content or credentials.
func ownerBirth(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("owner PID required")
	}
	raw, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "uid=").Output()
	fields := strings.Fields(string(raw))
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ExitCode() == 1 && len(fields) == 0 && ctx.Err() == nil {
		return "", errOwnerAbsent
	}
	if err != nil || len(fields) != 6 || fields[5] != strconv.Itoa(os.Getuid()) {
		return "", errors.New("own launcher process unavailable")
	}
	return strings.Join(fields, " "), nil
}

func sameRuntime(a, b codexwake.Binding) bool {
	return a.Identity == b.Identity && a.ThreadID == b.ThreadID && a.SessionID == b.SessionID && a.Endpoint == b.Endpoint && a.EndpointID == b.EndpointID && a.ServerVersion == b.ServerVersion && a.Host == b.Host && a.Cwd == b.Cwd && a.OwnerPID == b.OwnerPID && a.OwnerStart == b.OwnerStart
}

func cmdRuntime(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("runtime requires bind, dispatch, gate, abandon or status")
	}
	if args[0] == "bind" {
		return cmdRuntimeBind(ctx, a, args[1:])
	}
	fs := newFlagSet("runtime " + args[0])
	generation := fs.String("generation", "", "exact launcher generation")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if args[0] == "status" {
		if len(pos) != 0 {
			return usagef("runtime status takes no positional arguments")
		}
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.Runtime == nil {
			a.printf("%s unavailable: unbound\n", s.name)
			return nil
		}
		a.printf("%s generation=%s gate=%s wake=%s notification=%s\n", s.name, st.Runtime.Generation, st.Runtime.Gate, sanitizeLine(st.Wake.Outcome), notificationDiagnostic(st.Notification, a.Now()).State)
		return nil
	}
	if *generation == "" {
		return usagef("runtime operation requires --generation")
	}
	switch args[0] {
	case "abandon":
		if len(pos) != 0 {
			return usagef("runtime abandon takes no positional arguments")
		}
		return config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(100*time.Millisecond), func() error {
			return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					if st.Runtime == nil || st.Runtime.Generation != *generation || st.Runtime.Identity != s.name || st.Runtime.Host != machineLabel() {
						return errors.New("runtime stale generation")
					}
					probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
					defer cancel()
					birth, err := ownerBirth(probe, st.Runtime.OwnerPID)
					if probe.Err() != nil || (err != nil && !errors.Is(err, errOwnerAbsent)) {
						return errors.New("runtime owner observation unavailable")
					}
					if err == nil && birth == st.Runtime.OwnerStart {
						return errors.New("runtime abandon refused: owning launcher is still live")
					}
					if st.Runtime.Gate != "stopping" || st.Wake.Attempt == nil || st.Wake.Attempt.Generation != *generation {
						return errors.New("runtime abandon requires stopping generation and its uncertain attempt")
					}
					for _, id := range st.Wake.Attempt.IDs {
						if !slices.Contains(st.Wake.Rung, id) {
							st.Wake.Rung = append(st.Wake.Rung, id)
						}
					}
					st.Wake.Attempt = nil
					st.Runtime.Retired = true
					st.Wake.Outcome = "wake abandoned: retired generation; retained mail needs inbox"
					return nil
				})
				if err == nil {
					a.printf("%s uncertain generation retired; retained entries require inbox, no automatic replay\n", s.name)
				}
				return err
			})
		})
	case "gate":
		if len(pos) != 1 {
			return usagef("runtime gate requires ready, paused, budget, stopping or unknown")
		}
		switch pos[0] {
		case "ready", "paused", "budget", "stopping", "unknown":
		default:
			return usagef("invalid runtime gate")
		}
		return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				if st.Runtime == nil || st.Runtime.Generation != *generation || st.Runtime.Identity != s.name || st.Runtime.Host != machineLabel() {
					return errors.New("runtime stale generation")
				}
				if st.Runtime.Retired {
					return errors.New("runtime retired generation")
				}
				st.Runtime.Gate = pos[0]
				if pos[0] == "ready" && (st.Wake.LastTurnStatus == "failed" || st.Wake.LastTurnStatus == "interrupted") {
					st.Wake.Rung = slices.DeleteFunc(st.Wake.Rung, func(id string) bool { return slices.Contains(st.Wake.LastMailIDs, id) })
					st.Wake.LastTurn = ""
					st.Wake.LastTurnStatus = ""
					st.Wake.LastMailIDs = nil
				}
				st.Wake.Outcome = "wake suppressed: " + pos[0]
				return nil
			})
			if err != nil {
				return err
			}
			a.printf("%s gate=%s generation=%s\n", s.name, pos[0], *generation)
			return nil
		})
	case "dispatch":
		if len(pos) != 0 {
			return usagef("runtime dispatch takes no positional arguments")
		}
		return config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(100*time.Millisecond), func() error { return a.dispatchNative(ctx, s, *generation) })
	default:
		return usagef("unknown runtime operation")
	}
}

func cmdRuntimeBind(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("runtime bind")
	thread := fs.String("codex-thread", "", "own loaded thread")
	endpoint := fs.String("endpoint", "", "own managed Unix endpoint")
	pid := fs.Int("owner-pid", 0, "live owning launcher/TUI PID")
	heartbeat := fs.Duration("heartbeat", 0, "explicit consumer heartbeat interval")
	rate := fs.Int("max-wakes-per-hour", 0, "explicit per-identity wake turn limit")
	gate := fs.String("gate", "", "ready, paused, budget, stopping or unknown")
	hooks := fs.String("hooks", "", "installed or none")
	product := fs.String("product", "", "self-recorded product scope")
	watch := fs.Bool("watch", false, "run the sole consumer under launcher supervision")
	dir := fs.String("dir", "", "own thread working directory (default current directory)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *thread == "" || *endpoint == "" || *hooks == "" || *pid <= 0 || *heartbeat <= 0 || *rate <= 0 || *gate == "" {
		return usagef("runtime bind requires own --codex-thread, --endpoint, --owner-pid, --heartbeat, --max-wakes-per-hour, --gate and --hooks")
	}
	if own := a.Getenv("CODEX_THREAD_ID"); own != "" && own != *thread {
		return usagef("runtime bind refuses a different live Codex thread")
	}
	if own := a.Getenv(EnvSessionID); own != "" && own != *thread {
		return usagef("runtime bind refuses a different live session")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := refuseHostConsumer(s); err != nil {
		return err
	}
	cwd := *dir
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	profile := config.AgentStatus{State: "idle", HeartbeatEvery: heartbeat.String(), Runtime: "codex", Hooks: *hooks, Doorbell: "none", Product: *product}
	if err := statusValid(profile); err != nil {
		return usagef("%s", err)
	}
	switch *gate {
	case "ready", "paused", "budget", "stopping", "unknown":
	default:
		return usagef("invalid runtime gate")
	}
	endpointPath, err := codexwake.EndpointPath(*endpoint)
	if err != nil {
		return usagef("%s", err)
	}
	// Managed daemons publish an owned symlink to a private concrete socket.
	// Resolve it once at registration, then pin the concrete object; following
	// a later alias change must never move an existing launcher generation.
	concrete, err := filepath.EvalSymlinks(endpointPath)
	if err != nil {
		return errors.New("runtime endpoint unavailable")
	}
	*endpoint = "unix://" + concrete
	bindAndRun := func() error {
		var binding codexwake.Binding
		var preserve *config.AgentStatus
		preserveSource := "runtime"
		err = config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
			state, err := config.LoadState(s.paths, s.name)
			if err != nil {
				return err
			}
			if state.HostEnrollment != nil {
				return config.ErrHostEnrollment
			}
			probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			defer cancel()
			birth, err := ownerBirth(probe, *pid)
			if err != nil {
				return err
			}
			client, err := a.dialNative(probe, *endpoint)
			if err != nil {
				return err
			}
			defer client.Close()
			remote, err := client.Read(probe, *thread, false)
			if err != nil {
				return err
			}
			if remote.ID != *thread || filepath.Clean(remote.Cwd) != cwd || (remote.Status.Type != "idle" && remote.Status.Type != "active") {
				return errors.New("runtime binding requires its own loaded thread and exact working directory")
			}
			version, socket := client.Info()
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return err
			}
			binding = codexwake.Binding{Identity: s.name, ThreadID: *thread, SessionID: *thread, Endpoint: *endpoint, EndpointID: socket, ServerVersion: version, Generation: hex.EncodeToString(random[:]), Host: machineLabel(), Cwd: cwd, OwnerPID: *pid, OwnerStart: birth, Gate: *gate, MaxPerHour: *rate, HeartbeatEvery: *heartbeat}
			if err := binding.Validate(); err != nil {
				return err
			}
			old, err := config.LoadState(s.paths, s.name)
			if err != nil {
				return err
			}
			if old.Runtime != nil && !sameRuntime(*old.Runtime, binding) {
				oldBirth, ownerErr := ownerBirth(probe, old.Runtime.OwnerPID)
				if probe.Err() != nil || (ownerErr != nil && !errors.Is(ownerErr, errOwnerAbsent)) {
					return errors.New("runtime replacement refused: previous owner observation unavailable")
				}
				if old.Runtime.Gate != "stopping" || oldBirth == old.Runtime.OwnerStart || old.Wake.Attempt != nil {
					return errors.New("runtime replacement refused: close previous generation, stop its owner and reconcile any uncertain attempt first")
				}
			}
			if old.Runtime != nil && sameRuntime(*old.Runtime, binding) && old.Liveness != nil {
				copy := *old.Liveness
				preserve = &copy
				preserveSource = old.LivenessSource
			}
			if err := a.acquireLease(s, *thread, time.Now().Add(nativeRPCTimeout)); err != nil {
				return err
			}
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				if st.Runtime != nil && sameRuntime(*st.Runtime, binding) {
					binding.Generation = st.Runtime.Generation
				} else {
					st.Wake = codexwake.Ledger{Rung: slices.Clone(st.Wake.Rung), Reservations: slices.Clone(st.Wake.Reservations)}
					st.Wake.Outcome = "runtime bound: consumer not armed"
				}
				st.Runtime = &binding
				// Persist recovery before attempting HTTP, including a crash or
				// deadline with no previously acknowledged liveness record.
				st.RuntimeProfilePending = &config.RuntimeProfilePending{Generation: binding.Generation, Profile: profile}
				return nil
			})
			return err
		})
		if err != nil {
			return err
		}
		if preserve != nil {
			profile.State = preserve.State
			profile.Detail = preserve.Detail
			profile.Doorbell = preserve.Doorbell
		}
		budget := nativeRPCTimeout
		if a.runtimeBindStatusBudget > 0 {
			budget = a.runtimeBindStatusBudget
		}
		publishCtx, cancel := context.WithTimeout(ctx, budget)
		err = s.retryLivenessSource(publishCtx, profile, preserveSource)
		cancel()
		if errors.Is(err, ErrOwnStatusPending) {
			fmt.Fprintln(a.Stderr, "liveness not yet published; the first heartbeat republishes it")
		} else if err != nil {
			return err
		}
		a.printf("%s bound generation=%s session=%s; launcher must export A2A_SESSION_ID=%s\n", s.name, binding.Generation, binding.SessionID, binding.SessionID)
		if !*watch {
			return nil
		}
		return a.runNative(ctx, s, binding.Generation)
	}
	if *watch {
		return config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(100*time.Millisecond), bindAndRun)
	}
	return bindAndRun()
}

func manualWakeGate(st config.State) string {
	if st.Liveness == nil || st.LivenessSource == "runtime" || st.LivenessSource == "hook" || st.LivenessSource == "wait" {
		return ""
	}
	switch st.Liveness.State {
	case "waiting-human":
		return "wake suppressed: human"
	case "blocked":
		return "wake suppressed: blocked"
	case "stopping":
		return "wake suppressed: stopping"
	}
	return ""
}

func (a *App) runtimeOutcome(s *session, generation, outcome string) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		if st.Runtime == nil || st.Runtime.Generation != generation {
			return errors.New("runtime stale generation")
		}
		if st.Wake.Outcome != outcome {
			a.printf("%s %s\n", s.name, outcome)
		}
		st.Wake.Outcome = outcome
		return nil
	})
	return err
}

func (a *App) dispatchNative(ctx context.Context, s *session, generation string) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		defer cancel()
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.Runtime == nil || st.Runtime.Generation != generation || st.Runtime.Identity != s.name {
			return errors.New("runtime stale generation")
		}
		b := *st.Runtime
		if b.Retired {
			return a.runtimeOutcome(s, generation, "unavailable: retired generation")
		}
		if err := a.acquireLease(s, b.SessionID, time.Now().Add(nativeRPCTimeout)); err != nil {
			return err
		}
		birth, _ := ownerBirth(probe, b.OwnerPID)
		ownerAlive := birth == b.OwnerStart
		if b.Host != machineLabel() || (!ownerAlive && st.Wake.Attempt == nil) {
			return a.runtimeOutcome(s, generation, "unavailable: launcher")
		}
		socket, err := codexwake.SocketIdentity(b.Endpoint)
		if err != nil || socket != b.EndpointID {
			return a.runtimeOutcome(s, generation, "unavailable: endpoint replaced")
		}
		client, err := a.dialNative(probe, b.Endpoint)
		if err != nil {
			fmt.Fprintln(a.Stderr, "native connection probe:", a.secrets.Scrub(sanitizeLine(err.Error())))
			return a.runtimeOutcome(s, generation, "unavailable: native capability or transport")
		}
		defer client.Close()
		version, socket := client.Info()
		if version != b.ServerVersion || socket != b.EndpointID {
			return a.runtimeOutcome(s, generation, "unavailable: host replaced")
		}
		remote, err := client.Read(probe, b.ThreadID, false)
		if err != nil {
			fmt.Fprintln(a.Stderr, "native thread probe:", a.secrets.Scrub(sanitizeLine(err.Error())))
			return a.runtimeOutcome(s, generation, "unavailable: native thread")
		}
		if remote.ID != b.ThreadID || filepath.Clean(remote.Cwd) != b.Cwd {
			return a.runtimeOutcome(s, generation, "unavailable: thread ownership")
		}
		if st.Wake.Attempt != nil {
			if st.Wake.Attempt.Generation != generation {
				return a.runtimeOutcome(s, generation, "wake uncertain: stale attempt generation")
			}
			turn, reconcileErr := client.Reconcile(probe, b.ThreadID, st.Wake.Attempt.Output, st.Wake.Attempt.Started)
			if reconcileErr != nil {
				fmt.Fprintln(a.Stderr, "native acceptance probe:", a.secrets.Scrub(sanitizeLine(reconcileErr.Error())))
				return a.runtimeOutcome(s, generation, "wake uncertain: acceptance history unavailable; no retry")
			}
			if turn != "" {
				_, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
					token := ""
					if cur.Wake.Attempt != nil {
						token = cur.Wake.Attempt.NotificationToken
					}
					if err := cur.Wake.Accept(b, turn); err != nil {
						return err
					}
					return a.completeNotification(s, cur, token)
				})
				return err
			}
			return a.runtimeOutcome(s, generation, "wake uncertain: no authoritative acceptance; no retry")
		}
		admission := codexwake.Admission(b, machineLabel(), ownerAlive, st.HooksPaused, remote)
		if manual := manualWakeGate(st); manual != "" {
			admission = manual
		}
		if admission != "ready" {
			return a.runtimeOutcome(s, generation, admission)
		}
		if st.Wake.LastTurn != "" {
			status, err := client.TurnStatus(probe, b.ThreadID, st.Wake.LastTurn)
			if err != nil {
				return a.runtimeOutcome(s, generation, "unavailable: accepted turn state")
			}
			_, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error { cur.Wake.LastTurnStatus = status; return nil })
			if err != nil {
				return err
			}
			switch status {
			case "failed", "interrupted":
				return a.runtimeOutcome(s, generation, "wake "+status+": owner gate required")
			case "inProgress":
				return a.runtimeOutcome(s, generation, "wake suppressed: busy")
			case "completed":
			default:
				return a.runtimeOutcome(s, generation, "unavailable: accepted turn lifecycle")
			}
		}
		var items []waitItem
		_, err = s.lockedDeliver(ctx, time.Now().Add(nativeRPCTimeout), false, func(cur *config.State, mb *config.Mailbox) ([]string, error) {
			for _, entry := range mb.Pending() {
				if !slices.Contains(cur.Wake.Rung, entry.ID) && len(items) < 50 {
					items = append(items, itemOf(entry, s.server))
				}
			}
			return nil, nil
		})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return a.runtimeOutcome(s, generation, "runtime ready: no new mail")
		}
		notice := doorbellText(doorbellOf(items, s.server, s.verifySenders(probe, items)))
		ids := itemIDs(items)
		var attempt *codexwake.Attempt
		_, err = a.reserveNotification(ctx, s, time.Now().Add(nativeRPCTimeout), "native", b.SessionID, generation, false, func(cur *config.State, mb *config.Mailbox, token string) (bool, error) {
			if cur.Runtime == nil || cur.Runtime.Generation != generation {
				return false, errors.New("runtime stale generation")
			}
			if cur.HooksPaused {
				cur.Wake.Outcome = "wake suppressed: paused"
				return false, nil
			}
			if manual := manualWakeGate(*cur); manual != "" {
				cur.Wake.Outcome = manual
				return false, nil
			}
			var current []string
			for _, entry := range mb.Pending() {
				current = append(current, entry.ID)
			}
			cur.Wake.PruneRung(current)
			ids = slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(current, id) })
			attempt, err = cur.Wake.Begin(b, ids, notice, a.Now().UTC())
			if errors.Is(err, codexwake.ErrRate) {
				return false, nil
			}
			if attempt != nil {
				attempt.NotificationToken = token
			}
			return attempt != nil, err
		})
		if err != nil {
			return err
		}
		if attempt == nil {
			current, loadErr := config.LoadState(s.paths, s.name)
			if loadErr != nil {
				return loadErr
			}
			if current.Wake.Outcome != st.Wake.Outcome {
				a.printf("%s %s\n", s.name, current.Wake.Outcome)
			}
			return nil
		}
		if a.afterWakeReserve != nil {
			if err := a.afterWakeReserve(); err != nil {
				return err
			}
		}
		if err := s.checkConnection(); err != nil {
			return err
		}
		turn, err := client.Wake(probe, b.ThreadID, attempt.Output)
		if err != nil {
			if codexwake.DefinitiveRefusal(err) {
				_, updateErr := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
					if cur.Runtime == nil || cur.Runtime.Generation != generation || cur.Wake.Attempt == nil || cur.Wake.Attempt.ID != attempt.ID {
						return errors.New("runtime stale refusal")
					}
					if r := cur.Notification.Reservation; r != nil && r.Token == attempt.NotificationToken {
						if err := cur.Notification.Cancel(r.Token); err != nil {
							return err
						}
					}
					cur.Wake.Attempt = nil
					cur.Wake.Outcome = "wake refused: not accepted; hourly reservation retained"
					return nil
				})
				fmt.Fprintln(a.Stderr, a.secrets.Scrub(sanitizeLine(err.Error())))
				if updateErr == nil {
					a.printf("%s wake refused: not accepted; hourly reservation retained\n", s.name)
				}
				return updateErr
			}
			return a.runtimeOutcome(s, generation, "wake uncertain: transport result; reconcile before retry")
		}
		if a.afterWakeSubmit != nil {
			if err := a.afterWakeSubmit(); err != nil {
				return err
			}
		}
		_, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			if cur.Runtime == nil || cur.Runtime.Generation != generation || cur.Wake.Attempt == nil || cur.Wake.Attempt.ID != attempt.ID {
				return errors.New("runtime stale submission")
			}
			token := ""
			if cur.Wake.Attempt != nil {
				token = cur.Wake.Attempt.NotificationToken
			}
			if err := cur.Wake.Accept(b, turn); err != nil {
				return err
			}
			return a.completeNotification(s, cur, token)
		})
		if err == nil {
			a.printf("%s wake accepted\n", s.name)
		}
		return err
	})
}

func (a *App) runNative(ctx context.Context, s *session, generation string) error {
	initial, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if initial.Runtime == nil || initial.Runtime.Generation != generation {
		return errors.New("runtime stale generation")
	}
	if err := a.acquireLease(s, initial.Runtime.SessionID, time.Now().Add(nativeRPCTimeout)); err != nil {
		return err
	}
	st, _, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		st, err = s.takePosition(ctx, time.Now().Add(nativeRPCTimeout))
		if err != nil {
			return err
		}
	}
	if err := a.nativeConsumer(ctx, s, generation, true); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), nativeRPCTimeout)
		defer cancel()
		if err := a.nativeConsumer(cleanup, s, generation, false); err != nil {
			fmt.Fprintln(a.Stderr, "native consumer ownership cleanup unavailable")
		}
	}()
	a.printf("%s native consumer armed generation=%s\n", s.name, generation)
	var lastHeartbeat time.Time
	var lastOutcome string
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), nativeRPCTimeout)
		defer cancel()
		current, err := config.LoadState(s.paths, s.name)
		if err == nil && current.Runtime != nil && current.Runtime.Generation == generation && current.Liveness != nil {
			profile := *current.Liveness
			profile.Doorbell = "none"
			if err := s.retryLivenessSource(stopCtx, profile, current.LivenessSource); err != nil {
				fmt.Fprintln(a.Stderr, "native consumer disarm:", a.secrets.Scrub(sanitizeLine(err.Error())))
			}
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.dispatchNative(ctx, s, generation); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		st, err = config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.Runtime == nil || st.Runtime.Generation != generation {
			return errors.New("runtime stale generation")
		}
		birth, _ := ownerBirth(ctx, st.Runtime.OwnerPID)
		if birth != st.Runtime.OwnerStart {
			return errors.New("runtime owning launcher exited")
		}
		pending := st.RuntimeProfilePending
		hasPending := pending != nil && pending.Generation == generation
		if (hasPending || st.Liveness != nil) && (hasPending || lastHeartbeat.IsZero() || a.Now().Sub(lastHeartbeat) >= st.Runtime.HeartbeatEvery || lastOutcome != st.Wake.Outcome) {
			var profile config.AgentStatus
			if hasPending {
				profile = pending.Profile
				if st.Liveness != nil {
					profile.State, profile.Detail = st.Liveness.State, st.Liveness.Detail
				}
			} else {
				profile = *st.Liveness
			}
			profile.Doorbell = "armed"
			source := st.LivenessSource
			if manualWakeGate(st) == "" {
				source = "runtime"
				switch st.Wake.Outcome {
				case "runtime ready: no new mail":
					profile.State = "idle"
				case "wake accepted", "wake suppressed: busy":
					profile.State = "working"
				case "wake suppressed: human":
					profile.State = "waiting-human"
				case "wake suppressed: stopping":
					profile.State = "stopping"
				default:
					profile.State = "blocked"
				}
			}
			if err := s.writeLivenessSource(ctx, profile, source); err != nil {
				if !errors.Is(err, ErrOwnStatusPending) {
					return err
				}
			} else {
				lastHeartbeat = a.Now()
				lastOutcome = st.Wake.Outcome
			}
		}
		// A cooldown can expire during probes or heartbeat publication after
		// dispatch refused it. Recheck admission before starting a long poll;
		// another gate changes the outcome and resumes its ordinary polling.
		if st.Wake.Outcome == "wake suppressed: notification cooldown" && a.notificationReady(s, st.Notification, a.Now()) {
			continue
		}
		poll := a.notificationWait(st, min(maxPoll, st.Runtime.HeartbeatEvery))
		s.principals = s.readDeliveryPrincipals()
		resp, err := s.client.Sync(ctx, st.NextBatch, poll, syncFilter)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		subs, err := s.getSubs(ctx)
		if err != nil {
			return err
		}
		_, err = s.ingest(ctx, resp, st.NextBatch, subs.Subs, nil, time.Now().Add(nativeRPCTimeout))
		if err != nil {
			return err
		}
	}
}
