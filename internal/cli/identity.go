package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Stable identities (owner decision, #a2a-swarm stable-identity): an agent is
// <host>_<project>_<role>. The host slug is set once per machine (a2a host
// init), the project comes from --project or the git remote "origin" of the
// working directory, the role from --role. `a2a identity ensure` reuses the
// local credentials of that name, or registers it when the server does not
// know it, and refuses when the server has it but this machine does not.
//
// One live session per identity: wait, watch, inbox and the hooks take or
// refresh a lease in the agent's state (config.Lease, config.LeaseTTL) and
// refuse while another session holds it.

// EnvSessionID names this session for the lease (wait, watch, inbox, release).
const EnvSessionID = "A2A_SESSION_ID"

// envClaudeEnvFile is the file a Claude Code SessionStart hook may append
// `export` lines to; they reach every later Bash command of the session.
const envClaudeEnvFile = "CLAUDE_ENV_FILE"

const hostUsage = "usage: a2a host init <slug> [--force] | a2a host show | a2a host principals set|show|clear"

// cmdHost: a2a host init <slug> [--force] | a2a host show
func cmdHost(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", hostUsage)
	}
	switch args[0] {
	case "principals":
		return cmdHostPrincipals(ctx, a, args[1:])
	case "init":
		fs := newFlagSet("host init")
		force := fs.Bool("force", false, "replace a different existing slug (every identity on this machine changes)")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("%s", hostUsage)
		}
		if err := config.ValidateHost(pos[0]); err != nil {
			return usageError{err.Error()}
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		changed, err := config.SaveHost(p, pos[0], *force)
		if err != nil {
			return err
		}
		if changed {
			a.printf("host %s (%s)\n", pos[0], p.HostFile())
		} else {
			a.printf("host %s (unchanged)\n", pos[0])
		}
		return nil
	case "show":
		if len(args) != 1 {
			return usagef("%s", hostUsage)
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		slug, err := config.LoadHost(p)
		if err != nil {
			return err
		}
		a.printf("%s\n", slug)
		return nil
	}
	return usagef("%s", hostUsage)
}

const identityUsage = "usage: a2a identity ensure --role R [--project P] [--dir D] [--runtime claude|codex|none] [--keeper] [--product X --preset P] [--resource ROOM] | a2a identity release [--force]"

// cmdIdentity: a2a identity ensure|release
func cmdIdentity(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", identityUsage)
	}
	switch args[0] {
	case "ensure":
		return cmdIdentityEnsure(ctx, a, args[1:])
	case "release":
		return cmdIdentityRelease(a, args[1:])
	}
	return usagef("%s", identityUsage)
}

