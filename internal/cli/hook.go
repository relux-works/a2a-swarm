package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Environment delivery into agent sessions (protocol rule 4): the hooks of the
// agent's runtime (Claude Code or Codex CLI) ring a DOORBELL at the session's
// own lifecycle points, so an agent does not have to keep `a2a wait` armed.
//
//   - stop           (Stop): blocks the stop with the doorbell as the reason.
//   - session-start  (SessionStart) and prompt (UserPromptSubmit): add the
//     doorbell as additionalContext.
//
// Hook output is treated by the runtime with near-user authority, so it never
// carries peer-authored text: the doorbell is a fixed a2a-authored template
// with counts, relevance types, kinds from the fixed list, and sender names
// that are verified members of the room on our server (doorbellText). The
// content is PULLED by the agent with `a2a inbox`, which prints it as tool
// output inside an untrusted-data frame and receipts it.
//
// The doorbell receipts nothing. It persists what the sync covers into the
// durable mailbox (like every consumer, before the cursor moves), counts the
// unreceived entries (gap records included), records per session which
// entries it announced (the rung set), and rings again only when an un-rung
// entry is pending. The Stop hook also has a loop guard (hookGuardMax
// consecutive blocks) and a rate limit (one block per hookStopEvery).
//
// Both runtimes run the same doorbell; --runtime claude|codex selects only the
// output format (hookRuntime.render) and, for install, the file written
// (research/codex-hooks.md).
//
// A hook never breaks the session: on any error (including a state lock held
// past the hook budget) it prints nothing to stdout, one line to stderr, and
// exits 0. `a2a hook pause` silences every hook event of an agent.

// hookBudget bounds the whole network work of one hook.
const hookBudget = 5 * time.Second

// hookStopEvery is the Stop rate limit: at most one blocking Stop per session in this window.
const hookStopEvery = 60 * time.Second

// doorbellMax is the hard cap of the doorbell text, in characters.
const doorbellMax = 300

// doorbellVerifyMax bounds the membership lookups of one doorbell.
const doorbellVerifyMax = 4

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
	Payload        json.RawMessage `json:"-"`
	SessionID      string          `json:"session_id"`
	HookEventName  string          `json:"hook_event_name"`
	StopHookActive bool            `json:"stop_hook_active"`
	StopReason     string          `json:"stop_reason"`
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
	{name: "muse", title: "Muse", settings: DefaultMuseHooks, render: renderMuseHook},
}

const runtimeNames = "claude|codex|muse"

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

const hookUsage = "usage: a2a hook stop|session-start|prompt|session-end --runtime claude|codex|muse  (reads the hook JSON on stdin) | " +
	"a2a hook install --agent NAME --runtime claude|codex|muse [--settings PATH] [--executable PATH | --installed-command PATH] | a2a hook uninstall --runtime claude|codex|muse [--settings PATH] | " +
	"a2a hook pause|resume --agent NAME"

