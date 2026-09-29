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
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/redact"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// App carries the process environment so tests can run commands in-process.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Getenv func(string) string
	HTTP   *http.Client
	Now    func() time.Time

	// as is the identity from --as (overrides A2A_AGENT).
	as      string
	secrets redact.Set
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
		{"register", "register <name>                      create a Matrix account and a 0600 credential file", cmdRegister},
		{"whoami", "whoami                               show the current identity", cmdWhoami},
		{"rooms", "rooms                                list joined rooms (alias, name, id)", cmdRooms},
		{"join", "join <room>                          join a room", cmdJoin},
		{"invite", "invite <room> <@user|name>           invite someone to a room", cmdInvite},
		{"topic", "topic new <room> <slug> \"<title>\" [--body -|@file|text]   start a topic thread", cmdTopic},
		{"topics", "topics <room>                        list topic threads (slug, event id, replies)", cmdTopics},
		{"post", "post <room> --topic <slug|$event> [--kind K] [--re $ev] [--to a,b] [--reply-by T] [--trunk OID] <text|@file|->", cmdPost},
		{"attach", "attach <room> --topic <slug|$event> <file> [--kind K] [--to a,b] [--pin] [caption|@file|-]   share a file", cmdAttach},
		{"fetch", "fetch <room> <$event> [-o path] | fetch <mxc://...> --sha256 <hex> [-o path]   download and verify a file", cmdFetch},
		{"files", "files <room> [--topic X] [--last N] [--json]   list shared files, oldest first", cmdFiles},
		{"pin", "pin <room> <$event>                  pin an event (m.room.pinned_events)", cmdPin},
		{"unpin", "unpin <room> <$event>                unpin an event", cmdUnpin},
		{"read", "read <room> [--topic X] [--last N] [--event $ev] [--json]   print messages, oldest first", cmdRead},
		{"archive", "archive <room|all> [--dir archive] [--json-index]   deterministic markdown export of room history", cmdArchive},
		{"wait", "wait [--room R ...] [--all] [--timeout 25m] [--json]   block until something is addressed to me or subscribed", cmdWait},
		{"mute", "mute <room> --topic X                 stop replies in a thread from ringing (to/mentions still ring)", cmdMute},
		{"follow", "follow <room> --topic X               ring on replies in a thread again", cmdFollow},
		{"subscribe", "subscribe --kind k1,k2 [--room R|all]   wake on these message kinds without being addressed", cmdSubscribe},
		{"unsubscribe", "unsubscribe [--kind k1,k2] [--room R|all]   drop kind subscriptions (all kinds without --kind)", cmdUnsubscribe},
		{"subscriptions", "subscriptions [--json]               list kind subscriptions", cmdSubscriptions},
		{"project", "project [--system R] [--kind k1,k2] [--once]   run the #system projector (rule 24) as this identity", cmdProject},
		{"card", "card set --file card.md | card get <name>   agent card in #lobby", cmdCard},
		{"status", "status set <active|idle|away> [\"text\"] | status get [name]", cmdStatus},
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintln(w, "usage: a2a [--as <name>] <command> [args]")
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
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{}
	}
	rest, err := a.extractGlobal(args)
	if err != nil {
		return a.fail(err)
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
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

func (a *App) fail(err error) int {
	if err == nil {
		return ExitOK
	}
	msg := err.Error()
	if errors.Is(err, matrix.ErrAuth) {
		msg = "authentication failed: " + msg + "; the credential file's access token is invalid or revoked"
	}
	fmt.Fprintln(a.Stderr, "a2a: "+a.secrets.Scrub(sanitizeLine(msg)))
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, flag.ErrHelp) {
		return ExitUsage
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
	app    *App
	paths  config.Paths
	mx     config.Matrix
	name   string
	creds  config.Credentials
	client *matrix.Client
	server string
}

func (a *App) paths() (config.Paths, error) { return config.ResolvePaths(a.Getenv) }

func (a *App) loadMatrix() (config.Paths, config.Matrix, error) {
	p, err := a.paths()
	if err != nil {
		return p, config.Matrix{}, err
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
	if name == "" {
		name = a.Getenv(config.EnvAgent)
	}
	if name == "" {
		return "", usagef("no identity: pass --as <name> or set %s", config.EnvAgent)
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
	p, mx, err := a.loadMatrix()
	if err != nil {
		return nil, err
	}
	creds, err := config.LoadCredentials(p, name)
	if err != nil {
		return nil, err
	}
	a.secrets.Add(creds.AccessToken, creds.Password)
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
	return &session{app: a, paths: p, mx: mx, name: name, creds: creds, client: c, server: server}, nil
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}
