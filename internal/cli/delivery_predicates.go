package cli

import (
	"encoding/json"
	"flag"
	"regexp"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// These predicates narrow delivery, never ingestion, relevance or receipts.
type deliveryPredicates struct {
	text       *regexp.Regexp
	from       []string
	kinds      []string
	toMe       bool
	keywords   config.WaitKeywords
	me, name   string
	principals []string // refreshed from host configuration before showing mail
}

type deliveryPredicateFlags struct {
	text, from, kind stringList
	toMe             bool
}

func addDeliveryPredicateFlags(fs *flag.FlagSet) *deliveryPredicateFlags {
	f := &deliveryPredicateFlags{}
	fs.Var(&f.text, "match-text", "Go regexp over the full message text (case-sensitive by default)")
	fs.Var(&f.from, "from", "comma-separated senders (name, @name or full user id)")
	fs.Var(&f.kind, "kind", "comma-separated protocol message kinds")
	fs.BoolVar(&f.toMe, "to-me-only", false, "only messages addressed to or mentioning this identity")
	return f
}

// check runs before session loading, leases, liveness writes or network calls.
func (f *deliveryPredicateFlags) check() (deliveryPredicates, error) {
	p := deliveryPredicates{toMe: f.toMe}
	for _, option := range []struct {
		name   string
		values stringList
	}{{"match-text", f.text}, {"from", f.from}, {"kind", f.kind}} {
		if len(option.values) > 1 {
			return p, usagef("--%s may be supplied only once", option.name)
		}
		if len(option.values) == 1 && strings.TrimSpace(option.values[0]) == "" {
			return p, usagef("--%s must not be empty", option.name)
		}
	}
	if len(f.text) == 1 {
		var err error
		p.text, err = regexp.Compile(f.text[0])
		if err != nil {
			return p, usagef("invalid --match-text regexp")
		}
	}
	for _, option := range []struct {
		name   string
		values stringList
		dest   *[]string
	}{{"from", f.from, &p.from}, {"kind", f.kind, &p.kinds}} {
		if len(option.values) == 0 {
			continue
		}
		for _, value := range strings.Split(option.values[0], ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				return p, usagef("--%s must not contain empty entries", option.name)
			}
			if option.name == "kind" {
				if !contains(Kinds, value) {
					return p, usagef("invalid --kind")
				}
			} else if _, err := normalizeUser(value, "local"); err != nil || strings.ContainsAny(value, " \t\r\n\x00") {
				return p, usagef("invalid --from: use name, @name, or @name:server")
			}
			*option.dest = append(*option.dest, value)
		}
	}
	return p, nil
}

func (p deliveryPredicates) forSession(s *session) deliveryPredicates {
	p.me, p.name = s.creds.UserID, s.name
	p.keywords = s.creds.WaitKeywords
	for i, sender := range p.from {
		p.from[i], _ = normalizeUser(sender, s.server) // already checked; no lookup
	}
	return p
}

func (p deliveryPredicates) matches(e config.MailEntry) bool {
	if principalMail(e, p.principals) {
		return true
	}
	// Invites and history gaps retain their existing control-entry behavior.
	if e.Kind != config.MailEvent {
		return true
	}
	if p.text == nil && len(p.from) == 0 && len(p.kinds) == 0 && !p.toMe && len(p.keywords) == 0 {
		return true
	}
	var ev matrix.Event
	if json.Unmarshal(e.Event, &ev) != nil || ev.Type != "m.room.message" {
		return false
	}
	if len(p.from) > 0 && !contains(p.from, ev.Sender) {
		return false
	}
	m := ev.Message()
	h, hasHeader := decodeHeader(m.A2A)
	if len(p.kinds) > 0 && (!hasHeader || !contains(p.kinds, h.Kind)) {
		return false
	}
	if p.toMe {
		rel, _, _ := classify(ev, e.Room, p.me, p.name, config.State{}, nil)
		if rel != relTo && rel != relMention {
			return false
		}
	}
	text := m.Body
	if hasHeader {
		text = stripHeaderBody(text)
	}
	if p.text != nil && !p.text.MatchString(text) {
		return false
	}
	if len(p.keywords) > 0 {
		for _, word := range p.keywords {
			if containsToken(text, word) || hasHeader && h.Subject == word {
				return true
			}
		}
		return false
	}
	return true
}
