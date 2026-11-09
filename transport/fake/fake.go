// Package fake provides a deterministic, in-memory transport with shared identities.
package fake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tr "github.com/relux-works/a2a-swarm/transport"
)

type publication struct {
	event     tr.Event
	ack       map[tr.PrincipalID]bool
	uncertain bool
}

func (p *publication) targets(id tr.PrincipalID) bool {
	if p.event.Sender == id || len(p.event.Message.To) == 0 {
		return true
	}
	for _, to := range p.event.Message.To {
		if to == id {
			return true
		}
	}
	return false
}

var nextWorld atomic.Uint64

type World struct {
	id            uint64
	partCalls     map[string]map[tr.PrincipalID]int
	mu            sync.Mutex
	events        []tr.Event
	publications  map[string]*publication
	media         map[string][]byte
	identities    map[tr.PrincipalID]bool
	calls         int
	fail, partial bool
	// All fault/defect state is protected by mu.
	defect              string
	refused, preFailure bool
	unverified          map[tr.EventID]bool
	lossBefore          int
	sequence            int
}

func NewWorld() *World {
	return &World{id: nextWorld.Add(1), partCalls: map[string]map[tr.PrincipalID]int{}, unverified: map[tr.EventID]bool{}, publications: map[string]*publication{}, media: map[string][]byte{}, identities: map[tr.PrincipalID]bool{}}
}

type Transport struct {
	world  *World
	id     tr.PrincipalID
	state  string
	closed atomic.Bool
}

var _ tr.Transport = (*Transport)(nil)

func (w *World) New(id tr.PrincipalID, c tr.Config) (*Transport, error) {
	if c.StateDir == "" || id == "" {
		return nil, tr.NewError(tr.InvalidReference)
	}
	if i, e := os.Lstat(c.StateDir); e == nil && (!i.IsDir() || i.Mode()&os.ModeSymlink != 0) {
		return nil, tr.NewError(tr.InvalidReference)
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, tr.NewError(tr.Unavailable)
	}
	if err := os.Chmod(c.StateDir, 0700); err != nil {
		return nil, tr.NewError(tr.Unavailable)
	}
	// Exercise the private-file invariant as well as directory modes. Restart
	// reuses the existing regular private file without rewriting its contents.
	path := filepath.Join(c.StateDir, "identity")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, tr.NewError(tr.InvalidReference)
		}
	} else if err != nil {
		return nil, tr.NewError(tr.Unavailable)
	} else {
		_, writeErr := file.WriteString(string(id))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, tr.NewError(tr.Unavailable)
		}
	}
	w.mu.Lock()
	w.identities[id] = true
	w.mu.Unlock()
	return &Transport{world: w, id: id, state: c.StateDir}, nil
}
func (f *Transport) Self(ctx context.Context) (tr.Principal, error) {
	if ctx.Err() != nil || f.closed.Load() {
		return tr.Principal{}, tr.NewError(tr.Unavailable)
	}
	return tr.Principal{ID: f.id}, nil
}
func (f *Transport) Caps() tr.Capabilities {
	return tr.Capabilities{MaxContentBytes: 4096, IdempotentRetry: true, Membership: tr.SenderSide}
}
func (f *Transport) ConversationCaps(ctx context.Context, c tr.ConversationID) (tr.ConversationCaps, error) {
	if ctx.Err() != nil || f.closed.Load() {
		return tr.ConversationCaps{}, tr.NewError(tr.Unavailable)
	}
	if c != "room" {
		return tr.ConversationCaps{}, tr.NewError(tr.InvalidReference)
	}
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if f.world.defect == "caps" {
		return tr.ConversationCaps{}, tr.NewError(tr.Unavailable)
	}
	return tr.ConversationCaps{}, nil
}
func (f *Transport) Addressing() tr.Addressing { return addressing{f} }
func (f *Transport) Close() error              { f.closed.Store(true); return nil }
func Content(m tr.Message) ([]byte, error)     { return json.Marshal(m) }

