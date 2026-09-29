package cli

import (
	"context"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// parseSubArgs handles the shared --kind/--room flags of subscribe and unsubscribe.
func parseSubArgs(name string, args []string, kindsRequired bool) (kinds []string, room string, err error) {
	fs := newFlagSet(name)
	kind := fs.String("kind", "", "comma-separated message kinds")
	roomArg := fs.String("room", "all", "room, or all")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, "", err
	}
	if len(pos) != 0 {
		return nil, "", usagef("usage: a2a %s --kind k1,k2 [--room R|all]", name)
	}
	kinds = splitList(*kind)
	if kindsRequired && len(kinds) == 0 {
		return nil, "", usagef("--kind is required")
	}
	for _, k := range kinds {
		if !validKind(k) {
			return nil, "", usagef("invalid --kind %q (one of %s)", sanitizeLine(k), strings.Join(Kinds, ", "))
		}
	}
	return kinds, *roomArg, nil
}

// resolveSubRoom maps "all" to config.AllRooms and anything else to a room id.
func (s *session) resolveSubRoom(ctx context.Context, arg string) (id, label string, err error) {
	if arg == "all" || arg == config.AllRooms {
		return config.AllRooms, "all", nil
	}
	return s.resolveRoom(ctx, arg)
}

// cmdSubscribe: a2a subscribe --kind k1,k2 [--room R|all]
func cmdSubscribe(ctx context.Context, a *App, args []string) error {
	kinds, roomArg, err := parseSubArgs("subscribe", args, true)
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	room, label, err := s.resolveSubRoom(ctx, roomArg)
	if err != nil {
		return err
	}
	st, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
		cur.Subscribe(room, kinds)
		return nil
	})
	if err != nil {
		return err
	}
	a.printf("%s\t%s\n", label, strings.Join(st.Subscriptions[room], ","))
	return nil
}

// cmdUnsubscribe: a2a unsubscribe [--kind k1,k2] [--room R|all]; without --kind
// it drops every subscription for that room.
func cmdUnsubscribe(ctx context.Context, a *App, args []string) error {
	kinds, roomArg, err := parseSubArgs("unsubscribe", args, false)
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	room, label, err := s.resolveSubRoom(ctx, roomArg)
	if err != nil {
		return err
	}
	changed := false
	st, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
		changed = cur.Unsubscribe(room, kinds)
		return nil
	})
	if err != nil {
		return err
	}
	if !changed {
		a.printf("%s\tno matching subscription\n", label)
		return nil
	}
	a.printf("%s\t%s\n", label, firstNonEmpty(strings.Join(st.Subscriptions[room], ","), "-"))
	return nil
}

// cmdSubscriptions: a2a subscriptions [--json]
func cmdSubscriptions(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("subscriptions")
	asJSON := fs.Bool("json", false, "print as one JSON object")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subscriptions [--json]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	out := map[string][]string{}
	for _, room := range sortedKeys(st.Subscriptions) {
		label := "all"
		if room != config.AllRooms {
			label = s.roomLabel(ctx, room, labels)
		}
		out[label] = st.Subscriptions[room]
		if !*asJSON {
			a.printf("%s\t%s\n", label, strings.Join(st.Subscriptions[room], ","))
		}
	}
	if *asJSON {
		return a.printJSON(out)
	}
	return nil
}
