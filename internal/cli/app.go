// Package cli implements the a2a command line: every agent in the swarm uses it
// to talk over the local Matrix homeserver.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/redact"
	"github.com/relux-works/a2a-swarm/internal/voice"
)

// Exit codes.
const (
	ExitOK     = 0
	ExitError  = 1
	ExitUsage  = 2
	ExitConfig = 64
)

// App carries the process environment so tests can run commands in-process.
type App struct {
	courierEngine            bool
	courierFiles             map[string]courier.InputFile
	Stdout                   io.Writer
	Stderr                   io.Writer
	Stdin                    io.Reader
	Getenv                   func(string) string
	HTTP                     *http.Client
	Now                      func() time.Time
	notification             *notificationClock
	afterNotificationReserve func() error

	voiceRenderer voice.Renderer // test seam; production is strictly local

	// as is the identity from --as (overrides A2A_AGENT).
	as                    string
	profile               string
	connectionFingerprint string
	connection            *config.Connection
	connectionResolved    bool
	connectionError       error
	authorizeRegistration bool
	beforeNamedPublish    func() error
	connectionSecretStore connectionSecretStore
	// human selects a human operator's own account (humans/<name>.json); never
	// set by agents. It needs --as and ignores A2A_AGENT.
	human   bool
	secrets redact.Set

	// keychain is a package-test-only factory. Production stays pinned to security.
	keychain              func(config.Paths) *keychainStore
	bindStartupWait       func(context.Context, time.Duration) error // deterministic startup retry seam
	beforeCredentialScrub func() error
	ctx                   context.Context

	// afterPrint, when set (tests only), runs after wait, watch, inbox or a hook
	// printed and before it records that (shown, receipt, rung set): an error
	// simulates a crash there.
	afterPrint func() error
	// afterOwnStatusPending observes local contention after releasing the state
	// lock (tests only), so fixtures can clear a seeded fence deterministically.
	afterOwnStatusPending func()
	// runtimeBindStatusBudget shortens only the one-shot bind publication in tests.
	runtimeBindStatusBudget time.Duration
	// beforeDeliver, when set (tests only), runs after a consumer synced and
	// before it takes the state lock to persist and print: tests use it to let
	// a concurrent consumer win the race.
	beforeDeliver func()
	// hookBudget, when set (tests only), replaces the hook budget.
	hookBudget time.Duration
	// hookSelection replaces executable selection in installer merge tests only.
	hookSelection func(hookExecutableChoice) (hookExecutableSelection, error)
	// stdinTTY reports whether stdin is an interactive terminal, which --human
	// requires (nil: stdinIsTerminal on os.Stdin; tests override it).
	stdinTTY func() bool
	// beforeAppend and afterAppend, when set (tests only), run right before and
	// right after a sync batch is appended to the mailbox (before the cursor
	// moves): an error simulates a crash there.
	beforeAppend, afterAppend func() error
	// backfillPages, when set (tests only), replaces maxBackfillPages.
	backfillPages int
	// leaseTTL, when set (tests only), replaces config.LeaseTTL.
	leaseTTL time.Duration
	// leaseBirth reads a process start time for pid: lease holders; tests only replace it.
	leaseBirth func(ctx context.Context, pid int) (int64, error)
	// leaseProcessGone replaces the PID liveness probe in synthetic lease tests.
	leaseProcessGone func(int) bool
	// leaseReuse is the last pid: holder released because its PID was reused.
	leaseReuse *leaseReuse
	// leaseGOOS overrides runtime.GOOS for the start-time reader; tests only.
	leaseGOOS string
	// Owner terminal/deadline seams are tests only; production uses /dev/tty.
	ownerConfirm      func(string) error
	ownerMirrorBudget time.Duration
	// Native transport/fault hooks are test-only seams.
	postOperationFault                func(string) error
	postOperationOS                   string
	postOperationLateReserve          bool
	nativeDial                        func(context.Context, string) (nativeRuntime, error)
	afterWakeReserve, afterWakeSubmit func() error
}

// NewApp returns an App bound to the real process environment.
func NewApp() *App {
	return &App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Getenv: os.Getenv,
		HTTP:   &http.Client{},
		Now:    time.Now,
	}
}

