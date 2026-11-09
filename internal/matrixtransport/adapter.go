// Package matrixtransport adapts the Matrix wire client to the public transport API.
package matrixtransport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	tr "github.com/relux-works/a2a-swarm/transport"
)

const MaxContentBytes = 64 << 10
const MaxRawBytes = tr.MaxRawBytes

// MaxMediaBytes matches the existing attachment hard ceiling. The server may declare less.
const MaxMediaBytes int64 = 1 << 40

type Transport struct {
	client *matrix.Client
	closed atomic.Bool
}

var _ tr.Transport = (*Transport)(nil)
var _ tr.RoomState = (*Transport)(nil)
var _ tr.Redactor = (*Transport)(nil)

// New uses only the supplied client and private StateDir; it never reads ambient credentials.
// Matrix authentication remains in the client. This adapter writes no secret files.
func New(c *matrix.Client, cfg tr.Config) (*Transport, error) {
	if c == nil || cfg.StateDir == "" {
		return nil, tr.NewError(tr.InvalidReference)
	}
	if i, e := os.Lstat(cfg.StateDir); e == nil && (!i.IsDir() || i.Mode()&os.ModeSymlink != 0) {
		return nil, tr.NewError(tr.InvalidReference)
	}
	if e := os.MkdirAll(cfg.StateDir, 0700); e != nil {
		return nil, tr.NewError(tr.Unavailable)
	}
	if e := os.Chmod(cfg.StateDir, 0700); e != nil {
		return nil, tr.NewError(tr.Unavailable)
	}
	copy := *c
	return &Transport{client: &copy}, nil
}
func init() {
	if e := tr.Register("matrix", func(c tr.Config) (tr.Transport, error) {
		return New(matrix.New(c.Settings["base_url"], c.Settings["token"]), c)
	}); e != nil {
		panic("matrix transport registration failed")
	}
}

// Close is idempotent; the supplied client remains owned by its caller.
func (t *Transport) Close() error { t.closed.Store(true); return nil }
func (t *Transport) Caps() tr.Capabilities {
	return tr.Capabilities{Redaction: tr.Enforced, Threads: true, Membership: tr.ServerEnforced, MaxContentBytes: MaxContentBytes, MaxRawBytes: tr.MaxRawBytes, IdempotentRetry: true}
}

