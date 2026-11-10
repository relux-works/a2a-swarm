package cli

import (
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"strings"
)

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
		return o, daemon.NotAllowed
	}
	o.predicates, err = p.check()
	return o, err
}

// Serialize parsed semantics, rather than inserting transport flags into raw
// argv: a trailing -- and values that look like flags must retain their meaning.
func (o watchOptions) noticeListenerArgs(selection daemon.Selection) []string {
	args := []string{"watch", "--notices=true", "--listen=true", "--json=true"}
	for _, field := range []struct{ name, value string }{
		{"as", selection.As}, {"profile", selection.Profile}, {"connection-fingerprint", selection.Fingerprint},
	} {
		if field.value != "" {
			args = append(args, "--"+field.name+"="+field.value)
		}
	}
	for _, room := range o.rooms {
		args = append(args, "--room="+room)
	}
	if o.all {
		args = append(args, "--all=true")
	}
	if o.predicates.toMe {
		args = append(args, "--to-me-only=true")
	}
	if o.predicates.text != nil {
		args = append(args, "--match-text="+o.predicates.text.String())
	}
	if len(o.predicates.from) != 0 {
		args = append(args, "--from="+strings.Join(o.predicates.from, ","))
	}
	if len(o.predicates.kinds) != 0 {
		args = append(args, "--kind="+strings.Join(o.predicates.kinds, ","))
	}
	return args
}
