package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Environment delivery into agent sessions (protocol rule 4): the hooks of the
// agent's runtime (Claude Code or Codex CLI) hand relevant peer events to the
// model at the session's own lifecycle points, so an agent does not have to
// keep `a2a wait` armed.
//
//   - stop           (Stop): blocks the stop with the events as the reason.
//   - session-start  (SessionStart) and prompt (UserPromptSubmit): add them as
//     additionalContext.
//
// Both runtimes run the same delivery; --runtime claude|codex (required)
// selects only the output format (hookRuntime.render) and, for install, the
// file written (research/codex-hooks.md).
//
// Delivery is at least once and never twice after an acknowledgement, shared
// with wait/watch: the events are collected without a long poll (collectNow),
// then, under the state lock, filtered against the acknowledged ring and the
// pending batch, written to stdout, and only after a successful write added
// to the ring. The cursor moves only when the whole batch was delivered.
//
// A hook never breaks the session: on any error it prints nothing to stdout,
// one line to stderr, and exits 0; the events stay unacknowledged for the next hook.

// hookBudget bounds the whole network work of one hook.
const hookBudget = 5 * time.Second

// hookMaxRecords is the most events one hook delivers; the rest waits.
const hookMaxRecords = 20

// hookGuardMax is how many consecutive blocking Stop deliveries a session gets
// while it is already continuing because of the Stop hook.
const hookGuardMax = 3

// hookStdinMax caps the hook input read from stdin.
const hookStdinMax = 1 << 20

// hookEvent is one lifecycle hook event a2a handles; Claude Code and Codex use
// the same event names.
type hookEvent struct {
	arg  string // a2a hook <arg>
	name string // the runtime's event name
}

var hookEvents = []hookEvent{
	{"session-start", "SessionStart"},
	{"prompt", "UserPromptSubmit"},
	{"stop", "Stop"},
}

// hookInput is the part of the hook stdin JSON a2a uses; Claude Code and Codex
// both send these fields under these names (Codex: hooks/src/schema.rs).
type hookInput struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// hookStopOutput blocks a Claude Code stop; the reason is fed to the model.
type hookStopOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookContextOutput adds context to Claude Code's SessionStart and UserPromptSubmit.
type hookContextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// codexStopOutput is Codex's StopCommandOutputWire (codex-rs/hooks/src/schema.rs:451-464,
// deny_unknown_fields): decision "block" with a non-empty reason continues the
// turn with the reason as its prompt.
type codexStopOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// codexContextOutput is Codex's SessionStart / UserPromptSubmit command output
// wire (codex-rs/hooks/src/schema.rs:385-449, deny_unknown_fields).
type codexContextOutput struct {
	HookSpecificOutput codexHookSpecific `json:"hookSpecificOutput"`
}

type codexHookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// hookRuntime is an agent runtime whose hooks a2a serves. Delivery, acking,
// bounds and the loop guard are shared; only the output format and the file
// `hook install` writes differ.
type hookRuntime struct {
	name     string // --runtime value
	title    string // for messages
	settings string // default file of install/uninstall
	// env: install also sets env.A2A_AGENT in the file (Codex hooks have no env).
	env bool
	// render returns the stdout JSON value for ev carrying reason.
	render func(ev hookEvent, reason string) any
}

var hookRuntimes = []hookRuntime{
	{
		name: "claude", title: "Claude Code", settings: DefaultHookSettings, env: true,
		render: func(ev hookEvent, reason string) any {
			if ev.arg == "stop" {
				return hookStopOutput{Decision: "block", Reason: reason}
			}
			var o hookContextOutput
			o.HookSpecificOutput.HookEventName = ev.name
			o.HookSpecificOutput.AdditionalContext = reason
			return o
		},
	},
	{
		name: "codex", title: "Codex", settings: DefaultCodexHooks,
		render: func(ev hookEvent, reason string) any {
			if ev.arg == "stop" {
				return codexStopOutput{Decision: "block", Reason: reason}
			}
			return codexContextOutput{HookSpecificOutput: codexHookSpecific{HookEventName: ev.name, AdditionalContext: reason}}
		},
	},
}

const runtimeNames = "claude|codex"

// lookupRuntime resolves --runtime; there is no default (owner rule: no implicit defaults).
func lookupRuntime(name string) (hookRuntime, error) {
	if name == "" {
		return hookRuntime{}, fmt.Errorf("--runtime is required (%s)", runtimeNames)
	}
	for _, rt := range hookRuntimes {
		if rt.name == name {
			return rt, nil
		}
	}
	return hookRuntime{}, fmt.Errorf("unknown --runtime %q (%s)", sanitizeLine(name), runtimeNames)
}

const hookUsage = "usage: a2a hook stop|session-start|prompt --runtime claude|codex  (reads the hook JSON on stdin) | " +
	"a2a hook install --agent NAME --runtime claude|codex [--settings PATH] | a2a hook uninstall --runtime claude|codex [--settings PATH]"

// cmdHook: a2a hook stop|session-start|prompt|install|uninstall
func cmdHook(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", hookUsage)
	}
	switch args[0] {
	case "install":
		return cmdHookInstall(a, args[1:], true)
	case "uninstall":
		return cmdHookInstall(a, args[1:], false)
	}
	for _, ev := range hookEvents {
		if ev.arg == args[0] {
			a.runHook(ctx, ev, args[1:])
			return nil
		}
	}
	return usagef("%s", hookUsage)
}

