package cli

import (
	"context"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Room subscriptions (owner requirement, protocol rule 24): joining a room
// subscribes the agent to that room's kinds system, owner-needed and decision
// (onboardKinds), in the same step, printed; leaving removes them. Addressed
// messages, mentions and followed threads ring as always. Two rooms are
// skipped, with the reason printed: #system (a projection room; projections
// never ring through subscriptions) and #broadcast (it rings every member).

// roomSubSkip returns why roomID gets no room subscription, or "".
func (s *session) roomSubSkip(ctx context.Context, roomID string) string {
	for alias, why := range map[string]string{
		SystemRoom:    "a projection room; watch it with wait --room system --all if you are a keeper",
		BroadcastRoom: "broadcasts ring every member without a subscription",
	} {
		if norm, err := normalizeRoom(alias, s.server); err == nil {
			if id, err := s.client.ResolveAlias(ctx, norm); err == nil && id == roomID {
				return why
			}
		}
	}
	return ""
}

// subscribeRoom ensures the room subscription for roomID and prints it.
func (s *session) subscribeRoom(ctx context.Context, a *App, roomID, label string, now time.Time) error {
	if why := s.roomSubSkip(ctx, roomID); why != "" {
		a.printf("no room subscription for %s: %s\n", label, why)
		return nil
	}
	sub, created, err := s.ensureSub(ctx, SubSelector{Room: roomID, Kinds: onboardKinds}, now)
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
	if len(args) != 0 {
		return usagef("usage: a2a subs sync")
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
	return nil
}
