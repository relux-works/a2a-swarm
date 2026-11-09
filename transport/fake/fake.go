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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tr "github.com/relux-works/a2a-swarm/transport"
)

type publication struct {
	event tr.Event
	ack   map[tr.PrincipalID]bool
}

var nextWorld atomic.Uint64

type World struct {
	id            uint64
	partCalls     map[tr.PrincipalID]int
	mu            sync.Mutex
	events        []tr.Event
	publications  map[string]*publication
	media         map[string][]byte
	identities    map[tr.PrincipalID]bool
	calls         int
	fail, partial bool
	// DropKeyOnRetry and ReturnUnverified are deliberate defects for suite controls.
	DropKeyOnRetry, ReturnUnverified bool
	sequence                         int
}

func NewWorld() *World {
	return &World{id: nextWorld.Add(1), partCalls: map[tr.PrincipalID]int{}, publications: map[string]*publication{}, media: map[string][]byte{}, identities: map[tr.PrincipalID]bool{}}
}

type Transport struct {
	world *World
	id    tr.PrincipalID
	state string
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
	w.mu.Lock()
	w.identities[id] = true
	w.mu.Unlock()
	return &Transport{world: w, id: id, state: c.StateDir}, nil
}
func (f *Transport) Self(ctx context.Context) (tr.Principal, error) {
	if ctx.Err() != nil {
		return tr.Principal{}, tr.NewError(tr.Unavailable)
	}
	return tr.Principal{ID: f.id}, nil
}
func (f *Transport) Caps() tr.Capabilities {
	return tr.Capabilities{MaxContentBytes: 4096, IdempotentRetry: true, Membership: tr.SenderSide}
}
func (f *Transport) ConversationCaps(context.Context, tr.ConversationID) (tr.ConversationCaps, error) {
	return tr.ConversationCaps{}, nil
}
func (f *Transport) Addressing() tr.Addressing { return addressing{f.world} }
func (f *Transport) Close() error              { return nil }
func Content(m tr.Message) ([]byte, error)     { return json.Marshal(m) }

// MessageAtSize produces an ASCII message whose complete encoding has size n.
func MessageAtSize(n int) tr.Message {
	m := tr.Message{}
	b, _ := Content(m)
	m.Text = strings.Repeat("x", max(0, n-len(b)))
	return m
}
func (f *Transport) Send(ctx context.Context, r tr.SendRequest) (tr.SendResult, error) {
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
	if ctx.Err() != nil {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	key := string(f.id) + "/" + string(r.Conversation) + "/" + r.IdempotencyKey
	if w.DropKeyOnRetry {
		key += "/" + strconv.Itoa(w.calls)
	}
	p := w.publications[key]
	if p == nil {
		w.sequence++
		p = &publication{event: tr.Event{ID: tr.EventID(fmt.Sprintf("event-%d", w.sequence)), Conversation: r.Conversation, Sender: f.id, Time: time.Now(), Message: r.Message}, ack: map[tr.PrincipalID]bool{}}
		p.event = clone(p.event)
		p.event.Raw = append([]byte(nil), b...)
		w.publications[key] = p
	}
	// Each recipient, including the sender's self copy, is acknowledged once.
	recipients := append([]tr.PrincipalID{f.id}, r.Message.To...)
	for _, id := range recipients {
		if p.ack[id] {
			continue
		}
		if !w.identities[id] {
			return tr.SendResult{Outcome: tr.UNKNOWN}, nil
		}
		w.partCalls[id]++
		p.ack[id] = true
		if len(p.ack) == 1 {
			w.events = append(w.events, p.event)
		}
		if w.partial {
			w.partial = false
			return tr.SendResult{Outcome: tr.UNKNOWN}, nil
		}
	}
	if w.fail {
		w.fail = false
		return tr.SendResult{Outcome: tr.UNKNOWN}, nil
	}
	return tr.SendResult{Outcome: tr.ACK, Event: p.event.ID}, nil
}
func (w *World) FailAfterSubmit() { w.mu.Lock(); w.fail = true; w.mu.Unlock() }
func (w *World) PartialAck()      { w.mu.Lock(); w.partial = true; w.mu.Unlock() }
func (w *World) WireCalls() int   { w.mu.Lock(); defer w.mu.Unlock(); return w.calls }
func (w *World) Inject(sender tr.PrincipalID, text string, at time.Time) tr.EventID {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sequence++
	id := tr.EventID(fmt.Sprintf("event-%d", w.sequence))
	w.events = append(w.events, tr.Event{ID: id, Conversation: "room", Sender: sender, Time: at, Message: tr.Message{Text: text}})
	return id
}
func (f *Transport) Sync(ctx context.Context, c tr.Cursor, opt tr.SyncOptions) (tr.Batch, error) {
	if ctx.Err() != nil {
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
	for _, e := range w.events[n:] {
		v, ok, dropped := f.read(e)
		if dropped {
			b.Dropped++
		}
		if ok {
			b.Events = append(b.Events, v)
		}
	}

	return b, nil
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
	return w.partCalls[id]
}

// read runs with the world lock held and reports verification drops separately
// from events not yet acknowledged for this recipient.
func (f *Transport) read(e tr.Event) (tr.Event, bool, bool) {
	w := f.world
	b, _ := Content(e.Message)
	if len(b) > f.Caps().MaxContentBytes || !w.identities[e.Sender] && !w.ReturnUnverified {
		return tr.Event{}, false, true
	}
	for _, p := range w.publications {
		if p.event.ID == e.ID && !p.ack[f.id] {
			return tr.Event{}, false, false
		}
	}
	return clone(e), true, false
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
func (f *Transport) History(ctx context.Context, c tr.ConversationID, page []byte, limit int) (tr.Page, error) {
	if c != "room" || limit <= 0 {
		return tr.Page{}, tr.NewError(tr.InvalidReference)
	}
	if ctx.Err() != nil {
		return tr.Page{}, tr.NewError(tr.Unavailable)
	}
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.offset(page, len(page) == 0)
	if err != nil {
		return tr.Page{}, err
	}
	p := tr.Page{}
	for n < len(w.events) {
		e := w.events[n]
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
			return e, nil
		}
	}
	return tr.Event{}, tr.NewError(tr.InvalidReference)
}
func (f *Transport) Members(context.Context, tr.ConversationID) ([]tr.Member, error) {
	w := f.world
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []tr.Member
	for id := range w.identities {
		out = append(out, tr.Member{Principal: id, Membership: "join"})
	}
	return out, nil
}
func (f *Transport) Upload(ctx context.Context, r io.Reader, m tr.MediaMeta) (tr.MediaRef, error) {
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
	if ctx.Err() != nil {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	if n, err := out.Write(b); err != nil || n != len(b) {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	sum := sha256.Sum256(b)
	return tr.MediaMeta{Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

type addressing struct{ world *World }

func (a addressing) Principal(s string) (tr.PrincipalID, error) {
	a.world.mu.Lock()
	defer a.world.mu.Unlock()
	if a.world.identities[tr.PrincipalID(s)] {
		return tr.PrincipalID(s), nil
	}
	return "", tr.NewError(tr.InvalidReference)
}
func (addressing) Conversation(_ context.Context, s string) (tr.ConversationID, error) {
	if s == "room" {
		return "room", nil
	}
	return "", tr.NewError(tr.InvalidReference)
}
func (addressing) Format(p tr.PrincipalID) string { return string(p) }
