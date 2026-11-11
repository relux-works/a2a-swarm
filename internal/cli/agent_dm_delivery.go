package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type agentDMGate struct {
	Peer   string
	Active bool
}
type agentDMDelivery struct {
	Rooms   map[string]agentDMGate
	Unknown bool
	Stamp   string
}
type agentDMHeld struct {
	Room  string          `json:"room_id"`
	ID    string          `json:"event_id"`
	Event json.RawMessage `json:"event"`
}
type agentDMHeldStore struct {
	Contract string        `json:"contract"`
	Entries  []agentDMHeld `json:"entries"`
}

var errAgentHeldCapacity = errors.New("agent-dm: held-mail capacity reached; cursor retained")

// Explicit join keeps its existing remote semantics. If a host-admitted pair's
// exact deterministic alias selects the joined room, retain an unqualified
// binding before installing the ordinary join subscription. No marker alone
// establishes context and this function never joins or activates anything.
func (s *session) captureAgentJoinedRoom(ctx context.Context, room string) error {
	raw, err := agentRead(agentDMDir(s.paths, s.name), "admission.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("agent-dm: cannot read join admission; no room subscription installed")
	}
	v, err := matrix.ParseStrictJSON(raw)
	if err != nil {
		return errors.New("agent-dm: malformed join admission")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("agent-dm: malformed join admission")
	}
	peers, ok := m["peers"].([]any)
	if !ok || len(peers) == 0 || len(peers) > 64 {
		return errors.New("agent-dm: invalid join admission peers")
	}
	connection, err := s.agentConnection()
	if err != nil {
		return err
	}
	var matched string
	for _, item := range peers {
		p, ok := item.(map[string]any)
		if !ok {
			return errors.New("agent-dm: invalid join admission peer")
		}
		peer := agentString(p, "mxid")
		if _, err := parseAgentAdmission(raw, s.creds.UserID, connection, peer); err != nil {
			return err
		}
		r, err := agentPair(s.creds.UserID, peer)
		if err != nil {
			return err
		}
		resolved, err := s.client.WithoutRedirects().ResolvePairAlias(ctx, r.Alias)
		if errors.Is(err, matrix.ErrUnknownAlias) {
			continue
		}
		if err != nil {
			return errors.New("agent-dm: pair alias observation unavailable; join refused before subscription")
		}
		if err == nil && resolved == room {
			if matched != "" {
				return errors.New("agent-dm: conflicting joined pair mappings")
			}
			matched = peer
		}
	}
	if matched == "" {
		return nil
	}
	if _, err = s.agentAdmitted(matched); err != nil {
		return err
	}
	if err = s.agentWhoami(ctx); err != nil {
		return err
	}
	unlock, err := agentLock(ctx, agentDMDir(s.paths, s.name))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err = s.agentAdmitted(matched); err != nil {
		return err
	}
	st, _, err := s.agentRecords()
	if err != nil {
		return err
	}
	d, known := st.Pairs[matched]
	if known {
		if d.Room == nil || *d.Room != room {
			return errors.New("agent-dm: joined room conflicts with retained pair mapping")
		}
		return nil
	}
	r, _ := agentPair(s.creds.UserID, matched)
	st.Pairs[matched] = agentDMRecord{Contract: agentDMVersion, Caller: s.creds.UserID, Connection: connection, Peer: matched, PairKey: r.PairKey, Room: &room, Enabled: true, SendAttempts: []agentDMSend{}}
	return s.saveAgentRecords(ctx, st)
}