// T1 supports plaintext Matrix rooms. E2EE is not implemented by the wire client.
func (t *Transport) ConversationCaps(ctx context.Context, c tr.ConversationID) (tr.ConversationCaps, error) {
	if t.closed.Load() {
		return tr.ConversationCaps{}, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') {
		return tr.ConversationCaps{}, tr.NewError(tr.InvalidReference)
	}
	client, _ := readClient(t.client)
	var create json.RawMessage
	if e := client.GetState(ctx, string(c), "m.room.create", "", &create); e != nil {
		if matrix.IsCode(e, "M_NOT_FOUND") || matrix.IsCode(e, "M_FORBIDDEN") {
			return tr.ConversationCaps{}, tr.NewError(tr.InvalidReference)
		}
		return tr.ConversationCaps{}, safe(e)
	}
	return tr.ConversationCaps{E2EE: false}, nil
}
func (t *Transport) Self(ctx context.Context) (tr.Principal, error) {
	if t.closed.Load() {
		return tr.Principal{}, tr.NewError(tr.Unavailable)
	}
	client, _ := readClient(t.client)
	p, e := client.Whoami(ctx)
	if e != nil {
		return tr.Principal{}, safe(e)
	}
	if !receivedPrincipal(p.UserID) {
		return tr.Principal{}, tr.NewError(tr.Unauthenticated)
	}
	return tr.Principal{ID: tr.PrincipalID(p.UserID)}, nil
}
func (t *Transport) Addressing() tr.Addressing { return addressing{t} }
func TxnID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "a2a-k-" + base64.RawURLEncoding.EncodeToString(sum[:])[:32]
}
func (t *Transport) Send(ctx context.Context, r tr.SendRequest) (tr.SendResult, error) {
	if t.closed.Load() {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(r.Conversation), '!') || r.IdempotencyKey == "" {
		return tr.SendResult{}, tr.NewError(tr.InvalidReference)
	}
	b, e := Content(r.Message)
	if e != nil {
		return tr.SendResult{}, e
	}
	if len(b) > MaxContentBytes {
		return tr.SendResult{}, tr.NewError(tr.ContentTooLarge)
	}
	if ctx.Err() != nil {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	client, _ := readClient(t.client.WithoutRedirects())
	ack, e := client.SendMessageTxnReceipt(ctx, string(r.Conversation), TxnID(r.IdempotencyKey), json.RawMessage(b))
	return outcome(ack.EventID(), e), nil
}
func outcome(id string, e error) tr.SendResult {
	if e == nil && eventID(id) {
		return tr.SendResult{Outcome: tr.ACK, Event: tr.EventID(id)}
	}
	var me *matrix.Error
	if errors.As(e, &me) && me.OriginalRequest && me.Status >= 400 && me.Status < 500 && me.Status != 408 && me.Status != 429 && closedMatrixCode(me.ErrCode) {
		return tr.SendResult{Outcome: tr.REFUSED, Refusal: tr.NewRefusal(tr.Refused)}
	}
	return tr.SendResult{Outcome: tr.UNKNOWN}
}
func closedMatrixCode(s string) bool {
	switch s {
	case "M_FORBIDDEN", "M_NOT_FOUND", "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN", "M_BAD_JSON", "M_NOT_JSON", "M_TOO_LARGE", "M_UNRECOGNIZED", "M_INVALID_PARAM", "M_INVALID_ROOM_STATE", "M_GUEST_ACCESS_FORBIDDEN", "M_RESOURCE_LIMIT_EXCEEDED":
		return true
	}
	return false
}
func safe(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, matrix.ErrAuth) || matrix.IsCode(e, "M_UNKNOWN_TOKEN") || matrix.IsCode(e, "M_MISSING_TOKEN") {
		return tr.NewError(tr.Unauthenticated)
	}
	if matrix.IsCode(e, "M_UNKNOWN_POS") {
		return tr.NewError(tr.InvalidCursor)
	}
	return tr.NewError(tr.Unavailable)
}
func cursor(c []byte, empty bool) error {
	if (!empty && len(c) == 0) || !utf8.Valid(c) {
		return tr.NewError(tr.InvalidCursor)
	}
	return nil
}
func (t *Transport) Sync(ctx context.Context, from tr.Cursor, opt tr.SyncOptions) (tr.Batch, error) {
	if t.closed.Load() {
		return tr.Batch{}, tr.NewError(tr.Unavailable)
	}
	if e := cursor(from, opt.Initial); e != nil {
		return tr.Batch{}, e
	}
	filter := SyncFilter
	if opt.Initial && len(from) == 0 {
		filter = FirstDeliveryFilter
	}
	client, captured := readClient(t.client)
	r, e := client.Sync(ctx, string(from), opt.Timeout, filter)
	if e != nil {
		return tr.Batch{}, safe(e)
	}
	if e = cursor([]byte(r.NextBatch), false); e != nil {
		return tr.Batch{}, e
	}
	raw := rawIndex(captured.body, "", "sync")
	b := tr.Batch{Next: tr.Cursor(r.NextBatch)}
	count := 0
	for room, j := range r.Rooms.Join {
		if !canonical(room, '!') {
			b.Dropped += len(j.Timeline.Events)
			continue
		}
		if j.Timeline.Limited {
			b.Gaps = append(b.Gaps, tr.Gap{Conversation: tr.ConversationID(room), Reason: tr.TimelineLimited})
		}
		for _, ev := range j.Timeline.Events {
			count++
			if count > MaxBatchEvents {
				return tr.Batch{}, tr.NewError(tr.ContentTooLarge)
			}
			v, ok := projectRaw(room, ev, raw[room+"\x00"+ev.EventID])
			if !ok {
				b.Dropped++
				continue
			}
			b.Events = append(b.Events, v)
		}
	}
	for room, i := range r.Rooms.Invite {
		if !canonical(room, '!') {
			continue
		}
		for _, ev := range i.InviteState.Events {
			if ev.Type != "m.room.member" || !receivedPrincipal(ev.Sender) {
				continue
			}
			var m struct {
				Membership string `json:"membership"`
			}
			if json.Unmarshal(ev.Content, &m) == nil && m.Membership == "invite" {
				count++
				if count > MaxBatchEvents {
					return tr.Batch{}, tr.NewError(tr.ContentTooLarge)
				}
				b.Invites = append(b.Invites, tr.Invite{Conversation: tr.ConversationID(room), Sender: tr.PrincipalID(ev.Sender)})
				break
			}
		}
	}
	return b, nil
}
func (t *Transport) History(ctx context.Context, c tr.ConversationID, page tr.Cursor, limit int) (tr.Page, error) {
	return t.history(ctx, c, "", page, limit)
}

