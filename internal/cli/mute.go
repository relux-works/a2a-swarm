package cli

import (
	"context"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// cmdMute: a2a mute <room> --topic X. Replies in that thread stop ringing
// (addressed messages, mentions and kind subscriptions still do). The mute is
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
	fs.Usage = func() {
		a.printf("usage: a2a %s <room> --topic <slug|$root>\n", name)
		a.printf("Explicit follow enables thread replies and clears its mute.\n")
		a.printf("Mute excludes follows and topic-scoped subscriptions; direct-to, mentions, pinned owner messages and independent subscription scopes remain eligible.\n")
		a.printf("Posts and history never follow or lift a mute.\n")
	}
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
		if err := s.migrateThreadPolicy(ctx, cur); err != nil {
			return err
		}
		if follow {
			changed = cur.Follow(roomID, root)
		} else {
			changed = cur.Mute(root)
		}
		return nil
	}); err != nil {
		return err
	}
	state := map[bool]string{true: "followed (thread replies enabled; mute cleared)", false: "muted (thread-only replies excluded)"}[follow]
	if !changed {
		state = "already " + state
	}
	a.printf("%s\t%s\t%s\n", sanitizeLine(firstNonEmpty(slug, root)), root, state)
	return nil
}