// cmdIdentityEnsure: a2a identity ensure --role R [--project P] [--dir D]
// [--runtime claude|codex] [--keeper]. After the identity it onboards the
// session (onboard.go): rooms, subscriptions, hooks, and a summary.
func cmdIdentityEnsure(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("identity ensure")
	authorized := fs.Bool("authorize-registration", false, "explicit owner-admitted named first registration")
	role := fs.String("role", "", "the role of this session (required): [a-z0-9-], 1-24 characters")
	project := fs.String("project", "", "the project (default: the repo name of the git remote origin of --dir)")
	dir := fs.String("dir", "", "the project directory: its git remote names the project, its hooks file is written (the current directory when omitted)")
	runtime := fs.String("runtime", "", "agent runtime whose hooks are installed: "+runtimeNames+" (detected only from an unambiguous CLAUDECODE=1)")
	keeper := fs.Bool("keeper", false, "also subscribe to every event of #system (keepers and the meta-orchestrator)")
	product := fs.String("product", "", "product name for a preset identity")
	preset := fs.String("preset", "", "explicit preset(s), comma-separated: orch, keeper, meta-keeper, arbiter, service")
	resource := fs.String("resource", "", "arbiter resource room")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("%s", identityUsage)
	}
	if *product != "" && !productNameRE.MatchString(*product) {
		return usagef("invalid product name %q: use [a-z0-9-]{1,40}", sanitizeLine(*product))
	}
	if *product != "" && *preset == "" {
		return usagef("--product requires an explicit --preset (one of: %s)", strings.Join(productPresets, ", "))
	}
	presets, err := parsePresets(*preset, *product, *resource)
	if err != nil {
		return err
	}
	if contains(presets, "service") && *runtime != "none" {
		return usagef("--preset service requires --runtime none")
	}
	// A keeper lives in its product folder, which is not a git repository
	// (owner, 2026-10-01): its project is the explicitly given product.
	if *project == "" && contains(presets, "keeper") {
		*project = *product
	}
	if *role == "" {
		return usagef("--role is required; %s", identityUsage)
	}
	if err := config.ValidatePart("role", *role, config.RoleMax); err != nil {
		return usageError{err.Error()}
	}
	if *project != "" {
		if err := config.ValidatePart("project", *project, config.ProjectMax); err != nil {
			return usageError{err.Error()}
		}
	}
	a.authorizeRegistration = *authorized
	rt, rtSource, err := a.ensureRuntime(*runtime)
	if err != nil {
		return err
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	host, err := config.LoadHost(p)
	if err != nil {
		return err
	}
	projDir := *dir
	if projDir == "" {
		projDir = "."
	}
	proj := *project
	if proj == "" {
		if proj, err = detectProject(ctx, projDir); err != nil {
			return err
		}
	}
	name, err := config.ComposeIdentity(host, proj, *role)
	if err != nil {
		return usageError{err.Error()}
	}
	// A session bound to an identity never ensures another one.
	for _, bound := range []string{a.as, a.Getenv(config.EnvAgent)} {
		if bound != "" && bound != name {
			return usagef("this session is %s; identity ensure resolved %s: never act as another identity", bound, name)
		}
	}
	a.printf("identity %s (host %s, project %s, role %s)\n", name, host, proj, *role)
	var s *session
	if _, err := os.Lstat(p.AgentFile(name)); err == nil {
		if s, err = a.sessionAs(name); err != nil {
			return err
		}
		w, err := s.client.Whoami(ctx)
		if err != nil {
			return err
		}
		if w.UserID != s.creds.UserID {
			return fmt.Errorf("%s holds %s but the homeserver says the token is %s", p.AgentFile(name), s.creds.UserID, sanitizeLine(w.UserID))
		}
		a.printf("reusing %s (%s)\n", name, w.UserID)
	} else {
		_, mx, err := a.loadMatrix()
		if err != nil {
			return err
		}
		mxid := "@" + name + ":" + mx.ServerName
		creds, path, err := a.registerAgent(ctx, p, mx, name, func(c *matrix.Client) error {
			exists, err := c.UserExists(ctx, name)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("account %s exists but this machine holds no credentials for it; recovery only through the admin (a2a admin …, or ask a2a-swarm-ac)", mxid)
			}
			return nil
		})
		if err != nil {
			return err
		}
		a.printf("registered %s (device %s)\ncredentials: %s (0600)\n", creds.UserID, creds.DeviceID, path)
		if s, err = a.sessionAs(name); err != nil {
			return err
		}
	}
	a.printf("export %s=%s\n", config.EnvAgent, name)
	if len(presets) > 0 {
		return a.onboardPreset(ctx, s, presets, *product, proj, *resource, projDir, rt, rtSource)
	}
	return a.onboard(ctx, s, onboarding{host: host, project: proj, role: *role, dir: projDir, rt: rt, rtSource: rtSource, keeper: *keeper})
}

// detectProject returns the project part from the git remote "origin" of dir.
// The URL itself is never printed: it may carry credentials.
func detectProject(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		reason := "git failed"
		if errors.Is(err, exec.ErrNotFound) {
			reason = "git is not installed"
		} else if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() > 0 {
			reason = "not a git repository, or it has no remote origin"
		}
		return "", usagef("cannot detect the project in %s (%s): pass --project P", sanitizeLine(dir), reason)
	}
	proj, ok := repoSlug(strings.TrimSpace(string(out)))
	if !ok {
		return "", usagef("the git remote origin of %s does not give a usable project name (%s): pass --project P", sanitizeLine(dir), sanitizeLine(proj))
	}
	return proj, nil
}

var notSlug = regexp.MustCompile(`[^a-z0-9-]`)