// ThreadHistory retains Matrix's native thread pagination without expanding the core API.
func (t *Transport) ThreadHistory(ctx context.Context, c tr.ConversationID, root tr.EventID, page tr.Cursor, limit int) (tr.Page, error) {
	if t.closed.Load() {
		return tr.Page{}, tr.NewError(tr.Unavailable)
	}
	if !eventID(string(root)) {
		return tr.Page{}, tr.NewError(tr.InvalidReference)
	}
	return t.history(ctx, c, root, page, limit)
}
func (t *Transport) history(ctx context.Context, c tr.ConversationID, root tr.EventID, page tr.Cursor, limit int) (tr.Page, error) {
	if t.closed.Load() {
		return tr.Page{}, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') || limit < 1 {
		return tr.Page{}, tr.NewError(tr.InvalidReference)
	}
	if e := cursor(page, true); e != nil {
		return tr.Page{}, e
	}
	limit = min(limit, MaxBatchEvents)
	client, captured := readClient(t.client)
	var r matrix.Page
	var e error
	if root == "" {
		r, e = client.Messages(ctx, string(c), string(page), "b", limit)
	} else {
		r, e = client.ThreadReplies(ctx, string(c), string(root), string(page), "b", limit)
	}
	if e != nil {
		return tr.Page{}, safe(e)
	}
	if len(r.Chunk) > limit {
		return tr.Page{}, tr.NewError(tr.ContentTooLarge)
	}
	raw := rawIndex(captured.body, string(c), "page")
	next := r.End
	if root != "" {
		next = r.NextBatch
	}
	if next != "" && next == string(page) {
		return tr.Page{}, tr.NewError(tr.InvalidCursor)
	}
	if next == string(page) {
		next = ""
	}
	if e = cursor([]byte(next), true); e != nil {
		return tr.Page{}, e
	}
	p := tr.Page{Next: tr.Cursor(next)}
	for _, ev := range r.Chunk {
		v, ok := projectRaw(string(c), ev, raw[string(c)+"\x00"+ev.EventID])
		if !ok {
			p.Dropped++
			continue
		}
		p.Events = append(p.Events, v)
	}
	return p, nil
}
func (t *Transport) Event(ctx context.Context, c tr.ConversationID, id tr.EventID) (tr.Event, error) {
	if t.closed.Load() {
		return tr.Event{}, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') || !eventID(string(id)) {
		return tr.Event{}, tr.NewError(tr.InvalidReference)
	}
	client, captured := readClient(t.client)
	r, e := client.GetEvent(ctx, string(c), string(id))
	if e != nil {
		return tr.Event{}, safe(e)
	}
	v, ok := projectRaw(string(c), r, captured.body)
	if !ok || v.ID != id {
		return tr.Event{}, tr.NewError(tr.InvalidReference)
	}
	return v, nil
}
func (t *Transport) Members(ctx context.Context, c tr.ConversationID) ([]tr.Member, error) {
	if t.closed.Load() {
		return nil, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') {
		return nil, tr.NewError(tr.InvalidReference)
	}
	p, e := t.Self(ctx)
	if e != nil {
		return nil, e
	}
	r, e := t.client.CurrentRoomMembers(ctx, string(c), string(p.ID))
	if e != nil {
		return nil, safe(e)
	}
	out := make([]tr.Member, 0, len(r))
	for _, m := range r {
		out = append(out, tr.Member{Principal: tr.PrincipalID(m.UserID), Membership: m.Membership})
	}
	return out, nil
}
func (t *Transport) Upload(ctx context.Context, r io.Reader, m tr.MediaMeta) (tr.MediaRef, error) {
	if t.closed.Load() {
		return tr.MediaRef{}, tr.NewError(tr.Unavailable)
	}
	if m.Size <= 0 || m.Size > MaxMediaBytes {
		return tr.MediaRef{}, tr.NewError(tr.ContentTooLarge)
	}
	limit, e := t.mediaLimit(ctx)
	if e != nil {
		return tr.MediaRef{}, e
	}
	if m.Size > limit {
		return tr.MediaRef{}, tr.NewError(tr.ContentTooLarge)
	}
	s, e := t.client.Upload(ctx, m.Name, m.ContentType, r, m.Size)
	if e != nil {
		return tr.MediaRef{}, safe(e)
	}
	return tr.NewMediaRef(s), nil
}

