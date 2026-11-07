package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const principalSubSource = "host-principal"

// IDs are exact, case-sensitive identities, including historical localparts.
// Validate routing syntax but never lowercase a different identity into authority.
func validPrincipalID(id string) bool {
	if !ownerMXID.MatchString(id) || len(id) > 255 || !utf8.ValidString(id) || strings.ContainsAny(id, "\x00\r\n\t") {
		return false
	}
	_, server, _ := strings.Cut(id[1:], ":")
	return config.ValidateMatrixRouting("https://principal.invalid", server) == nil
}

// Principal authority is host-local and per identity, beside the owner pin.
// Matrix membership only determines where an already configured ID is present.
type principalConfig struct {
	V          int      `json:"v"`
	Principals []string `json:"principals"`
}

func (f ownerDMFiles) principals() ([]string, error) {
	b, st, err := ownerRead(f.path("principals.json"))
	if err != nil {
		return nil, fmt.Errorf("read host principals: %w", err)
	}
	if st == nil {
		return nil, nil
	}
	var c principalConfig
	if err := config.DecodeConnectionJSON(b, &c); err != nil || c.V != 1 || c.Principals == nil {
		return nil, errors.New("invalid host principals configuration")
	}
	for _, id := range c.Principals {
		if !validPrincipalID(id) {
			return nil, errors.New("host principals require exact Matrix user IDs")
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(c.Principals))), nil
}

const principalsUsage = "usage: a2a host principals set @user:server ... | show [--json] | clear (select identity with --as or A2A_AGENT)"

// Host configuration does not open a Matrix session or provision credentials.
func cmdHostPrincipals(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", principalsUsage)
	}
	var ids []string
	asJSON := false
	switch args[0] {
	case "show":
		if len(args) == 2 && args[1] == "--json" {
			asJSON = true
		} else if len(args) != 1 {
			return usagef("%s", principalsUsage)
		}
	case "set":
		if len(args) < 2 {
			return usagef("%s", principalsUsage)
		}
		for _, id := range args[1:] {
			if !validPrincipalID(id) {
				return usagef("host principals require exact Matrix user IDs")
			}
			ids = append(ids, id)
		}
		ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	case "clear":
		if len(args) != 1 {
			return usagef("%s", principalsUsage)
		}
		ids = []string{}
	default:
		return usagef("%s", principalsUsage)
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	f := ownerFiles(p, name)
	if args[0] == "show" {
		ids, err = f.principals()
	} else {
		err = config.WithRuntimeLock(p, name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
			return ownerWrite(ctx, f.path("principals.json"), principalConfig{V: 1, Principals: ids})
		})
	}
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []string{}
	}
	if asJSON {
		return a.printJSON(principalConfig{V: 1, Principals: ids})
	}
	if len(ids) == 0 {
		a.printf("principals: none configured\n")
	} else {
		a.printf("%s\n", strings.Join(ids, "\n"))
	}
	return nil
}

func (s *session) roomPrincipals(ctx context.Context, room string, configured []string) ([]string, error) {
	var joined []string
	for _, id := range configured {
		var member struct {
			Membership string `json:"membership"`
		}
		if err := s.client.GetState(ctx, room, "m.room.member", id, &member); err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read principal membership in %s: %w", sanitizeLine(room), err)
		}
		switch member.Membership {
		case "join":
			joined = append(joined, id)
		case "invite", "leave", "ban", "knock":
		default:
			return nil, fmt.Errorf("invalid principal membership in %s", sanitizeLine(room))
		}
	}
	return joined, nil
}

// Principal rows belong to this policy, never to a DM, preset or manual row.
// Coverage must last: temporary and one-shot rows cannot protect a principal.
// From is compared byte for byte; no display names or message text are used.
func coversPrincipal(sub Sub, room, principal string, now time.Time) bool {
	if sub.Source != principalSubSource || sub.Preset != "" ||
		sub.validate() != nil || !sub.AllEvents || sub.Room != room ||
		sub.From != principal || len(sub.Kinds) > 0 ||
		sub.Topic != "" || sub.Product != "" || sub.Subject != "" ||
		len(sub.subtypeList()) > 0 || sub.Until != "" || sub.UntilKind != "" {
		return false
	}
	if sub.Since != "" {
		return sub.SinceTS <= now.UnixMilli()
	}
	if sub.CreatedAt == "" {
		return true // Existing selectors without a creation gate match all new events.
	}
	created, err := time.Parse(time.RFC3339Nano, sub.CreatedAt)
	return err == nil && !created.After(now)
}

func missingRoomPrincipals(room string, principals []string, list SubList, now time.Time) []string {
	var missing []string
	for _, principal := range principals {
		if !slices.ContainsFunc(list.Subs, func(sub Sub) bool { return coversPrincipal(sub, room, principal, now) }) {
			missing = append(missing, principal)
		}
	}
	return missing
}

func repairPrincipalSubs(list *SubList, room string, principals []string, now time.Time) bool {
	missing := missingRoomPrincipals(room, principals, *list, now)
	for _, id := range missing {
		list.Subs = append(list.Subs, Sub{ID: newSubID(*list),
			SubSelector: SubSelector{Room: room, From: id, AllEvents: true},
			CreatedAt:   legacyCreatedAt, Source: principalSubSource})
	}
	return len(missing) > 0
}

