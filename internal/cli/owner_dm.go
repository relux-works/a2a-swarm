package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// ownerAudience evaluates one current-state response. It never repairs state.
func (s *session) ownerAudience(ctx context.Context, room, owner string, allowInvite bool) (bool, error) {
	events, err := s.client.GetRoomState(ctx, room)
	if err != nil {
		return false, fmt.Errorf("audience unavailable: %w", err)
	}
	members := map[string]string{}
	rule, history := "", ""
	ownJoin := false
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.StateKey == nil {
			continue
		}
		k := ev.Type + "|" + *ev.StateKey
		if seen[k] {
			return false, errors.New("audience: duplicate state")
		}
		seen[k] = true
		if ev.Type == "m.room.encryption" {
			return false, errors.New("bound room is encrypted; v1 cannot read or post there")
		}
		switch ev.Type {
		case "m.room.member":
			var m struct {
				Membership string `json:"membership"`
			}
			if json.Unmarshal(ev.Content, &m) != nil {
				return false, errors.New("audience: invalid member")
			}
			if m.Membership == "join" || m.Membership == "invite" {
				members[*ev.StateKey] = m.Membership
			}
			if *ev.StateKey == owner && m.Membership == "join" && ev.Sender == owner {
				ownJoin = true
			}
		case "m.room.join_rules":
			var r struct {
				Rule string `json:"join_rule"`
			}
			if json.Unmarshal(ev.Content, &r) != nil {
				return false, errors.New("audience: invalid join rule")
			}
			rule = r.Rule
		case "m.room.history_visibility":
			var h struct {
				History string `json:"history_visibility"`
			}
			if json.Unmarshal(ev.Content, &h) != nil {
				return false, errors.New("audience: invalid history visibility")
			}
			history = h.History
		}
	}
	if rule != "invite" {
		return false, errors.New("audience: join_rule must be invite")
	}
	if history == "" || history == "world_readable" {
		return false, errors.New("audience: invalid history_visibility")
	}
	if len(members) != 2 || members[s.creds.UserID] != "join" || (members[owner] != "join" && !(allowInvite && members[owner] == "invite")) {
		return false, errors.New("audience: expected only joined identity and owner")
	}
	return ownJoin, nil
}

// Compute pending transitions without writes, so refused requests cannot make
// an incidental activation/expiry mutation. Status uses this read-only path.
func (s *session) ownerTransition(ctx context.Context, c *ownerDMConfig, st os.FileInfo) (string, error) {
	if c == nil || !c.Pending {
		return "", nil
	}
	if !s.app.Now().Before(st.ModTime().Add(24 * time.Hour)) {
		c.Pending = false
		c.Active = false
		return "expired", nil
	}
	joined, err := s.ownerAudience(ctx, c.Room, c.Owner, true)
	if err != nil {
		return "", err
	}
	if joined {
		c.Pending = false
		c.Active = true
		return "activated", nil
	}
	return "", nil
}
func (s *session) ownerSaveTransition(ctx context.Context, f ownerDMFiles, c *ownerDMConfig, transition string) error {
	return s.ownerSaveTransitionWithJournal(ctx, ctx, f, c, transition)
}

func (s *session) ownerSaveTransitionWithJournal(ctx, journalCtx context.Context, f ownerDMFiles, c *ownerDMConfig, transition string) error {
	if transition == "" {
		return nil
	}
	if err := ownerWrite(ctx, f.path("json"), c); err != nil {
		return err
	}
	if transition == "activated" {
		key := fmt.Sprintf("%d|confirm", c.Generation)
		entries, err := f.journal(s.app.Now())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Key == key {
				return nil
			}
		}
		entries = append(entries, ownerDMJournalEntry{Key: key, State: "pending", At: s.app.Now()})
		if err = f.saveJournal(ctx, entries); err != nil {
			return err
		}
		text := fmt.Sprintf("owner-dm active: %s; tag=%t (forced off in S1), mirror=%t, render=%s; config %s; unbind: a2a owner-dm unbind %s", s.name, c.Tag, c.Mirror, c.Render, ownerFingerprint(c), s.name)
		content, err := textContent(text, nil)
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(key))
		_, err = s.client.WithoutRedirects().SendMessageTxn(ctx, c.Room, fmt.Sprintf("a2a-odm-%x-confirm", digest[:12]), content)
		entries[len(entries)-1].State = "sent:confirm"
		if err != nil {
			entries[len(entries)-1].State = "failed:confirm"
		}
		_ = f.saveJournal(journalCtx, entries)
		return err
	}
	return nil
}
func (s *session) ownerSubscription(ctx context.Context, c *ownerDMConfig, add bool) error {
	deadline, _ := ctx.Deadline()
	_, _, err := s.updateSubsBy(ctx, deadline, func(_ *config.State, list *SubList) (bool, error) {
		changed := false
		if c.SubID != "" {
			changed = list.remove(c.SubID)
		}
		if add {
			c.SubID = newSubID(*list)
			list.Subs = append(list.Subs, Sub{ID: c.SubID, SubSelector: SubSelector{Room: c.Room, AllEvents: true}, CreatedAt: s.app.Now().UTC().Format(createdAtLayout)})
			return true, nil
		}
		c.SubID = ""
		return changed, nil
	})
	return err
}
func ownerNext(c *ownerDMConfig) (uint64, error) {
	if c == nil {
		return 1, nil
	}
	if c.Generation == ^uint64(0) {
		return 0, errors.New("owner-dm generation exhausted")
	}
	return c.Generation + 1, nil
}
func (s *session) ownerNewRoom(ctx context.Context, owner string) (string, error) {
	return s.client.CreateRoom(ctx, matrix.CreateRoomRequest{Preset: "private_chat", IsDirect: true, Invite: []string{owner}, InitialState: []matrix.InitialStateEvent{
		{Type: "m.room.join_rules", StateKey: "", Content: json.RawMessage(`{"join_rule":"invite"}`)},
		{Type: "m.room.history_visibility", StateKey: "", Content: json.RawMessage(`{"history_visibility":"joined"}`)},
	}})
}
func ownerEmptyKey() *string { k := ""; return &k }

