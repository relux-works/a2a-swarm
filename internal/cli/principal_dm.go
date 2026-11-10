package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Principal DM S1: the Owner DM S1 room shape for additional human principals
// named by a host-written pin. There is no Matrix-triggered bind, no mirror
// and no trust change; only the terminal creates or retires a room.

const principalDMPendingTTL = 24 * time.Hour
const principalDMMaxBody = 32 << 10

type principalDMPin struct {
	Principals []string `json:"principals"`
}
type principalDM struct {
	Generation uint64 `json:"generation"`
	Active     bool   `json:"active"`
	Pending    bool   `json:"pending_join"`
	Room       string `json:"room"`
	SubID      string `json:"sub_id"`
	CreatedAt  int64  `json:"created_at"`
}
type principalDMState struct {
	V   int                    `json:"v"`
	DMs map[string]principalDM `json:"dms"`
}

func principalFiles(p config.Paths, name string) ownerDMFiles {
	return ownerDMFiles{base: filepath.Join(p.Base, "principal-dm", name)}
}

// The pin is the host's allowlist. A missing pin pins nobody; an invalid one
// refuses every principal-dm mutation instead of being partially honoured.
func principalPin(f ownerDMFiles, forbidden ...string) (map[string]bool, error) {
	out := map[string]bool{}
	b, st, err := ownerRead(f.path("pin"))
	if err != nil {
		return nil, fmt.Errorf("invalid principal-dm pin: %w", err)
	}
	if st == nil {
		return out, nil
	}
	var raw map[string]json.RawMessage
	var p principalDMPin
	if config.DecodeConnectionJSON(b, &p) != nil || json.Unmarshal(b, &raw) != nil || len(raw) != 1 || raw["principals"] == nil || json.Unmarshal(raw["principals"], &p.Principals) != nil || p.Principals == nil {
		return nil, fmt.Errorf("invalid principal-dm pin %s: expected {\"principals\":[...]}", f.path("pin"))
	}
	for _, m := range p.Principals {
		if !validPrincipalID(m) || out[m] {
			return nil, fmt.Errorf("invalid principal-dm pin %s: bad or duplicate principal", f.path("pin"))
		}
		for _, id := range forbidden {
			if id != "" && strings.EqualFold(m, id) {
				return nil, errors.New("invalid principal-dm pin: may not list the pinned owner (use owner-dm) or self (is this identity)")
			}
		}
		out[m] = true
	}
	return out, nil
}