// cmdHook: a2a hook stop|session-start|prompt|session-end|install|uninstall
func cmdHook(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", hookUsage)
	}
	switch args[0] {
	case "install":
		return cmdHookInstall(a, args[1:], true)
	case "uninstall":
		return cmdHookInstall(a, args[1:], false)
	case "pause":
		return cmdHookPause(a, args[1:], true)
	case "resume":
		return cmdHookPause(a, args[1:], false)
	}
	if args[0] == "session-end" {
		a.runHook(ctx, hookEvent{"session-end", "SessionEnd"}, args[1:])
		return nil
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
		a.noteDaemonError(err)
		if errors.Is(err, matrix.ErrAuth) {
			err = fmt.Errorf("authentication failed; ask the keeper to rotate credentials")
		}
		fmt.Fprintln(a.Stderr, "a2a hook "+ev.arg+": "+a.secrets.Scrub(sanitizeLine(err.Error()))+"; nothing new was acknowledged")
	}
	fs := newFlagSet("hook " + ev.arg)
	runtime := fs.String("runtime", "", "agent runtime: "+runtimeNames+" (required for Muse; legacy events may omit it)")
	pos, err := parseInterspersed(fs, args)
	if err == nil && len(pos) != 0 {
		err = errors.New("unexpected arguments")
	}
	if err != nil {
		fail(fmt.Errorf("%v; %s", err, hookUsage))
		return
	}
	// Mailbox output is identical for every runtime. Legacy hooks omitting
	// --runtime keep delivering but do not opt into automatic binding; the
	// explicit runtime selects the ancestry check without a provider default.
	rt := hookRuntimes[0]
	if *runtime != "" {
		if rt, err = lookupRuntime(*runtime); err != nil {
			fail(fmt.Errorf("%v; re-run a2a hook install --agent NAME --runtime %s", err, runtimeNames))
			return
		}
	}
	if rt.name == "muse" {
		a.runMuseHook(ctx, rt, ev)
		return
	}
	// Refuse before terminal startup, session exports or owner mirror work.
	name, err := a.identity()
	if err != nil {
		fail(err)
		return
	}
	paths, err := a.pathsFor(name)
	if err != nil {
		fail(err)
		return
	}
	if err = config.CheckIdentityMigration(paths, name); err != nil {
		fail(err)
		return
	}
	if selected, err := a.hostBackendSelected(); err != nil {
		fail(err)
		return
	} else if selected {
		return // Host owns notification; no auto-bind, session export or hook fallback.
	}
	in, err := readHookInput(a.Stdin)
	blocked := false
	if ev.arg == "stop" && *runtime != "" {
		parent := ctx
		defer func() {
			if !blocked {
				a.ownerMirrorStop(parent, rt.name, in)
			}
		}()
	}
	if err != nil {
		fail(err)
		return
	}
	if ev.arg == "session-start" {
		// The session's own a2a commands then hold the same lease as its hooks.
		if err := a.exportSessionID(in.SessionID); err != nil {
			fail(err)
		}
	}
	budget := hookBudget
	if a.hookBudget > 0 {
		budget = a.hookBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	priorContext := a.ctx
	a.ctx = ctx
	defer func() { a.ctx = priorContext }()
	if *runtime != "" {
		if err := a.recordHookOwner(ctx, rt, ev, in); err != nil {
			fail(err)
			return
		}
	}
	if ev.arg == "session-end" || ev.arg == "stop" && in.StopReason == "exit" {
		return
	}
	if *runtime != "" {
		var bindErr error
		if ev.arg == "session-start" {
			bindErr = a.autoBindHook(ctx, *runtime, in, bindingSessionStart)
		} else {
			bindErr = a.healHookBinding(ctx, *runtime, in)
		}
		if bindErr != nil {
			fail(bindErr)
		}
	}
	err = a.hookDeliver(ctx, rt, ev, in, &blocked)
	if err != nil {
		fail(err)
		return
	}
	// Mail output and its acknowledgement are already committed. Status gets
	// only the remaining budget; its retries/failures are private diagnostics.
	if ctx.Err() == nil {
		_ = a.hookLiveness(ctx, ev, in)
	}
}