// usageError marks errors that should exit with ExitUsage.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, a *App, args []string) error
}

func commands() []command {
	return []command{
		{"courier", "courier serve|register-station|unregister-station|enroll   local Courier and station enrollment", cmdCourier},
		{"connection", "connection list|show NAME|add NAME --file FILE|use NAME|register NAME --via-admin-room   named Matrix routing", cmdConnection},
		{"voice", "voice status [--json] | voice config --enable | voice install   optional local voice replies (180s cap)", cmdVoice},
		{"mail-alarm", "mail-alarm set|unset|status   per-identity stale addressed mailbox alarm", cmdMailAlarm},
		{"owner-dm", "owner-dm bind|unbind --event ROOM EVENT | ensure|set|unset|status   per-identity owner channel (default off)", cmdOwnerDM},
		{"register", "register <name>                      create a Matrix account and a 0600 credential file (low level: use identity ensure)", cmdRegister},
		{"availability", "availability <localpart|@localpart:server> [--json]   read selected-server registration availability (no existence guarantee)", cmdAvailability},
		{"host", "host init <slug> [--force] | host show | host principals set|show|clear   this machine's host slug, the first part of every identity", cmdHost},
		{"identity", "identity ensure --role R [--project P] [--runtime R] [--product X --preset P] [--resource ROOM] | identity release   onboard a stable or preset identity; its lease", cmdIdentity},
		{"whoami", "whoami                               show the current identity", cmdWhoami},
		{"rooms", "rooms                                list joined rooms (alias, name, id)", cmdRooms},
		{"room", "room new <alias> [--name N] [--topic T] [--invite a,b] [--product X] | room retire <alias> | room members <room> [--membership join|invite|join,invite] [--json]   create, retire an EMPTY room, or read current members", cmdRoom},
		{"product", "product new X [--keeper NAME] [--projector NAME] | adopt X <room> | list | show X   manage product spaces and rooms", cmdProduct},
		{"broadcast-room", "broadcast-room init   create the global #broadcast room", cmdBroadcastRoom},
		{"join", "join <room>                          join a room", cmdJoin},
		{"leave", "leave <room> [--force]               leave a room (#lobby only with --force)", cmdLeave},
		{"invite", "invite <room> <@user|name>           invite someone to a room", cmdInvite},
		{"topic", "topic new <room> <slug> \"<title>\" [--body -|@file|text] [--to a,b] [--kind K]   start a topic thread", cmdTopic},
		{"topics", "topics <room>                        list topic threads (slug, event id, replies)", cmdTopics},
		{"post-operation", "post-operation status <id> --json   local retained evidence; never resend", cmdPostOperation},
		{"post", "post <room> [--topic <slug|$event>] [--kind K] [--re $ev] [--to a,b] [--reply-by T] [--trunk OID] [--voice] <text|@file|->", cmdPost},
		{"attach", "attach <room> --topic <slug|$event> <file> [--kind K] [--to a,b] [--pin] [caption|@file|-]   share a file", cmdAttach},
		{"fetch", "fetch <room> <$event> [-o path] | fetch <mxc://...> --sha256 <hex> [-o path]   download a file (native hash is unverified)", cmdFetch},
		{"files", "files <room> [--topic X] [--last N] [--json]   list shared files, oldest first", cmdFiles},
		{"pin", "pin <room> <$event>                  pin an event (m.room.pinned_events)", cmdPin},
		{"unpin", "unpin <room> <$event>                unpin an event", cmdUnpin},
		{"redact", "redact <room> <event> --reason TEXT [--txn-id ID]   best-effort server event redaction", cmdRedact},
		{"read", "read <room> [--topic X] [--last N] [--event $ev] [--json] [--verification-input|--envelope-input]   print messages or a strict single-event observation", cmdRead},
		{"archive", "archive <room|all> [--dir archive] [--json-index]   deterministic markdown export of room history", cmdArchive},
		{"wait", "wait [--room R ...] [--all] [--match-text REGEX] [--from A,B] [--kind k1,k2] [--to-me-only] [--timeout 25m|0] [--json]   block for selected new mailbox entries (acknowledges nothing)", cmdWait},
		{"watch", "watch [--room R ...] [--all] [--match-text REGEX] [--from A,B] [--kind k1,k2] [--to-me-only] [--json]   stream selected new mailbox entries (acknowledges nothing)", cmdWatch},
		{"inbox", "inbox [--limit N] [--json]           print unreceived mailbox entries inside an untrusted-data frame, then receipt exactly them (N <= 50)", cmdInbox},
		{"hook", "hook stop|session-start|prompt --runtime R | hook install --agent NAME --runtime R [--settings PATH] [--executable PATH | --installed-command PATH] | hook uninstall --runtime R [--settings PATH] | hook pause|resume --agent NAME   Claude Code (R=claude), Codex (R=codex) and Muse (R=muse) hooks: a doorbell (counts only) in the session", cmdHook},
		{"ack", "ack <id...>|all-shown               receipt mailbox entries (all-shown: every entry wait/watch printed)", cmdAck},
		{"mute", "mute <room> --topic X | mute list [--json]   suppress topic delivery or list local mutes (to/mentions/from still ring)", cmdMute},
		{"follow", "follow <room> --topic X               ring on replies in a thread again", cmdFollow},
		{"subscribe", "subscribe [--room R|all] [--topic X] [--from A] [--subject X] [--subtype S] [--kind k1,k2] [--until T] [--until-kind K] [--since $ev] [--all-events]   add a runtime subscription (muted topics need follow; --from still matches)", cmdSubscribe},
		{"unsubscribe", "unsubscribe <sub-id>|--all             remove subscriptions", cmdUnsubscribe},
		{"subs", "subs [--json] | subs check [--json] | subs trim --room R (--drop K --reason TEXT | --clear) | subs sync [--preset P --product X]", cmdSubs},
		{"subscriptions", "subscriptions [--json]               alias of subs", cmdSubs},
		{"sub-request", "sub-request <agent> --room R --topic T <selector flags> [--note text]   ask an agent to subscribe (rule 10)", cmdSubRequest},
		{"sub-accept", "sub-accept <room> <$event>           accept a subscription request addressed to me", cmdSubAccept},
		{"project", "project --system R --source R [--source R...] [--kind k1,k2] [--once]   project allowed sources to one explicit target", cmdProject},
		{"skill", "skill install [--force] | skill print   install the a2a agent skill into ~/.agents, ~/.claude, ~/.codex", cmdSkill},
		{"creds", "creds migrate | move-human <name>   migrate selected credentials to Keychain, or move HUMAN metadata into humans/", cmdCreds},
		{"card", "card set --file card.md | card get <name>   agent card in #lobby", cmdCard},
		{"status", "status init | set STATE TEXT --heartbeat DUR --runtime R --hooks H --doorbell D [--product X] | get [name] (legacy presence)", cmdStatus},
		{"who", "who --stale-after DUR [--product X] [--json]   read self-written agent liveness", cmdWho},
		{"runtime", "runtime bind --codex-thread ID --endpoint unix:///SOCK --owner-pid PID --heartbeat DUR --max-wakes-per-hour N --gate G --hooks H [--product X] [--watch] | dispatch|gate|abandon|status   launcher-owned native Codex wake", cmdRuntime},
		{"bind", "bind association --private-intent-fd=3 --json | bind --runtime R --owner-pid PID [--interval DUR --retry-max DUR --max-wakes-per-hour N] [--session-id ID]   stored association or persistent counts-only wake", cmdBind},
		{"unbind", "unbind                               stop this session's persistent wake process", cmdUnbind},
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintln(w, "usage: a2a [--profile <connection>] [--as <name>] <command> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Identity: --as <name> or A2A_AGENT; credentials in ~/.config/a2a-swarm/agents/<name>.json (A2A_CONFIG_DIR overrides the base dir).")
	fmt.Fprintln(w, "Rooms: #alias, alias, #alias:server, or !roomid.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	for _, c := range commands() {
		fmt.Fprintln(w, "  "+c.summary)
	}
}

// Run executes one a2a invocation and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	// This exact hidden probe must bypass connection preflight and owner-DM
	// diagnostics: neither state nor credentials may be consulted by negotiation.
	if len(args) == 2 && args[0] == "bind" && args[1] == "--consumer-protocol" {
		return a.fail(cmdBind(ctx, a, args[1:]))
	}
	a.ctx = ctx
	a.connectionResolved = false
	a.connection = nil
	a.connectionError = nil
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{}
	}
	command := stationCommand(args)
	if command != "fetch" && command != "courier" && command != "help" && command != "--help" && command != "-h" && command != "" && !a.courierEngine {
		if handled, code := a.stationInvocation(ctx, args); handled {
			return code
		}
	}
	globalFailure := func(err error) int {
		if rest := associationInvocation(args); rest != nil {
			// Global guards still refuse, through the association's closed output.
			return a.bindAssociation(append(rest, "--invalid-global-selection"))
		}
		return a.fail(err)
	}
	if err := verificationGlobalSelection(args); err != nil {
		return globalFailure(err)
	}
	rest, err := a.extractGlobal(args)
	if err != nil {
		return globalFailure(err)
	}
	if len(rest) > 0 && rest[0] == "fetch" {
		// Fetch help and flag errors have always been environment-free. Parse
		// them before discovery; an executable fetch still reaches Courier's
		// closed command allowlist before credentials or filesystem effects.
		if _, err := parseFetchFlags(a, rest[1:]); err != nil {
			return a.fail(err)
		}
	}
	if command == "fetch" && !a.courierEngine {
		if handled, code := a.stationInvocation(ctx, args); handled {
			return code
		}
	}

	if a.human && !a.interactive() {
		// Agents' tool calls have no terminal on stdin, so an agent cannot use a
		// human's account even when it knows the flag.
		return globalFailure(usagef("--human needs an interactive terminal on stdin: only a human at a terminal uses a human account"))
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		if len(rest) == 0 {
			a.usage(a.Stderr)
			return ExitUsage
		}
		a.usage(a.Stdout)
		return ExitOK
	}
	for _, c := range commands() {
		if c.name == rest[0] {
			// Enrollment is a local operator command; it must not read a
			// connection, credentials, environment identity or owner-DM state.
			if c.name == "courier" && len(rest) > 1 && rest[1] == "enroll" {
				return a.fail(c.run(ctx, a, rest[1:]))
			}
			// Stored association is effect-free and has its own closed JSON
			// refusals. Both preflight and owner warnings read unrelated state.
			if c.name == "bind" && len(rest) > 1 && rest[1] == "association" {
				return a.bindAssociation(rest[2:])
			}
			// Courier manages only its explicit local home and socket. Never run
			// connection preflight or owner diagnostics on this path.
			if c.name == "courier" {
				return a.fail(c.run(ctx, a, rest[1:]))
			}
			// These inspections have their own routing-only validation. They
			// must not consult credentials, secret stores or owner-DM state.
			if c.name == "post-operation" || c.name == "post" && durablePostInvocation(rest[1:]) || c.name == "availability" || c.name == "voice" && len(rest) > 1 && rest[1] == "status" || c.name == "mute" && len(rest) > 1 && rest[1] == "list" {
				return a.fail(c.run(ctx, a, rest[1:]))
			}
			// Fetch parses usage/help before consulting connection or identity state.
			if c.name != "fetch" {
				if err := a.connectionPreflight(c.name, rest[1:]); err != nil {
					if c.name == "connection" && len(rest) > 1 && rest[1] == "register" {
						return a.fail(adminRegistrationError("configuration", err))
					}
					return a.fail(err)
				}
				if c.name != "owner-dm" {
					a.warnOwnerDM()
				}
			}
			return a.fail(c.run(ctx, a, rest[1:]))
		}
	}
	return a.fail(usagef("unknown command %q (see: a2a help)", rest[0]))
}