// MessageAtSize produces an ASCII message whose complete encoding has size n.
func MessageAtSize(n int) tr.Message {
	m := tr.Message{}
	b, _ := Content(m)
	m.Text = strings.Repeat("x", max(0, n-len(b)))
	return m
}
func (f *Transport) Send(ctx context.Context, r tr.SendRequest) (tr.SendResult, error) {
	if f.closed.Load() {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	if r.Conversation != "room" || r.IdempotencyKey == "" {
		return tr.SendResult{}, tr.NewError(tr.InvalidReference)
	}
	b, err := Content(r.Message)
	if err != nil {
		return tr.SendResult{}, tr.NewError(tr.InvalidReference)
	}
	if len(b) > f.Caps().MaxContentBytes {
		return tr.SendResult{}, tr.NewError(tr.ContentTooLarge)
	}
	if ctx.Err() != nil || f.closed.Load() {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.preFailure {
		w.preFailure = false
		if w.defect == "transient-refused" {
			return tr.SendResult{Outcome: tr.REFUSED, Refusal: tr.NewRefusal(tr.Refused)}, nil
		}
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	w.calls++
	if w.refused {
		w.refused = false
		return tr.SendResult{Outcome: tr.REFUSED, Refusal: tr.NewRefusal(tr.Refused)}, nil
	}
	key := string(f.id) + "/" + r.IdempotencyKey
	if w.defect == "drop-key" {
		key += "/" + strconv.Itoa(w.calls)
	}
	p := w.publications[key]
	if p != nil && p.uncertain && w.defect == "unknown-new-id" {
		p = nil
	}
	if p == nil {
		w.sequence++
		p = &publication{event: tr.Event{ID: tr.EventID(fmt.Sprintf("event-%d", w.sequence)), Conversation: r.Conversation, Sender: f.id, Time: time.Now(), Message: r.Message}, ack: map[tr.PrincipalID]bool{}}
		p.event = clone(p.event)
		p.event.Raw = append([]byte(nil), b...)
		w.publications[key] = p
	}
	// Each recipient, including the sender's self copy, is acknowledged once.
	recipients := append([]tr.PrincipalID{f.id}, r.Message.To...)
	if len(r.Message.To) == 0 {
		for id := range w.identities {
			if id != f.id {
				recipients = append(recipients, id)
			}
		}
	}
	for _, id := range recipients {
		if p.ack[id] {
			if w.defect == "resend-all" {
				w.countPart(r.IdempotencyKey, id)
			}
			continue
		}
		if !w.identities[id] {
			return tr.SendResult{Outcome: tr.UNKNOWN}, nil
		}
		w.countPart(r.IdempotencyKey, id)
		p.ack[id] = true
		if len(p.ack) == 1 {
			w.events = append(w.events, p.event)
		}
		if w.partial && len(p.ack) == min(2, len(recipients)) {
			w.partial = false
			if w.defect == "early-ack" {
				return tr.SendResult{Outcome: tr.ACK, Event: p.event.ID}, nil
			}
			return tr.SendResult{Outcome: tr.UNKNOWN}, nil
		}
	}
	if w.fail {
		w.fail = false
		p.uncertain = true
		if w.defect == "error-after-submit" {
			return tr.SendResult{}, tr.NewError(tr.Unavailable)
		}
		return tr.SendResult{Outcome: tr.UNKNOWN}, nil
	}
	return tr.SendResult{Outcome: tr.ACK, Event: p.event.ID}, nil
}
func (w *World) FailAfterSubmit() { w.mu.Lock(); w.fail = true; w.mu.Unlock() }

// SetDefect installs a deliberate conformance defect under the world lock.
func (w *World) SetDefect(name string) { w.mu.Lock(); w.defect = name; w.mu.Unlock() }

// DefiniteRefusal makes the next request definitely refused.
func (w *World) DefiniteRefusal() { w.mu.Lock(); w.refused = true; w.mu.Unlock() }

// PreSubmitFailure fails the next request before any wire call.
func (w *World) PreSubmitFailure() context.Context {
	w.mu.Lock()
	w.preFailure = true
	w.mu.Unlock()
	return context.Background()
}
func (w *World) countPart(key string, id tr.PrincipalID) {
	if w.partCalls[key] == nil {
		w.partCalls[key] = map[tr.PrincipalID]int{}
	}
	w.partCalls[key][id]++
}

// PartWireCalls returns a snapshot for one send key.
func (w *World) PartWireCalls(key string) map[tr.PrincipalID]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[tr.PrincipalID]int{}
	for id, n := range w.partCalls[key] {
		out[id] = n
	}
	return out
}

// InduceLoss discards the current delivery window, requiring a gap on resume.
func (w *World) InduceLoss() { w.mu.Lock(); w.lossBefore = len(w.events); w.mu.Unlock() }

// InjectUnverified claims an existing identity without its authentication.
func (w *World) InjectUnverified(sender tr.PrincipalID, text string) tr.EventID {
	return w.inject(sender, text, time.Now(), true)
}
func (w *World) PartialAck()    { w.mu.Lock(); w.partial = true; w.mu.Unlock() }
func (w *World) WireCalls() int { w.mu.Lock(); defer w.mu.Unlock(); return w.calls }
func (w *World) Inject(sender tr.PrincipalID, text string, at time.Time) tr.EventID {
	return w.inject(sender, text, at, false)
}

// InjectRaw publishes otherwise valid content with n Raw bytes at the receive
// boundary. n must fit the encoded message; trailing JSON whitespace supplies
// the remaining bytes. Read paths still perform their normal validation.
func (w *World) InjectRaw(sender tr.PrincipalID, n int) tr.EventID {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sequence++
	id := tr.EventID(fmt.Sprintf("event-%d", w.sequence))
	message := tr.Message{Text: "raw boundary"}
	encoded, _ := Content(message)
	raw := append(encoded, bytes.Repeat([]byte{' '}, n-len(encoded))...)
	w.events = append(w.events, tr.Event{ID: id, Conversation: "room", Sender: sender, Time: time.Now(), Message: message, Raw: raw})
	return id
}
func (w *World) inject(sender tr.PrincipalID, text string, at time.Time, unverified bool) tr.EventID {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sequence++
	id := tr.EventID(fmt.Sprintf("event-%d", w.sequence))
	w.unverified[id] = unverified
	w.events = append(w.events, tr.Event{ID: id, Conversation: "room", Sender: sender, Time: at, Message: tr.Message{Text: text}})
	return id
}
func (f *Transport) Sync(ctx context.Context, c tr.Cursor, opt tr.SyncOptions) (tr.Batch, error) {
	if ctx.Err() != nil || f.closed.Load() {
		return tr.Batch{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.offset(c, opt.Initial)
	if err != nil {
		return tr.Batch{}, err
	}
	b := tr.Batch{Next: w.cursor(len(w.events))}
	if n < w.lossBefore {
		if w.defect != "silent-loss" {
			b.Gaps = append(b.Gaps, tr.Gap{Conversation: "room", Reason: tr.WindowExceeded})
		}
		n = w.lossBefore
	}
	for i, e := range w.events[n:] {
		if f.pending(e) && w.defect != "advance-pending" {
			b.Next = w.cursor(n + i)
			return b, nil
		}
		v, ok, dropped := f.read(e)
		if w.defect == "backdated" && e.Time.Before(time.Now().Add(-time.Hour)) {
			ok = false
		}
		if dropped {
			b.Dropped++
		}
		if ok {
			b.Events = append(b.Events, v)
		}
	}

	return b, nil
}

// pending holds a reader before an unacknowledged deliverable copy. Caller holds mu.
func (f *Transport) pending(e tr.Event) bool {
	for _, p := range f.world.publications {
		if p.event.ID == e.ID && p.targets(f.id) && !p.ack[f.id] {
			return true
		}
	}
	return false
}
func (w *World) cursor(n int) tr.Cursor { return tr.Cursor(fmt.Sprintf("fake:%d:%d", w.id, n)) }
func (w *World) offset(c tr.Cursor, initial bool) (int, error) {
	if initial && len(c) == 0 {
		return 0, nil
	}
	prefix := fmt.Sprintf("fake:%d:", w.id)
	if !strings.HasPrefix(string(c), prefix) {
		return 0, tr.NewError(tr.InvalidCursor)
	}
	suffix := strings.TrimPrefix(string(c), prefix)
	n, e := strconv.Atoi(suffix)
	if e != nil || n < 0 || n > len(w.events) || strconv.Itoa(n) != suffix {
		return 0, tr.NewError(tr.InvalidCursor)
	}
	return n, nil
}

// Cursor and PartCalls expose evidence without changing the world.
func (w *World) Cursor() tr.Cursor { w.mu.Lock(); defer w.mu.Unlock(); return w.cursor(len(w.events)) }
func (w *World) PartCalls(id tr.PrincipalID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, calls := range w.partCalls {
		n += calls[id]
	}
	return n
}

// read runs with the world lock held and reports verification drops separately
// from events not yet acknowledged for this recipient.
func (f *Transport) read(e tr.Event) (tr.Event, bool, bool) {
	w := f.world
	b, _ := Content(e.Message)
	oversizeRaw := len(e.Raw) > tr.MaxRawBytes && w.defect != "accept-raw" && w.defect != "clamp-raw"
	if oversizeRaw || len(e.Raw) >= tr.MaxRawBytes && w.defect == "reject-raw-bound" || len(b) > f.Caps().MaxContentBytes || (!w.identities[e.Sender] || w.unverified[e.ID]) && w.defect != "sender" && w.defect != "sender-rewrite" {
		return tr.Event{}, false, true
	}
	for _, p := range w.publications {
		if p.event.ID == e.ID && (!p.targets(f.id) || !p.ack[f.id]) {
			return tr.Event{}, false, false
		}
	}
	e = clone(e)
	if w.defect == "clamp-raw" && len(e.Raw) > tr.MaxRawBytes {
		e.Raw = e.Raw[:tr.MaxRawBytes]
	}
	if w.defect == "sender-rewrite" && w.unverified[e.ID] {
		e.ID = "rewritten-" + e.ID
	}
	if w.defect == "message" {
		e.Message.Kind = "changed"
	}
	if w.defect == "raw" {
		e.Raw = make([]byte, tr.MaxRawBytes+1)
	}
	return e, true, false
}
func clone(e tr.Event) tr.Event {
	e.Raw = bytes.Clone(e.Raw)
	e.Message.To = append([]tr.PrincipalID(nil), e.Message.To...)
	e.Message.Attachments = append([]tr.MediaRef(nil), e.Message.Attachments...)
	if e.Message.Extensions != nil {
		copy := make(map[string]json.RawMessage, len(e.Message.Extensions))
		for k, v := range e.Message.Extensions {
			copy[k] = bytes.Clone(v)
		}
		e.Message.Extensions = copy
	}
	return e
}
func (f *Transport) History(ctx context.Context, c tr.ConversationID, page tr.Cursor, limit int) (tr.Page, error) {
	if f.closed.Load() {
		return tr.Page{}, tr.NewError(tr.Unavailable)
	}
	if c != "room" || limit <= 0 {
		return tr.Page{}, tr.NewError(tr.InvalidReference)
	}
	if ctx.Err() != nil || f.closed.Load() {
		return tr.Page{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.offset(page, len(page) == 0)
	if err != nil && w.defect == "history-cursor" {
		n, err = 0, nil
	}
	if err != nil {
		return tr.Page{}, err
	}
	p := tr.Page{}
	// History exposes the same retained window as Sync and cannot skip a pending
	// recipient copy: return its cursor for retry after that copy is acknowledged.
	n = max(n, w.lossBefore)
	for n < len(w.events) {
		e := w.events[n]
		if f.pending(e) {
			p.Next = w.cursor(n)
			return p, nil
		}
		n++
		v, ok, dropped := f.read(e)
		if dropped {
			p.Dropped++
		}
		if ok {
			p.Events = append(p.Events, v)
		}
		if len(p.Events) == limit {
			break
		}
	}
	if n < len(w.events) {
		p.Next = w.cursor(n)
	}
	return p, nil
}
func (f *Transport) Event(ctx context.Context, c tr.ConversationID, id tr.EventID) (tr.Event, error) {
	b, err := f.Sync(ctx, nil, tr.SyncOptions{Initial: true})
	if err != nil {
		return tr.Event{}, err
	}
	for _, e := range b.Events {
		if e.ID == id && e.Conversation == c {
			f.world.mu.Lock()
			defect := f.world.defect
			f.world.mu.Unlock()
			if defect == "event" {
				e.Sender = f.id
			}
			return e, nil
		}
	}
	return tr.Event{}, tr.NewError(tr.InvalidReference)
}
func (f *Transport) Members(ctx context.Context, c tr.ConversationID) ([]tr.Member, error) {
	if ctx.Err() != nil || f.closed.Load() {
		return nil, tr.NewError(tr.Unavailable)
	}
	if c != "room" {
		return nil, tr.NewError(tr.InvalidReference)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []tr.Member
	for id := range w.identities {
		if w.defect == "members" && id == "bob" {
			continue
		}
		out = append(out, tr.Member{Principal: id, Membership: "join"})
	}
	return out, nil
}
func (f *Transport) Upload(ctx context.Context, r io.Reader, m tr.MediaMeta) (tr.MediaRef, error) {
	if f.closed.Load() {
		return tr.MediaRef{}, tr.NewError(tr.Unavailable)
	}
	if m.Size <= 0 || m.Size > 1<<20 {
		return tr.MediaRef{}, tr.NewError(tr.ContentTooLarge)
	}
	b, err := io.ReadAll(io.LimitReader(r, m.Size+1))
	if err != nil || int64(len(b)) != m.Size || ctx.Err() != nil {
		return tr.MediaRef{}, tr.NewError(tr.Unavailable)
	}
	sum := sha256.Sum256(b)
	id := hex.EncodeToString(sum[:])
	w := f.world
	w.mu.Lock()
	w.media[id] = b
	w.mu.Unlock()
	return tr.NewMediaRef(id), nil
}
func (f *Transport) Download(ctx context.Context, r tr.MediaRef, out io.Writer, limit int64) (tr.MediaMeta, error) {
	if f.closed.Load() {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	b, ok := w.media[r.Opaque()]
	b = bytes.Clone(b)
	w.mu.Unlock()
	if !ok {
		return tr.MediaMeta{}, tr.NewError(tr.InvalidReference)
	}
	if int64(len(b)) > limit || limit <= 0 {
		return tr.MediaMeta{}, tr.NewError(tr.ContentTooLarge)
	}
	if ctx.Err() != nil || f.closed.Load() {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	if n, err := out.Write(b); err != nil || n != len(b) {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	sum := sha256.Sum256(b)
	return tr.MediaMeta{Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

type addressing struct{ transport *Transport }

func (a addressing) Principal(s string) (tr.PrincipalID, error) {
	if a.transport.closed.Load() {
		return "", tr.NewError(tr.Unavailable)
	}
	a.transport.world.mu.Lock()
	defer a.transport.world.mu.Unlock()
	if a.transport.world.identities[tr.PrincipalID(s)] || a.transport.world.defect == "addressing" {
		return tr.PrincipalID(s), nil
	}
	return "", tr.NewError(tr.InvalidReference)
}
func (a addressing) Conversation(ctx context.Context, s string) (tr.ConversationID, error) {
	if ctx.Err() != nil || a.transport.closed.Load() {
		return "", tr.NewError(tr.Unavailable)
	}
	if s == "room" {
		return "room", nil
	}
	return "", tr.NewError(tr.InvalidReference)
}
func (addressing) Format(p tr.PrincipalID) string { return string(p) }
