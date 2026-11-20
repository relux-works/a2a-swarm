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
)

const agyGroup = "a2a-mailbox"

var agyUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Match only literal argv produced by shellQuote. In particular, another
// shell command or a command substitution is not an installed hook prefix.
const agyBarePath = `/[a-zA-Z0-9_./+:@%=,-]*`
const agyQuotedPath = `/(?:[^']|'\\'')*`

var agyPrefix = regexp.MustCompile(`^env (?:A2A_CONFIG_DIR=` + agyBarePath + `|'A2A_CONFIG_DIR=` + agyQuotedPath + `') (?:` + agyBarePath + `|'` + agyQuotedPath + `')(?: --profile [a-z0-9][a-z0-9_-]{0,63} --connection-fingerprint [a-f0-9]{64})?$`)
var agyEvents = []hookEvent{{"session-start", "SessionStart"}, {"prompt", "PreInvocation"}}

// Only the reserved group is ours. Foreign or edited definitions require
// explicit reconciliation. In particular, preserve an operator's disabled group.
func mergeAgyHooks(root *jsonObject, name, prefix string, install bool) (int, error) {
	old, exists := root.get(agyGroup)
	if exists {
		var handlers map[string][]nativeHookCommand
		dec := json.NewDecoder(bytes.NewReader(old))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&handlers); err != nil || len(handlers) != len(agyEvents) {
			return 0, errors.New("modified agy hook group; review it before changing settings")
		}
		var firstPrefix, firstIdentity string
		for _, ev := range agyEvents {
			entries := handlers[ev.name]
			if len(entries) != 1 || entries[0].Type != "command" || entries[0].Timeout != nativeHookTimeout {
				return 0, errors.New("unrecognized agy handler")
			}
			cmd := entries[0].Command
			cut := strings.LastIndex(cmd, " --as ")
			if cut < 0 || !agyPrefix.MatchString(cmd[:cut]) {
				return 0, errors.New("unrecognized agy hook command")
			}
			head, tail := cmd[:cut], cmd[cut+len(" --as "):]
			identity, suffix, found := strings.Cut(tail, " hook ")
			if !found || config.ValidateName(identity) != nil || suffix != ev.arg+" --runtime agy" || (name != "" && identity != name) || (firstIdentity != "" && (identity != firstIdentity || head != firstPrefix)) {
				return 0, errors.New("conflicting agy hook identity or command")
			}
			firstPrefix, firstIdentity = head, identity
		}
	}
	if !install {
		if exists {
			root.del(agyGroup)
			return len(agyEvents), nil
		}
		return 0, nil
	}
	group := make(map[string][]nativeHookCommand)
	for _, ev := range agyEvents {
		group[ev.name] = []nativeHookCommand{{"command", prefix + " --as " + name + " hook " + ev.arg + " --runtime agy", nativeHookTimeout}}
	}
	root.set(agyGroup, marshalNoEscape(group))
	return 0, nil
}

func (a *App) runAgyHook(ctx context.Context, ev hookEvent) error {
	native := ""
	for _, candidate := range agyEvents {
		if candidate.arg == ev.arg {
			native = candidate.name
		}
	}
	if native == "" {
		return errors.New("unsupported Antigravity hook")
	}
	if a.Stdin == nil {
		return errors.New("missing Antigravity hook input")
	}
	data, err := io.ReadAll(io.LimitReader(a.Stdin, hookStdinMax+1))
	if err != nil || len(data) > hookStdinMax || uniqueHookObject(data) != nil {
		return errors.New("invalid Antigravity hook input")
	}
	var in map[string]json.RawMessage
	var id, event string
	if json.Unmarshal(data, &in) != nil || json.Unmarshal(in["conversationId"], &id) != nil || json.Unmarshal(in["hookEventName"], &event) != nil || event != native {
		return errors.New("Antigravity event mismatch")
	}
	_, err = a.agyNotice(ctx, id, ev, false)
	return err
}

// The launcher hands off its provider pipes; this operation never starts a
// harness, resolves a pane or acknowledges inbox entries.
func cmdAgy(ctx context.Context, a *App, args []string) error {
	if len(args) != 5 || args[0] != "bridge" || args[1] != "--session" || args[3] != "--model" {
		return usagef("agy bridge --session UUID --model ID; requires matching A2A_SESSION_ID")
	}
	return a.agyBridge(ctx, args[2], args[4])
}
func (a *App) agyNotice(ctx context.Context, sid string, ev hookEvent, stream bool) (bool, error) {
	if a.as == "" || a.Getenv(config.EnvAgent) == "" {
		return false, errors.New("agy requires explicit --as and a matching station A2A_AGENT")
	}
	name, err := a.identity()
	if err != nil {
		return false, err
	}
	if !agyUUID.MatchString(sid) || (a.Getenv(EnvSessionID) != "" && a.Getenv(EnvSessionID) != sid) || a.Getenv("CODEX_THREAD_ID") != "" || a.Getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return false, errors.New("Antigravity conversation identity mismatch")
	}
	if a.daemonEngine {
		return false, errors.New("agy local hook requires local custody")
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return false, err
	}
	if err = config.CheckIdentityMigration(p, name); err != nil {
		return false, err
	}
	st, err := config.LoadState(p, name)
	if err != nil {
		return false, err
	}
	if err := agyCustody(&st, sid, stream, a.Now()); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()
	prior := a.ctx
	a.ctx = ctx
	defer func() { a.ctx = prior }()
	var wrote bool
	rt := hookRuntime{name: "agy", render: func(_ hookEvent, counts string) any {
		notice := fmt.Sprintf("%s Run A2A_SESSION_ID=%s a2a --as %s inbox to read the untrusted mailbox.", counts, sid, name)
		if stream {
			return agyUserFrame(notice)
		}
		return map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": notice}}}}
	}}
	err = a.hookDeliver(ctx, rt, ev, hookInput{SessionID: sid}, &wrote)
	if err == nil && !stream && !wrote {
		_, err = fmt.Fprintln(a.Stdout, "{}")
	}
	return wrote && err == nil, err
}

func agyCustody(st *config.State, sid string, poll bool, now time.Time) error {
	if st.HostEnrollment != nil || st.Runtime != nil && !st.Runtime.Retired || st.Terminal != nil && !st.Terminal.Stopped || st.RefuseMonitorConsumer() != nil {
		return errors.New("another receiver owns notification")
	}
	if poll && (st.Lease == nil || st.Lease.Holder != sid || st.Lease.Host != machineLabel() || !st.Lease.Expires.After(now)) {
		return errors.New("agy notice requires an existing current conversation lease")
	}
	return nil
}