// Liveness is opt-in via a typed status record. Hook availability is reported
// as declared by its owner; executing a hook is not proof of future idle wake.
func (a *App) hookLiveness(ctx context.Context, ev hookEvent, in hookInput) error {
	s, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.HooksPaused || st.Liveness == nil {
		return nil
	}
	if st.Runtime != nil && st.Runtime.SessionID != in.SessionID {
		return errors.New("liveness hook refuses a foreign runtime session")
	}
	if st.Terminal != nil && (ev.arg == "prompt" || ev.arg == "stop") {
		if err := a.clearTerminalNoticeOnTurn(ctx, s, in.SessionID); err != nil {
			return err
		}
		st, err = config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
	}
	if st.Liveness == nil {
		return nil
	}

	profile := *st.Liveness
	if st.LivenessSource == "manual" && (profile.State == "blocked" || profile.State == "stopping" || (profile.State == "waiting-human" && ev.arg != "prompt")) {
		if st.Terminal != nil {
			return s.publishTerminalNotice(ctx, st.Terminal.Generation)
		}
		return nil
	}
	profile.State = "working"
	if ev.arg == "stop" && st.HookBlocks(hookSessionKey(in.SessionID)) == 0 {
		profile.State = "idle"
	}
	err = s.writeLivenessSource(ctx, profile, "hook")
	if errors.Is(err, ErrOwnStatusPending) {
		return nil
	}
	return err
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
	in.Payload = append(json.RawMessage(nil), data...)
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

// hookDeliver rings the doorbell for what is pending for this agent: it
// brings the mailbox up to date like inbox, skips what this session already
// rang, and prints the fixed template for the unreceived entries. It receipts
// nothing; only `a2a inbox` and `a2a ack` do.
func (a *App) hookDeliver(ctx context.Context, rt hookRuntime, ev hookEvent, in hookInput, blocked *bool) error {
	s, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.HostEnrollment != nil {
		return nil
	}
	if st.HooksPaused {
		return nil // kill switch: no network, no output
	}
	deadline, _ := ctx.Deadline()
	// One live session per identity: while another session's lease is live
	// this hook stays quiet (one stderr line, exit 0).
	if err := a.acquireLease(s, in.SessionID, deadline); err != nil {
		return err
	}
	stop := ev.arg == "stop" && rt.name != "muse"
	key := hookSessionKey(in.SessionID)
	if stop && in.StopHookActive && st.HookBlocks(key) >= hookGuardMax {
		// Loop guard: let this stop happen; the events wait for the next hook or turn.
		if _, err := a.updateStateBy(s.paths, s.name, deadline, func(cur *config.State) error {
			cur.SetHookBlocks(key, 0)
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "a2a hook stop: %d consecutive blocks in this session; letting it stop, run `a2a inbox` to read what is pending\n", hookGuardMax)
		return nil
	}
	list, err := s.getSubs(ctx) // read only: no pruning
	if err != nil {
		return err
	}
	now := a.Now()
	var subs []Sub
	for _, sub := range list.Subs {
		if !sub.expiredAt(now) {
			subs = append(subs, sub)
		}
	}
	// Persist what the sync covers (the doorbell never receipts), then ring for
	// the unreceived mailbox entries.
	if _, err := s.syncNow(ctx, st, subs, deadline); err != nil {
		return err
	}
	var members map[string]bool
	if mb, err := config.OpenMailbox(s.paths, s.name); err == nil {
		// First-sync positioning may not collect a batch. Sender metadata must
		// apply the same fresh pair policy as the later notification selection.
		gate, err := s.refreshAgentGate(ctx)
		if err != nil {
			return err
		}
		s.agentDelivery = &gate
		s.agentMailboxView(mb)
		members = s.verifySenders(ctx, itemsOf(mb.Pending(), s.server))
	}
	limited := false
	err = a.withRuntimeLock(s.paths, s.name, "control", deadline, func() error {
		current, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		sid := in.SessionID
		if sid == "" && current.Lease != nil {
			sid = current.Lease.Holder
		}
		if sid == "" {
			sid = key
		}
		bypass := stop && !a.hasTrailingConsumer(ctx, s, current, sid)
		var out string
		var keys []string
		token, err := a.reserveNotification(ctx, s, deadline, "hook", sid, "", bypass, func(cur *config.State, mb *config.Mailbox, _ string) (bool, error) {
			if cur.HooksPaused {
				return false, nil
			}
			items := itemsOf(mb.Pending(), s.server)
			h := cur.HookSession(key)
			keys = rungKeys(items)
			fresh := false
			for _, k := range keys {
				fresh = fresh || !contains(h.Rung, k)
			}
			if !fresh {
				if stop {
					h.Blocks = 0
				}
				h.Rung = keys
				cur.PutHookSession(h)
				return false, nil
			}
			if stop && h.LastBlock != 0 {
				if since := a.Now().Sub(time.Unix(h.LastBlock, 0)); since >= 0 && since < hookStopEvery {
					limited = true
					h.Blocks = 0
					cur.PutHookSession(h)
					return false, nil
				}
			}
			out, err = hookOutput(rt, ev, doorbellOf(items, s.server, members))
			return err == nil, err
		})
		if err != nil || token == "" {
			return err
		}
		if err = s.checkConnection(); err != nil {
			return err
		}
		written, writeErr := io.WriteString(a.Stdout, out)
		if writeErr == nil && written != len(out) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			if written == 0 {
				if cancelErr := a.cancelNotification(ctx, s, deadline, token); cancelErr != nil {
					return cancelErr
				}
			}
			return fmt.Errorf("write output: %w", writeErr)
		}
		if f, ok := a.Stdout.(interface{ Flush() error }); ok {
			if err = f.Flush(); err != nil {
				return fmt.Errorf("flush output: %w", err)
			}
		}
		if stop {
			*blocked = true
		}
		if a.afterPrint != nil {
			if err = a.afterPrint(); err != nil {
				return err
			}
		}
		_, err = s.lockedDeliver(ctx, deadline, false, func(cur *config.State, mb *config.Mailbox) ([]string, error) {
			if cur.Notification.Reservation == nil || cur.Notification.Reservation.Token != token {
				return nil, config.ErrNotificationStale
			}
			h := cur.HookSession(key)
			// Muse Stop is UI-only. Preserve prior context eligibility so the
			// same pending batch can reach Prompt/Start after shared cooldown.
			// Notification completion below still records/spaces the UI notice.
			if rt.name != "muse" || ev.arg != "stop" {
				h.Rung = keys
			}
			if stop {
				n := 0
				if in.StopHookActive {
					n = h.Blocks
				}
				h.Blocks, h.LastBlock = n+1, a.Now().Unix()
			}
			cur.PutHookSession(h)
			return nil, a.completeNotification(s, cur, token)
		})
		return err
	})
	if err == nil && limited {
		fmt.Fprintf(a.Stderr, "a2a hook stop: this session was blocked less than %s ago; letting it stop, run `a2a inbox` to read what is pending\n", hookStopEvery)
	}
	return err
}

// rungKeys names the pending items for the rung set: their mailbox ids.
func rungKeys(items []waitItem) []string { return itemIDs(items) }

// agentNameRE is the only sender name shape the doorbell prints.
var agentNameRE = regexp.MustCompile(`^[a-z0-9._=-]{1,64}$`)

// unknownSender replaces every sender the doorbell cannot vouch for.
const unknownSender = "unknown sender"

// doorbellName returns the localpart of userID when it may appear in the
// doorbell as a candidate: on our server and shaped like an agent name.
func doorbellName(userID, server string) (string, bool) {
	if !strings.HasPrefix(userID, "@") {
		return "", false
	}
	local, srv, ok := strings.Cut(userID[1:], ":")
	if !ok || srv != server || !agentNameRE.MatchString(local) {
		return "", false
	}
	return local, true
}

// verifySenders checks, for the first doorbellVerifyMax candidate senders
// (sorted), that they are joined members of the room they wrote in. Only a
// name marked true is ever printed; a failed lookup counts as unknown.
func (s *session) verifySenders(ctx context.Context, items []waitItem) map[string]bool {
	room := map[string]string{}
	for _, it := range items {
		if it.ev == nil {
			continue
		}
		if n, ok := doorbellName(it.ev.Sender, s.server); ok {
			if _, seen := room[n]; !seen {
				room[n] = it.rec.RoomID
			}
		}
	}
	out := map[string]bool{}
	for i, n := range sortedKeys(room) {
		if i >= doorbellVerifyMax {
			break
		}
		var m struct {
			Membership string `json:"membership"`
		}
		err := s.client.GetState(ctx, room[n], "m.room.member", "@"+n+":"+s.server, &m)
		out[n] = err == nil && m.Membership == "join"
	}
	return out
}

// doorbell is the a2a-authored summary of the pending events. Every field is
// either a count or one of our own constants; no peer-controlled text.
type doorbell struct {
	total   int
	types   map[string]int
	kinds   []string // in Kinds order, then "other"
	names   []string // verified member localparts, sorted
	unknown bool     // some sender could not be vouched for
	more    int      // candidate senders not looked up
	gaps    int      // gap records: events that could not be checked
}

// doorbellTypes is the fixed order of the relevance types in the template.
var doorbellTypes = []string{string(relTo), string(relMention), string(relThread), string(relSub), string(relAll), "redacted", "invite"}

// doorbellOf summarizes the pending items; members is verifySenders' result.
func doorbellOf(items []waitItem, server string, members map[string]bool) doorbell {
	d := doorbell{types: map[string]int{}}
	kinds := map[string]bool{}
	names := map[string]bool{}
	for _, it := range items {
		if it.gap != nil {
			d.gaps++
			continue
		}
		d.total++
		d.types[it.rec.Type]++
		if it.ev == nil {
			continue
		}
		if h, ok := decodeHeader(it.ev.Message().A2A); ok && h.Kind != "" {
			if validKind(h.Kind) {
				kinds[h.Kind] = true
			} else {
				kinds["other"] = true
			}
		}
		n, ok := doorbellName(it.ev.Sender, server)
		verified, looked := members[n]
		switch {
		case !ok || (looked && !verified):
			d.unknown = true
		case !looked:
			names[n] = false
		default:
			names[n] = true
		}
	}
	for _, k := range append(append([]string(nil), Kinds...), "other") {
		if kinds[k] {
			d.kinds = append(d.kinds, k)
		}
	}
	for _, n := range sortedKeys(names) {
		if names[n] {
			d.names = append(d.names, n)
		} else {
			d.more++
		}
	}
	return d
}

// doorbellText renders the template, dropping kinds and sender names from the
// longer list first (counted as "more") until it fits doorbellMax characters.
func doorbellText(d doorbell) string {
	return doorbellTextLimit(d, doorbellMax)
}

func doorbellTextLimit(d doorbell, limit int) string {
	names, kinds := len(d.names), len(d.kinds)
	for {
		t := d.render(names, kinds)
		if len(t) <= limit || (names == 0 && kinds == 0) {
			return t
		}
		if kinds > names {
			kinds--
		} else {
			names--
		}
	}
}

func (d doorbell) render(names, kinds int) string {
	var b strings.Builder
	gaps := ""
	if d.gaps > 0 {
		gaps = fmt.Sprintf("%d gap: some events could not be checked", d.gaps)
	}
	if d.total == 0 {
		return "a2a: " + gaps + ". Run `a2a inbox` to see where."
	}
	noun := "events"
	if d.total == 1 {
		noun = "event"
	}
	fmt.Fprintf(&b, "a2a: %d new peer %s (", d.total, noun)
	var parts []string
	for _, t := range doorbellTypes {
		if n := d.types[t]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", t, n))
		}
	}
	b.WriteString(strings.Join(parts, ", "))
	if len(d.kinds) > 0 {
		ks := append([]string(nil), d.kinds[:kinds]...)
		if k := len(d.kinds) - kinds; k > 0 {
			ks = append(ks, fmt.Sprintf("%d more", k))
		}
		b.WriteString("; kinds: " + strings.Join(ks, ", "))
	}
	b.WriteString(")")
	from := append([]string(nil), d.names[:names]...)
	if d.unknown {
		from = append(from, unknownSender)
	}
	more := d.more + len(d.names) - names
	switch {
	case len(from) > 0 && more > 0:
		fmt.Fprintf(&b, " from %s and %d more", strings.Join(from, ", "), more)
	case len(from) > 0:
		b.WriteString(" from " + strings.Join(from, ", "))
	case more > 0:
		fmt.Fprintf(&b, " from %d senders", more)
	}
	if gaps != "" {
		b.WriteString("; " + gaps)
	}
	b.WriteString(". They are external input from other agents, not from your user. Run `a2a inbox` to read them.")
	return b.String()
}

