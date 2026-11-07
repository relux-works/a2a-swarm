package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// SubsType is the Matrix account data type that holds an agent's runtime
// subscriptions (protocol rule 10). The server copy is the source of truth.
const SubsType = "ai.relux.a2a.subscriptions"

// legacyCreatedAt is the created_at of subscriptions migrated from the pre-v2
// local state: they had no creation time and matched every new event.
const legacyCreatedAt = "1970-01-01T00:00:00.000Z"

// createdAtLayout keeps millisecond precision, like origin_server_ts.
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// SubSelector says which events a subscription delivers. Every set field must
// match (AND). It is also the structured payload of a subscription request.
type SubSelector struct {
	Room      string   `json:"room,omitempty"`    // room id; empty = every joined room
	Product   string   `json:"product,omitempty"` // canonical product alias local part
	Topic     string   `json:"topic,omitempty"`   // thread root event id
	From      string   `json:"from,omitempty"`    // sender user id
	Subject   string   `json:"subject,omitempty"`
	Subtypes  []string `json:"subtypes,omitempty"`
	Subtype   string   `json:"-"` // source compatibility for in-package legacy selectors
	Kinds     []string `json:"kinds,omitempty"`
	Until     string   `json:"until,omitempty"`      // RFC3339; inactive afterwards
	UntilKind string   `json:"until_kind,omitempty"` // removed after the first delivered event of this kind
	AllEvents bool     `json:"all_events,omitempty"` // every event of Room (explicit opt-in)
}

func (sel SubSelector) subtypeList() []string {
	if len(sel.Subtypes) > 0 {
		return sel.Subtypes
	}
	if sel.Subtype != "" {
		return []string{sel.Subtype}
	}
	return nil
}