// mediaLimit retains the existing server-declared positive limit and hard cap.
func (t *Transport) mediaLimit(ctx context.Context) (int64, error) {
	n, e := t.client.MediaConfigOpaque(ctx)
	if e != nil || n <= 0 {
		return 0, tr.NewError(tr.Unavailable)
	}
	return min(n, MaxMediaBytes), nil
}

// Download streams at most limit bytes to w. On a transfer, writer or size
// failure w can contain a bounded prefix; callers that need atomic output stage it.
func (t *Transport) Download(ctx context.Context, ref tr.MediaRef, w io.Writer, limit int64) (tr.MediaMeta, error) {
	if t.closed.Load() {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	if _, e := matrix.ParseMXC(ref.Opaque()); e != nil {
		return tr.MediaMeta{}, tr.NewError(tr.InvalidReference)
	}
	if limit <= 0 || limit > MaxMediaBytes {
		return tr.MediaMeta{}, tr.NewError(tr.ContentTooLarge)
	}
	server, e := t.mediaLimit(ctx)
	if e != nil {
		return tr.MediaMeta{}, e
	}
	limit = min(limit, server)
	body, e := t.client.Download(ctx, ref.Opaque())
	if e != nil {
		return tr.MediaMeta{}, safe(e)
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(w, hash), io.LimitReader(body, limit))
	if copyErr != nil || ctx.Err() != nil {
		_ = body.Close()
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	var extra [1]byte
	more, tailErr := io.ReadFull(body, extra[:])
	closeErr := body.Close()
	if copyErr != nil || closeErr != nil || ctx.Err() != nil {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	if more > 0 {
		return tr.MediaMeta{}, tr.NewError(tr.ContentTooLarge)
	}
	if tailErr != nil && tailErr != io.EOF {
		return tr.MediaMeta{}, tr.NewError(tr.Unavailable)
	}
	return tr.MediaMeta{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
func (t *Transport) GetState(ctx context.Context, c tr.ConversationID, typ, key string) (json.RawMessage, error) {
	if t.closed.Load() {
		return nil, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') || typ == "" {
		return nil, tr.NewError(tr.InvalidReference)
	}
	var b json.RawMessage
	client, _ := readClient(t.client)
	e := client.GetState(ctx, string(c), typ, key, &b)
	return b, safe(e)
}
func (t *Transport) PutState(ctx context.Context, c tr.ConversationID, typ, key string, b json.RawMessage) (tr.EventID, error) {
	if t.closed.Load() {
		return "", tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') || typ == "" || !json.Valid(b) {
		return "", tr.NewError(tr.InvalidReference)
	}
	client, _ := readClient(t.client)
	id, e := client.PutState(ctx, string(c), typ, key, b)
	return tr.EventID(id), safe(e)
}
func (t *Transport) Redact(ctx context.Context, c tr.ConversationID, id tr.EventID, key string) (tr.SendResult, error) {
	if t.closed.Load() {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	if !canonical(string(c), '!') || !eventID(string(id)) || key == "" {
		return tr.SendResult{}, tr.NewError(tr.InvalidReference)
	}
	if ctx.Err() != nil {
		return tr.SendResult{}, tr.NewError(tr.Unavailable)
	}
	client, _ := readClient(t.client)
	r, e := client.RedactEventTxn(ctx, string(c), string(id), TxnID(key), "")
	return outcome(r, e), nil
}