// extractGlobal removes --as from anywhere in args.
func (a *App) extractGlobal(args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case arg == "--as" || arg == "-as":
			if i+1 >= len(args) {
				return nil, usagef("--as needs a name")
			}
			a.as = args[i+1]
			i++
		case strings.HasPrefix(arg, "--as=") || strings.HasPrefix(arg, "-as="):
			a.as = arg[strings.Index(arg, "=")+1:]
		case arg == "--profile" || strings.HasPrefix(arg, "--profile="):
			var v string
			if arg == "--profile" {
				if i+1 >= len(args) {
					return nil, usagef("--profile needs a name")
				}
				i++
				v = args[i]
			} else {
				v = strings.TrimPrefix(arg, "--profile=")
			}
			if v == "" || a.profile != "" && a.profile != v {
				return nil, usagef("empty or conflicting --profile")
			}
			a.profile = v
		case arg == "--connection-fingerprint" || strings.HasPrefix(arg, "--connection-fingerprint="):
			var v string
			if arg == "--connection-fingerprint" {
				if i+1 >= len(args) {
					return nil, usagef("missing connection fingerprint")
				}
				i++
				v = args[i]
			} else {
				v = strings.TrimPrefix(arg, "--connection-fingerprint=")
			}
			if len(v) != 64 || a.connectionFingerprint != "" && a.connectionFingerprint != v {
				return nil, usagef("invalid or conflicting connection fingerprint")
			}
			a.connectionFingerprint = v
		case arg == "--human":
			a.human = true
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

// interactive reports whether stdin is an interactive terminal.
func (a *App) interactive() bool {
	if a.stdinTTY != nil {
		return a.stdinTTY()
	}
	return stdinIsTerminal(os.Stdin)
}

// stdinIsTerminal: f is a character device other than the null device (a
// process started with </dev/null has a character device on stdin too).
func stdinIsTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}

