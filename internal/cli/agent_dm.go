package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const agentDMUsage = "usage: a2a agent-dm ensure|unset|leave FULL_PEER | status [FULL_PEER] [--json] | say FULL_PEER --kind KIND BODY"

func cmdAgentDM(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", agentDMUsage)
	}
	action := args[0]
	peer, body, kind := "", "", ""
	asJSON := false
	switch action {
	case "status":
		for _, arg := range args[1:] {
			if arg == "--json" && !asJSON {
				asJSON = true
			} else if peer == "" && !strings.HasPrefix(arg, "--") {
				peer = arg
			} else {
				return usagef("%s", agentDMUsage)
			}
		}
	case "ensure", "unset", "leave":
		if len(args) != 2 {
			return usagef("%s", agentDMUsage)
		}
		peer = args[1]
	case "say":
		if len(args) != 5 || args[2] != "--kind" || !contains(Kinds, args[3]) {
			return usagef("%s", agentDMUsage)
		}
		peer, kind, body = args[1], args[3], args[4]
		if strings.TrimSpace(body) == "" || len(body) > agentDMMaxBody {
			return usagef("agent-dm: body must contain 1..%d bytes", agentDMMaxBody)
		}
	default:
		return usagef("%s", agentDMUsage)
	}
	if peer != "" && !agentFullID(peer) {
		return usagef("agent-dm: exact full supported peer MXID required")
	}
	if a.human {
		return errors.New("agent-dm: human credentials refused")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if action == "status" {
		return s.agentStatus(ctx, peer, asJSON)
	}
	r, err := agentPair(s.creds.UserID, peer)
	if err != nil {
		return err
	}
	if _, err = s.agentAdmitted(peer); err != nil {
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
	if _, err = s.agentAdmitted(peer); err != nil {
		return err
	}
	st, _, err := s.agentRecords()
	if err != nil {
		return err
	}
	d, known := st.Pairs[peer]
	if !known {
		d = agentDMRecord{Contract: r.Contract, Caller: r.Sender, Connection: st.Connection, Peer: peer, PairKey: r.PairKey, Enabled: true, SendAttempts: []agentDMSend{}}
	}
	save := func() error { st.Pairs[peer] = d; return s.saveAgentRecords(ctx, st) }
	if known && d.Room != nil && s.adminRoomExcluded(*d.Room) {
		return errors.New("agent-dm: protected admin room refused")
	}
	switch action {
	case "unset", "leave":
		if !known {
			a.printf("absent-own-record\n")
			return nil
		}
		d.Enabled, d.Qualified = false, false
		if err = save(); err != nil {
			return err
		}
		// This feature creates no subscription rows. Generic, principal and
		// explicit join rows belong to their existing owners and remain intact.
		if action == "unset" {
			a.printf("inactive; room and recovery evidence retained\n")
			return nil
		}
		if d.Room == nil {
			return errors.New("agent-dm: no exact bound room to leave; recovery fence retained")
		}
		if d.LeaveAttempt == "confirmed" {
			a.printf("inactive; own leave already acknowledged\n")
			return nil
		}
		if d.LeaveAttempt != "" {
			return errors.New("agent-dm: cleanup_pending; previous own leave has uncertain ACK")
		}
		if _, err = s.agentAdmitted(peer); err != nil {
			return err
		}
		d.LeaveAttempt = "started"
		if err = save(); err != nil {
			return err
		}
		if err = s.client.WithoutRedirects().Leave(ctx, *d.Room); err != nil {
			return fmt.Errorf("agent-dm: cleanup_pending; own leave attempted, ACK uncertain: %w", err)
		}
		d.LeaveAttempt = "confirmed"
		if err = save(); err != nil {
			return err
		}
		a.printf("inactive; own leave acknowledged; creator authority is unchanged\n")
		return nil
	case "say":
		if !known || !d.Enabled || !d.Qualified || d.Room == nil || d.LeaveAttempt != "" {
			return errors.New("agent-dm: no active qualified pair binding")
		}
		if len(d.SendAttempts) >= 256 {
			return errors.New("agent-dm: send evidence bound reached")
		}
		for _, attempt := range d.SendAttempts {
			if attempt.State == "started" {
				return errors.New("agent-dm: uncertain send retained; no automatic resend")
			}
		}
		if err = s.agentAliasMatches(ctx, r, d.Room); err != nil && !errors.Is(err, matrix.ErrUnknownAlias) {
			return err
		}
		state, err := s.observeAgentPair(ctx, r, *d.Room, false)
		if err != nil {
			return err
		}
		if state != "active" {
			return errors.New("agent-dm: pair not active")
		}
		if _, err = s.agentAdmitted(peer); err != nil {
			return err
		}
		h := Header{V: 1, Type: "message", From: s.creds.UserID, To: []string{peer}, Kind: kind, At: a.Now().UTC().Format(time.RFC3339)}
		content, err := framedTextContent(formatHeaderBody(h, body), map[string]any{ContentKey: h, "m.mentions": matrix.Mentions{UserIDs: []string{peer}}}, formatHeaderBody(h, ""), body, "")
		if err != nil {
			return err
		}
		if err = matrix.CheckEventContentBudget(content); err != nil {
			return err
		}
		txn := "a2a-adm-" + matrix.NewTxnID()
		d.SendAttempts = append(d.SendAttempts, agentDMSend{Transaction: txn, State: "started"})
		if err = save(); err != nil {
			return err
		}
		id, err := s.client.WithoutRedirects().SendMessageTxn(ctx, *d.Room, txn, content)
		if err != nil {
			return fmt.Errorf("agent-dm: send attempted; uncertain outcome retained, never automatically resent: %w", err)
		}
		d.SendAttempts[len(d.SendAttempts)-1].State = "confirmed"
		d.SendAttempts[len(d.SendAttempts)-1].Event = id
		if err = save(); err != nil {
			return err
		}
		a.printf("sent %s; server ACK only\n", sanitizeLine(id))
		return nil
	}
	// ensure never joins. Once an attempt fence exists it can discover the exact
	// alias/mapping, but cannot allocate a replacement after any uncertainty.
	if known && d.LeaveAttempt != "" {
		return errors.New("agent-dm: inactive after own leave; mapping retained")
	}
	if err = s.agentAliasMatches(ctx, r, d.Room); err != nil && !errors.Is(err, matrix.ErrUnknownAlias) {
		return err
	}
	if d.Room == nil {
		room, resolveErr := s.client.WithoutRedirects().ResolvePairAlias(ctx, r.Alias)
		if resolveErr == nil {
			if !matrix.ValidVerificationID(room, '!') || s.adminRoomExcluded(room) {
				return errors.New("agent-dm: invalid/protected alias room")
			}
			d.Room = &room
			d.Enabled = true
			d.Qualified = false
			// Only exact alias recovery establishes this unverified local context.
			if err = save(); err != nil {
				return err
			}
		} else if !errors.Is(resolveErr, matrix.ErrUnknownAlias) {
			return resolveErr
		} else if r.Creator != s.creds.UserID {
			a.printf("needs_creator\n")
			return nil
		} else {
			if d.CreateAttempt != "" {
				return errors.New("agent-dm: unknown; create recovery fence forbids replacement creation")
			}
			if _, err = s.agentAdmitted(peer); err != nil {
				return err
			}
			d.Enabled = true
			d.CreateAttempt = "started"
			if err = save(); err != nil {
				return err
			}
			aID, bID := r.Sender, r.Recipient
			if aID > bID {
				aID, bID = bID, aID
			}
			state := []matrix.InitialStateEvent{}
			for _, item := range []struct {
				typ     string
				content any
			}{
				{agentDMMarker, map[string]any{"contract": agentDMVersion, "pair": []string{aID, bID}, "creator_mxid": r.Creator}},
				{"m.room.join_rules", map[string]string{"join_rule": "invite"}},
				{"m.room.history_visibility", map[string]string{"history_visibility": "joined"}},
				{"m.room.guest_access", map[string]string{"guest_access": "forbidden"}},
			} {
				raw, _ := json.Marshal(item.content)
				state = append(state, matrix.InitialStateEvent{Type: item.typ, Content: raw})
			}
			aliasLocal, _, _ := strings.Cut(strings.TrimPrefix(r.Alias, "#"), ":")
			room, createErr := s.client.WithoutRedirects().CreateRoom(ctx, matrix.CreateRoomRequest{IsDirect: true, Visibility: "private", Preset: "private_chat", RoomAliasName: aliasLocal, InitialState: state})
			if createErr != nil {
				return fmt.Errorf("agent-dm: create attempted; unknown ACK retained, no replacement create: %w", createErr)
			}
			if !matrix.ValidVerificationID(room, '!') || s.adminRoomExcluded(room) {
				return errors.New("agent-dm: invalid create ACK; recovery fence retained")
			}
			d.Room = &room
			d.CreateAttempt = "confirmed"
			if err = save(); err != nil {
				return err
			}
			// No peer invitation is emitted until full private policy readback.
			if _, err = s.observeAgentPair(ctx, r, room, true); err != nil {
				return err
			}
		}
	}
	room := *d.Room
	state, err := s.observeAgentPair(ctx, r, room, false)
	if err != nil {
		// Only our own confirmed creation may run the pre-invite phase. A
		// recovered alias cannot be repaired by inviting an absent participant.
		if d.CreateAttempt == "confirmed" && d.InviteAttempt == "" && r.Creator == s.creds.UserID {
			if _, preErr := s.observeAgentPair(ctx, r, room, true); preErr == nil {
				if _, err = s.agentAdmitted(peer); err != nil {
					return err
				}
				d.InviteAttempt = "started"
				if err = save(); err != nil {
					return err
				}
				if err = s.client.WithoutRedirects().Invite(ctx, room, peer); err != nil {
					return fmt.Errorf("agent-dm: invitation attempted; unknown ACK retained: %w", err)
				}
				d.InviteAttempt = "confirmed"
				if err = save(); err != nil {
					return err
				}
				state, err = s.observeAgentPair(ctx, r, room, false)
			}
		}
		if err != nil {
			if r.Creator != s.creds.UserID {
				return fmt.Errorf("agent-dm: needs_explicit_join/unknown; exact room %s; use existing explicit join, then ensure: %w", sanitizeLine(room), err)
			}
			return err
		}
	}
	if state == "active" {
		d.Enabled, d.Qualified = true, true
		if err = save(); err != nil {
			return err
		}
		a.printf("active %s\n", sanitizeLine(room))
		return nil
	}
	d.Enabled, d.Qualified = true, false
	if err = save(); err != nil {
		return err
	}
	a.printf("pending_invite %s; peer must explicitly join as itself, then ensure\n", sanitizeLine(room))
	return nil
}

func (s *session) agentWhoami(ctx context.Context) error {
	who, err := s.client.WithoutRedirects().Whoami(ctx)
	if err != nil {
		return err
	}
	if who.UserID != s.creds.UserID {
		return errors.New("agent-dm: selected caller whoami mismatch")
	}
	return nil
}
func (s *session) agentAliasMatches(ctx context.Context, r agentDMRoute, known *string) error {
	room, err := s.client.WithoutRedirects().ResolvePairAlias(ctx, r.Alias)
	if err != nil {
		return err
	}
	if !matrix.ValidVerificationID(room, '!') || known != nil && room != *known {
		return errors.New("agent-dm: conflicting alias mapping")
	}
	return nil
}
func (s *session) observeAgentPair(ctx context.Context, r agentDMRoute, room string, preInvite bool) (string, error) {
	if s.adminRoomExcluded(room) {
		return "invalid", errors.New("agent-dm: protected admin room")
	}
	events, err := s.client.GetRoomPolicyState(ctx, room)
	if err != nil {
		return "unknown", err
	}
	return agentAudience(events, r, preInvite)
}

type agentDMStatus struct {
	agentDMRoute
	Connection     string            `json:"connection_id"`
	Server         string            `json:"selected_server"`
	Declared       *agentDMAdmission `json:"declared_admission"`
	Observed       string            `json:"observed"`
	LocalEnabled   bool              `json:"local_enabled"`
	LocalQualified bool              `json:"local_qualified"`
	Protection     string            `json:"host_protection"`
	Recovery       agentDMRecovery   `json:"recovery"`
}

type agentDMRecovery struct {
	Create        string `json:"create_attempt"`
	Invite        string `json:"invite_attempt"`
	Leave         string `json:"leave_attempt"`
	UncertainSend bool   `json:"uncertain_send"`
	RetainedSends int    `json:"retained_sends"`
}

func (s *session) agentStatus(ctx context.Context, peer string, asJSON bool) error {
	st, _, loadErr := s.agentRecords()
	connection, err := s.agentConnection()
	if err != nil {
		return err
	}
	peers := []string{}
	if peer != "" {
		peers = append(peers, peer)
	} else {
		peers = sortedKeys(st.Pairs)
	}
	if len(peers) > 64 {
		return errors.New("agent-dm: status bound exceeded")
	}
	if loadErr != nil && peer == "" {
		return errors.New("agent-dm: unknown; pair records unreadable")
	}
	statuses := []agentDMStatus{}
	whoErr := s.agentWhoami(ctx)
	for _, p := range peers {
		r, err := agentPair(s.creds.UserID, p)
		if err != nil {
			return err
		}
		out := agentDMStatus{agentDMRoute: r, Connection: connection, Server: s.server, Observed: "not observed", Protection: "OS-user protected; not a same-user security boundary"}
		d, known := st.Pairs[p]
		if known {
			out.Room = d.Room
			out.LocalEnabled = d.Enabled
			out.LocalQualified = d.Qualified
			out.Recovery = agentDMRecovery{Create: d.CreateAttempt, Invite: d.InviteAttempt, Leave: d.LeaveAttempt, RetainedSends: len(d.SendAttempts)}
			for _, attempt := range d.SendAttempts {
				if attempt.State == "started" {
					out.Recovery.UncertainSend = true
				}
			}
		}
		ad, adErr := s.agentAdmitted(p)
		if adErr == nil {
			out.Declared = &ad
		}
		switch {
		case loadErr != nil || whoErr != nil:
			out.State = "unknown"
		case adErr != nil:
			out.State = "invalid"
			out.Observed = "host admission unavailable or refused"
		case !known:
			out.State = "absent-own-record"
			if s.creds.UserID != r.Creator {
				out.State = "needs_creator"
			}
		case d.LeaveAttempt == "started":
			out.State = "cleanup_pending"
		case !d.Enabled:
			out.State = "inactive"
		case d.Room == nil:
			out.State = "unknown"
			out.Observed = "retained create fence; no known room"
		default:
			if err := s.agentAliasMatches(ctx, r, d.Room); err != nil && !errors.Is(err, matrix.ErrUnknownAlias) {
				out.State = "invalid"
				out.Observed = "conflicting or unreadable alias"
				break
			}
			state, err := s.observeAgentPair(ctx, r, *d.Room, false)
			out.State = state
			if err != nil {
				out.Observed = sanitizeLine(err.Error())
				if state == "unknown" && s.creds.UserID != r.Creator {
					out.State = "needs_explicit_join"
				}
			} else {
				out.Observed = "complete strict current state; status did not activate local binding"
			}
		}
		statuses = append(statuses, out)
	}
	if asJSON {
		return s.app.printJSON(statuses)
	}
	if len(statuses) == 0 {
		s.app.printf("absent-own-record\n")
	}
	for _, out := range statuses {
		room := "null"
		if out.Room != nil {
			room = sanitizeLine(*out.Room)
		}
		s.app.printf("%s %s room=%s; %s\n", sanitizeLine(out.Recipient), out.State, room, out.Observed)
	}
	return nil
}
