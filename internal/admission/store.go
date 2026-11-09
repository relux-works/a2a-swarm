package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

const HeldCountLimit = 256
const HeldAgeLimit = 24 * time.Hour
const ExchangeSizeLimit int64 = 16 << 20
const stateSizeLimit = 32 << 20

var ErrStorage = errors.New("admission: storage_unavailable")

// Item's payload is private delivery metadata, never a diagnostic projection.
type Item struct {
	Inbound Inbound         `json:"inbound"`
	Payload json.RawMessage `json:"payload"`
}
type held struct {
	Item  Item      `json:"item"`
	Since time.Time `json:"since"`
}
type diskState struct {
	Held   []held            `json:"held,omitempty"`
	Counts map[string]uint64 `json:"counts"`
}

// Store operations take a private cross-process lock; Process also runs under the mailbox lock.
// No mutable cache is kept across operations or processes.
type Store struct{ Dir string }

func (s Store) root() (*os.Root, error) {
	if err := PrivateDir(s.Dir); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, ErrStorage
	}
	return r, nil
}
func privateFile(r *os.Root, name string) error {
	st, err := r.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return ErrStorage
	}
	return nil
}
func load(r *os.Root) (diskState, error) {
	st := diskState{Counts: map[string]uint64{}}
	if err := privateFile(r, "held.json"); err != nil {
		return st, err
	}
	f, err := r.Open("held.json")
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, ErrStorage
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, stateSizeLimit+1))
	if err != nil || len(raw) > stateSizeLimit || json.Unmarshal(raw, &st) != nil || len(st.Held) > HeldCountLimit {
		return st, ErrStorage
	}
	if st.Counts == nil {
		st.Counts = map[string]uint64{}
	}
	for reason := range st.Counts {
		if !ValidReason(reason) {
			return st, ErrStorage
		}
	}
	return st, nil
}
func save(r *os.Root, st diskState) error {
	raw, err := json.Marshal(st)
	if err != nil || len(raw) > stateSizeLimit {
		return ErrStorage
	}
	if err := privateFile(r, "held.tmp"); err != nil {
		return err
	}
	f, err := r.OpenFile("held.tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return ErrStorage
	}
	defer r.Remove("held.tmp")
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || r.Rename("held.tmp", "held.json") != nil {
		return ErrStorage
	}
	d, err := r.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer d.Close()
	if d.Sync() != nil {
		return ErrStorage
	}
	return nil
}

// Counts returns only validated closed codes and counts, never queue contents.
func (s Store) Counts(ctx context.Context) (map[string]uint64, error) {
	r, err := s.root()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	unlock, err := lock(ctx, r)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := load(r)
	return st.Counts, err
}
func (s Store) Count(ctx context.Context, reason string) error {
	if !ValidReason(reason) {
		return ErrStorage
	}
	r, err := s.root()
	if err != nil {
		return err
	}
	defer r.Close()
	unlock, err := lock(ctx, r)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := load(r)
	if err != nil {
		return err
	}
	st.Counts[reason]++
	return save(r, st)
}

// ExchangeLine is the v1 append-only exchange protocol. []byte encodes as base64.
type ExchangeLine struct {
	Version   int    `json:"version"`
	EventID   string `json:"event_id"`
	Room      string `json:"room"`
	Sender    string `json:"sender"`
	Timestamp int64  `json:"timestamp"`
	Kind      string `json:"kind"`
	Body      []byte `json:"body"`
	Reason    string `json:"reason"`
}

func spool(r *os.Root, in Inbound, reason string) (bool, error) {
	if err := privateFile(r, "exchange.jsonl"); err != nil {
		return false, err
	}
	f, err := r.OpenFile("exchange.jsonl", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return false, ErrStorage
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > ExchangeSizeLimit {
		return false, ErrStorage
	}
	body := in.Body
	if body == nil {
		body = []byte{}
	}
	raw, err := json.Marshal(ExchangeLine{1, in.EventID, in.RoomID, in.Sender, in.Timestamp, in.Kind, body, reason})
	if err != nil {
		return false, ErrStorage
	}
	raw = append(raw, '\n')
	if info.Size()+int64(len(raw)) > ExchangeSizeLimit {
		return false, nil
	}
	n, err := f.Write(raw)
	if err != nil || n != len(raw) || f.Sync() != nil {
		return false, ErrStorage
	}
	d, err := r.Open(".")
	if err != nil {
		return false, ErrStorage
	}
	defer d.Close()
	if d.Sync() != nil {
		return false, ErrStorage
	}
	return true, nil
}
func key(in Inbound) string { return in.RoomID + "\x00" + in.EventID }

// Process rechecks old held items first and delivers before removing them.
// The mailbox's durable event-ID dedup makes replay after a crash harmless.
// deliver must persist its input before returning success.
func (s Store) Process(ctx context.Context, checker Checker, now time.Time, items []Item, deliver func([]Item) error) error {
	r, err := s.root()
	if err != nil {
		return err
	}
	defer r.Close()
	unlock, err := lock(ctx, r)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := load(r)
	if err != nil {
		return err
	}
	previous := st.Held
	st.Held = nil
	seen := map[string]bool{}
	var live []Item
	check := func(item Item, since time.Time) error {
		if ctx.Err() != nil {
			return errors.New("admission: cancelled")
		}
		id := key(item.Inbound)
		if seen[id] {
			return nil
		}
		seen[id] = true
		if !since.IsZero() && now.Sub(since) >= HeldAgeLimit {
			st.Counts["held_age"]++
			return nil
		}
		// Give the policy owned bytes: mutation cannot change mailbox or spool payloads.
		in := item.Inbound
		in.Body = bytes.Clone(in.Body)
		d, e := checker.Inbound(ctx, in)
		d = Normalize(d, e)
		if ctx.Err() != nil {
			return errors.New("admission: cancelled")
		}
		switch d.Verdict {
		case Live:
			live = append(live, item)
		case Exchange:
			ok, err := spool(r, item.Inbound, d.Reason)
			if err != nil {
				return err
			}
			if !ok {
				st.Counts["exchange_capacity"]++
			}
		case Refused:
			if !d.Retryable {
				st.Counts[d.Reason]++
				return nil
			}
			if len(st.Held) >= HeldCountLimit {
				st.Counts["held_count"]++
				return nil
			}
			if since.IsZero() {
				since = now
			}
			st.Held = append(st.Held, held{item, since})
		}
		return nil
	}
	for _, h := range previous {
		if err := check(h.Item, h.Since); err != nil {
			return err
		}
	}
	for _, item := range items {
		if err := check(item, time.Time{}); err != nil {
			return err
		}
	}
	if err := deliver(live); err != nil {
		return err
	}
	return save(r, st)
}