// Only a local binding establishes pair context. Peer markers and headers do
// not create it. Admission/qualification failure closes its ordinary mail gate.
func (s *session) refreshAgentGate(ctx context.Context) (agentDMDelivery, error) {
	stamp := s.agentGateStamp()
	st, _, err := s.agentRecords()
	out := agentDMDelivery{Rooms: map[string]agentDMGate{}, Unknown: err != nil}
	out.Stamp = stamp
	if err != nil {
		return out, nil
	}
	var whoErr error
	if len(st.Pairs) > 0 {
		whoErr = s.agentWhoami(ctx)
	}
	for peer, d := range st.Pairs {
		if d.Room == nil {
			continue
		}
		g := agentDMGate{Peer: peer}
		if whoErr == nil && d.Enabled && d.Qualified && d.LeaveAttempt == "" {
			if _, err := s.agentAdmitted(peer); err == nil {
				r, _ := agentPair(s.creds.UserID, peer)
				if err := s.agentAliasMatches(ctx, r, d.Room); err == nil || errors.Is(err, matrix.ErrUnknownAlias) {
					state, err := s.observeAgentPair(ctx, r, *d.Room, false)
					g.Active = err == nil && state == "active"
				}
			}
		}
		out.Rooms[*d.Room] = g
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if stamp != s.agentGateStamp() {
		return out, errDeliveryMutesChanged
	}
	return out, nil
}
func (s *session) agentGateStamp() string {
	h := sha256.New()
	for _, item := range []struct{ dir, name string }{
		{agentDMDir(s.paths, s.name), "records.json"},
		{agentDMDir(s.paths, s.name), "admission.json"},
	} {
		raw, err := agentRead(item.dir, item.name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				h.Write([]byte("absent\x00"))
			} else {
				h.Write([]byte("unreadable\x00"))
			}
		} else {
			h.Write(raw)
			h.Write([]byte{0})
		}
	}
	for _, path := range []string{ownerFiles(s.paths, s.name).path("pin"), ownerFiles(s.paths, s.name).path("principals.json"), principalFiles(s.paths, s.name).path("pin")} {
		raw, _, err := ownerRead(path)
		if err != nil {
			h.Write([]byte("unreadable"))
		} else {
			h.Write(raw)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agentAddressed consumes the strict content/header trees. The exact actual
// Matrix sender must match the full sender context, one full To and one user
// mention. Projection/thread/body-mention selectors cannot loosen this gate.
func agentAddressed(ev matrix.Event, me, peer string) bool {
	if ev.Type != "m.room.message" || ev.Sender != peer || ev.IsRedacted() {
		return false
	}
	if !matrix.ValidVerificationID(ev.EventID, '$') {
		return false
	}
	v, err := matrix.ParseStrictJSON(ev.Content)
	if err != nil {
		return false
	}
	m, ok := v.(map[string]any)
	if !ok || agentString(m, "msgtype") != "m.text" {
		return false
	}
	for k := range m {
		for _, name := range []string{"msgtype", "body", ContentKey, "m.mentions", "m.relates_to"} {
			if k != name && strings.EqualFold(k, name) {
				return false
			}
		}
	}
	if body, ok := m["body"].(string); !ok || len(body) == 0 || len(body) > matrix.VerificationContentLimit {
		return false
	}
	h, ok := m[ContentKey].(map[string]any)
	if !ok {
		return false
	}
	allowed := map[string]bool{"v": true, "type": true, "from": true, "to": true, "at": true, "kind": true, "reply_by": true, "re": true, "slug": true, "title": true, "topic": true, "trunk": true, "subtype": true, "subject": true, "product_alias": true}
	for k := range h {
		if !allowed[k] {
			return false
		}
		if k != "v" && k != "to" {
			if _, ok := h[k].(string); !ok {
				return false
			}
		}
	}
	version, ok := h["v"].(json.Number)
	if !ok || version != "1" || agentString(h, "type") != "message" || agentString(h, "from") != ev.Sender || !contains(Kinds, agentString(h, "kind")) {
		return false
	}
	if _, err := time.Parse(time.RFC3339, agentString(h, "at")); err != nil {
		return false
	}
	to, ok := h["to"].([]any)
	if !ok || len(to) != 1 || to[0] != me {
		return false
	}
	mentions, ok := m["m.mentions"].(map[string]any)
	if !ok {
		return false
	}
	for k := range mentions {
		if k != "user_ids" && k != "room" {
			return false
		}
	}
	if room, present := mentions["room"]; present && room != false {
		return false
	}
	users, ok := mentions["user_ids"].([]any)
	if !ok || len(users) != 1 || users[0] != me {
		return false
	}
	return true
}

// Decision: allow, hold (well-formed peer mail before qualification), or drop.
func (g agentDMDelivery) decision(ev matrix.Event, room, me string, principals []string) string {
	if mandatoryPrincipal(ev, principals) {
		return "allow"
	}
	pair, known := g.Rooms[room]
	if !known {
		if g.Unknown && ev.Type == "m.room.message" {
			return "hold"
		}
		return "allow"
	}
	if ev.Type != "m.room.message" {
		return "allow"
	}
	if !agentAddressed(ev, me, pair.Peer) {
		return "drop"
	}
	if !pair.Active {
		return "hold"
	}
	return "allow"
}

func (s *session) readAgentHeld() (agentDMHeldStore, error) {
	out := agentDMHeldStore{Contract: agentDMVersion, Entries: []agentDMHeld{}}
	raw, err := agentRead(agentDMDir(s.paths, s.name), "held.json")
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = agentDecode(raw, &out); err != nil || out.Contract != agentDMVersion || len(out.Entries) > 512 {
		return out, errors.New("agent-dm: invalid held mail")
	}
	for _, entry := range out.Entries {
		if !matrix.ValidVerificationID(entry.Room, '!') || !matrix.ValidVerificationID(entry.ID, '$') {
			return out, errors.New("agent-dm: invalid held mail identity")
		}
	}
	return out, nil
}

// Holds are owned by the existing delivery state lock, never the pair operation
// lock. A crash after mailbox append replays safely through mailbox deduplication.
func (s *session) persistAgentHeld(ctx context.Context, mb *config.Mailbox, add []agentDMHeld) error {
	held, err := s.readAgentHeld()
	if err != nil {
		return err
	}
	if len(held.Entries) == 0 && len(add) == 0 {
		return nil
	}
	seen := map[string]bool{}
	entries := []agentDMHeld{}
	for _, entry := range append(held.Entries, add...) {
		key := entry.Room + "\x00" + entry.ID
		if mb.Has(entry.ID) || mb.IsRedacted(entry.Room, entry.ID) || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, entry)
	}
	if len(entries) > 512 {
		return errAgentHeldCapacity
	}
	held.Entries = entries
	raw, err := json.Marshal(held)
	if err != nil {
		return err
	}
	if len(raw) > matrix.VerificationEntityLimit {
		return errAgentHeldCapacity
	}
	return agentWrite(ctx, agentDMDir(s.paths, s.name), "held.json", held)
}

func (s *session) agentMailboxView(mb *config.Mailbox) {
	g := s.agentDelivery
	if g == nil || g.Stamp != s.agentGateStamp() {
		local, _, err := s.agentRecords()
		snap := agentDMDelivery{Rooms: map[string]agentDMGate{}, Unknown: err != nil}
		for peer, d := range local.Pairs {
			if d.Room != nil {
				snap.Rooms[*d.Room] = agentDMGate{Peer: peer}
			}
		}
		g = &snap
	}
	principals := s.principals
	// Standalone dispatch/ack sessions may not have started a sync consumer.
	// Load existing host authority locally before applying the peer-only view.
	if principals == nil && s.app != nil && (len(g.Rooms) > 0 || g.Unknown) {
		principals = s.readDeliveryPrincipals()
	}
	mb.SetPendingFilter(func(e config.MailEntry) bool {
		if principalMail(e, principals) {
			return true
		}
		if e.Kind != config.MailEvent {
			return true
		}
		var ev matrix.Event
		if json.Unmarshal(e.Event, &ev) != nil {
			return false
		}
		return g.decision(ev, e.Room, s.creds.UserID, principals) == "allow"
	})
}
