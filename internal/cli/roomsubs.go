package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Room subscriptions (owner requirement, protocol rule 24): joining a room
// subscribes the agent to that room's kinds system, owner-needed and decision
// (onboardKinds), in the same step, printed; leaving removes them. Addressed
// messages, mentions and followed threads ring as always. Two rooms are
// skipped, with the reason printed: #system (a projection room; projections
// never ring through subscriptions) and #broadcast (it rings every member).

// roomSubSkip returns why roomID is exempt, or "" only after successful
// classification. An absent alias is allowed; an unreadable alias is unknown.
func (s *session) roomSubSkip(ctx context.Context, roomID string) (string, error) {
	for _, exemption := range []struct{ alias, why string }{
		{SystemRoom, "a projection room; watch it with wait --room system --all if you are a keeper"},
		{BroadcastRoom, "broadcasts ring every member without a subscription"},
	} {
		norm, err := normalizeRoom(exemption.alias, s.server)
		if err != nil {
			return "", err
		}
		id, err := s.client.ResolveAlias(ctx, norm)
		if err != nil {
			if errors.Is(err, matrix.ErrUnknownAlias) {
				continue
			}
			return "", fmt.Errorf("resolve subscription exemption %s: %w", exemption.alias, err)
		}
		if id == roomID {
			return exemption.why, nil
		}
	}
	return "", nil
}

// subscribeRoom ensures the room subscription for roomID and prints it.
func (s *session) subscribeRoom(ctx context.Context, a *App, roomID, label string, now time.Time) error {
	return config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		return s.subscribeRoomWithPolicy(ctx, a, roomID, label, now)
	})
}

func (s *session) subscribeRoomWithPolicy(ctx context.Context, a *App, roomID, label string, now time.Time) error {
	why, err := s.roomSubscriptionSkip(ctx, roomID)
	if err != nil {
		return err
	}
	if why != "" {
		a.printf("no room subscription for %s: %s\n", label, why)
		return nil
	}
	policy, err := s.subscriptionPolicy()
	if err != nil {
		return err
	}
	kinds := withoutTrim(onboardKinds, policy.Trims[roomID])
	if len(kinds) == 0 {
		a.printf("trimmed %s\tall standard kinds deliberately omitted\n", label)
		return nil
	}
	sub, created, err := s.ensureSub(ctx, SubSelector{Room: roomID, Kinds: kinds}, now, policy)
	if err != nil {
		return err
	}
	state := "already subscribed"
	if created {
		state = "subscribed"
	}
	a.printf("%s %s\t%s\n", state, sub.ID, sub.summary(label, s.server))
	return nil
}

// Baseline onboarding and ordinary join/repair share the same exemptions.
// Unreadable type or exemption-alias metadata fails before adding a baseline.
func (s *session) roomSubscriptionSkip(ctx context.Context, roomID string) (string, error) {
	var create struct {
		Type string `json:"type"`
	}
	if err := s.client.GetState(ctx, roomID, "m.room.create", "", &create); err != nil {
		return "", err
	}
	if create.Type == "m.space" {
		return "space", nil
	}
	return s.roomSubSkip(ctx, roomID)
}

// unsubscribeRoom removes every subscription scoped to roomID and prints them.
func (s *session) unsubscribeRoom(ctx context.Context, a *App, roomID, label string) error {
	var removed []string
	_, _, err := s.updateSubs(ctx, func(_ *config.State, list *SubList) (bool, error) {
		for _, cur := range list.Subs {
			if cur.Room == roomID {
				removed = append(removed, cur.ID)
			}
		}
		return list.remove(removed...), nil
	})
	if err != nil {
		return err
	}
	for _, id := range removed {
		a.printf("unsubscribed %s (%s)\n", id, label)
	}
	return nil
}

// cmdSubsSync: a2a subs sync. Ensures the room subscription for every joined
// room (idempotent) and prints what it created or found.
func cmdSubsSync(ctx context.Context, a *App, args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--preset") {
			return cmdSubsPresetSync(ctx, a, args)
		}
	}
	fs := newFlagSet("subs sync")
	var owners ownerListFlag
	fs.Var(&owners, "owner", "owner account for this sync only (repeatable; replaces owners.json for this invocation)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subs sync [--owner @owner:server]...")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for _, id := range rooms {
		if err := s.subscribeRoom(ctx, a, id, s.roomLabel(ctx, id, labels), a.Now()); err != nil {
			return err
		}
	}
	return s.ensureOwnerSubs(ctx, a, owners)
}
