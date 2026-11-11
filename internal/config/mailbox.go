package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// The durable mailbox (state/<name>.mailbox.jsonl, mode 0600) is the delivery
// boundary of one agent: every relevant event a sync covers is appended here
// and fsynced BEFORE the cursor (State.NextBatch) moves past it, and an entry
// leaves only by an explicit receipt (`a2a inbox` after printing, `a2a ack`).
// Irrelevant events never enter it. The file is an append-only log of JSON
// lines (add, rcpt, shown, redact), rewritten atomically by redaction or
// compaction. Redaction removes old payload lines without receiving entries. Compaction
// drops received entries and keeps the last ReceiptIndexMax received ids for
// deduplicating events a later sync covers again. No bound ever drops an
// unreceipted entry.
//
// Mailbox mutations run under the state lock (UpdateState). Read-only snapshots
// may open the file without that lock; redaction rewrites use atomic rename.

// MailboxFile is the durable mailbox of name.
func (p Paths) MailboxFile(name string) string {
	return filepath.Join(p.Base, "state", name+".mailbox.jsonl")
}

// ReceiptIndexMax bounds the received ids kept for deduplication after compaction.
const ReceiptIndexMax = 10000

// compactMin is how many dead lines (received entries, receipt and shown
// lines) the file may hold before a receipt compacts it.
const compactMin = 1000

// Mail entry kinds.
const (
	MailEvent  = "event"
	MailInvite = "invite"
	MailGap    = "gap"
)

// MailEntry is one mailbox entry: a relevant event, an invite, or a gap record
// (events between From and To could not be checked).
type MailEntry struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Room   string          `json:"room"`
	Label  string          `json:"label,omitempty"`
	Rel    string          `json:"rel,omitempty"`
	Sub    string          `json:"sub,omitempty"`
	Sender string          `json:"sender,omitempty"`
	TS     int64           `json:"ts,omitempty"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`
	// Shown is set once `wait` or `watch` printed the entry (a shown line).
	Shown bool `json:"-"`
}

type mailLine struct {
	Op      string      `json:"op"` // add, rcpt, shown
	Entry   *MailEntry  `json:"entry,omitempty"`
	IDs     []string    `json:"ids,omitempty"`
	Targets []Redaction `json:"targets,omitempty"`
}

// Redaction is scoped to a room, independently of delivery receipts. Keep it
// indefinitely: pruning by the receipt window would resurrect replayed content.
type Redaction struct {
	Room    string `json:"room"`
	EventID string `json:"event_id"`
}

// Mailbox is the loaded mailbox of one agent.
type Mailbox struct {
	pendingFilter         func(MailEntry) bool
	path                  string
	order                 []string // ids of entries in append order (received ones are skipped)
	live                  map[string]*MailEntry
	received              []string // receipt order
	recv                  map[string]bool
	dead                  int
	redacted              map[Redaction]bool
	needsRedactionRewrite bool // stale payload scrubbed while opening an older mailbox
}

// OpenMailbox loads state/<name>.mailbox.jsonl. A missing file is an empty
// mailbox; anything but a regular 0600 file (a symlink, say) is refused.
func OpenMailbox(p Paths, name string) (*Mailbox, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	m := &Mailbox{path: p.MailboxFile(name), live: map[string]*MailEntry{}, recv: map[string]bool{}, redacted: map[Redaction]bool{}}
	if err := checkSecretFile(m.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m, nil
		}
		return nil, fmt.Errorf("mailbox: %w", err)
	}
	f, err := os.OpenFile(m.path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("mailbox: %w", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			complete := line[len(line)-1] == '\n'
			var l mailLine
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &l); jerr != nil {
				if !complete {
					break // a torn final line of a crashed append: it was never durable
				}
				return nil, fmt.Errorf("mailbox %s: line %d is not valid JSON", m.path, n)
			}
			m.apply(l)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("mailbox: %w", err)
		}
	}
	return m, nil
}

