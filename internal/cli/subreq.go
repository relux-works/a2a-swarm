package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Subscription requests (protocol rule 10): an agent asks another one to hold
// a subscription. The request is a kind=subscribe message addressed to the
// target, carrying the selector as the structured header field sub_request.
// Nothing is subscribed until the target runs `a2a sub-accept`, which creates
// the subscription (source = the request event id) and answers kind=accept.

// cmdSubRequest: a2a sub-request <agent> --room R --topic T <selector flags> [--note text]
func cmdSubRequest(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a sub-request <agent> --room R --topic <slug|$root> [--sub-room R|all] [--sub-topic X] [--from A] [--subject X] [--subtype S] [--kind k1,k2] [--until RFC3339] [--until-kind K] [--all-events] [--note text]"
	fs := newFlagSet("sub-request")
	room := fs.String("room", "", "room of the thread the request is posted in")
	topic := fs.String("topic", "", "thread (slug or root event id) the request is posted in")
	note := fs.String("note", "", "free text for the target")
	f := addSelectorFlags(fs, "sub-room", "sub-topic", "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *room == "" || *topic == "" {
		return usagef(use)
	}
	if _, err := normalizeUser(pos[0], "x"); err != nil {
		return err
	}
	subRoom := firstNonEmpty(*f.room, *room)
	now := a.Now()
	if err := f.check(now, subRoom); err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	toNames, mentions, err := addressees(pos[0], s.server)
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, *room)
	if err != nil {
		return err
	}
	root, slug, err := s.resolveTopic(ctx, roomID, label, *topic)
	if err != nil {
		return err
	}
	sel, subLabel, err := f.resolve(ctx, s, subRoom)
	if err != nil {
		return err
	}
	text := "subscription request: " + Sub{SubSelector: sel}.summary(subLabel, s.server)
	if n := strings.TrimSpace(sanitizeText(*note)); n != "" {
		text += "\n\n" + n
	}
	h := Header{V: 1, Type: "message", Slug: slug, Topic: root, From: s.name, To: toNames,
		At: now.UTC().Format(time.RFC3339), Kind: "subscribe", SubRequest: &sel}
	id, err := s.sendThreadMessage(ctx, roomID, label, h, text, mentions)
	if err != nil {
		return err
	}
	a.printf("%s\n", id)
	return nil
}

// cmdSubAccept: a2a sub-accept <room> <$event>
func cmdSubAccept(ctx context.Context, a *App, args []string) error {
	if len(args) != 2 || !strings.HasPrefix(args[1], "$") {
		return usagef("usage: a2a sub-accept <room> <$event>")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, args[0])
	if err != nil {
		return err
	}
	ev, err := s.client.GetEvent(ctx, roomID, args[1])
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("event %s not found in %s", sanitizeLine(args[1]), label)
		}
		return err
	}
	h, ok := decodeHeader(ev.Message().A2A)
	if ev.Type != "m.room.message" || !ok || h.Kind != "subscribe" || h.SubRequest == nil {
		return fmt.Errorf("%s is not a subscription request (kind=subscribe with sub_request)", sanitizeLine(ev.EventID))
	}
	if !contains(h.To, s.name) && !contains(h.To, s.creds.UserID) {
		return fmt.Errorf("subscription request %s is addressed to %s, not to %s; refused",
			sanitizeLine(ev.EventID), sanitizeLine(strings.Join(h.To, ",")), s.name)
	}
	sel := *h.SubRequest
	if err := sel.validate(); err != nil {
		return fmt.Errorf("subscription request %s is invalid: %v", sanitizeLine(ev.EventID), err)
	}
	sub := Sub{SubSelector: sel, CreatedAt: a.Now().UTC().Truncate(time.Millisecond).Format(createdAtLayout), Source: ev.EventID}
	existing := false
	if _, _, err := s.updateSubs(ctx, func(_ *config.State, list *SubList) (bool, error) {
		for _, cur := range list.Subs {
			if cur.Source == ev.EventID {
				sub, existing = cur, true
				return false, nil
			}
		}
		sub.ID = newSubID(*list)
		list.Subs = append(list.Subs, sub)
		return true, nil
	}); err != nil {
		return err
	}
	subLabel := "all"
	if sub.Room != "" {
		subLabel = s.roomLabel(ctx, sub.Room, nil)
	}
	summary := sub.summary(subLabel, s.server)
	if existing {
		fmt.Fprintf(a.Stderr, "a2a: request %s was already accepted\n", sanitizeLine(ev.EventID))
		a.printf("%s\t%s\n", sanitizeLine(sub.ID), summary)
		return nil
	}
	root := firstNonEmpty(ev.ThreadRoot(), ev.EventID)
	toNames, mentions, err := addressees(ev.Sender, s.server)
	if err != nil {
		return err
	}
	reply := Header{V: 1, Type: "message", Slug: h.Slug, Topic: root, From: s.name, To: toNames,
		At: a.Now().UTC().Format(time.RFC3339), Re: ev.EventID, Kind: "accept"}
	if _, err := s.sendThreadMessage(ctx, roomID, label, reply, "accepted: "+sub.ID+" "+summary, mentions); err != nil {
		return fmt.Errorf("%s was created, but the accept reply failed: %w", sub.ID, err)
	}
	a.printf("%s\t%s\n", sub.ID, summary)
	return nil
}