func (a *App) fail(err error) int {
	if err == nil {
		return ExitOK
	}
	msg := err.Error()
	if errors.Is(err, matrix.ErrAuth) {
		msg = "authentication failed: " + msg + "; ask the keeper to rotate credentials"
	}
	fmt.Fprintln(a.Stderr, "a2a: "+a.secrets.Scrub(sanitizeLine(msg)))
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, flag.ErrHelp) {
		return ExitUsage
	}
	var ce livenessConfigError
	if errors.As(err, &ce) {
		return ExitConfig
	}
	return ExitError
}

// newFlagSet returns a FlagSet that reports errors instead of exiting.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseInterspersed parses flags that may appear before, between, or after
// positional arguments ("a2a post room --topic x text"). "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			pos = append(pos, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		hasValue := strings.Contains(name, "=")
		if hasValue {
			name = name[:strings.Index(name, "=")]
		}
		flags = append(flags, arg)
		f := fs.Lookup(name)
		if f == nil || hasValue {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, usagef("%s: %v", fs.Name(), err)
	}
	return pos, nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// session is an authenticated identity bound to one homeserver.
type session struct {
	app              *App
	paths            config.Paths
	mx               config.Matrix
	name             string
	creds            config.Credentials
	client           *matrix.Client
	server           string
	checkBinding     func() error // Immutable operational snapshot; nil for legacy sessions.
	principals       []string     // Local snapshot for this delivery cycle; no lock spans I/O.
	principalWarning bool         // Emit malformed/unreadable metadata warning once per session.
	longConsumer     bool         // Process-owned watch/unbounded wait; ignore ambient runtime session IDs.
}

