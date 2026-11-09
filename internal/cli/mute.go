package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// cmdMute: a2a mute <room> --topic X. Thread replies and ordinary room-scoped
// subscription matches without a sender filter stop ringing. Addresses, mentions,
// sender-filtered and roomless subscriptions
// and diagnostic capture retain their delivery semantics. The mute is
// sticky: posting in the thread again does not re-follow it; only follow does.
func cmdMute(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "list" {
		return cmdMuteList(a, args[1:])
	}
	return followCommand(ctx, a, args, false)
}

// cmdMuteList reads only the selected identity's local state. It does not
// resolve rooms/topics, load credentials, sync, acquire a lease or save state.
func cmdMuteList(a *App, args []string) error {
	fs := newFlagSet("mute list")
	asJSON := fs.Bool("json", false, "print identity and muted root IDs as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a mute list [--json]")
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return err
	}
	st, err := config.LoadState(p, name)
	if err != nil {
		return err
	}
	roots := append([]string{}, st.Muted...)
	slices.Sort(roots)
	if *asJSON {
		data, err := json.Marshal(struct {
			Identity string   `json:"identity"`
			Muted    []string `json:"muted"`
		}{Identity: name, Muted: roots})
		if err != nil {
			return err
		}
		return writeCLIOutput(a.Stdout, string(escapeJSONFormatCharacters(data))+"\n")
	}
	if len(roots) == 0 {
		return writeCLIOutput(a.Stdout, "no muted topics\n")
	}
	for i, root := range roots {
		roots[i] = sanitizeLine(root)
	}
	return writeCLIOutput(a.Stdout, strings.Join(roots, "\n")+"\n")
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
	resolve := s.resolveTopic
	if !follow {
		resolve = s.resolveMuteTopic
	}
	root, slug, err := resolve(ctx, roomID, label, *topic)
	if err != nil {
		return err
	}
	changed := false
	if _, err := a.updateState(s.paths, s.name, func(cur *config.State) error {
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

// resolveMuteTopic can use membership this identity already holds when history
// visibility hides the root. It neither fetches a child/root to establish new
// evidence nor trusts a sender's header claim. Other resolution stays unchanged.
func (s *session) resolveMuteTopic(ctx context.Context, roomID, label, topic string) (string, string, error) {
	if matrix.ValidVerificationID(topic, '$') {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return "", "", err
		}
		for _, e := range st.ThreadEvents {
			if e.Room == roomID && e.Root == topic {
				return topic, "", nil
			}
		}
		mb, err := config.OpenMailbox(s.paths, s.name)
		if err != nil {
			return "", "", err
		}
		for _, e := range mb.Pending() {
			if e.Kind != config.MailEvent || e.Room != roomID {
				continue
			}
			var ev matrix.Event
			if json.Unmarshal(e.Event, &ev) == nil && ev.EventID == e.ID &&
				(ev.RoomID == "" || ev.RoomID == roomID) && ev.Type == "m.room.message" &&
				!ev.IsRedacted() && ev.EventID != topic && eventTopicRoot(ev) == topic {
				return topic, "", nil
			}
		}
	}
	return s.resolveTopic(ctx, roomID, label, topic)
}