func principalLoad(f ownerDMFiles, clock ...time.Time) (principalDMState, error) {
	out := principalDMState{V: 1, DMs: map[string]principalDM{}}
	b, st, err := ownerRead(f.path("json"))
	if err != nil {
		return out, fmt.Errorf("invalid principal-dm config: %w", err)
	}
	if st == nil {
		return out, nil
	}
	now := time.Now()
	if len(clock) != 0 {
		now = clock[0]
	}
	bad := fmt.Errorf("invalid principal-dm config %s", f.path("json"))
	var top struct {
		V   int                        `json:"v"`
		DMs map[string]json.RawMessage `json:"dms"`
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil || len(raw) != 2 || ownerObject(b, map[string]byte{"v": 'i'}, &top) != nil || top.V != 1 || json.Unmarshal(raw["dms"], &top.DMs) != nil || top.DMs == nil {
		return out, bad
	}
	// Every field the CLI writes must be present, non-null and typed; unknown
	// fields refuse, so an entry the CLI never wrote is never acted on.
	shape := map[string]byte{"generation": 'i', "active": 'b', "pending_join": 'b', "room": 's', "sub_id": 's', "created_at": 'i'}
	for m, entry := range top.DMs {
		var fields map[string]json.RawMessage
		var d principalDM
		if !ownerMXID.MatchString(m) || json.Unmarshal(entry, &fields) != nil || len(fields) != len(shape) || ownerObject(entry, shape, &d) != nil ||
			!ownerRoomID.MatchString(d.Room) || d.Generation == 0 || d.CreatedAt <= 0 || d.CreatedAt > now.UnixMilli() || (d.Active && d.Pending) || ((d.Active || d.Pending) && d.SubID == "") {
			return out, fmt.Errorf("invalid principal-dm config %s: entry %s", f.path("json"), sanitizeLine(m))
		}
		out.DMs[m] = d
	}
	return out, nil
}

func (a *App) principalExpired(d principalDM) bool {
	return d.Pending && !a.Now().Before(time.UnixMilli(d.CreatedAt).Add(principalDMPendingTTL))
}

func (s *session) principalSubscription(ctx context.Context, principal, room, old string, add bool) (string, error) {
	configured, err := ownerFiles(s.paths, s.name).principals()
	if err != nil {
		return "", err
	}
	deadline, _ := ctx.Deadline()
	id := ""
	_, _, err = s.updateSubsBy(ctx, deadline, func(_ *config.State, list *SubList) (bool, error) {
		// Keep mandatory sender coverage independent of the DM lifecycle,
		// including a stale config that names a protected principal row.
		c := ownerDMConfig{Owner: principal, Room: room, SubID: old}
		if add {
			c.SubID = ""
		}
		changed := changeOwnerSubscription(list, &c, add, configured, s.app.Now())
		id = c.SubID
		return changed, nil
	})
	return id, err
}

// Principal rooms require joined history without changing Owner DM S1's policy.
func (s *session) principalAudience(ctx context.Context, room, principal string, allowInvite bool) (bool, error) {
	return s.dmAudience(ctx, room, principal, allowInvite, true)
}

func cmdPrincipalDM(ctx context.Context, a *App, args []string) error {
	use := "usage: a2a principal-dm ensure|unset @user:server | status [@user:server] | say @user:server <text|@file|->"
	if len(args) == 0 {
		return usagef("%s", use)
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
	f := principalFiles(p, name)
	switch action {
	case "status":
		if len(args) > 2 || (len(args) == 2 && !ownerMXID.MatchString(args[1])) {
			return usagef("%s", use)
		}
		return a.principalStatus(ctx, f, args[1:])
	case "ensure", "unset":
		if len(args) != 2 {
			return usagef("%s", use)
		}
	case "say":
		if len(args) < 3 || (len(args) > 3 && (args[2] == "-" || strings.HasPrefix(args[2], "@"))) {
			return usagef("give exactly one body: text, @file, or - (%s)", use)
		}
	default:
		return usagef("unknown principal-dm command %q", action)
	}
	principal := args[1]
	if !validPrincipalID(principal) {
		return usagef("invalid principal %q: expected @user:server", sanitizeLine(principal))
	}
	body := ""
	if action == "say" {
		if body, err = a.readBody(strings.Join(args[2:], " ")); err != nil {
			return err
		}
		if body == "" {
			return usagef("empty message body refused")
		}
		if len(body) > principalDMMaxBody {
			return usagef("message body over %d bytes refused", principalDMMaxBody)
		}
	}
	meta, err := config.LoadCredentialMetadata(p, name, a.human)
	if err != nil {
		return err
	}
	if err = principalAdmitted(f, ownerFiles(p, name), principal, action, meta.UserID); err != nil {
		return err
	}
	if action != "say" {
		if err = a.ownerTypeBack(principal); err != nil {
			return err
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if strings.EqualFold(principal, s.creds.UserID) {
		return errors.New("principal-dm: the principal is this identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unlock, err := f.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-read every host input after waiting for another writer.
	if err = principalAdmitted(f, ownerFiles(p, name), principal, action, s.creds.UserID); err != nil {
		return err
	}
	state, err := principalLoad(f, a.Now())
	if err != nil {
		return err
	}
	d, known := state.DMs[principal]
	switch action {
	case "unset":
		if !known {
			a.printf("never-configured\n")
			return nil
		}
		if d.Active || d.Pending {
			next, err := ownerNext(&ownerDMConfig{Generation: d.Generation})
			if err != nil {
				return err
			}
			d.Generation, d.Active, d.Pending = next, false, false
			state.DMs[principal] = d
			if err = ownerWrite(ctx, f.path("json"), state); err != nil {
				return err
			}
		}
		if _, err = s.principalSubscription(ctx, principal, d.Room, d.SubID, false); err != nil {
			return fmt.Errorf("principal-dm inactive; subscription cleanup pending; retry unset: %w", err)
		}
		d.SubID = ""
		state.DMs[principal] = d
		if err = ownerWrite(ctx, f.path("json"), state); err != nil {
			return err
		}
		a.printf("principal-dm unset: inactive; subscription cleanup complete\n")
		a.printf("principal-dm unset: %s, generation %d, room %s left in place\n", sanitizeLine(principal), d.Generation, sanitizeLine(d.Room))
		return nil
	case "say":
		if !known || !(d.Active || d.Pending) || a.principalExpired(d) {
			return errors.New("principal-dm: no active room for this principal; the host runs ensure")
		}
		joined, err := s.principalAudience(ctx, d.Room, principal, d.Pending)
		if err != nil {
			return err
		}
		if d.Pending && !joined {
			return errors.New("principal-dm: pending until the principal joins")
		}
		if err = principalAdmitted(f, ownerFiles(p, name), principal, action, s.creds.UserID); err != nil {
			return err
		}
		txn := make([]byte, 12)
		if _, err = rand.Read(txn); err != nil {
			return err
		}
		id, err := s.client.WithoutRedirects().SendMessageTxn(ctx, d.Room, "a2a-pdm-"+hex.EncodeToString(txn), map[string]any{"msgtype": "m.text", "body": body})
		if err != nil {
			return err
		}
		if d.Pending {
			d.Pending, d.Active = false, true
			state.DMs[principal] = d
			if err = ownerWrite(ctx, f.path("json"), state); err != nil {
				return err
			}
		}
		a.printf("sent %s to %s\n", sanitizeLine(id), sanitizeLine(principal))
		return nil
	}
	// ensure: an active room stays; a live pending claim activates or refuses;
	// a missing, inactive or expired one gets a fresh room.
	if known && d.Active {
		if _, err = s.principalAudience(ctx, d.Room, principal, false); err != nil {
			return fmt.Errorf("%w; run principal-dm unset, then ensure", err)
		}
		a.printf("principal-dm ensure: %s already active, generation %d, room %s\n", sanitizeLine(principal), d.Generation, sanitizeLine(d.Room))
		return nil
	}
	if known && d.Pending && !a.principalExpired(d) {
		joined, err := s.principalAudience(ctx, d.Room, principal, true)
		if err != nil {
			return err
		}
		if !joined {
			return errors.New("principal-dm already has a pending claim; waiting for the principal to join")
		}
		d.Pending, d.Active = false, true
		state.DMs[principal] = d
		if err = ownerWrite(ctx, f.path("json"), state); err != nil {
			return err
		}
		a.printf("principal-dm ensure: %s activated, generation %d, room %s\n", sanitizeLine(principal), d.Generation, sanitizeLine(d.Room))
		return nil
	}
	var prev *ownerDMConfig
	if known {
		prev = &ownerDMConfig{Generation: d.Generation}
	}
	generation, err := ownerNext(prev)
	if err != nil {
		return err
	}
	if err = principalAdmitted(f, ownerFiles(p, name), principal, action, s.creds.UserID); err != nil {
		return err
	}
	// Serialise with archive inventory and reserve a fail-closed exclusion
	// before creating anything. A failed exact-room write retains the fence.
	of := ownerFiles(p, name)
	ownerUnlock, err := of.lock(ctx)
	if err != nil {
		return err
	}
	defer ownerUnlock()
	exclusions, err := of.exclusions()
	if err != nil {
		return err
	}
	reserved := exclusions[principalArchiveReservation]
	if err = of.exclude(ctx, principalArchiveReservation); err != nil {
		return err
	}
	room, err := s.ownerNewRoom(ctx, principal)
	if err != nil {
		return err
	} // Unknown server outcome: keep the reservation.
	orphan := func(cause error, sub string) error {
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelCleanup()
		// Re-read account data even after a failed Put (lost response/local
		// state-save failure). The tentative ID is known before the Put.
		if sub != "" {
			if _, e := s.principalSubscription(cleanup, principal, room, sub, false); e != nil {
				cause = fmt.Errorf("%w; subscription cleanup pending for %s: %v", cause, sub, e)
			}
		}
		return fmt.Errorf("%w; room %s was created but not recorded", cause, sanitizeLine(room))
	}
	if err = of.exclude(ctx, room); err != nil {
		return orphan(err, "")
	}
	// Only our own reservation can be released. A prior unresolved creation
	// keeps archive closed until the host accounts for every orphan room.
	if !reserved {
		exclusions, err = of.exclusions()
		if err != nil {
			return orphan(err, "")
		}
		delete(exclusions, principalArchiveReservation)
		if err = writeSettings(of.path("archive-exclude"), []byte(strings.Join(sortedKeys(exclusions), "\n")+"\n"), 0600); err != nil {
			return orphan(err, "")
		}
	}
	joined, err := s.principalAudience(ctx, room, principal, true)
	if err != nil {
		return orphan(err, "")
	}
	sub, err := s.principalSubscription(ctx, principal, room, "", true)
	if err != nil {
		return orphan(err, sub)
	}
	next := principalDM{Generation: generation, Room: room, Pending: !joined, Active: joined, SubID: sub, CreatedAt: a.Now().UnixMilli()}
	state.DMs[principal] = next
	if err = ownerWrite(ctx, f.path("json"), state); err != nil {
		return orphan(err, sub)
	}
	// New subscription and local config are confirmed before retiring the old
	// row. Never trade away the old row on an uncertain add.
	if d.SubID != "" {
		if _, err = s.principalSubscription(ctx, principal, d.Room, d.SubID, false); err != nil {
			return fmt.Errorf("principal-dm recorded; old subscription cleanup pending: %w", err)
		}
	}
	d = next
	a.printf("principal-dm ensure: %s generation %d, room %s, active=%t pending_join=%t\n", sanitizeLine(principal), d.Generation, sanitizeLine(d.Room), d.Active, d.Pending)
	return nil
}

// A principal must be pinned, and the pinned owner keeps using owner-dm.
// Unset stays possible after the pin drops the principal.
func principalAdmitted(f, owner ownerDMFiles, principal, action, self string) error {
	_, st, err := ownerRead(owner.path("pin"))
	if err != nil || (st != nil && !validPrincipalID(owner.pin().Owner)) {
		return errors.New("principal-dm: owner-dm pin is present but invalid; fix it first")
	}
	if strings.EqualFold(principal, owner.pin().Owner) {
		return errors.New("principal-dm: this principal is the pinned owner; use owner-dm")
	}
	pinned, err := principalPin(f, owner.pin().Owner, self)
	if err != nil {
		return err
	}
	if strings.EqualFold(principal, self) {
		return errors.New("principal-dm: the principal is this identity")
	}
	if action != "unset" && !pinned[principal] {
		return errors.New("principal-dm: principal is not in the host pin")
	}
	return nil
}

func (a *App) principalStatus(ctx context.Context, f ownerDMFiles, only []string) error {
	p, _ := a.paths()
	name, _ := a.identity()
	meta, _ := config.LoadCredentialMetadata(p, name, a.human)
	pinned, perr := principalPin(f, ownerFiles(p, name).pin().Owner, meta.UserID)
	state, err := principalLoad(f, a.Now())
	if err != nil {
		fmt.Fprintln(a.Stderr, "a2a: "+sanitizeLine(err.Error()))
		a.printf("invalid: %s\n", sanitizeLine(err.Error()))
		return nil
	}
	if perr != nil {
		a.printf("pin invalid: %s\n", sanitizeLine(perr.Error()))
	}
	names := []string{}
	for m := range state.DMs {
		names = append(names, m)
	}
	for m := range pinned {
		if _, ok := state.DMs[m]; !ok {
			names = append(names, m)
		}
	}
	sort.Strings(names)
	if len(only) == 1 {
		names = []string{only[0]}
	}
	if len(names) == 0 {
		a.printf("never-configured\n")
		return nil
	}
	var s *session
	for _, m := range names {
		d, known := state.DMs[m]
		line := sanitizeLine(m) + ": pinned=" + fmt.Sprint(pinned[m])
		if !known {
			a.printf("%s never-configured\n", line)
			continue
		}
		st := "inactive"
		switch {
		case d.Active:
			st = "active"
		case a.principalExpired(d):
			st = "expired"
		case d.Pending:
			st = "pending (expires at " + time.UnixMilli(d.CreatedAt).Add(principalDMPendingTTL).UTC().Format(time.RFC3339) + ")"
		}
		audience := "not checked"
		if st == "active" || strings.HasPrefix(st, "pending") {
			if s == nil {
				var e error
				if s, e = a.session(); e != nil {
					audience = e.Error()
				}
			}
			if s != nil {
				joined, e := s.principalAudience(ctx, d.Room, m, d.Pending)
				switch {
				case e != nil:
					audience = e.Error()
				case d.Pending && joined:
					audience = "would activate: principal joined"
				default:
					audience = "valid"
				}
			}
		}
		a.printf("%s %s generation %d room %s audience: %s\n", line, st, d.Generation, sanitizeLine(d.Room), sanitizeLine(audience))
	}
	return nil
}

// Expiry is effective on inbound admission too, without mutating account data
// or granting the pin any mandatory-sender authority.
func (s *session) principalLiveSubscriptions(subs []Sub) ([]Sub, error) {
	// Pure classifiers have no filesystem namespace. Operational sessions do.
	if s.paths.Base == "" {
		return subs, nil
	}
	state, err := principalLoad(principalFiles(s.paths, s.name), s.app.Now())
	if err != nil {
		return nil, err
	}
	inactive := map[string]bool{}
	for _, d := range state.DMs {
		if !(d.Active || d.Pending) || s.app.principalExpired(d) {
			inactive[d.SubID] = true
		}
	}
	out := make([]Sub, 0, len(subs))
	for _, sub := range subs {
		if !inactive[sub.ID] {
			out = append(out, sub)
		}
	}
	return out, nil
}