func (a *App) paths() (config.Paths, error) {
	name := a.as
	if name == "" {
		name = a.Getenv(config.EnvAgent)
	}
	return a.pathsFor(name)
}

func (a *App) loadMatrix() (config.Paths, config.Matrix, error) {
	p, err := a.paths()
	if err != nil {
		return p, config.Matrix{}, err
	}
	c, err := a.selectedConnection()
	if err != nil {
		return p, config.Matrix{}, err
	}
	if c != nil {
		return p, config.Matrix{URL: c.URL, ServerName: c.ServerName}, nil
	}
	mx, err := config.LoadMatrix(p, a.Getenv)
	if err != nil {
		return p, mx, err
	}
	a.secrets.Add(mx.RegistrationToken)
	return p, mx, nil
}

// identity returns the agent name from --as or A2A_AGENT.
func (a *App) identity() (string, error) {
	name := a.as
	env := a.Getenv(config.EnvAgent)
	if a.human {
		if name == "" {
			return "", usagef("--human needs --as <your own human account>")
		}
	} else {
		// A session bound to an identity (hook install sets A2A_AGENT) never
		// acts as someone else by accident.
		if name != "" && env != "" && name != env {
			return "", usagef("this session is %s (%s); --as %s refused: never act as another identity (operators: unset %s)", env, config.EnvAgent, name, config.EnvAgent)
		}
		if name == "" {
			name = env
		}
	}
	if name == "" {
		return "", usagef("no identity: pass --as <your-own-name> or set %s; never borrow another identity (after your owner confirms: a2a identity ensure --role <your-role>)", config.EnvAgent)
	}
	if err := config.ValidateName(name); err != nil {
		return "", usageError{err.Error()}
	}
	return name, nil
}

