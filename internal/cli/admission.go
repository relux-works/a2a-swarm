package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/relux-works/a2a-swarm/internal/admission"
	"github.com/relux-works/a2a-swarm/internal/config"
)

type identityAdmission struct {
	mu      sync.Mutex
	checker admission.Checker
	store   admission.Store
	none    bool
}

func (a *App) closeAdmission() {
	for _, p := range a.admissionSessions {
		p.mu.Lock()
		_ = p.checker.Close()
		p.mu.Unlock()
	}
	a.admissionSessions = nil
}
func (s *session) openAdmission() error {
	name := s.creds.AdmissionPolicy
	if name == "" {
		name = "none"
	}
	if config.ValidateName(s.name) != nil {
		return errors.New("admission: identity_invalid")
	}
	dir := filepath.Join(s.paths.Base, "admission", s.name)
	c, err := admission.Open(name, admission.Config{Identity: s.name, StateDir: dir, Nostr: false})
	if err != nil {
		return err
	}
	s.admission = &identityAdmission{checker: c, store: admission.Store{Dir: dir}, none: name == "none"}
	s.app.admissionSessions = append(s.app.admissionSessions, s.admission)
	if !s.admission.none {
		s.client.BeforeSend = s.checkOutbound
	}
	return nil
}

// The checker is serialized across delivery and asynchronous alarm workers.
func (p *identityAdmission) Inbound(ctx context.Context, in admission.Inbound) (admission.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checker.Inbound(ctx, in)
}
func (p *identityAdmission) Outbound(ctx context.Context, out admission.Outbound) (admission.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checker.Outbound(ctx, out)
}
func (p *identityAdmission) Close() error { return nil } // Owned by App, not Store.Process.

type admissionRefusal struct{ reason string }

func (e *admissionRefusal) Error() string { return "admission: " + e.reason }
func isAdmissionRefusal(err error) bool {
	var refusal *admissionRefusal
	return errors.As(err, &refusal)
}

func (s *session) checkOutbound(ctx context.Context, room, txn, kind string, request []byte) error {
	p := s.admission
	if p == nil || p.none {
		return nil
	}
	d, err := p.Outbound(ctx, admission.Outbound{Identity: s.name, RoomID: room, TransactionID: txn, Kind: kind, Request: bytes.Clone(request)})
	d = admission.Normalize(d, err)
	if d.Verdict == admission.Exchange {
		d = admission.Decision{Verdict: admission.Refused, Reason: "exchange_outbound"}
	}
	if d.Verdict == admission.Live {
		return nil
	}
	if err := p.store.Count(ctx, d.Reason); err != nil {
		return &admissionRefusal{reason: "storage_error"}
	}
	return &admissionRefusal{reason: d.Reason}
}

// appendAdmitted is the sole sync-to-mailbox boundary, including initial recovery.
func (s *session) appendAdmitted(ctx context.Context, mb *config.Mailbox, entries []config.MailEntry) error {
	p := s.admission
	// Preserve both the exact append call and zero Inbound construction for none.
	if p == nil {
		_, err := mb.Append(entries)
		return err
	}
	if p.none {
		// A host can select none after a previous policy held mail. Recheck only
		// retained inputs; never construct Inbound values for the new batch.
		if _, err := os.Lstat(filepath.Join(p.store.Dir, "held.json")); err == nil {
			if err := p.store.Process(ctx, p, s.app.Now(), nil, func(items []admission.Item) error { return s.appendAdmissionItems(mb, items) }); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return admission.ErrStorage
		}
		_, err := mb.Append(entries)
		return err
	}
	items := make([]admission.Item, 0, len(entries))
	for _, e := range entries {
		if mb.Has(e.ID) {
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return admission.ErrStorage
		}
		body := []byte(e.Event)
		kind := e.Kind
		if len(e.Event) > 0 {
			var event struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(e.Event, &event) != nil {
				return admission.ErrStorage
			}
			body, kind = event.Content, event.Type
		}
		items = append(items, admission.Item{Inbound: admission.Inbound{RoomID: e.Room, EventID: e.ID, Sender: e.Sender, Kind: kind, Timestamp: e.TS, Body: body}, Payload: raw})
	}
	return p.store.Process(ctx, p, s.app.Now(), items, func(live []admission.Item) error {
		return s.appendAdmissionItems(mb, live)
	})
}
func (s *session) appendAdmissionItems(mb *config.Mailbox, live []admission.Item) error {
	out := make([]config.MailEntry, 0, len(live))
	for _, item := range live {
		var e config.MailEntry
		if json.Unmarshal(item.Payload, &e) != nil {
			return admission.ErrStorage
		}
		out = append(out, e)
	}
	if _, err := mb.Append(out); err != nil {
		return err
	}
	if s.app.afterAdmissionAppend != nil {
		return s.app.afterAdmissionAppend()
	}
	return nil
}

func cmdAdmissionStatus(ctx context.Context, a *App, args []string) error {
	if len(args) != 1 || args[0] != "--json" {
		return usagef("usage: a2a status admission --json")
	}
	s, err := a.sessionMetadata()
	if err != nil {
		return err
	}
	counts, err := s.admission.store.Counts(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(a.Stdout).Encode(struct {
		Counts map[string]uint64 `json:"counts"`
	}{counts})
}