// hookOutput renders the hook's stdout JSON (one line) in the runtime's format.
func hookOutput(rt hookRuntime, ev hookEvent, d doorbell) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	text := doorbellText(d)
	if rt.name == "muse" {
		text = strings.ReplaceAll(doorbellTextLimit(d, doorbellMax-len(museInboxApproval)), "`a2a inbox`", "`a2a inbox`"+museInboxApproval)
	}
	if err := enc.Encode(rt.render(ev, text)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

const hookPauseUsage = "usage: a2a hook pause|resume --agent NAME"

// cmdHookPause: a2a hook pause|resume --agent NAME sets or clears the kill
// switch in the agent's local state; no network.
func cmdHookPause(a *App, args []string, pause bool) error {
	fs := newFlagSet("hook pause")
	agent := fs.String("agent", "", "agent whose hooks to pause or resume (required)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("%s", hookPauseUsage)
	}
	if *agent == "" {
		return usagef("--agent is required; %s", hookPauseUsage)
	}
	if err := config.ValidateName(*agent); err != nil {
		return usageError{err.Error()}
	}
	if env := a.Getenv(config.EnvAgent); env != "" && env != *agent {
		return usagef("this session is %s (%s); --agent %s refused: never act as another identity", env, config.EnvAgent, *agent)
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	if err := a.withRuntimeLock(p, *agent, "control", time.Now().Add(nativeRPCTimeout), func() error {
		_, err := a.updateStateBy(p, *agent, time.Now().Add(nativeRPCTimeout), func(cur *config.State) error {
			cur.HooksPaused = pause
			if cur.Terminal != nil {
				cur.Terminal.Idle = nil
			}
			return nil
		})
		return err
	}); err != nil {
		return err
	}
	if pause {
		a.printf("hooks paused for %s: every hook event prints nothing until a2a hook resume --agent %s\n", *agent, *agent)
	} else {
		a.printf("hooks resumed for %s\n", *agent)
	}
	return nil
}