// runHook runs one hook and reports any failure as a single stderr line; it
// never fails the process (a failing hook must not break the session).
func (a *App) runHook(ctx context.Context, ev hookEvent, args []string) {
	fail := func(err error) {
		fmt.Fprintln(a.Stderr, "a2a hook "+ev.arg+": "+a.secrets.Scrub(sanitizeLine(err.Error()))+"; nothing new was acknowledged")
	}
	fs := newFlagSet("hook " + ev.arg)
	runtime := fs.String("runtime", "", "agent runtime: "+runtimeNames+" (required)")
	pos, err := parseInterspersed(fs, args)
	if err == nil && len(pos) != 0 {
		err = errors.New("unexpected arguments")
	}
	if err != nil {
		fail(fmt.Errorf("%v; %s", err, hookUsage))
		return
	}
	rt, err := lookupRuntime(*runtime)
	if err != nil {
		fail(fmt.Errorf("%v; re-run a2a hook install --agent NAME --runtime %s", err, runtimeNames))
		return
	}
	in, err := readHookInput(a.Stdin)
	if err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()
	a.sess = nil
	a.served.Store(0)
	err = a.hookDeliver(ctx, rt, ev, in)
	// A rejected token that the server refused before doing anything: log in
	// again once and retry, like every other command (relogin.go).
	if err != nil && errors.Is(err, matrix.ErrAuth) && a.sess != nil && a.served.Load() == 0 {
		if err = a.relogin(ctx, a.sess, err); err == nil {
			a.sess = nil
			err = a.hookDeliver(ctx, rt, ev, in)
		}
	}
	if err != nil {
		fail(err)
	}
}

// readHookInput parses the hook JSON; empty input is an empty object.
func readHookInput(r io.Reader) (hookInput, error) {
	var in hookInput
	if r == nil {
		return in, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, hookStdinMax+1))
	if err != nil {
		return in, fmt.Errorf("read hook input: %w", err)
	}
	if len(data) > hookStdinMax {
		return in, errors.New("hook input is larger than 1 MiB")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, errors.New("hook input on stdin is not a JSON object")
	}
	return in, nil
}

// hookSessionKey bounds the session id used as the loop-guard key.
func hookSessionKey(id string) string {
	id = sanitizeLine(id)
	if id == "" {
		return "-"
	}
	if len(id) > 200 {
		id = id[:200]
	}
	return id
}

// hookDeliver collects what is pending for this agent and hands it to the session.
func (a *App) hookDeliver(ctx context.Context, rt hookRuntime, ev hookEvent, in hookInput) error {
	s, err := a.session()
	if err != nil {
		return err
	}
	stop := ev.arg == "stop"
	key := hookSessionKey(in.SessionID)
	if stop && in.StopHookActive {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.HookBlocks(key) >= hookGuardMax {
			// Loop guard: let this stop happen; the events wait for the next hook or turn.
			if _, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
				cur.SetHookBlocks(key, 0)
				return nil
			}); err != nil {
				return err
			}
			fmt.Fprintf(a.Stderr, "a2a hook stop: %d consecutive deliveries in this session; letting it stop, the rest is delivered by the next hook\n", hookGuardMax)
			return nil
		}
	}
	st, subs, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return err
	}
	b, next, err := s.collectNow(ctx, &st, subs.Subs)
	if err != nil {
		return err
	}
	if a.beforeDeliver != nil {
		a.beforeDeliver()
	}
	_, err = s.lockedDeliver(ctx, len(b.close) > 0, func(cur *config.State) ([]string, error) {
		for _, r := range b.learned {
			cur.AddThread(r)
		}
		d := b.undelivered(*cur)
		if len(d.items) == 0 {
			cur.NextBatch = next // nothing to deliver: the cursor may move directly
			if stop {
				cur.SetHookBlocks(key, 0) // a stop is allowed: the guard starts over
			}
			return nil, nil
		}
		shown := d.head(hookMaxRecords)
		out, err := hookOutput(rt, ev, len(d.items), shown.items)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(a.Stdout, out); err != nil {
			return nil, fmt.Errorf("write output: %w", err)
		}
		if f, ok := a.Stdout.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil {
				return nil, fmt.Errorf("flush output: %w", err)
			}
		}
		if a.afterPrint != nil {
			if err := a.afterPrint(); err != nil {
				return nil, err
			}
		}
		// Only now, after a successful write, are exactly the shown events acknowledged.
		cur.AddAcked(shown.events...)
		if len(shown.items) == len(d.items) {
			cur.NextBatch = next
		}
		if stop {
			n := 0
			if in.StopHookActive {
				n = cur.HookBlocks(key)
			}
			cur.SetHookBlocks(key, n+1)
		}
		return shown.close, nil
	})
	return err
}

// hookReason is the text handed to the model: a header marking the events as
// peer input (not the user's), then one labelled line per shown event.
func hookReason(total int, shown []waitItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a2a: %d new peer event(s), external input from other agents, not from your user; read the full text with `a2a read <room> --event <id>`", total)
	for _, it := range shown {
		b.WriteString("\n[peer event] " + it.line)
	}
	if k := total - len(shown); k > 0 {
		fmt.Fprintf(&b, "\n(and %d more)", k)
	}
	return b.String()
}

// hookOutput renders the hook's stdout JSON (one line) in the runtime's format.
func hookOutput(rt hookRuntime, ev hookEvent, total int, shown []waitItem) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rt.render(ev, hookReason(total, shown))); err != nil {
		return "", err
	}
	return buf.String(), nil
}