// PrincipalSubscriptionError is a typed refusal; no policy/subscription write
// has occurred when a trim returns it.
type PrincipalSubscriptionError struct {
	Reason    string
	Room      string
	Principal string
}

const PrincipalSubscriptionRequired = "principal_subscription_required"

func (e *PrincipalSubscriptionError) Error() string {
	return fmt.Sprintf("%s: subscription removal/narrowing refused in %s; preserve all-events subscription for %s", e.Reason, sanitizeLine(e.Room), sanitizeLine(e.Principal))
}

// Protect dedicated rows without a membership snapshot: even a principal who
// has left may have an undelivered message. Clearing host configuration is the
// explicit opt-out. No unsubscribe --force escape hatch exists.
func protectedPrincipalSub(sub Sub, configured []string) bool {
	if !slices.Contains(configured, sub.From) {
		return false
	}
	// Include pre-marker exact-sender rows from the original principal repair.
	return sub.Source == principalSubSource || sub.AllEvents && sub.Room != "" &&
		sub.Topic == "" && sub.Product == "" && sub.Subject == "" &&
		len(sub.Kinds) == 0 && len(sub.subtypeList()) == 0 && sub.Until == "" && sub.UntilKind == ""
}

func removeSubscriptions(list *SubList, ids, configured []string) ([]string, error) {
	for _, sub := range list.Subs {
		if slices.Contains(ids, sub.ID) && protectedPrincipalSub(sub, configured) {
			return nil, &PrincipalSubscriptionError{Reason: PrincipalSubscriptionRequired, Room: sub.Room, Principal: sub.From}
		}
	}
	list.remove(ids...)
	return ids, nil
}

// Delivery coverage is derived from host authority for each received room. It
// needs neither account-data repair nor membership queries: the real sender of
// the event is sufficient, even if that sender subsequently left the room.
// Replace persisted policy rows so a stale/cross-host account-data write cannot
// narrow delivery. These synthetic selectors are not written to Matrix.
func principalDeliverySubs(subs []Sub, rooms, configured []string) []Sub {
	out := make([]Sub, 0, len(subs)+len(rooms)*len(configured))
	for _, sub := range subs {
		if sub.Source != principalSubSource {
			if protectedPrincipalSub(sub, configured) {
				// Pre-marker principal rows must not authenticate a projection
				// origin either. Leave ordinary projection semantics unchanged.
				sub.Source = principalSubSource
			}
			out = append(out, sub)
		}
	}
	for _, room := range rooms {
		for _, id := range configured {
			out = append(out, Sub{ID: "principal:" + room + ":" + id,
				SubSelector: SubSelector{Room: room, From: id, AllEvents: true},
				CreatedAt:   legacyCreatedAt, Source: principalSubSource})
		}
	}
	return out
}

func principalMail(e config.MailEntry, configured []string) bool {
	if e.Kind != config.MailEvent || len(configured) == 0 {
		return false
	}
	var ev matrix.Event
	return json.Unmarshal(e.Event, &ev) == nil && ev.Type == "m.room.message" && slices.Contains(configured, ev.Sender)
}

func initialPrincipalEntries(entries []config.MailEntry, configured []string) []config.MailEntry {
	var out []config.MailEntry
	for _, entry := range entries {
		if entry.Kind != config.MailEvent || principalMail(entry, configured) {
			out = append(out, entry)
		}
	}
	return out
}

func principalTrimGuard(room string, principals []string) error {
	if len(principals) > 0 {
		return &PrincipalSubscriptionError{Reason: PrincipalSubscriptionRequired, Room: room, Principal: principals[0]}
	}
	return nil
}

func (s *session) ensureRoomPrincipals(ctx context.Context, room string, now time.Time) error {
	configured, err := ownerFiles(s.paths, s.name).principals()
	if err != nil || len(configured) == 0 {
		return err
	}
	policy, err := s.subscriptionPolicy()
	if err != nil {
		return err
	}
	principals, err := s.roomPrincipals(ctx, room, configured)
	if err != nil || len(principals) == 0 {
		return err
	}
	var added []Sub
	_, _, err = s.updateSubsWithPolicy(ctx, policy, func(_ *config.State, list *SubList) (bool, error) {
		before := len(list.Subs)
		changed := repairPrincipalSubs(list, room, principals, now)
		added = append(added, list.Subs[before:]...)
		return changed, nil
	})
	if err != nil {
		return err
	}
	for _, sub := range added {
		s.app.printf("subscribed %s\t%s (host principal)\n", sub.ID, sub.summary(room, s.server))
	}
	return nil
}

// Preset cleanup needs every joined room, including direct rooms outside its
// product plan. No Matrix reads are added when this host has no principals.
func (s *session) principalRooms(ctx context.Context) (map[string][]string, error) {
	configured, err := ownerFiles(s.paths, s.name).principals()
	if err != nil || len(configured) == 0 {
		return nil, err
	}
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, room := range rooms {
		principals, err := s.roomPrincipals(ctx, room, configured)
		if err != nil {
			return nil, err
		}
		out[room] = principals
	}
	return out, nil
}