func (m *Mailbox) apply(l mailLine) {
	switch l.Op {
	case "add":
		if l.Entry == nil || l.Entry.ID == "" || m.Has(l.Entry.ID) {
			m.dead++
			return
		}
		e := *l.Entry
		if m.scrub(&e) {
			m.needsRedactionRewrite = true
		}
		m.live[e.ID] = &e
		m.order = append(m.order, e.ID)
	case "rcpt":
		m.dead++
		for _, id := range l.IDs {
			if _, ok := m.live[id]; ok {
				delete(m.live, id)
				m.dead++
			}
			if id != "" && !m.recv[id] {
				m.recv[id] = true
				m.received = append(m.received, id)
			}
		}
	case "shown":
		m.dead++
		for _, id := range l.IDs {
			if e, ok := m.live[id]; ok {
				e.Shown = true
			}
		}
	case "redact":
		for _, target := range l.Targets {
			m.redacted[target] = true
		}
		for _, e := range m.live {
			if m.scrub(e) {
				m.needsRedactionRewrite = true
			}
		}
	}
}

// IsRedacted checks durable local knowledge, including targets not yet received.
func (m *Mailbox) IsRedacted(room, id string) bool {
	return m.redacted[Redaction{room, id}]
}

func (m *Mailbox) scrub(e *MailEntry) bool {
	if e.Kind != MailEvent {
		return false
	}
	var ev matrix.Event
	if json.Unmarshal(e.Event, &ev) != nil {
		return false
	}
	if m.IsRedacted(e.Room, e.ID) || m.IsRedacted(e.Room, ev.ReplacementTarget()) || ev.IsRedacted() {
		raw, _ := json.Marshal(ev.WithoutContent())
		changed := !bytes.Equal(e.Event, raw)
		e.Event = raw
		return changed
	}
	return false
}