// The body is an exact, single command line. No quoting, extra whitespace,
// repeated flags or agent-provided overrides are accepted.
func ownerCommand(body, action, name string) (ownerDMConfig, error) {
	c := ownerDMConfig{Render: "in-frame"}
	parts := strings.Split(body, " ")
	if len(parts) < 4 || strings.Join(parts[:4], " ") != "a2a owner-dm "+action+" "+name {
		return c, errors.New("owner event body is not the exact command for this identity")
	}
	seen := map[string]bool{}
	for _, p := range parts[4:] {
		key := strings.SplitN(p, "=", 2)[0]
		if action != "bind" || seen[key] {
			return c, errors.New("owner event has invalid or duplicate flags")
		}
		seen[key] = true
		switch p {
		case "tag":
			c.Tag = true
		case "mirror":
			c.Mirror = true
		case "render=in-frame":
			c.Render = "in-frame"
		case "render=outside-frame":
			c.Render = "outside-frame"
		default:
			return c, errors.New("owner event has invalid flags")
		}
	}
	return c, nil
}
func (s *session) ownerEvent(ctx context.Context, pin ownerDMPin, room, id, action string) (matrix.Event, ownerDMConfig, error) {
	ev, err := s.client.GetEvent(ctx, room, id)
	if err != nil {
		return ev, ownerDMConfig{}, err
	}
	var content matrix.MessageContent
	if ev.EventID != id || (ev.RoomID != "" && ev.RoomID != room) || ev.Type != "m.room.message" || ev.Sender != pin.Owner || json.Unmarshal(ev.Content, &content) != nil || isRedacted(ev) || (content.RelatesTo != nil && content.RelatesTo.RelType == "m.replace") {
		return ev, ownerDMConfig{}, errors.New("unverified owner event")
	}
	age := s.app.Now().Sub(time.UnixMilli(ev.OriginServerTS))
	if age > 15*time.Minute || age < -2*time.Minute {
		return ev, ownerDMConfig{}, errors.New("owner event outside freshness window")
	}
	flags, err := ownerCommand(content.Body, action, s.name)
	if err != nil {
		return ev, flags, err
	}
	var member struct {
		Membership string `json:"membership"`
	}
	if err = s.client.GetState(ctx, room, "m.room.member", s.creds.UserID, &member); err != nil {
		return ev, flags, err
	}
	if member.Membership != "join" {
		return ev, flags, errors.New("identity is not joined to request room")
	}
	ev.RoomID = room
	return ev, flags, nil
}
func (s *session) ownerOrdered(ctx context.Context, ev matrix.Event, store ownerDMBindings, owner string) error {
	for _, r := range store.Records {
		if r.ID == ev.EventID {
			return errors.New("owner event already used")
		}
	}
	previous, ok := store.Watermarks[owner]
	if !ok {
		return nil
	}
	if ev.OriginServerTS > previous.TS {
		return nil
	}
	if ev.OriginServerTS < previous.TS || ev.RoomID != previous.Room || ev.EventID == previous.ID {
		return errors.New("owner event is not strictly newer than order watermark")
	}
	// Equal timestamps need actual room order; event IDs are not an order.
	from := ""
	seen := map[string]bool{}
	candidate := false
	for pages := 0; pages < 100; pages++ {
		page, err := s.client.Messages(ctx, ev.RoomID, from, "b", 100)
		if err != nil {
			return err
		}
		for _, item := range page.Chunk {
			if item.EventID == ev.EventID {
				candidate = true
			}
			if item.EventID == previous.ID {
				if candidate {
					return nil
				}
				return errors.New("owner event precedes order watermark in room order")
			}
		}
		next := firstNonEmpty(page.End, page.NextBatch)
		if next == "" || seen[next] {
			break
		}
		seen[next] = true
		from = next
	}
	return errors.New("cannot establish owner event order")
}