// repoSlug maps a remote URL to the repo name: the last path element, without
// .git, lowercased, every character outside [a-z0-9-] mapped to '-'.
func repoSlug(url string) (string, bool) {
	s := strings.TrimRight(url, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	s = notSlug.ReplaceAllString(strings.ToLower(s), "-")
	return s, config.ValidatePart("project", s, config.ProjectMax) == nil
}

// cmdIdentityRelease: a2a identity release [--force] clears this session's lease
// (with --force, whichever session holds it).
func cmdIdentityRelease(a *App, args []string) error {
	fs := newFlagSet("identity release")
	force := fs.Bool("force", false, "also clear a live lease of another session (only when that session is gone)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("%s", identityUsage)
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	cleared := false
	a.leaseReuse = nil
	if _, err := config.UpdateState(p, name, func(cur *config.State) error {
		var err error
		cleared, err = cur.Release(name, a.leaseHolder(p, "", time.Time{}), *force, a.Now(), a.leaseDead(p))
		return err
	}); err != nil {
		return err
	}
	if cleared && a.leaseReuse != nil {
		a.printf("released %s: %s\n", name, a.leaseReuse.note())
	} else if cleared {
		a.printf("released %s\n", name)
	} else {
		a.printf("%s has no lease\n", name)
	}
	return nil
}

// leaseHolder names this session: hook id, explicit A2A_SESSION_ID, native
// CODEX_THREAD_ID, CLAUDE_CODE_SESSION_ID, then the PID fallback. Native
// session commands can share their hooks' lease without a manual export.
// Long-lived consumers use leaseHolderFor with their own process scope.
// A zero deadline means none (as in config.UpdateStateUntil); the start-time
// read is still capped at pidBirthTimeout.
func (a *App) leaseHolder(p config.Paths, sessionID string, deadline time.Time) string {
	return a.leaseHolderFor(p, sessionID, deadline, false)
}

// leaseHolderFor keeps explicit ownership for every consumer. A watch or
// unbounded wait can outlive its launching runtime, so ambient runtime IDs
// cannot name its lease. The existing PID lifetime and acquisition rules apply.
func (a *App) leaseHolderFor(p config.Paths, sessionID string, deadline time.Time, longConsumer bool) string {
	if id := hookSessionKey(sessionID); sessionID != "" && id != "-" {
		return id
	}
	if id := hookSessionKey(a.Getenv(EnvSessionID)); a.Getenv(EnvSessionID) != "" && id != "-" {
		return id
	}
	if id := a.Getenv("CODEX_THREAD_ID"); !longConsumer && claudeSessionRE.MatchString(id) {
		return id
	}
	if !longConsumer && a.Getenv(EnvSessionID) == "" && a.Getenv("CODEX_THREAD_ID") == "" {
		if id := a.Getenv("CLAUDE_CODE_SESSION_ID"); claudeSessionRE.MatchString(id) {
			return id
		}
	}
	pid := os.Getpid()
	holder := "pid:" + strconv.Itoa(pid) + "@" + machineLabel()
	// The holder carries its own start time, so a later reuse of the PID is
	// recognised. Without a readable start time it stays in the legacy form.
	ctx, cancel := probeContext(deadline)
	defer cancel()
	if birth, err := a.pidBirth(ctx, pid); err == nil {
		holder += "#" + strconv.FormatInt(birth, 10)
	}
	return holder
}

// pidBirthTimeout caps one start-time read; callers may shorten it further.
const pidBirthTimeout = 500 * time.Millisecond

// probeContext bounds a start-time read by the caller's deadline. A zero
// deadline means "no deadline" (the inbox/wait/watch convention), never an
// already expired one; pidBirth applies its own cap either way.
func probeContext(deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), deadline)
}

// leaseReuse records the evidence for releasing a pid: holder whose PID now
// belongs to another process: the recorded and the observed start time.
type leaseReuse struct {
	Holder             string
	Recorded, Observed int64
}

func (r *leaseReuse) note() string {
	return fmt.Sprintf("pid holder %s was reused (recorded start %d, observed start %d)", r.Holder, r.Recorded, r.Observed)
}

// errPIDBirthUnsupported marks platforms where the reported start time is not
// a stable process identity.
var errPIDBirthUnsupported = errors.New("process start time is not a stable identity on this platform")

