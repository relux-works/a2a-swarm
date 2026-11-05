package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type replyPolicy struct {
	keepers   map[string]bool
	aliases   map[string][]string
	canonical map[string]string
}

func newReplyPolicy(keepers, aliases []string) (replyPolicy, error) {
	p := replyPolicy{keepers: map[string]bool{}, aliases: map[string][]string{}, canonical: map[string]string{}}
	for _, name := range keepers {
		if config.ValidateName(name) != nil {
			return p, errors.New("invalid keeper")
		}
		p.keepers[name] = true
	}
	for _, pair := range aliases {
		name, alias, ok := strings.Cut(pair, "=")
		if !ok || config.ValidateName(name) != nil || config.ValidateName(alias) != nil {
			return p, errors.New("reply-alias requires recorded NAME=ALIAS")
		}
		if prior, ok := p.canonical[alias]; ok && prior != name {
			return p, errors.New("reply-alias has ambiguous destination")
		}
		p.canonical[alias] = name
		p.aliases[name] = append(p.aliases[name], alias)
	}
	for alias, name := range p.canonical {
		if parent := p.canonical[name]; parent != "" && parent != name && alias != name {
			return p, errors.New("reply-alias canonical name is itself an alias")
		}
	}
	return p, nil
}

type observedEvent struct {
	room   string
	event  matrix.Event
	header Header
}

// Request age is based on visible message history, not mailbox receipts or
// successful wakes. No implicit prefix stripping grants reply authority.
func unanswered(events []observedEvent, server string, policy replyPolicy) map[string]time.Time {
	out := map[string]time.Time{}
	replies := map[string][]observedEvent{}
	for _, event := range events {
		if event.header.Re != "" {
			replies[event.header.Re] = append(replies[event.header.Re], event)
		}
	}
	for _, request := range events {
		h := request.header
		if h.Kind != "request" && h.Kind != "question" && h.Kind != "subscribe" && h.Kind != "owner-needed" {
			continue
		}
		if request.event.EventID == "" || request.event.OriginServerTS <= 0 {
			continue
		}
		var recipients []string
		allowed := map[string]bool{}
		for _, raw := range h.To {
			name := raw
			if strings.HasPrefix(name, "@") {
				var ok bool
				name, ok = doorbellName(name, server)
				if !ok {
					continue
				}
			}
			if name == "all" || config.ValidateName(name) != nil {
				continue
			}
			if canonical := policy.canonical[name]; canonical != "" {
				name = canonical
			}
			recipients = append(recipients, name)
			allowed[name] = true
			for _, alias := range policy.aliases[name] {
				allowed[alias] = true
			}
		}
		answered := false
		for _, reply := range replies[request.event.EventID] {
			if reply.header.Re != request.event.EventID || reply.event.OriginServerTS < request.event.OriginServerTS {
				continue
			}
			name, ok := doorbellName(reply.event.Sender, server)
			if ok && (allowed[name] || policy.keepers[name]) {
				answered = true
				break
			}
		}
		if answered {
			continue
		}
		at := time.UnixMilli(request.event.OriginServerTS)
		for _, name := range recipients {
			if out[name].IsZero() || at.Before(out[name]) {
				out[name] = at
			}
		}
	}
	return out
}

// Scans caller-visible history within an explicit window. Empty filtered
// pages are followed; cycles, timestamp anomalies and bounds fail visibly
// instead of issuing an alert based on an incomplete request/reply set.
func (a *App) addUnanswered(ctx context.Context, s *session, rows []whoRow, since time.Time, policy replyPolicy) error {
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	var events []observedEvent
	for _, room := range rooms {
		from := ""
		tokens := map[string]bool{}
		terminal := false
		for n := 0; n < 100; n++ {
			page, err := s.client.Messages(ctx, room, from, "b", 100)
			if err != nil {
				return err
			}
			for _, ev := range page.Chunk {
				event, keep, err := observeLivenessEvent(ev, room, since, a.Now(), s.creds.UserID)
				if err != nil {
					return err
				}
				if keep {
					events = append(events, event)
				}
				if len(events) > 100000 {
					return errors.New("unanswered history exceeds scan bound; no alerts sent")
				}
			}
			next := firstNonEmpty(page.End, page.NextBatch)
			if next == "" {
				terminal = true
				break
			}
			if next == from || tokens[next] {
				return errors.New("unanswered history pagination cycle; no alerts sent")
			}
			tokens[next] = true
			from = next
		}
		if !terminal {
			return errors.New("unanswered history exceeds page bound; no alerts sent")
		}
	}
	applyUnanswered(rows, unanswered(events, s.server, policy), a.Now())
	return nil
}