// Redact atomically rewrites this mailbox without old payload lines, retaining
// pending entries, shown marks, receipts and permanent target tombstones. It
// must complete before the sync cursor advances. Other copies are untouched.
func (m *Mailbox) Redact(targets []Redaction) error {
	changed := m.needsRedactionRewrite
	for _, target := range targets {
		if target.Room != "" && target.EventID != "" && !m.redacted[target] {
			m.redacted[target] = true
			changed = true
		}
	}
	for _, e := range m.live {
		if m.scrub(e) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	if err := m.compact(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(m.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	m.needsRedactionRewrite = false
	return nil
}

// Has reports whether id is in the mailbox, unreceipted or received (within
// the receipt index).
func (m *Mailbox) Has(id string) bool {
	_, ok := m.live[id]
	return ok || m.recv[id]
}

// Received reports whether id was received (it is in the receipt index).
func (m *Mailbox) Received(id string) bool { return m.recv[id] }

// Pending returns the unreceipted entries in append order.
func (m *Mailbox) Pending() []MailEntry {
	var out []MailEntry
	for _, id := range m.order {
		if e, ok := m.live[id]; ok && (m.pendingFilter == nil || m.pendingFilter(*e)) {
			out = append(out, *e)
		}
	}
	return out
}

// SetPendingFilter restricts this in-memory delivery view only. Entries,
// receipts, deduplication and durable history are preserved.
func (m *Mailbox) SetPendingFilter(filter func(MailEntry) bool) { m.pendingFilter = filter }

// Append adds the entries that are not in the mailbox yet (by id) and fsyncs
// the file before it returns; it returns how many were added.
func (m *Mailbox) Append(entries []MailEntry) (int, error) {
	var buf bytes.Buffer
	var added []MailEntry
	seen := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || m.Has(e.ID) || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		e.Shown = false
		m.scrub(&e)
		if err := writeLine(&buf, mailLine{Op: "add", Entry: &e}); err != nil {
			return 0, err
		}
		added = append(added, e)
	}
	if len(added) == 0 {
		return 0, nil
	}
	if err := m.write(buf.Bytes()); err != nil {
		return 0, err
	}
	for _, e := range added {
		e := e
		m.live[e.ID] = &e
		m.order = append(m.order, e.ID)
	}
	return len(added), nil
}

// Receive records a receipt for the ids that are unreceipted entries (the
// others are ignored) and returns how many left the mailbox.
func (m *Mailbox) Receive(ids []string) (int, error) { return m.receive(ids, false) }

// AddReceived puts ids into the receipt index (deduplication only).
func (m *Mailbox) AddReceived(ids []string) error {
	_, err := m.receive(ids, true)
	return err
}

func (m *Mailbox) receive(ids []string, index bool) (int, error) {
	var keep []string
	n := 0
	for _, id := range ids {
		if _, ok := m.live[id]; ok {
			keep = append(keep, id)
			n++
		} else if index && id != "" && !m.recv[id] {
			keep = append(keep, id)
		}
	}
	if len(keep) == 0 {
		return 0, nil
	}
	var buf bytes.Buffer
	if err := writeLine(&buf, mailLine{Op: "rcpt", IDs: keep}); err != nil {
		return 0, err
	}
	if err := m.write(buf.Bytes()); err != nil {
		return 0, err
	}
	m.apply(mailLine{Op: "rcpt", IDs: keep})
	if m.dead >= compactMin && m.dead > len(m.live) {
		if err := m.compact(); err != nil {
			return n, fmt.Errorf("compact mailbox: %w", err)
		}
	}
	return n, nil
}

// MarkShown records that wait/watch printed the ids (unreceipted entries only).
func (m *Mailbox) MarkShown(ids []string) error {
	var keep []string
	for _, id := range ids {
		if e, ok := m.live[id]; ok && !e.Shown {
			keep = append(keep, id)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := writeLine(&buf, mailLine{Op: "shown", IDs: keep}); err != nil {
		return err
	}
	if err := m.write(buf.Bytes()); err != nil {
		return err
	}
	m.apply(mailLine{Op: "shown", IDs: keep})
	return nil
}

// compact rewrites the file atomically: the receipt index (the last
// ReceiptIndexMax received ids), then every unreceipted entry and its shown
// mark. Unreceipted entries are never dropped.
func (m *Mailbox) compact() error {
	if len(m.received) > ReceiptIndexMax {
		drop := m.received[:len(m.received)-ReceiptIndexMax]
		for _, id := range drop {
			delete(m.recv, id)
		}
		m.received = append([]string(nil), m.received[len(m.received)-ReceiptIndexMax:]...)
	}
	var buf bytes.Buffer
	var targets []Redaction
	for target := range m.redacted {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Room != targets[j].Room {
			return targets[i].Room < targets[j].Room
		}
		return targets[i].EventID < targets[j].EventID
	})
	if len(targets) > 0 {
		if err := writeLine(&buf, mailLine{Op: "redact", Targets: targets}); err != nil {
			return err
		}
	}
	for i := 0; i < len(m.received); i += 1000 {
		end := min(i+1000, len(m.received))
		if err := writeLine(&buf, mailLine{Op: "rcpt", IDs: m.received[i:end]}); err != nil {
			return err
		}
	}
	var order, shown []string
	for _, id := range m.order {
		e, ok := m.live[id]
		if !ok {
			continue
		}
		order = append(order, id)
		if err := writeLine(&buf, mailLine{Op: "add", Entry: e}); err != nil {
			return err
		}
		if e.Shown {
			shown = append(shown, id)
		}
	}
	if len(shown) > 0 {
		if err := writeLine(&buf, mailLine{Op: "shown", IDs: shown}); err != nil {
			return err
		}
	}
	if err := writeSecretAtomic(m.path, filepath.Base(m.path), buf.Bytes()); err != nil {
		return err
	}
	m.order = order
	m.dead = (len(m.received) + 999) / 1000
	if len(shown) > 0 {
		m.dead++
	}
	return nil
}

func writeLine(buf *bytes.Buffer, l mailLine) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	buf.Write(data)
	buf.WriteByte('\n')
	return nil
}

// write appends data to the file (created 0600, never through a symlink) and
// fsyncs it; a newly created file also syncs its directory.
func (m *Mailbox) write(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	_, statErr := os.Lstat(m.path)
	created := errors.Is(statErr, fs.ErrNotExist)
	f, err := os.OpenFile(m.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|openNoFollow, SecretFileMode)
	if err != nil {
		return fmt.Errorf("mailbox: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("mailbox: %s is not a regular file; refusing to use it", m.path)
	}
	if created {
		if err := f.Chmod(SecretFileMode); err != nil {
			f.Close()
			return err
		}
	} else if perm := st.Mode().Perm(); perm != SecretFileMode {
		f.Close()
		return fmt.Errorf("mailbox: %s has mode %04o, want 0600; refusing to use it", m.path, perm)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("append mailbox: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync mailbox: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		if d, err := os.Open(filepath.Dir(m.path)); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}
