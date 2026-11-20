package audience

import (
	"encoding/json"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type Key struct{ Room, EventID string }

// Cycle shares a bounded lookup budget across all rooms in one delivery cycle.
// Fetch receives an explicit room and target. A failed fetch is memoized too.
type Cycle struct {
	Me, Name string
	Now      time.Time
	Evidence func(Key) (Evidence, bool)
	Fetch    func(Key) (matrix.Event, error)
	events   map[Key]matrix.Event
	results  map[Key]Evidence
	fetched  map[Key]bool
	fetches  int
}

func NewCycle(me, name string, now time.Time, evidence func(Key) (Evidence, bool), fetch func(Key) (matrix.Event, error)) *Cycle {
	return &Cycle{Me: me, Name: name, Now: now, Evidence: evidence, Fetch: fetch, events: map[Key]matrix.Event{}, results: map[Key]Evidence{}, fetched: map[Key]bool{}}
}

func (c *Cycle) Add(room string, events []matrix.Event) {
	for _, ev := range events {
		if ev.EventID != "" && (ev.RoomID == "" || ev.RoomID == room) {
			c.events[Key{room, ev.EventID}] = ev
		}
	}
}

func (c *Cycle) Resolved() []Evidence {
	out := make([]Evidence, 0, len(c.results))
	for _, e := range c.results {
		out = append(out, e)
	}
	return out
}

func (c *Cycle) Resolve(room string, ev matrix.Event) Evidence {
	c.Add(room, []matrix.Event{ev})
	return c.resolve(Key{room, ev.EventID}, ev, map[Key]bool{}, 0)
}

func (c *Cycle) resolve(key Key, ev matrix.Event, visiting map[Key]bool, hops int) Evidence {
	if e, ok := c.results[key]; ok {
		return e
	}
	e := Evidence{Version: Version, Room: key.Room, EventID: key.EventID, UserID: c.Me, Name: c.Name, Sender: ev.Sender, Source: Legacy, Decision: Unresolved, ObservedAt: c.Now.UTC()}
	if visiting[key] || hops > 8 {
		return e
	}
	if len(ev.NotificationAudience) > 0 {
		var observed Evidence
		if json.Unmarshal(ev.NotificationAudience, &observed) != nil || observed.Version != Version || observed.UserID != c.Me || observed.Name != c.Name || observed.Room != key.Room || observed.EventID != key.EventID {
			e.Decision = Unavailable
		} else {
			e = observed
		}
		c.results[key] = e
		return e
	}
	if c.Evidence != nil {
		if old, ok := c.Evidence(key); ok {
			if old.UserID != c.Me || old.Name != c.Name || old.Room != key.Room || old.EventID != key.EventID || old.Version > Version || old.Version < 1 {
				e.Decision = Unavailable
				c.results[key] = e
				return e
			}
			// Version upgrades reclassify intact content lazily. A deferred
			// target is retried with this cycle's bounded budget.
			// Once a deferred event itself is scrubbed, its former selector
			// eligibility cannot be reconstructed. Retain its minimal target
			// reference without manufacturing eligibility or losing the reference.
			if old.Version == Version && (old.Decision != Unresolved || ev.IsRedacted()) {
				c.results[key] = old
				return old
			}
		}
	}
	visiting[key] = true
	defer delete(visiting, key)
	if ev.RoomID != "" && ev.RoomID != key.Room {
		c.results[key] = e
		return e
	}
	if ev.Type == "m.reaction" {
		e.Decision = Other
		c.results[key] = e
		return e
	}
	target := ev.ReplacementTarget()
	if ev.Type == "m.room.message" && !ev.IsRedacted() {
		content, err := object(ev.Content)
		if err == nil && !absent(content["m.relates_to"]) {
			relation, err := object(content["m.relates_to"])
			if err != nil {
				c.results[key] = e
				return e
			}
			var typ string
			if raw := relation["rel_type"]; !absent(raw) && json.Unmarshal(raw, &typ) != nil {
				c.results[key] = e
				return e
			}
			if typ == "m.replace" && (target == "" || !matrix.ValidVerificationID(target, '$')) {
				c.results[key] = e
				return e
			}
		}
	}
	if ev.Type == "m.room.redaction" {
		target = ev.RedactionTarget()
	}
	if target == "" {
		if ev.Type == "m.room.message" {
			e = Parse(ev, key.Room, c.Me, c.Name, c.Now)
		}
		c.results[key] = e
		return e
	}
	e.Target, e.Inherited = target, true
	if hops >= 8 {
		c.results[key] = e
		return e
	}
	// Edits require the ordinary replacement shape. Invalid relation
	// structure is unresolved, never R1's malformed-recipient exception.
	if ev.Type == "m.room.message" {
		content, err := object(ev.Content)
		if err != nil {
			c.results[key] = e
			return e
		}
		var outerBody, outerType string
		if json.Unmarshal(content["body"], &outerBody) != nil || json.Unmarshal(content["msgtype"], &outerType) != nil || outerType == "" {
			c.results[key] = e
			return e
		}
		replacement, err := object(content["m.new_content"])
		if err != nil {
			c.results[key] = e
			return e
		}
		var body, typ string
		if json.Unmarshal(replacement["body"], &body) != nil || json.Unmarshal(replacement["msgtype"], &typ) != nil || typ == "" {
			c.results[key] = e
			return e
		}
	}
	tk := Key{key.Room, target}
	original, ok := c.events[tk]
	if !ok && c.Evidence != nil {
		if known, exists := c.Evidence(tk); exists {
			original = matrix.Event{EventID: target, Type: "m.room.message", Sender: known.Sender, RoomID: key.Room}
			ok = known.Decision != Unresolved
		}
	}
	if !ok && c.Fetch != nil && !c.fetched[tk] && c.fetches < 4 {
		c.fetched[tk] = true
		c.fetches++
		var err error
		original, err = c.Fetch(tk)
		ok = err == nil && original.EventID == target && (original.RoomID == "" || original.RoomID == key.Room)
		if ok {
			c.events[tk] = original
		}
	}
	if !ok || original.EventID != target || original.Type != "m.room.message" || (ev.Type == "m.room.message" && original.Sender != ev.Sender) {
		c.results[key] = e
		return e
	}
	parent := c.resolve(tk, original, visiting, hops+1)
	e.Decision, e.Source = parent.Decision, parent.Source
	// A redaction actor cannot borrow R1 from a different malformed sender.
	if parent.Decision == Invalid && parent.Sender != ev.Sender {
		e.Decision = Unresolved
	}
	// Keep the relation actor as Sender; immutable evidence of the target is
	// separate. R1 must never rescue unavailable or unresolved target state.
	if parent.Version != Version {
		e.Decision = Unavailable
	}
	c.results[key] = e
	return e
}