type livenessIncident struct {
	key, value  string
	names       []string
	ownerNeeded bool
	superseded  bool
}

func incidents(rows []whoRow, now time.Time, after time.Duration) []livenessIncident {
	groups := map[string][]whoRow{}
	for _, row := range rows {
		if row.Record != nil && row.Record.Host != "" && (row.Health == "stale" || row.Health == "down") {
			groups[row.Record.Host] = append(groups[row.Record.Host], row)
		}
	}
	covered := map[string]bool{}
	var out []livenessIncident
	for _, host := range sortedKeys(groups) {
		if len(groups[host]) < 2 {
			continue
		}
		incident := livenessIncident{key: "host:" + sanitizeLine(host), value: "host unavailable"}
		for _, row := range groups[host] {
			covered[row.Name] = true
			incident.names = append(incident.names, row.Name)
			incident.ownerNeeded = incident.ownerNeeded || row.State == "waiting-human"
		}
		slices.Sort(incident.names)
		out = append(out, incident)
	}
	for _, row := range rows {
		// Reply evidence has its own journal key: changing transport health
		// cannot erase an unanswered request while history is unknown.
		if after > 0 {
			reply := livenessIncident{key: "unanswered:" + row.Name, value: "healthy", names: []string{row.Name}}
			if strings.HasPrefix(row.Unanswered, "unknown") {
				reply.value = "unknown"
			} else if !row.UnansweredSince.IsZero() && now.Sub(row.UnansweredSince) >= after {
				reply.value = "unanswered"
			}
			out = append(out, reply)
		}
		if covered[row.Name] {
			continue
		}
		i := livenessIncident{key: "agent:" + row.Name, value: "healthy", names: []string{row.Name}}
		switch {
		case row.State == "waiting-human":
			i.value = "waiting-human"
			i.ownerNeeded = true
		case row.Health != "fresh":
			i.value = row.Health
			i.ownerNeeded = row.State == "idle" && row.Record != nil && row.Record.Doorbell == "none"
		case row.State == "blocked":
			i.value = "blocked"
		case row.State == "idle" && row.Record != nil && (row.Record.Doorbell == "none" || (row.Record.Runtime == "codex" && !strings.HasPrefix(row.Record.Wake, "runtime ready:") && row.Record.Wake != "wake accepted")):
			i.value = "wake unavailable"
			i.ownerNeeded = true
		}
		out = append(out, i)
	}
	return out
}

