package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/relux-works/a2a-swarm/internal/audience"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Emit each fixed code at most once per session, never bodies or addressees.
func (s *session) addressDiagnostic(code string) {
	var bit uint32
	switch code {
	case audience.AddressOther:
		bit = 1
	case audience.AddressMetadataInvalid:
		bit = 2
	case audience.AddressTargetUnresolved:
		bit = 4
	case audience.AddressStateUnavailable:
		bit = 8
	default:
		return
	}
	for {
		old := atomic.LoadUint32(&s.addressWarnings)
		if old&bit != 0 {
			return
		}
		if atomic.CompareAndSwapUint32(&s.addressWarnings, old, old|bit) {
			fmt.Fprintln(s.app.Stderr, code)
			return
		}
	}
}

func (s *session) audienceCycle(ctx context.Context, mb *config.Mailbox, fetch bool) *audience.Cycle {
	var lookup func(audience.Key) (matrix.Event, error)
	if fetch {
		lookup = func(k audience.Key) (matrix.Event, error) { return s.client.GetEvent(ctx, k.Room, k.EventID) }
	}
	c := audience.NewCycle(s.creds.UserID, s.name, s.app.Now(), mb.Audience, lookup)
	for _, entry := range mb.RawPending() {
		if entry.Kind != config.MailEvent {
			continue
		}
		if e, ok := mb.Audience(audience.Key{Room: entry.Room, EventID: entry.ID}); ok && e.Version == audience.Version && e.Decision != audience.Unresolved {
			continue
		}
		var ev matrix.Event
		if json.Unmarshal(entry.Event, &ev) == nil {
			c.Add(entry.Room, []matrix.Event{ev})
		}
	}
	return c
}

// The view intersects ordinary delivery policy. Raw state remains available to
// redaction and security checks; exclusion never changes Shown or receipts.
func (s *session) installAudienceView(mb *config.Mailbox) *audience.Cycle {
	c := s.audienceCycle(context.Background(), mb, false)
	principals := s.readDeliveryPrincipals()
	mb.SetPendingFilter(func(entry config.MailEntry) bool {
		if entry.Kind != config.MailEvent {
			return true
		}
		if e, ok := mb.Audience(audience.Key{Room: entry.Room, EventID: entry.ID}); ok && e.Version == audience.Version && e.Decision != audience.Unresolved && e.UserID == s.creds.UserID && e.Name == s.name {
			s.addressDiagnostic(e.Diagnostic())
			return entry.Rel != "" && e.Allows(slices.Contains(principals, e.Sender))
		}
		var ev matrix.Event
		if json.Unmarshal(entry.Event, &ev) != nil || ev.EventID != entry.ID {
			s.addressDiagnostic(audience.AddressStateUnavailable)
			return false
		}
		e := c.Resolve(entry.Room, ev)
		s.addressDiagnostic(e.Diagnostic())
		return entry.Rel != "" && ev.Type == "m.room.message" && e.Allows(slices.Contains(principals, ev.Sender))
	})
	return c
}

func (s *session) openAudienceMailbox() (*config.Mailbox, error) {
	mb, err := config.OpenMailbox(s.paths, s.name)
	if err != nil {
		s.addressDiagnostic(audience.AddressStateUnavailable)
		return nil, err
	}
	s.installAudienceView(mb)
	return mb, nil
}

var errAddressChanged = errors.New("ADDRESS_STATE_UNAVAILABLE: notification audience changed; mail retained")

func (s *session) audienceStatus() string {
	mb, err := config.OpenMailbox(s.paths, s.name)
	if err != nil {
		return audience.AddressStateUnavailable
	}
	c := s.audienceCycle(context.Background(), mb, false)
	for _, entry := range mb.RawPending() {
		if entry.Kind != config.MailEvent {
			continue
		}
		var ev matrix.Event
		if json.Unmarshal(entry.Event, &ev) != nil || ev.EventID != entry.ID {
			return audience.AddressStateUnavailable
		}
		if c.Resolve(entry.Room, ev).Diagnostic() == audience.AddressStateUnavailable {
			return audience.AddressStateUnavailable
		}
	}
	return ""
}

// A count notice is valid only for the exact still-eligible ID set. Callers
// requalify instead of typing or submitting a stale count, including on restart.
func (s *session) requireAudienceIDs(ids []string) error {
	if len(ids) == 0 {
		return errAddressChanged
	}
	mb, err := s.openAudienceMailbox()
	if err != nil {
		return err
	}
	pending := mb.Pending()
	for _, id := range ids {
		if !slices.ContainsFunc(pending, func(e config.MailEntry) bool { return e.ID == id }) {
			return errAddressChanged
		}
	}
	return nil
}

// audienceClassify places the hard boundary before all positive selectors.
// Related events keep today's selectors: inherited self is never a new one.
func audienceClassify(ev matrix.Event, room, me, name string, st config.State, subs []Sub, principals []string, e audience.Evidence) (relevance, string, []string) {
	if ev.Sender != me && !e.Allows(slices.Contains(principals, ev.Sender)) {
		return "", "", nil
	}
	rel, root, matched := classifyEligible(ev, room, me, name, st, subs)
	if rel == "" && ev.Sender != me && mandatoryPrincipal(ev, principals) {
		return relSub, "", nil
	}
	if !e.Inherited && e.Decision == audience.Self && ev.Sender != me && ev.Type == "m.room.message" {
		h, ok := decodeHeader(ev.Message().A2A)
		if !(ok && h.Type == "projection") {
			if e.Source == audience.Header {
				return relTo, root, matched
			}
			return relMention, root, matched
		}
	}
	return rel, root, matched
}
