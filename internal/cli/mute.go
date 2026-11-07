package cli

import (
	"context"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// cmdMute: a2a mute <room> --topic X. Thread replies and ordinary room-scoped
// subscription matches without a sender filter stop ringing. Addresses, mentions,
// sender-filtered and roomless subscriptions
// and diagnostic capture retain their delivery semantics. The mute is
// sticky: posting in the thread again does not re-follow it; only follow does.
func cmdMute(ctx context.Context, a *App, args []string) error {
	return followCommand(ctx, a, args, false)
}

// cmdFollow: a2a follow <room> --topic X, the inverse of mute.
func cmdFollow(ctx context.Context, a *App, args []string) error {
	return followCommand(ctx, a, args, true)
}

func followCommand(ctx context.Context, a *App, args []string, follow bool) error {
	name := map[bool]string{true: "follow", false: "mute"}[follow]
	fs := newFlagSet(name)
	topic := fs.String("topic", "", "topic slug or root event id")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *topic == "" {
		return usagef("usage: a2a %s <room> --topic <slug|$root>", name)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	root, slug, err := s.resolveTopic(ctx, roomID, label, *topic)
	if err != nil {
		return err
	}
	changed := false
	if _, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
		if follow {
			changed = cur.Follow(root)
		} else {
			changed = cur.Mute(root)
		}
		return nil
	}); err != nil {
		return err
	}
	state := map[bool]string{true: "followed", false: "muted"}[follow]
	if !changed {
		state = "already " + state
	}
	a.printf("%s\t%s\t%s\n", sanitizeLine(firstNonEmpty(slug, root)), root, state)
	return nil
}