func (a *App) alertLiveness(ctx context.Context, s *session, rows []whoRow, roomName string, keepers []string, after time.Duration, product string, since time.Time, stale time.Duration) error {
	room, _, err := s.resolveRoom(ctx, roomName)
	if err != nil {
		return err
	}
	for _, keeper := range keepers {
		var member struct {
			Membership string `json:"membership"`
		}
		if err := s.client.GetState(ctx, room, "m.room.member", "@"+keeper+":"+s.server, &member); err != nil || member.Membership != "join" {
			return fmt.Errorf("keeper %s is not a verified joined member of the alert room; no alerts sent", keeper)
		}
	}
	slices.Sort(keepers)
	scopeBytes := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s", room, product, strings.Join(keepers, ","), since.UTC().Format(time.RFC3339), stale, after)))
	scope := hex.EncodeToString(scopeBytes[:]) + ":"
	current := incidents(rows, a.Now(), after)
	// A correlated host disappearing from the incident set gets one recovery.
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	for key, record := range st.LivenessAlerts {
		if !strings.HasPrefix(key, scope) || (record.Value == "healthy" && record.Pending == nil) {
			continue
		}
		short := strings.TrimPrefix(key, scope)
		found := false
		for _, i := range current {
			found = found || i.key == short
		}
		if !found {
			replacement := livenessIncident{key: short, value: "healthy"}
			if strings.HasPrefix(short, "agent:") {
				name := strings.TrimPrefix(short, "agent:")
				for _, i := range current {
					if strings.HasPrefix(i.key, "host:") && slices.Contains(i.names, name) {
						replacement.value = "covered by " + i.key
						replacement.names = []string{name}
						replacement.superseded = true
						break
					}
				}
			}
			current = append(current, replacement)
		}
	}
	for _, incident := range current {
		if err := a.publishLivenessTransition(ctx, s, scope+incident.key, incident, room, keepers); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) publishLivenessTransition(ctx context.Context, s *session, key string, incident livenessIncident, room string, keepers []string) error {
	for tries := 0; tries < 2; tries++ {
		var pending *config.LivenessAlertMessage
		_, err := config.UpdateStateBy(s.paths, s.name, time.Now().Add(nativeRPCTimeout), func(st *config.State) error {
			if st.LivenessAlerts == nil {
				st.LivenessAlerts = map[string]config.LivenessAlert{}
			}
			previous, exists := st.LivenessAlerts[key]
			if previous.Pending != nil {
				copy := *previous.Pending
				pending = &copy
				return nil
			}
			if incident.value == "unknown" {
				if previous.Value == "unanswered" {
					return nil
				}
				incident.value = "healthy"
			}
			if previous.Value == incident.value || (incident.value == "healthy" && !exists) {
				return nil
			}
			body := "Liveness transition " + incident.key + ": " + incident.value + "; agents=" + strings.Join(incident.names, ",") + ". Keeper action required; waiter freshness, native wake reachability and reply age are separate evidence."
			if incident.value == "healthy" {
				body = "Liveness transition " + incident.key + ": previous condition is no longer observed in this scope; inspect current individual records."
			}
			kind, subtype := "system", "incident"
			if incident.superseded {
				kind = "notice"
				subtype = ""
				body = "Liveness transition " + incident.key + ": individual alert superseded by " + incident.value + "; this is not a recovery. Inspect the correlated host incident."
			}
			if incident.ownerNeeded {
				kind = "owner-needed"
				subtype = ""
			}
			h := Header{V: 1, Type: "message", From: s.name, To: keepers, At: a.Now().UTC().Format(time.RFC3339), Kind: kind, Subtype: subtype}
			mentions := []string{}
			for _, keeper := range keepers {
				mentions = append(mentions, "@"+keeper+":"+s.server)
			}
			content, err := textContent(body, map[string]any{"ai.relux.a2a": h, "m.mentions": map[string]any{"user_ids": mentions}, "ai.relux.a2a.liveness_observation": 1})
			if err != nil {
				return err
			}
			pending = &config.LivenessAlertMessage{Txn: matrix.NewTxnID(), Room: room, Content: content}
			st.LivenessAlerts[key] = config.LivenessAlert{Value: incident.value, Pending: pending}
			return nil
		})
		if err != nil {
			return err
		}
		if pending == nil {
			return nil
		}
		if _, err := s.client.SendMessageTxnReceipt(ctx, pending.Room, pending.Txn, pending.Content); err != nil {
			return err
		}
		_, err = config.UpdateStateBy(s.paths, s.name, time.Now().Add(nativeRPCTimeout), func(st *config.State) error {
			record := st.LivenessAlerts[key]
			if record.Pending != nil && record.Pending.Txn == pending.Txn {
				record.Pending = nil
				st.LivenessAlerts[key] = record
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func isLivenessObservation(ev matrix.Event) bool {
	var content struct {
		Observation int `json:"ai.relux.a2a.liveness_observation"`
	}
	return json.Unmarshal(ev.Content, &content) == nil && content.Observation == 1
}
