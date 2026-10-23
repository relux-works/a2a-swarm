package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// One-command onboarding (owner requirement, 2026-09-29): after the identity,
// `a2a identity ensure` joins the swarm rooms, creates the onboarding
// subscriptions, installs the notification hooks and prints a summary. Every
// step is printed, nothing is a hidden default, and a re-run changes nothing
// (it says "already" / "unchanged").

const (
	// BroadcastRoom is joined when it exists (it may not exist yet).
	BroadcastRoom = "#broadcast"
	// SystemRoom is the #system projection room (rule 24).
	SystemRoom = "#system"
	// envClaudeCode is set to "1" by Claude Code in every command it runs.
	envClaudeCode = "CLAUDECODE"
)

// codexMarkers are environment variables Codex sets in the commands it runs.
// They are never used to pick codex; they only make a CLAUDECODE ambiguous.
var codexMarkers = []string{"CODEX_THREAD_ID", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED"}

// onboardKinds are the kinds the project room subscription delivers.
var onboardKinds = []string{"system", "owner-needed", "decision"}

// onboarding is what identity ensure resolved before the onboarding steps.
type onboarding struct {
	host, project, role string
	dir                 string // the project directory (hooks file)
	rt                  hookRuntime
	rtSource            string // "--runtime" or "detected: CLAUDECODE=1"
	keeper              bool
}

// ensureRuntime resolves the hook runtime: --runtime wins; without it only an
// unambiguous Claude Code environment (CLAUDECODE=1 and no Codex marker) is
// accepted. Anything else is a usage error asking for --runtime.
func (a *App) ensureRuntime(flagValue string) (hookRuntime, string, error) {
	if flagValue == "none" {
		// A service account (e.g. the projector) has no session to notify.
		return hookRuntime{name: "none", title: "none"}, "--runtime", nil
	}
	if flagValue != "" {
		rt, err := lookupRuntime(flagValue)
		if err != nil {
			return hookRuntime{}, "", usagef("%v; %s", err, identityUsage)
		}
		return rt, "--runtime", nil
	}
	cc := a.Getenv(envClaudeCode)
	var markers []string
	for _, k := range codexMarkers {
		if a.Getenv(k) != "" {
			markers = append(markers, k)
		}
	}
	if cc == "1" && len(markers) == 0 {
		rt, err := lookupRuntime("claude")
		return rt, "detected: " + envClaudeCode + "=1", err
	}
	if cc != "" || len(markers) > 0 {
		set := markers
		if cc != "" {
			set = append([]string{envClaudeCode + "=" + sanitizeLine(cc)}, markers...)
		}
		return hookRuntime{}, "", usagef("the runtime is ambiguous (%s): pass --runtime %s", strings.Join(set, ", "), runtimeNames)
	}
	return hookRuntime{}, "", usagef("cannot detect the runtime (no %s=1 in the environment): pass --runtime %s", envClaudeCode, runtimeNames)
}

// onboardRoom is one room of the onboarding.
type onboardRoom struct {
	label, id string
	state     string // joined, already, created, skipped
}

// onboard runs the onboarding steps for s and prints each one, then the summary.
func (a *App) onboard(ctx context.Context, s *session, o onboarding) error {
	joined, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("list joined rooms: %w", err)
	}
	in := map[string]bool{}
	for _, id := range joined {
		in[id] = true
	}
	// Resolve first, so a missing required room fails before anything changes.
	resolve := func(alias string) (string, string, error) {
		id, label, err := s.resolveRoom(ctx, alias)
		if errors.Is(err, matrix.ErrUnknownAlias) {
			return "", s.shortAlias(mustNormalize(alias, s.server)), nil
		}
		return id, label, err
	}
	lobbyID, lobbyLabel, err := resolve(LobbyRoom)
	if err != nil {
		return err
	}
	if lobbyID == "" {
		return fmt.Errorf("%s does not exist on %s: the swarm is not set up (ask a2a-swarm-ac)", LobbyRoom, s.server)
	}
	bcID, bcLabel, err := resolve(BroadcastRoom)
	if err != nil {
		return err
	}
	projAlias := "#" + o.project
	projID, projLabel, err := resolve(projAlias)
	if err != nil {
		return err
	}
	var sysID, sysLabel string
	if o.keeper {
		if sysID, sysLabel, err = resolve(SystemRoom); err != nil {
			return err
		}
		if sysID == "" {
			return fmt.Errorf("%s does not exist on %s: --keeper subscribes to it (ask a2a-swarm-ac)", SystemRoom, s.server)
		}
	}

	a.printf("rooms:\n")
	var rooms []onboardRoom
	join := func(id, label string) error {
		if in[id] {
			a.printf("already in %s %s\n", label, id)
			rooms = append(rooms, onboardRoom{label, id, "already"})
			return nil
		}
		if _, err := s.client.Join(ctx, id); err != nil {
			if matrix.IsCode(err, "M_FORBIDDEN") {
				return fmt.Errorf("join %s: forbidden (the room is invite-only; ask a member: a2a invite %s %s)", label, label, s.name)
			}
			return fmt.Errorf("join %s: %w", label, err)
		}
		in[id] = true
		a.printf("joined %s %s\n", label, id)
		rooms = append(rooms, onboardRoom{label, id, "joined"})
		return nil
	}
	if err := join(lobbyID, lobbyLabel); err != nil {
		return err
	}
	if bcID == "" {
		a.printf("%s does not exist yet; skipped\n", BroadcastRoom)
		rooms = append(rooms, onboardRoom{bcLabel, "", "skipped"})
	} else if err := join(bcID, bcLabel); err != nil {
		return err
	}
	if projID == "" {
		id, created, err := s.newRoom(ctx, roomSpec{local: o.project})
		if err != nil {
			return err
		}
		projID = id
		if created {
			in[id] = true
			rooms = append(rooms, onboardRoom{projLabel, id, "created"})
		} else if err := join(id, projLabel); err != nil {
			return err
		}
	} else if err := join(projID, projLabel); err != nil {
		return err
	}
	if o.keeper {
		if err := join(sysID, sysLabel); err != nil {
			return err
		}
	}

	a.printf("subscriptions:\n")
	var subs []string
	sels := []struct {
		sel   SubSelector
		label string
	}{{SubSelector{Room: projID, Kinds: onboardKinds}, projLabel}, {SubSelector{Room: lobbyID, Kinds: onboardKinds}, lobbyLabel}}
	if o.keeper {
		sels = append(sels, struct {
			sel   SubSelector
			label string
		}{SubSelector{Room: sysID, AllEvents: true}, sysLabel})
	}
	for _, x := range sels {
		if !x.sel.AllEvents {
			why, err := s.roomSubscriptionSkip(ctx, x.sel.Room)
			if err != nil {
				return err
			}
			if why != "" {
				a.printf("no room subscription for %s: %s\n", x.label, why)
				continue
			}
		}
		sub, created, err := s.ensureOnboardSub(ctx, x.sel, a.Now())
		if err != nil {
			return err
		}
		if sub.ID == "" {
			a.printf("trimmed %s\tno onboarding subscription added under recorded trim\n", x.label)
			continue
		}
		state := "already"
		if created {
			state = "created"
		}
		line := sub.ID + "\t" + sub.summary(x.label, s.server)
		a.printf("%s %s\n", state, line)
		subs = append(subs, line)
	}

	hooksFile := "none (service account: no session to notify)"
	if o.rt.name == "none" {
		a.printf("hooks: none (--runtime none: a service account, no session to notify)\n")
	} else {
		a.printf("hooks (%s, %s):\n", o.rt.name, o.rtSource)
		hooksFile = filepath.Join(o.dir, o.rt.settings)
		if abs, err := filepath.Abs(hooksFile); err == nil {
			hooksFile = abs
		}
		if err := cmdHookInstall(a, []string{"--agent", s.name, "--runtime", o.rt.name, "--settings", hooksFile}, true); err != nil {
			return err
		}
		a.printWakeGuidance(o.rt.name)
	}

	operator := sanitizeLine(a.Getenv("USER"))
	if operator == "" {
		operator = "unknown (USER unset)"
	}
	a.printf("\n== a2a onboarding summary ==\n")
	a.printf("identity:  %s\n", s.name)
	a.printf("mxid:      %s\n", sanitizeLine(s.creds.UserID))
	a.printf("operator:  %s   host: %s   project: %s   role: %s\n", operator, o.host, o.project, o.role)
	for i, r := range rooms {
		head := "rooms:    "
		if i > 0 {
			head = "          "
		}
		if r.id == "" {
			a.printf("%s %s (%s)\n", head, r.label, r.state)
		} else {
			a.printf("%s %s %s (%s)\n", head, r.label, r.id, r.state)
		}
	}
	for i, line := range subs {
		head := "subscriptions:"
		if i > 0 {
			head = "              "
		}
		a.printf("%s %s\n", head, line)
	}
	a.printf("hooks:     %s (%s)\n", sanitizeLine(hooksFile), o.rt.title)
	a.printf("next steps:\n")
	a.printf("  1. write agents/%s.md in the a2a-swarm repo (see the other cards), commit it straight to main, then: a2a card set --file agents/%s.md\n", s.name, s.name)
	a.printf("  2. post a join notice in %s: a2a topic new lobby join-%s \"%s joined: <what you do>\"\n", LobbyRoom, strings.ReplaceAll(s.name, "_", "-"), s.name)
	return nil
}