func cmdOwnerDM(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("usage: a2a owner-dm bind|unbind --event ROOM EVENT | ensure|set|unset|status")
	}
	action := args[0]
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	f := ownerFiles(p, name)
	pin := f.pin()
	// Host-mode requests are a true no-op, even without credentials or a config.
	if (action == "bind" || action == "unbind") && pin.Mode != "owner-channel" {
		return nil
	}
	c, st, err := f.load()
	if action == "status" {
		if len(args) != 1 {
			return usagef("owner-dm status takes no flags")
		}
		if err != nil {
			fmt.Fprintln(a.Stderr, "a2a: "+sanitizeLine(err.Error())+"; owner-dm feature off")
			a.printf("invalid: %s\n", sanitizeLine(err.Error()))
			return nil
		}
		if c == nil {
			a.printf("never-configured\n")
			return nil
		}
		state := "inactive"
		if c.Pending {
			state = "pending (expires at " + st.ModTime().Add(24*time.Hour).UTC().Format(time.RFC3339) + ")"
		}
		if c.Active {
			state = "active"
		}
		if pin.Owner != c.Owner {
			state = "inactive (pin owner differs)"
		}
		audience := "inactive: pin owner differs"
		if pin.Owner == c.Owner {
			s, e := a.session()
			if e != nil {
				audience = e.Error()
			} else {
				joined, e := s.ownerAudience(ctx, c.Room, c.Owner, c.Pending)
				if e != nil {
					audience = e.Error()
				} else {
					audience = "valid"
					if c.Pending && joined {
						audience = "would activate: owner joined"
					}
				}
			}
		}
		a.printf("%s\nconfig %s\nfingerprint %s\nmtime %s\naudience: %s\ntag forced off in S1\n", state, string(marshalNoEscape(c)), ownerFingerprint(c), st.ModTime().UTC().Format(time.RFC3339Nano), sanitizeLine(audience))
		return nil
	}
	if err != nil {
		return err
	}
	var flags ownerDMConfig
	flags.Render = "in-frame"
	var ev matrix.Event
	confirmedOwner := ""
	room := ""
	var s *session
	if action == "bind" || action == "unbind" {
		if len(args) != 4 || args[1] != "--event" || !strings.HasPrefix(args[3], "$") {
			return usagef("owner-dm %s accepts only --event ROOM EVENT; flags come from the event", action)
		}
		s, err = a.session()
		if err != nil {
			return err
		}
		room, _, err = s.resolveRoom(ctx, args[2])
		if err != nil {
			return err
		}
		ev, flags, err = s.ownerEvent(ctx, pin, room, args[3], action)
		if err != nil {
			return err
		}
	} else {
		if action != "ensure" && action != "set" && action != "unset" {
			return usagef("unknown owner-dm command %q", action)
		}
		fs := newFlagSet("owner-dm " + action)
		rf := fs.String("room", "", "existing room id")
		tag := fs.Bool("tag", false, "stored only in S1")
		mirror := fs.Bool("mirror", false, "mirror final messages")
		render := fs.String("render", "in-frame", "stored rendering")
		pos, e := parseInterspersed(fs, args[1:])
		if e != nil {
			return e
		}
		if len(pos) != 0 || (*render != "in-frame" && *render != "outside-frame") || (action == "set" && !ownerRoomID.MatchString(*rf)) || (action != "set" && *rf != "") || (action == "unset" && len(args) != 1) {
			return usagef("invalid owner-dm %s arguments", action)
		}
		flags = ownerDMConfig{Tag: *tag, Mirror: *mirror, Render: *render}
		room = *rf
		target := pin.Owner
		if action == "unset" {
			target = "unset"
		} else if !ownerMXID.MatchString(target) {
			return errors.New("owner-dm requires a pin naming an owner")
		}
		confirmedOwner = target
		if err = a.ownerTypeBack(target); err != nil {
			return err
		}
		s, err = a.session()
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unlock, err := f.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-read all identity inputs after waiting for another writer/mirror.
	pin = f.pin()
	c, st, err = f.load()
	if err != nil {
		return err
	}
	if action == "bind" || action == "unbind" {
		if pin.Mode != "owner-channel" {
			return nil
		}
		ev, flags, err = s.ownerEvent(ctx, pin, room, ev.EventID, action)
		if err != nil {
			return err
		}
	}
	if confirmedOwner != "" && action != "unset" && confirmedOwner != pin.Owner {
		return errors.New("owner-dm pin changed after confirmation")
	}
	if action == "unset" && c == nil {
		a.printf("never-configured\n")
		return nil
	}
	store := ownerDMBindings{}
	if ev.EventID != "" {
		store, err = f.bindings()
		if err != nil {
			return err
		}
	}
	if ev.EventID != "" {
		if err = s.ownerOrdered(ctx, ev, store, pin.Owner); err != nil {
			return err
		}
	}
	if pin.Owner == "" && action != "unset" {
		return errors.New("owner-dm pin no longer names an owner")
	}
	bindLike := action == "bind" || action == "ensure"
	transition := ""
	if bindLike && c != nil && c.Owner == pin.Owner {
		transition, err = s.ownerTransition(ctx, c, st)
		if err != nil {
			return err
		}
	}
	current := c != nil && c.Owner == pin.Owner
	if bindLike && current && c.Pending {
		return errors.New("owner-dm already has a pending claim")
	}
	flagChange := bindLike && current && c.Active
	if flagChange {
		if action == "bind" && room != c.Room {
			return errors.New("active owner-dm flags must change in bound room")
		}
		if _, err = s.ownerAudience(ctx, c.Room, c.Owner, false); err != nil {
			return err
		}
	}
	if action == "set" {
		if _, err = s.ownerAudience(ctx, room, pin.Owner, true); err != nil {
			return err
		}
	}
	generation := uint64(0)
	if flagChange {
		generation = c.Generation
	} else {
		generation, err = ownerNext(c)
		if err != nil {
			return err
		}
	}
	// Every request check has passed: persist its single-use claim/watermark first.
	if ev.EventID != "" {
		store.accept(ownerDMMutation{ID: ev.EventID, Room: room, Owner: pin.Owner, TS: ev.OriginServerTS, Accepted: a.Now().UnixMilli(), Generation: generation}, a.Now())
		if err = ownerWrite(ctx, f.path("bindings"), store); err != nil {
			return err
		}
	}
	if flagChange {
		c.Tag, c.Mirror, c.Render = flags.Tag, flags.Mirror, flags.Render
		if action == "bind" {
			c.BoundBy = ev.EventID
		}
		if err = ownerWrite(ctx, f.path("json"), c); err != nil {
			return err
		}
		if err = s.ownerSaveTransition(ctx, f, c, transition); err != nil {
			return err
		}
	} else if action == "unset" || action == "unbind" {
		if c == nil {
			// The verified unbind is already consumed above; no binding exists to remove.
			a.printf("never-configured\n")
			return nil
		}
		if err = s.ownerSubscription(ctx, c, false); err != nil {
			return err
		}
		c.Generation = generation
		c.Active = false
		c.Pending = false
		if err = ownerWrite(ctx, f.path("json"), c); err != nil {
			return err
		}
	} else {
		if action != "set" {
			room, err = s.ownerNewRoom(ctx, pin.Owner)
			if err != nil {
				return err
			}
		}
		ownJoin, err := s.ownerAudience(ctx, room, pin.Owner, true)
		if err != nil {
			return err
		}
		if err = f.exclude(ctx, room); err != nil {
			return err
		}
		if c != nil && c.SubID != "" {
			if err = s.ownerSubscription(ctx, c, false); err != nil {
				return err
			}
		}
		c = &ownerDMConfig{V: 1, Generation: generation, Owner: pin.Owner, Room: room, Pending: !ownJoin, Active: ownJoin, Tag: flags.Tag, Mirror: flags.Mirror, Render: flags.Render, BoundBy: ev.EventID}
		if err = s.ownerSubscription(ctx, c, true); err != nil {
			return err
		}
		if err = ownerWrite(ctx, f.path("json"), c); err != nil {
			return err
		}
		if ownJoin {
			if err = s.ownerSaveTransition(ctx, f, c, "activated"); err != nil {
				return err
			}
		}
	}
	a.printf("owner-dm %s: generation %d, room %s, active=%t pending_join=%t; tag forced off in S1\n", action, c.Generation, sanitizeLine(c.Room), c.Active, c.Pending)
	return nil
}

func (a *App) ownerTypeBack(want string) error {
	if a.ownerConfirm != nil {
		return a.ownerConfirm(want)
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return errors.New("owner-dm terminal command requires interactive stdin and stdout")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	fmt.Fprintf(tty, "Type %s to confirm: ", want)
	got, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSuffix(got, "\n") != want {
		return errors.New("owner-dm type-back mismatch")
	}
	return nil
}