// pidBirth returns a process start time as Unix seconds. Only on Darwin is
// that value recorded at fork and left alone by later clock steps; ps runs with
// TZ=UTC and LC_ALL=C so readers with different environments agree. On Linux
// ps derives lstart from the boot time when it is read, so a clock step would
// make a live holder look reused: there the start time is reported unknown and
// pid holders keep the legacy behaviour.
func (a *App) pidBirth(ctx context.Context, pid int) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, pidBirthTimeout)
	defer cancel()
	if a.leaseBirth != nil {
		return a.leaseBirth(ctx, pid)
	}
	goos := a.leaseGOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return 0, errPIDBirthUnsupported
	}
	if pid <= 0 {
		return 0, errors.New("invalid process")
	}
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "))
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// machineLabel names this machine for the lease: the OS hostname, which does
// not change when the naming slug is set later ('a2a host init' mid-session
// must not make the same machine look like two hosts).
func machineLabel() string {
	h, _ := os.Hostname()
	return machineLabelFromHostname(h)
}

func machineLabelFromHostname(h string) string {
	if h = sanitizeLine(h); h == "" {
		h = "unknown"
	}
	return h
}

// leaseDead reports whether a "pid:<pid>@<host>[#<birth>]" holder on this
// host has exited, or whether its PID now belongs to a process with another
// start time. A holder without a start time, or a start time that cannot be
// read before the deadline, is never treated as reused. Every other holder
// counts as live until its lease expires.
func (a *App) leaseDead(p config.Paths) func(string) bool {
	return a.leaseDeadBy(p, time.Time{})
}

// leaseDeadBy is leaseDead bounded by the caller's deadline, because the
// check runs inside the state lock. A confirmed reuse is kept in a.leaseReuse.
func (a *App) leaseDeadBy(p config.Paths, deadline time.Time) func(string) bool {
	return func(holder string) bool {
		rest, ok := strings.CutPrefix(holder, "pid:")
		if !ok {
			return false
		}
		pidText, host, ok := strings.Cut(rest, "@")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 {
			return false
		}
		host, birthText, hasBirth := strings.Cut(host, "#")
		if host != machineLabel() {
			return false
		}
		gone := processGone
		if a.leaseProcessGone != nil {
			gone = a.leaseProcessGone
		}
		if gone(pid) {
			return true
		}
		if !hasBirth {
			return false
		}
		want, err := strconv.ParseInt(birthText, 10, 64)
		if err != nil {
			return false
		}
		ctx, cancel := probeContext(deadline)
		defer cancel()
		got, err := a.pidBirth(ctx, pid)
		if err != nil || got == want {
			return false
		}
		a.leaseReuse = &leaseReuse{Holder: holder, Recorded: want, Observed: got}
		return true
	}
}

// processGone reports whether no process pid exists (signal 0 is refused
// with ESRCH); a process we may not signal still exists.
func processGone(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// acquireLease takes or refreshes the lease of the session's identity for
// this session (sessionID: the hook's session id, "" elsewhere).
func (a *App) acquireLease(s *session, sessionID string, deadline time.Time) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	holder := a.leaseHolderFor(s.paths, sessionID, deadline, s.longConsumer)
	ttl := config.LeaseTTL
	if a.leaseTTL != 0 {
		ttl = a.leaseTTL
	}
	a.leaseReuse = nil
	_, err := config.UpdateStateContext(a.ctx, s.paths, s.name, deadline, func(cur *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		return cur.Acquire(s.name, holder, machineLabel(), os.Getpid(), a.Now(), ttl, a.leaseDeadBy(s.paths, deadline))
	})
	if err == nil && a.leaseReuse != nil {
		// stderr only: hook stdout carries the doorbell and must stay unchanged.
		fmt.Fprintf(a.Stderr, "a2a: lease of %s taken over: %s\n", s.name, a.leaseReuse.note())
	}
	return err
}

var claudeSessionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// exportSessionID appends `export A2A_SESSION_ID=<id>` to CLAUDE_ENV_FILE (set
// by Claude Code for SessionStart hooks), so that the session's own a2a
// inbox/wait hold the same lease as its hooks. Anything else is left alone.
func (a *App) exportSessionID(sessionID string) error {
	path := a.Getenv(envClaudeEnvFile)
	if path == "" || !claudeSessionRE.MatchString(sessionID) {
		return nil
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return err
	}
	return config.WithIdentityEffect(p, name, time.Now().Add(hookBudget), func() error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("export %s: %w", EnvSessionID, err)
		}
		_, err = fmt.Fprintf(f, "export %s=%s\n", EnvSessionID, sessionID)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("export %s: %w", EnvSessionID, err)
		}
		return nil
	})
}