// Advice is conditional: installing hooks does not establish a live binding.
// Do not inspect or change runtime state just to print onboarding instructions.
func (a *App) printWakeGuidance(runtime string) {
	a.printf("Read messages with `a2a inbox`. Inspect your own binding with `a2a bind status`.\n")
	a.printf("With a live binding, use its supervised consumer; do not start a parallel wait/watch or Monitor.\n")
	if runtime == "claude" {
		a.printf("Only for an unbound Claude fallback, keep one supervised background `a2a wait --timeout 0` and re-arm it after completion.\n")
	} else if runtime == "codex" {
		a.printf("For idle Codex wake, use the owning launcher's binding; a background `a2a wait` alone cannot wake idle Codex.\n")
	}
}

// mustNormalize is normalizeRoom for the built-in room constants.
func mustNormalize(alias, server string) string {
	n, err := normalizeRoom(alias, server)
	if err != nil {
		return alias
	}
	return n
}

// equal reports whether two selectors deliver the same events (kinds are a set).
func (sel SubSelector) equal(o SubSelector) bool {
	return sel.Room == o.Room && sel.Topic == o.Topic && sel.From == o.From && sel.Subject == o.Subject &&
		strings.Join(sel.subtypeList(), ",") == strings.Join(o.subtypeList(), ",") && sel.Until == o.Until && sel.UntilKind == o.UntilKind && sel.AllEvents == o.AllEvents &&
		kindSet(sel.Kinds) == kindSet(o.Kinds)
}

func kindSet(kinds []string) string {
	k := append([]string(nil), kinds...)
	sort.Strings(k)
	return strings.Join(k, ",")
}

// ensureSub returns the live subscription with selector sel, adding it only
// when none exists (created reports which).
func (s *session) ensureSub(ctx context.Context, sel SubSelector, now time.Time) (sub Sub, created bool, err error) {
	if err := sel.validate(); err != nil {
		return Sub{}, false, err
	}
	_, _, err = s.updateSubs(ctx, func(_ *config.State, list *SubList) (bool, error) {
		for _, cur := range list.Subs {
			if cur.SubSelector.equal(sel) && !cur.expiredAt(now) {
				sub = cur
				return false, nil
			}
		}
		sub = Sub{ID: newSubID(*list), SubSelector: sel, CreatedAt: now.UTC().Truncate(time.Millisecond).Format(createdAtLayout)}
		list.Subs = append(list.Subs, sub)
		created = true
		return true, nil
	})
	return sub, created, err
}
