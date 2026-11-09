package cli

import "github.com/relux-works/a2a-swarm/internal/courier"

type watchOptions struct {
	rooms                      stringList
	all, json, notices, listen bool
	predicates                 deliveryPredicates
}

// One grammar serves raw watch, engine notices and the station listener. Flag
// values, literal operands and repeated booleans keep their ordinary meaning.
func parseWatchOptions(args []string) (watchOptions, error) {
	var o watchOptions
	fs := newFlagSet("watch")
	p := addDeliveryPredicateFlags(fs)
	fs.Var(&o.rooms, "room", "room to watch (repeatable)")
	fs.BoolVar(&o.json, "json", false, "JSON output")
	fs.BoolVar(&o.all, "all", false, "every message in selected rooms")
	fs.BoolVar(&o.notices, "notices", false, "owner-bound counts-only notices")
	fs.BoolVar(&o.listen, "listen", false, "single-client station listener")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return o, err
	}
	if len(pos) != 0 {
		return o, usagef("usage: a2a watch [--room R ...] [--all] [--json] [--notices [--listen]]")
	}
	if o.all && len(o.rooms) == 0 {
		return o, usagef("--all needs at least one --room")
	}
	if o.listen && !o.notices {
		return o, courier.NotAllowed
	}
	o.predicates, err = p.check()
	return o, err
}