// UnmarshalJSON accepts the former single subtype field while writing the
// current list representation.
func (sel *SubSelector) UnmarshalJSON(data []byte) error {
	type plain SubSelector
	var raw struct {
		plain
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*sel = SubSelector(raw.plain)
	if len(sel.Subtypes) == 0 && raw.Subtype != "" {
		sel.Subtypes = []string{raw.Subtype}
	}
	return nil
}

// Sub is one stored subscription.
type Sub struct {
	ID string `json:"id"`
	SubSelector
	Preset string `json:"preset,omitempty"`
	// Since, when set, replaces the created_at gate: events after this event match.
	Since     string `json:"since,omitempty"`
	SinceTS   int64  `json:"since_ts,omitempty"`
	CreatedAt string `json:"created_at"`
	// Source is the accepted sub-request event id, "legacy" for migrated rows,
	// or principalSubSource for independent host-principal policy rows.
	Source string `json:"source,omitempty"`
}

// UnmarshalJSON keeps subscriptions readable when account data still uses the
// former single subtype field. The explicit fields also prevent the embedded
// SubSelector decoder from consuming the surrounding subscription object.
func (sub *Sub) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string   `json:"id"`
		Room      string   `json:"room,omitempty"`
		Product   string   `json:"product,omitempty"`
		Topic     string   `json:"topic,omitempty"`
		From      string   `json:"from,omitempty"`
		Subject   string   `json:"subject,omitempty"`
		Subtypes  []string `json:"subtypes,omitempty"`
		Subtype   string   `json:"subtype,omitempty"`
		Kinds     []string `json:"kinds,omitempty"`
		Until     string   `json:"until,omitempty"`
		UntilKind string   `json:"until_kind,omitempty"`
		AllEvents bool     `json:"all_events,omitempty"`
		Preset    string   `json:"preset,omitempty"`
		Since     string   `json:"since,omitempty"`
		SinceTS   int64    `json:"since_ts,omitempty"`
		CreatedAt string   `json:"created_at"`
		Source    string   `json:"source,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Subtypes) == 0 && raw.Subtype != "" {
		raw.Subtypes = []string{raw.Subtype}
	}
	*sub = Sub{ID: raw.ID, SubSelector: SubSelector{Room: raw.Room, Product: raw.Product, Topic: raw.Topic, From: raw.From, Subject: raw.Subject,
		Subtypes: raw.Subtypes, Kinds: raw.Kinds, Until: raw.Until, UntilKind: raw.UntilKind, AllEvents: raw.AllEvents},
		Preset: raw.Preset, Since: raw.Since, SinceTS: raw.SinceTS, CreatedAt: raw.CreatedAt, Source: raw.Source}
	return nil
}

// MarshalJSON also writes the former single "subtype" field when the list has
// exactly one entry, so a binary from before subtype lists still filters the
// same way (downgrade fidelity, tb-keeper's advice on #45). With more entries
// an old binary sees no subtype and matches more broadly: extra wakes, never
// missed ones.
func (sub Sub) MarshalJSON() ([]byte, error) {
	type plain Sub // no methods: avoids recursion
	out := struct {
		plain
		Subtype string `json:"subtype,omitempty"`
	}{plain: plain(sub)}
	if len(sub.Subtypes) == 1 {
		out.Subtype = sub.Subtypes[0]
	}
	return json.Marshal(out)
}

// SubList is the account data content of SubsType.
type SubList struct {
	V    int   `json:"v"`
	Subs []Sub `json:"subs"`
}

func (sel SubSelector) hasSelector() bool {
	return sel.Topic != "" || sel.Product != "" || sel.From != "" || sel.Subject != "" || len(sel.subtypeList()) > 0 || len(sel.Kinds) > 0
}

// validate checks a selector that is already resolved to ids (a stored or a
// requested one). Messages name the CLI flags.
func (sel SubSelector) validate() error {
	switch {
	case sel.AllEvents && sel.Room == "":
		return fmt.Errorf("--all-events needs a concrete --room")
	case !sel.hasSelector() && !sel.AllEvents:
		return fmt.Errorf("name at least one selector (--topic, --product, --from, --subject, --subtype, --kind), or use --room R --all-events")
	case sel.Topic != "" && sel.Room == "":
		return fmt.Errorf("--topic needs a concrete --room")
	case sel.Room != "" && !strings.HasPrefix(sel.Room, "!"):
		return fmt.Errorf("invalid room id %q", sanitizeLine(sel.Room))
	case sel.Topic != "" && !strings.HasPrefix(sel.Topic, "$"):
		return fmt.Errorf("invalid topic root %q", sanitizeLine(sel.Topic))
	case sel.From != "" && (!strings.HasPrefix(sel.From, "@") || !strings.Contains(sel.From, ":")):
		return fmt.Errorf("invalid --from %q", sanitizeLine(sel.From))
	case sel.Subject != "" && !subjectRE.MatchString(sel.Subject):
		return fmt.Errorf("invalid --subject %q: 1-128 chars of A-Z a-z 0-9 # . _ / : @ -", sanitizeLine(sel.Subject))
	case len(sel.subtypeList()) > 0:
		for _, subtype := range sel.subtypeList() {
			if !validSubtype(subtype) {
				return fmt.Errorf("invalid --subtype %q (one of: %s)", sanitizeLine(subtype), strings.Join(Subtypes, ", "))
			}
		}
	case sel.UntilKind != "" && !validKind(sel.UntilKind):
		return fmt.Errorf("invalid --until-kind %q (one of: %s)", sanitizeLine(sel.UntilKind), strings.Join(Kinds, ", "))
	}
	if sel.Product != "" && !productNameRE.MatchString(sel.Product) {
		return fmt.Errorf("invalid --product %q: use [a-z0-9-]{1,40}", sanitizeLine(sel.Product))
	}
	for _, k := range sel.Kinds {
		if !validKind(k) {
			return fmt.Errorf("invalid --kind %q (one of: %s)", sanitizeLine(k), strings.Join(Kinds, ", "))
		}
	}
	if sel.Until != "" {
		if _, err := time.Parse(time.RFC3339, sel.Until); err != nil {
			return fmt.Errorf("invalid --until %q: use RFC3339", sanitizeLine(sel.Until))
		}
	}
	return nil
}

// sanitized returns a copy whose text fields are safe to print.
func (sel SubSelector) sanitized() SubSelector {
	out := sel
	for _, p := range []*string{&out.Room, &out.Product, &out.Topic, &out.From, &out.Subject, &out.Until, &out.UntilKind} {
		*p = sanitizeLine(*p)
	}
	out.Subtypes = append([]string(nil), sel.subtypeList()...)
	for i := range out.Subtypes {
		out.Subtypes[i] = sanitizeLine(out.Subtypes[i])
	}
	out.Kinds = nil
	for _, k := range sel.Kinds {
		out.Kinds = append(out.Kinds, sanitizeLine(k))
	}
	return out
}

// expiredAt reports whether the subscription is inactive at t (--until).
func (sub Sub) expiredAt(t time.Time) bool {
	if sub.Until == "" {
		return false
	}
	u, err := time.Parse(time.RFC3339, sub.Until)
	return err == nil && t.After(u)
}

// after is the creation gate: only events created after the subscription (or
// after its --since event) match.
func (sub Sub) after(ev matrix.Event) bool {
	if sub.Since != "" {
		return ev.EventID != sub.Since && ev.OriginServerTS >= sub.SinceTS
	}
	c, err := time.Parse(time.RFC3339Nano, sub.CreatedAt)
	if err != nil {
		return true
	}
	return ev.OriginServerTS >= c.UnixMilli()
}

// matches reports whether a message event from another agent in roomID is
// delivered by this subscription.
func (sub Sub) matches(ev matrix.Event, roomID string, h Header, hasHeader bool) bool {
	if ev.Type != "m.room.message" || (sub.Room != "" && sub.Room != roomID) {
		return false
	}
	if sub.expiredAt(time.UnixMilli(ev.OriginServerTS)) || !sub.after(ev) {
		return false
	}
	projected := hasHeader && h.Type == "projection"
	if sub.Product != "" && (!projected || h.ProductAlias != "#"+sub.Product) {
		return false
	}
	if projected && sub.Room == "" && sub.Product == "" {
		// A room=all subscription already matched the original in its own
		// room; only a subscription bound to the projection room itself
		// (a keeper watching #system) is delivered the mirror, filtered by
		// the origin's kind, subtype, subject and sender from its header.
		return false
	}
	if sub.Topic != "" && ev.ThreadRoot() != sub.Topic {
		return false
	}
	if sub.From != "" && ev.Sender != sub.From && !(projected && sub.Source != principalSubSource && originSender(sub.From, h.From)) {
		return false
	}
	if len(sub.subtypeList()) > 0 && !contains(sub.subtypeList(), h.Subtype) {
		return false
	}
	if len(sub.Kinds) > 0 && !contains(sub.Kinds, h.Kind) {
		return false
	}
	if sub.Subject != "" && h.Subject != sub.Subject && !containsToken(ev.Message().Body, sub.Subject) {
		return false
	}
	return true
}

// originSender reports whether the subscription's --from (a full user id)
// names the origin sender of a projection, whose header holds its localpart
// (or a full id for a remote sender).
func originSender(subFrom, from string) bool {
	return from != "" && (subFrom == from || strings.HasPrefix(subFrom, "@"+from+":"))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func subjectChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("#._/:@-", b) >= 0
}

// containsToken reports whether tok occurs in body as a whole token: not
// inside a longer subject-like word ("BUG-1" is not in "BUG-12"). A trailing
// '.', ':' or '/' followed by a non-token character is punctuation.
func containsToken(body, tok string) bool {
	for i := 0; i < len(body); {
		j := strings.Index(body[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		startOK := start == 0 || !subjectChar(body[start-1])
		endOK := end == len(body) || !subjectChar(body[end]) ||
			(strings.IndexByte(".:/", body[end]) >= 0 && (end+1 == len(body) || !subjectChar(body[end+1])))
		if startOK && endOK {
			return true
		}
		i = start + 1
	}
	return false
}

// summary renders a subscription for humans; roomLabel is the room's alias (or "all").
func (sub Sub) summary(roomLabel, server string) string {
	parts := []string{"room=" + firstNonEmpty(roomLabel, "all")}
	if sub.Topic != "" {
		parts = append(parts, "topic="+sub.Topic)
	}
	if sub.Product != "" {
		parts = append(parts, "product="+sub.Product)
	}
	if sub.From != "" {
		parts = append(parts, "from="+localpartIfLocal(sub.From, server))
	}
	if sub.Subject != "" {
		parts = append(parts, "subject="+sub.Subject)
	}
	if len(sub.subtypeList()) > 0 {
		parts = append(parts, "subtype="+strings.Join(sub.subtypeList(), ","))
	}
	if len(sub.Kinds) > 0 {
		parts = append(parts, "kinds="+strings.Join(sub.Kinds, ","))
	}
	if sub.AllEvents {
		parts = append(parts, "all-events")
	}
	if sub.Until != "" {
		parts = append(parts, "until="+sub.Until)
	}
	if sub.UntilKind != "" {
		parts = append(parts, "until-kind="+sub.UntilKind)
	}
	if sub.Since != "" {
		parts = append(parts, "since="+sub.Since)
	}
	if sub.Source != "" {
		parts = append(parts, "source="+sub.Source)
	}
	return sanitizeLine(strings.Join(parts, " "))
}

func newSubID(list SubList) string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := "sub-" + hex.EncodeToString(b[:])
		if list.find(id) < 0 {
			return id
		}
	}
}

func (l SubList) find(id string) int {
	for i, s := range l.Subs {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// remove drops the given ids; it reports whether anything was removed.
func (l *SubList) remove(ids ...string) bool {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	keep := l.Subs[:0]
	for _, s := range l.Subs {
		if !drop[s.ID] {
			keep = append(keep, s)
		}
	}
	changed := len(keep) != len(l.Subs)
	l.Subs = keep
	return changed
}

// pruneExpired drops subscriptions whose --until passed; it returns their ids.
func (l *SubList) pruneExpired(now time.Time) []string {
	var gone []string
	for _, s := range l.Subs {
		if s.expiredAt(now) {
			gone = append(gone, s.ID)
		}
	}
	l.remove(gone...)
	return gone
}

// migrateLegacy moves the pre-v2 local kind subscriptions into list (one
// subscription per room entry) and drops them from st. It is idempotent: an
// identical migrated entry is not added twice.
func migrateLegacy(st *config.State, list *SubList, policy *config.SubscriptionPolicy) (bool, error) {
	if len(st.LegacySubscriptions) == 0 {
		return false, nil
	}
	if policy != nil {
		// A global selector cannot express a room exception. Refuse the
		// conflicting migration before changing anything, rather than widen
		// a trim or silently drop coverage in unrelated rooms.
		for _, trim := range policy.Trims {
			for _, kind := range st.LegacySubscriptions[config.LegacyAllRooms] {
				if contains(trim.Dropped, kind) {
					return false, fmt.Errorf("legacy global subscription conflicts with a recorded room trim; deliberately migrate and narrow legacy subscriptions before automatic repair")
				}
			}
		}
	}
	for _, room := range sortedKeys(st.LegacySubscriptions) {
		kinds := st.LegacySubscriptions[room]
		if policy != nil {
			kinds = withoutTrim(kinds, policy.Trims[room])
		}
		if room == config.LegacyAllRooms {
			room = ""
		}
		if len(kinds) == 0 {
			continue
		}
		dup := false
		for _, s := range list.Subs {
			if s.Source == "legacy" && s.Room == room && strings.Join(s.Kinds, ",") == strings.Join(kinds, ",") {
				dup = true
			}
		}
		if !dup {
			list.Subs = append(list.Subs, Sub{ID: newSubID(*list), SubSelector: SubSelector{Room: room, Kinds: kinds},
				CreatedAt: legacyCreatedAt, Source: "legacy"})
		}
	}
	st.LegacySubscriptions = nil
	return true, nil
}

// getSubs reads the subscriptions from account data (none when never set).
func (s *session) getSubs(ctx context.Context) (SubList, error) {
	var list SubList
	err := s.client.GetAccountData(ctx, s.creds.UserID, SubsType, &list)
	if err != nil {
		if isNotFound(err) {
			return SubList{V: 1}, nil
		}
		return SubList{}, fmt.Errorf("read subscriptions (account data %s): %w", SubsType, err)
	}
	if list.V > 1 {
		return SubList{}, fmt.Errorf("account data %s has version %d; this a2a understands only 1", SubsType, list.V)
	}
	list.V = 1
	return list, nil
}

// updateSubs runs fn on the local state and the subscriptions under the state
// lock, after migrating legacy local subscriptions. When fn (or the migration)
// changed the subscriptions they are written back to account data; the local
// state is saved only after that write succeeded.
func (s *session) updateSubs(ctx context.Context, fn func(st *config.State, list *SubList) (bool, error)) (config.State, SubList, error) {
	return s.updateSubsBy(ctx, time.Time{}, fn)
}

// updateSubsBy is updateSubs with a bounded wait for the state lock when
// deadline is set (hooks); the zero deadline blocks.
func (s *session) updateSubsBy(ctx context.Context, deadline time.Time, fn func(st *config.State, list *SubList) (bool, error)) (config.State, SubList, error) {
	return s.updateSubsWithPolicyBy(ctx, deadline, nil, fn)
}

// Automatic repair supplies the policy already protected by its outer policy
// lock. Explicit subscription/delivery callers retain their existing behavior.
func (s *session) updateSubsWithPolicy(ctx context.Context, policy config.SubscriptionPolicy, fn func(st *config.State, list *SubList) (bool, error)) (config.State, SubList, error) {
	return s.updateSubsWithPolicyBy(ctx, time.Time{}, &policy, fn)
}

func (s *session) updateSubsWithPolicyBy(ctx context.Context, deadline time.Time, policy *config.SubscriptionPolicy, fn func(st *config.State, list *SubList) (bool, error)) (config.State, SubList, error) {
	if err := s.checkConnection(); err != nil {
		return config.State{}, SubList{}, err
	}
	var list SubList
	st, err := config.UpdateStateUntil(s.paths, s.name, deadline, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		var err error
		if list, err = s.getSubs(ctx); err != nil {
			return err
		}
		changed, err := migrateLegacy(st, &list, policy)
		if err != nil {
			return err
		}
		if fn != nil {
			c, err := fn(st, &list)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		if !changed {
			return nil
		}
		if list.Subs == nil {
			list.Subs = []Sub{}
		}
		if err := s.client.PutAccountData(ctx, s.creds.UserID, SubsType, list); err != nil {
			return fmt.Errorf("save subscriptions (account data %s): %w", SubsType, err)
		}
		return nil
	})
	return st, list, err
}

// selectorFlags are the selector flags shared by subscribe and sub-request.
type selectorFlags struct {
	room, product, topic, from, subject, subtype, kind, until, untilKind *string
	allEvents                                                            *bool
}

func addSelectorFlags(fs *flag.FlagSet, roomFlag, topicFlag, roomDefault string) *selectorFlags {
	return &selectorFlags{
		room:      fs.String(roomFlag, roomDefault, "room, or all"),
		product:   fs.String("product", "", "product projections by canonical alias name"),
		topic:     fs.String(topicFlag, "", "topic slug or root event id (needs a concrete room)"),
		from:      fs.String("from", "", "sender (name or user id)"),
		subject:   fs.String("subject", "", "structured subject or whole token in the body"),
		subtype:   fs.String("subtype", "", "comma-separated system subtypes"),
		kind:      fs.String("kind", "", "comma-separated kinds"),
		until:     fs.String("until", "", "RFC3339 expiry"),
		untilKind: fs.String("until-kind", "", "remove after the first delivered event of this kind"),
		allEvents: fs.Bool("all-events", false, "every event of the concrete room"),
	}
}

// check validates the raw flags without any network call; room is the
// effective room argument ("all" or "" for every joined room).
func (f *selectorFlags) check(now time.Time, room string) error {
	concrete := room != "" && room != "all"
	sel := SubSelector{Product: *f.product, Subject: *f.subject, Subtypes: splitList(*f.subtype), Kinds: splitList(*f.kind), UntilKind: *f.untilKind, AllEvents: *f.allEvents}
	if concrete {
		sel.Room = "!placeholder"
	}
	if *f.topic != "" {
		sel.Topic = "$placeholder"
		if !strings.HasPrefix(*f.topic, "$") && !slugRE.MatchString(*f.topic) {
			return usagef("invalid --topic %q: use a slug or a $event_id", sanitizeLine(*f.topic))
		}
	}
	if *f.from != "" {
		if _, err := normalizeUser(*f.from, "x"); err != nil {
			return usagef("invalid --from %q: use name, @name, or @name:server", sanitizeLine(*f.from))
		}
		sel.From = "@placeholder:x"
	}
	if *f.until != "" {
		u, err := time.Parse(time.RFC3339, *f.until)
		if err != nil {
			return usagef("invalid --until %q: use RFC3339, for example 2026-09-30T12:00:00Z", sanitizeLine(*f.until))
		}
		if !u.After(now) {
			return usagef("--until %s is not in the future", u.UTC().Format(time.RFC3339))
		}
		sel.Until = u.UTC().Format(time.RFC3339)
	}
	if err := sel.validate(); err != nil {
		return usageError{err.Error()}
	}
	return nil
}

// resolve turns checked flags into a stored selector: room and topic become
// ids, from a user id. roomLabel is the room's display label ("all" for every room).
func (f *selectorFlags) resolve(ctx context.Context, s *session, room string) (sel SubSelector, roomLabel string, err error) {
	sel = SubSelector{Product: *f.product, Subject: *f.subject, Subtypes: splitList(*f.subtype), Kinds: splitList(*f.kind), UntilKind: *f.untilKind, AllEvents: *f.allEvents}
	roomLabel = "all"
	if room != "" && room != "all" {
		if sel.Room, roomLabel, err = s.resolveRoom(ctx, room); err != nil {
			return sel, "", err
		}
	}
	if *f.topic != "" {
		if sel.Topic, _, err = s.resolveTopic(ctx, sel.Room, roomLabel, *f.topic); err != nil {
			return sel, "", err
		}
	}
	if *f.from != "" {
		if sel.From, err = normalizeUser(*f.from, s.server); err != nil {
			return sel, "", err
		}
	}
	if *f.until != "" {
		u, _ := time.Parse(time.RFC3339, *f.until)
		sel.Until = u.UTC().Format(time.RFC3339)
	}
	return sel, roomLabel, sel.validate()
}

// findSince locates the --since event (in the subscription's room, or in any
// joined room) and returns its origin timestamp.
func (s *session) findSince(ctx context.Context, room, eventID string) (int64, error) {
	rooms := []string{room}
	if room == "" {
		var err error
		if rooms, err = s.client.JoinedRooms(ctx); err != nil {
			return 0, err
		}
	}
	for _, r := range rooms {
		ev, err := s.client.GetEvent(ctx, r, eventID)
		if err == nil {
			return ev.OriginServerTS, nil
		}
		if !isNotFound(err) && !matrix.IsStatus(err, 403) {
			return 0, err
		}
	}
	return 0, fmt.Errorf("--since event %s not found in the subscription's rooms", sanitizeLine(eventID))
}

// cmdSubscribe: a2a subscribe [--room R|all] [--product X] [--topic X] [--from A] [--subject X]
// [--subtype S] [--kind k1,k2] [--until T] [--until-kind K] [--since $ev] [--all-events]
func cmdSubscribe(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("subscribe")
	f := addSelectorFlags(fs, "room", "topic", "all")
	since := fs.String("since", "", "deliver events after this event id instead of after now")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subscribe [--room R|all] [--product X] [--topic X] [--from A] [--subject X] [--subtype S] [--kind k1,k2] [--until RFC3339] [--until-kind K] [--since $event] [--all-events]")
	}
	now := a.Now()
	if err := f.check(now, *f.room); err != nil {
		return err
	}
	if *since != "" && !strings.HasPrefix(*since, "$") {
		return usagef("invalid --since %q: must be an event id ($...)", sanitizeLine(*since))
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	sel, label, err := f.resolve(ctx, s, *f.room)
	if err != nil {
		return err
	}
	sub := Sub{SubSelector: sel, CreatedAt: now.UTC().Truncate(time.Millisecond).Format(createdAtLayout)}
	if *since != "" {
		if sub.SinceTS, err = s.findSince(ctx, sel.Room, *since); err != nil {
			return err
		}
		sub.Since = *since
	}
	if _, _, err := s.updateSubs(ctx, func(_ *config.State, list *SubList) (bool, error) {
		sub.ID = newSubID(*list)
		list.Subs = append(list.Subs, sub)
		return true, nil
	}); err != nil {
		return err
	}
	a.printf("%s\t%s\n", sub.ID, sub.summary(label, s.server))
	return nil
}

// cmdUnsubscribe: a2a unsubscribe <sub-id>|--all
func cmdUnsubscribe(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("unsubscribe")
	all := fs.Bool("all", false, "remove every subscription")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if (*all && len(pos) != 0) || (!*all && len(pos) != 1) {
		return usagef("usage: a2a unsubscribe <sub-id>|--all")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	var removed []string
	err = config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		configured, err := ownerFiles(s.paths, s.name).principals()
		if err != nil {
			return err
		}
		_, _, err = s.updateSubs(ctx, func(_ *config.State, list *SubList) (bool, error) {
			var ids []string
			if *all {
				for _, sub := range list.Subs {
					ids = append(ids, sub.ID)
				}
			} else if list.find(pos[0]) < 0 {
				return false, fmt.Errorf("no subscription %q (see: a2a subs)", sanitizeLine(pos[0]))
			} else {
				ids = []string{pos[0]}
			}
			removed, err = removeSubscriptions(list, ids, configured)
			return len(removed) > 0, err
		})
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range removed {
		a.printf("%s\tremoved\n", sanitizeLine(id))
	}
	if len(removed) == 0 {
		a.printf("no subscriptions\n")
	}
	return nil
}

// cmdSubs: a2a subs [--json] lists the live subscriptions.
func cmdSubs(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "trim" {
		return cmdSubsTrim(ctx, a, args[1:])
	}
	if len(args) > 0 && args[0] == "check" {
		return cmdSubsCheck(ctx, a, args[1:])
	}
	if len(args) > 0 && args[0] == "sync" {
		return cmdSubsSync(ctx, a, args[1:])
	}
	fs := newFlagSet("subs")
	asJSON := fs.Bool("json", false, "print the subscription list as one JSON object")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subs [--json]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	_, list, err := s.updateSubs(ctx, nil)
	if err != nil {
		return err
	}
	now := a.Now()
	live := SubList{V: 1, Subs: []Sub{}}
	for _, sub := range list.Subs {
		if !sub.expiredAt(now) {
			live.Subs = append(live.Subs, sub)
		}
	}
	if *asJSON {
		for i, sub := range live.Subs {
			live.Subs[i].SubSelector = sub.SubSelector.sanitized()
			live.Subs[i].ID, live.Subs[i].Source = sanitizeLine(sub.ID), sanitizeLine(sub.Source)
			live.Subs[i].Since, live.Subs[i].CreatedAt = sanitizeLine(sub.Since), sanitizeLine(sub.CreatedAt)
		}
		return a.printJSON(live)
	}
	labels := map[string]string{}
	for _, sub := range live.Subs {
		label := "all"
		if sub.Room != "" {
			label = s.roomLabel(ctx, sub.Room, labels)
		}
		a.printf("%s\t%s\n", sanitizeLine(sub.ID), sub.summary(label, s.server))
	}
	return nil
}