func (a *App) session() (*session, error) {
	name, err := a.identity()
	if err != nil {
		return nil, err
	}
	return a.sessionAs(name)
}

// sessionAs opens the session of name (already validated and guarded).
func (a *App) sessionAs(name string) (*session, error) {
	return a.sessionAsMode(name, false)
}

func (a *App) sessionAsMode(name string, metadataOnly bool) (*session, error) {
	p, mx, err := a.loadMatrix()
	if err != nil {
		return nil, err
	}
	p, err = a.pathsFor(name)
	if err != nil {
		return nil, err
	}
	var creds config.Credentials
	if metadataOnly {
		creds, err = a.credentialMetadata(p, name)
	} else {
		creds, err = a.loadCredentials(a.context(), p, name)
	}
	if err != nil {
		return nil, err
	}
	a.secrets.Add(creds.AccessToken, creds.Password)
	if metadataOnly {
		creds.AccessToken, creds.Password = "", ""
	}
	base := creds.Homeserver
	if base == "" {
		base = mx.URL
	}
	if base == "" {
		return nil, fmt.Errorf("no homeserver URL: set %s in %s", config.EnvMatrixURL, p.EnvFile())
	}
	server := creds.ServerName()
	if server == "" {
		server = mx.ServerName
	}
	c := matrix.New(base, creds.AccessToken)
	c.HTTP = a.HTTP
	s := &session{app: a, paths: p, mx: mx, name: name, creds: creds, client: c, server: server}
	if selected, e := a.selectedConnection(); e != nil {
		return nil, e
	} else if selected != nil {
		binding, e := selected.Binding(name, a.human)
		if e != nil {
			return nil, e
		}
		label := a.profile
		root := config.Paths{Base: p.ConfigRoot()}
		s.checkBinding = func() error {
			r, e := config.LoadConnections(root)
			if e != nil {
				return e
			}
			if r == nil {
				return errors.New("selected connection registry disappeared")
			}
			selected, ok := r.Profiles[label]
			if !ok {
				return errors.New("selected connection disappeared")
			}
			fresh, e := selected.Binding(name, a.human)
			if e != nil {
				return e
			}
			if fresh.Fingerprint() != binding.Fingerprint() || !sameAdminPolicy(selected.AdminRegistration, binding.Connection.AdminRegistration) {
				return errors.New("selected connection changed; explicit migration required")
			}
			_, e = a.namedMetadata(p, name)
			return e
		}
	}
	if selected, _ := a.selectedConnection(); selected != nil && selected.AdminRegistration != nil {
		c.ProtectedRoom = selected.AdminRegistration.RoomID
	}
	c.BeforeRequest = s.checkBinding
	if metadataOnly {
		c.BeforeRequest = func() error { return errors.New("credentials unavailable; metadata session only") }
	}
	s.installRedactionCache()
	return s, nil
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}

func (a *App) context() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// checkConnection guards consumer effects independently of HTTP. In particular,
// native dispatch, leases, receipts and terminal operations can run without a
// Matrix request. Current and unrelated registry edits remain valid.
func (s *session) checkConnection() error {
	if s.checkBinding != nil {
		return s.checkBinding()
	}
	return nil
}
