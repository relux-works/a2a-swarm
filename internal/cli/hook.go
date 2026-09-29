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

// Environment delivery into Claude Code sessions (protocol rule 4): the hooks
// hand relevant peer events to the model at the session's own lifecycle
// points, so an agent does not have to keep `a2a wait` armed.
//
//   - stop           (Stop): blocks the stop with the events as the reason.
//   - session-start  (SessionStart) and prompt (UserPromptSubmit): add them as
//     additionalContext.
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

// hookEvent is one Claude Code hook event a2a handles.
type hookEvent struct {
	arg  string // a2a hook <arg>
	name string // Claude Code event name
}

var hookEvents = []hookEvent{
	{"session-start", "SessionStart"},
	{"prompt", "UserPromptSubmit"},
	{"stop", "Stop"},
}

// hookInput is the part of the Claude Code hook stdin JSON a2a uses.
type hookInput struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// hookStopOutput blocks a stop; the reason is fed to the model.
type hookStopOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookContextOutput adds context to SessionStart and UserPromptSubmit.
type hookContextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

const hookUsage = "usage: a2a hook stop|session-start|prompt  (reads the Claude Code hook JSON on stdin) | " +
	"a2a hook install --agent NAME [--settings PATH] | a2a hook uninstall [--settings PATH]"

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
	if len(args) != 0 {
		fail(errors.New("unexpected arguments; " + hookUsage))
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
	err = a.hookDeliver(ctx, ev, in)
	// A rejected token that the server refused before doing anything: log in
	// again once and retry, like every other command (relogin.go).
	if err != nil && errors.Is(err, matrix.ErrAuth) && a.sess != nil && a.served.Load() == 0 {
		if err = a.relogin(ctx, a.sess, err); err == nil {
			a.sess = nil
			err = a.hookDeliver(ctx, ev, in)
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
func (a *App) hookDeliver(ctx context.Context, ev hookEvent, in hookInput) error {
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
		out, err := hookOutput(ev, len(d.items), shown.items)
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

// hookOutput renders the hook's stdout JSON (one line).
func hookOutput(ev hookEvent, total int, shown []waitItem) (string, error) {
	reason := hookReason(total, shown)
	var v any
	if ev.arg == "stop" {
		v = hookStopOutput{Decision: "block", Reason: reason}
	} else {
		var o hookContextOutput
		o.HookSpecificOutput.HookEventName = ev.name
		o.HookSpecificOutput.AdditionalContext = reason
		v = o
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return buf.String(), nil
}
