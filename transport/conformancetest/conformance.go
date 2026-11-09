// Package conformancetest tests the public transport contract without internal imports.
// The implementer supplies the harness: an adversarial harness can special-case
// markers or fabricate observations. This is a contract for honest implementers,
// not an adversarial proof. Per-run markers reduce accidental special-casing.
//
// Compatibility: PrincipalReferences, ConversationReferences and ReceivedRawAtSize
// are optional fixture hooks. Missing hooks report named skips at
// addressing/principal_references, addressing/conversation_references and
// raw_bounds/received respectively; these checks are unverified, never passes.
// Existing required hooks and fixture data remain required.
//
// Implementers must declare Caps().MaxRawBytes on every fixture identity in
// 0..transport.MaxRawBytes. Zero means no Raw on any read path; keyed capability
// literals still compile, but transports exposing Raw must explicitly set a bound.
// With ReceivedRawAtSize, positive bounds exercise exactly N and N+1 bytes.
// Zero checks an ordinary received event with empty Raw and still injects an
// absolute-ceiling+1 wire payload to verify rejection and drop accounting.
package conformancetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tr "github.com/relux-works/a2a-swarm/transport"
)

// Harness creates a fresh isolated world for each group. Fault hooks must act at
// the actual I/O boundary. Missing required hooks fail instead of skipping.
type Harness struct{ New func(*testing.T) *Fixture }
type Fixture struct {
	A, B tr.Transport
	// C is a second explicit recipient for multi-part publication checks.
	C tr.Transport
	// Extensions supplies at least one transport-admitted opaque message extension.
	Extensions   map[string]json.RawMessage
	nonce        string
	Conversation tr.ConversationID
	// PrincipalReferences returns a valid reference for B and an invalid reference
	// in this transport's grammar. Optional; nil skips addressing/principal_references.
	PrincipalReferences func() (valid, invalid string)
	// ConversationReferences returns a valid reference for Conversation and an
	// invalid reference. Optional; nil skips addressing/conversation_references.
	ConversationReferences func() (valid, invalid string)
	// ReceivedRawAtSize publishes an authenticated event from A, visible to B,
	// with exactly n wire Raw bytes at the receive boundary and otherwise valid content.
	// It bypasses send-size limits, not receive validation, and returns the wire ID.
	// Even when Caps().MaxRawBytes is zero, the hook supplies wire bytes; read
	// projections must suppress Raw and still reject oversized wire payloads.
	// Optional; nil skips raw_bounds/received.
	ReceivedRawAtSize func(n int) tr.EventID
	// RestartB reopens the same identity without resetting the world.
	RestartB        func() tr.Transport
	FailAfterSubmit func()
	// PartialAck accepts self and the first explicit recipient, leaving the second pending.
	PartialAck func()
	// PartWireCalls snapshots actual delivery attempts by recipient for one send key.
	PartWireCalls   func(string) map[tr.PrincipalID]int
	DefiniteRefusal func()
	// PreSubmitFailure either arms a one-shot transport-internal transient failure
	// and returns a live context, or returns an already-cancelled context. The next
	// Send must fail before any wire call with unavailable.
	// For a cancelled context only, errors.Is(err, context.Canceled) is also accepted.
	// The hook must not publish.
	PreSubmitFailure func() context.Context
	// InduceLoss drops already published events beyond the transport window.
	InduceLoss func()
	// UnverifiableSender claims the supplied existing principal without authentication.
	UnverifiableSender    func(tr.PrincipalID, string) tr.EventID
	BackdatedPublish      func(time.Time) tr.EventID
	ForeignCursor         func() tr.Cursor
	OversizeReceived      func(string) error
	WireCalls             func() int
	MessageAtSize         func(int) tr.Message
	Logs                  func() []string
	StateDir, SentinelDir string
}

func required(t *testing.T, name string, ok bool) {
	t.Helper()
	if !ok {
		t.Fatalf("harness missing fault hook %s", name)
	}
}
func fixture(t *testing.T, h Harness) *Fixture {
	t.Helper()
	required(t, "New", h.New != nil)
	f := h.New(t)
	if f == nil || f.A == nil || f.B == nil {
		t.Fatal("harness missing identities")
	}
	// Reject incomplete fixtures before exercising a transport; otherwise missing
	// recipients/extensions or unchecked filesystem roots could masquerade as coverage.
	for _, field := range []struct {
		name string
		ok   bool
	}{{"C", f.C != nil}, {"Extensions", len(f.Extensions) > 0}, {"StateDir", f.StateDir != ""}, {"SentinelDir", f.SentinelDir != ""}} {
		if !field.ok {
			t.Fatalf("harness missing fixture field %s", field.name)
		}
	}
	// Reject invalid declarations on every identity before transport operations;
	// zero and the absolute ceiling are valid neighbouring fixture declarations.
	for _, identity := range []struct {
		name      string
		transport tr.Transport
	}{{"A", f.A}, {"B", f.B}, {"C", f.C}} {
		if n := identity.transport.Caps().MaxRawBytes; n < 0 || n > tr.MaxRawBytes {
			t.Fatalf("fixture %s Caps.MaxRawBytes=%d outside 0..%d", identity.name, n, tr.MaxRawBytes)
		}
	}
	f.nonce = marker()
	before := snapshot(t, f.SentinelDir)
	t.Cleanup(func() {
		checkState(t, f.StateDir)
		if after := snapshot(t, f.SentinelDir); after != before {
			t.Error("sentinel sibling changed")
		}
	})
	t.Cleanup(func() {
		_ = f.A.Close()
		_ = f.B.Close()
		if f.C != nil {
			_ = f.C.Close()
		}
	})
	return f
}
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&out, "%s %v %d ", rel, i.Mode(), i.ModTime().UnixNano())
		if !d.IsDir() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			fmt.Fprintf(&out, "%x", sha256.Sum256(b))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}
func checkState(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		want := fs.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if i.Mode()&fs.ModeSymlink != 0 || i.Mode().Perm() != want {
			t.Errorf("StateDir mode: %s has %v, want %v", filepath.Base(p), i.Mode(), want)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}
func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}
func send(t *testing.T, f *Fixture, key string) tr.SendResult {
	t.Helper()
	p, e := f.B.Self(ctx(t))
	if e != nil {
		t.Fatal(e)
	}
	r, e := f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: f.nonce + key, Message: tr.Message{Text: "valid neighbour", To: []tr.PrincipalID{p.ID}}})
	if e != nil {
		t.Fatal(e)
	}
	checkOutcome(t, r)
	return r
}
func marker() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func checkOutcome(t *testing.T, r tr.SendResult) {
	t.Helper()
	switch r.Outcome {
	case tr.ACK:
		if r.Event == "" || r.Refusal != (tr.Refusal{}) {
			t.Fatal("invalid ACK fields")
		}
	case tr.UNKNOWN:
		if r.Event != "" || r.Refusal != (tr.Refusal{}) {
			t.Fatal("invalid UNKNOWN fields")
		}
	case tr.REFUSED:
		if r.Event != "" || !r.Refusal.Valid() {
			t.Fatal("REFUSED requires closed refusal code")
		}
	default:
		t.Fatal("invalid outcome")
	}
}
func exactlyOne(t *testing.T, b tr.Batch, id tr.EventID) {
	t.Helper()
	distinct := map[tr.EventID]bool{}
	for _, e := range b.Events {
		distinct[e.ID] = true
	}
	if len(distinct) != 1 || !distinct[id] {
		t.Fatal("retry created multiple distinct logical EventIDs")
	}
	one(t, b, id)
}
func syncIdentity(t *testing.T, identity tr.Transport, c tr.Cursor, initial bool) tr.Batch {
	t.Helper()
	b, e := identity.Sync(ctx(t), c, tr.SyncOptions{Initial: initial})
	if e != nil {
		t.Fatal(e)
	}
	checkGaps(t, b)
	return b
}
func syncBatch(t *testing.T, f *Fixture, c tr.Cursor, initial bool) tr.Batch {
	t.Helper()
	b, e := f.B.Sync(ctx(t), c, tr.SyncOptions{Initial: initial})
	if e != nil {
		t.Fatal(e)
	}
	checkGaps(t, b)
	return b
}
func checkGaps(t *testing.T, b tr.Batch) {
	t.Helper()
	for _, g := range b.Gaps {
		if !g.Reason.Valid() {
			t.Fatalf("invalid Gap reason %q", g.Reason)
		}
	}
}
func baseline(t *testing.T, f *Fixture) tr.Cursor { return syncBatch(t, f, nil, true).Next }
func one(t *testing.T, b tr.Batch, id tr.EventID) {
	t.Helper()
	n := 0
	for _, e := range b.Events {
		if e.ID == id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logical event %s seen %d times, want exactly one", id, n)
	}
}

// Run proves the exercised API contract and fault handling, not live network,
// cryptographic security, remote durability or deployment admission.
func Run(t *testing.T, h Harness) {
	t.Run("self", func(t *testing.T) {
		f := fixture(t, h)
		a, e := f.A.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		b, e := f.A.Self(ctx(t))
		if e != nil || a.ID == "" || a.ID != b.ID {
			t.Fatal("Self is not stable")
		}
	})
	t.Run("send_idempotency", func(t *testing.T) {
		f := fixture(t, h)
		c := baseline(t, f)
		a := send(t, f, "same-key")
		b := send(t, f, "same-key")
		if a.Outcome != tr.ACK || a.Event == "" || b.Event != a.Event || b.Outcome != tr.ACK {
			t.Fatal("idempotency key retry created a second logical event")
		}
		events := syncBatch(t, f, c, false)
		one(t, events, a.Event)
		p, e := f.A.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		for _, ev := range events.Events {
			if ev.ID == a.Event && ev.Sender != p.ID {
				t.Fatal("sender not verified")
			}
		}
	})
	t.Run("resume_backdated_history", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "RestartB", f.RestartB != nil)
		required(t, "BackdatedPublish", f.BackdatedPublish != nil)
		start := baseline(t, f)
		r := send(t, f, "before")
		b := syncBatch(t, f, start, false)
		one(t, b, r.Event)
		if len(b.Events) == 0 {
			t.Fatal("no event")
		}
		id := f.BackdatedPublish(b.Events[0].Time.Add(-24 * time.Hour))
		_ = f.B.Close()
		f.B = f.RestartB()
		after := syncBatch(t, f, b.Next, false)
		one(t, after, id)
		seen := map[tr.EventID]bool{}
		for _, e := range append(b.Events, after.Events...) {
			seen[e.ID] = true
		}
		page := tr.Cursor(nil)
		history := map[tr.EventID]bool{}
		for i := 0; i < 100; i++ {
			p, e := f.B.History(ctx(t), f.Conversation, page, 1)
			if e != nil {
				t.Fatal(e)
			}
			for _, ev := range p.Events {
				history[ev.ID] = true
			}
			if len(p.Next) == 0 {
				break
			}
			if bytes.Equal(page, p.Next) {
				t.Fatal("history cursor did not advance")
			}
			page = p.Next
			if i == 99 {
				t.Fatal("history did not terminate")
			}
		}
		if len(history) != len(seen) {
			t.Fatal("History differs from Sync")
		}
		for id := range seen {
			if !history[id] {
				t.Fatal("History lost event")
			}
		}
		again := syncBatch(t, f, b.Next, false)
		for _, e := range again.Events {
			seen[e.ID] = true
		}
		if len(seen) != 2 {
			t.Fatal("resume ID dedup differs")
		}
	})
	t.Run("unknown_retry", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "FailAfterSubmit", f.FailAfterSubmit != nil)
		c := baseline(t, f)
		f.FailAfterSubmit()
		r := send(t, f, "uncertain")
		if r.Outcome != tr.UNKNOWN || r.Event != "" {
			t.Fatal("after-submit failure must give UNKNOWN")
		}
		r = send(t, f, "uncertain")
		if r.Outcome != tr.ACK {
			t.Fatal("retry not ACK")
		}
		exactlyOne(t, syncBatch(t, f, c, false), r.Event)
	})
	// Proves three-part ACK discipline and observable retry calls, not remote persistence.
	t.Run("partial_ack", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "PartialAck", f.PartialAck != nil)
		required(t, "PartWireCalls", f.PartWireCalls != nil)
		required(t, "C", f.C != nil)
		identities := []tr.Transport{f.A, f.B, f.C}
		cursors := make([]tr.Cursor, 3)
		ids := make([]tr.PrincipalID, 3)
		for i, identity := range identities {
			cursors[i] = syncIdentity(t, identity, nil, true).Next
			p, e := identity.Self(ctx(t))
			if e != nil {
				t.Fatal(e)
			}
			ids[i] = p.ID
		}
		key := marker()
		req := tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: key, Message: tr.Message{Text: marker(), To: ids[1:]}}
		f.PartialAck()
		first, e := f.A.Send(ctx(t), req)
		if e != nil || first.Outcome != tr.UNKNOWN {
			t.Fatal("partial acknowledgement must give UNKNOWN")
		}
		checkOutcome(t, first)
		before := f.PartWireCalls(key)
		if before[ids[0]] != 1 || before[ids[1]] != 1 || before[ids[2]] != 0 {
			t.Fatal("partial hook must satisfy self plus first recipient only")
		}
		self := syncIdentity(t, f.A, cursors[0], false)
		if len(self.Events) != 1 {
			t.Fatal("partial publication missing self copy")
		}
		second, e := f.A.Send(ctx(t), req)
		if e != nil || second.Outcome != tr.ACK || second.Event != self.Events[0].ID {
			t.Fatal("partial retry changed logical EventID")
		}
		checkOutcome(t, second)
		after := f.PartWireCalls(key)
		for _, id := range ids {
			if after[id] != 1 {
				t.Fatal("retry resent an acknowledged part")
			}
		}
		for i, identity := range identities {
			exactlyOne(t, syncIdentity(t, identity, cursors[i], false), second.Event)
		}
	})
	// Proves a cursor observed during a partial publication cannot silently lose its pending part.
	t.Run("partial_window", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "PartialAck", f.PartialAck != nil)
		required(t, "C", f.C != nil)
		start := baseline(t, f)
		cp, e := f.C.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		bp, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		req := tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: marker(), Message: tr.Message{Text: marker(), To: []tr.PrincipalID{cp.ID, bp.ID}}}
		f.PartialAck()
		r, e := f.A.Send(ctx(t), req)
		if e != nil || r.Outcome != tr.UNKNOWN {
			t.Fatal("partial acknowledgement must give UNKNOWN")
		}
		during := syncBatch(t, f, start, false)
		r, e = f.A.Send(ctx(t), req)
		if e != nil || r.Outcome != tr.ACK {
			t.Fatal("partial retry not ACK")
		}
		after := syncBatch(t, f, during.Next, false)
		for _, ev := range append(during.Events, after.Events...) {
			if ev.ID == r.Event {
				return
			}
		}
		for _, g := range append(during.Gaps, after.Gaps...) {
			if g.Conversation == f.Conversation && g.Reason.Valid() {
				return
			}
		}
		t.Fatal("cursor advanced past pending event without Gap")
	})
	t.Run("bounds", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "WireCalls", f.WireCalls != nil)
		required(t, "MessageAtSize", f.MessageAtSize != nil)
		n := f.A.Caps().MaxContentBytes
		if n <= 0 {
			t.Fatal("MaxContentBytes must be positive")
		}
		r, e := f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: "bound", Message: f.MessageAtSize(n)})
		if e != nil || r.Outcome != tr.ACK {
			t.Fatalf("exact content bound refused: %v", e)
		}
		calls := f.WireCalls()
		_, e = f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: "over", Message: f.MessageAtSize(n + 1)})
		if tr.Code(e) != tr.ContentTooLarge || f.WireCalls() != calls {
			t.Fatal("oversize content must be refused with zero wire calls")
		}
	})
	t.Run("unverifiable_sender", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "UnverifiableSender", f.UnverifiableSender != nil)
		c := baseline(t, f)
		good := send(t, f, "verified")
		principal, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		bad := f.UnverifiableSender(principal.ID, marker())
		b := syncBatch(t, f, c, false)
		one(t, b, good.Event)
		// Catch a transport that rewrites an unauthenticated event's ID but still
		// attributes it to B. This fixture publishes no authenticated event from B.
		for _, e := range b.Events {
			if e.ID == bad {
				t.Fatal("unverifiable sender delivered")
			}
			if e.Sender == principal.ID {
				t.Fatal("unauthenticated claim of B delivered")
			}
		}
		if b.Dropped < 1 {
			t.Fatal("unverifiable sender not counted")
		}
	})
	t.Run("cursors", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "ForeignCursor", f.ForeignCursor != nil)
		c := baseline(t, f)
		_ = syncBatch(t, f, c, false)
		for _, invalid := range []tr.Cursor{nil, {0xff}, f.ForeignCursor()} {
			_, e := f.B.Sync(ctx(t), invalid, tr.SyncOptions{})
			if tr.Code(e) != tr.InvalidCursor {
				t.Fatal("foreign or invalid cursor accepted")
			}

		}
		_ = syncBatch(t, f, c, false)
		send(t, f, "history-one")
		send(t, f, "history-two")
		valid, e := f.B.History(ctx(t), f.Conversation, nil, 1)
		if e != nil || len(valid.Events) != 1 || len(valid.Next) == 0 {
			t.Fatal("valid History cursor unavailable")
		}
		for _, bad := range []tr.Cursor{{0xff}, f.ForeignCursor()} {
			if _, e := f.B.History(ctx(t), f.Conversation, bad, 1); tr.Code(e) != tr.InvalidCursor {
				t.Fatal("invalid History cursor accepted")
			}
		}
		again, e := f.B.History(ctx(t), f.Conversation, valid.Next, 1)
		if e != nil || len(again.Events) != 1 {
			t.Fatal("valid History cursor damaged by invalid attempts")
		}
		if _, e = f.B.History(ctx(t), f.Conversation, nil, 0); tr.Code(e) != tr.InvalidReference {
			t.Fatal("invalid History limit accepted")
		}
	})
	t.Run("media_redaction_errors", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "Logs", f.Logs != nil)
		required(t, "OversizeReceived", f.OversizeReceived != nil)
		required(t, "UnverifiableSender", f.UnverifiableSender != nil)
		data := []byte("media valid neighbour")
		ref, e := f.A.Upload(ctx(t), bytes.NewReader(data), tr.MediaMeta{Size: int64(len(data))})
		if e != nil {
			t.Fatal(e)
		}
		var out bytes.Buffer
		meta, e := f.B.Download(ctx(t), ref, &out, 1024)
		sum := sha256.Sum256(data)
		if e != nil || !bytes.Equal(data, out.Bytes()) || meta.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatal("media round trip hash changed")
		}
		marker := marker()
		secret := tr.NewMediaRef(marker)
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			if s := fmt.Sprintf(format, secret); s != "MediaRef(redacted)" {
				t.Fatalf("MediaRef formatting leaked: %s", s)
			}
		}
		check := func(e error) {
			t.Helper()
			if e == nil {
				t.Fatal("negative neighbour did not refuse")
			}
			if strings.Contains(e.Error(), marker) {
				t.Fatal("received marker in error")
			}
		}
		_, e = f.B.Download(ctx(t), secret, &out, 1024)
		check(e)
		_, e = f.B.Download(ctx(t), ref, &out, 1)
		check(e)
		check(f.OversizeReceived(marker))
		c := baseline(t, f)
		principal, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		f.UnverifiableSender(principal.ID, marker)
		_ = syncBatch(t, f, c, false)
		for _, line := range f.Logs() {
			if strings.Contains(line, marker) {
				t.Fatal("received marker in log")
			}
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		f := fixture(t, h)
		// Discover all optional interfaces; implementing one must never fail this group.
		extensions := []struct {
			name        string
			implemented bool
		}{
			{"Rooms", implements[tr.Rooms](f.A)}, {"RoomState", implements[tr.RoomState](f.A)},
			{"AccountData", implements[tr.AccountData](f.A)}, {"Presence", implements[tr.Presence](f.A)},
			{"Registration", implements[tr.Registration](f.A)}, {"Redactor", implements[tr.Redactor](f.A)},
		}
		for _, extension := range extensions {
			t.Logf("checked %s implemented=%v", extension.name, extension.implemented)
			if !extension.implemented && tr.Code(tr.Unsupported()) != tr.UnsupportedByTransport {
				t.Fatal("unsupported extension not refused")
			}
		}
		r := send(t, f, "supported")
		if r.Outcome != tr.ACK {
			t.Fatal("supported neighbour refused")
		}
	})
	// Proves the formatter round trip; optional probes use implementer-supplied grammar.
	t.Run("addressing", func(t *testing.T) {
		f := fixture(t, h)
		p, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		addr := f.A.Addressing()
		if id, e := addr.Principal(addr.Format(p.ID)); e != nil || id != p.ID {
			t.Fatal("principal round trip changed")
		}
		// Proves valid principal parsing beside rejection; catches an accept-all parser.
		t.Run("principal_references", func(t *testing.T) {
			if f.PrincipalReferences == nil {
				t.Skip("harness missing optional hook PrincipalReferences")
			}
			valid, invalid := f.PrincipalReferences()
			if id, e := addr.Principal(valid); e != nil || id != p.ID {
				t.Fatal("valid principal reference changed")
			}
			if _, e := addr.Principal(invalid); tr.Code(e) != tr.InvalidReference {
				t.Fatal("invalid principal reference accepted")
			}
		})
		// Proves conversation resolution without requiring IDs to be references;
		// catches an accept-all resolver beside a supported reference.
		t.Run("conversation_references", func(t *testing.T) {
			if f.ConversationReferences == nil {
				t.Skip("harness missing optional hook ConversationReferences")
			}
			valid, invalid := f.ConversationReferences()
			if c, e := addr.Conversation(ctx(t), valid); e != nil || c != f.Conversation {
				t.Fatal("valid conversation reference changed")
			}
			if _, e := addr.Conversation(ctx(t), invalid); tr.Code(e) != tr.InvalidReference {
				t.Fatal("invalid conversation reference accepted")
			}
		})
	})
	// Proves known/unknown conversation capability discrimination.
	t.Run("conversation_caps", func(t *testing.T) {
		f := fixture(t, h)
		if _, e := f.A.ConversationCaps(ctx(t), f.Conversation); e != nil {
			t.Fatal("known conversation refused")
		}
		if _, e := f.A.ConversationCaps(ctx(t), tr.ConversationID("unknown-"+marker())); tr.Code(e) != tr.InvalidReference {
			t.Fatal("unknown conversation accepted")
		}
	})
	// Proves both shared identities appear in the membership snapshot.
	t.Run("members", func(t *testing.T) {
		f := fixture(t, h)
		members, e := f.A.Members(ctx(t), f.Conversation)
		if e != nil {
			t.Fatal(e)
		}
		for _, identity := range []tr.Transport{f.A, f.B} {
			p, e := identity.Self(ctx(t))
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, m := range members {
				if m.Principal == p.ID {
					found = true
				}
			}
			if !found {
				t.Fatal("shared identity missing from Members")
			}
		}
	})
	// Proves full neutral payload preservation and Event/Sync agreement; media bytes have a separate group.
	t.Run("event_message", func(t *testing.T) {
		f := fixture(t, h)
		start := baseline(t, f)
		anchor := send(t, f, "anchor")
		p, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		data := []byte(marker())
		ref, e := f.A.Upload(ctx(t), bytes.NewReader(data), tr.MediaMeta{Size: int64(len(data))})
		if e != nil {
			t.Fatal(e)
		}
		m := tr.Message{Text: marker(), Kind: "notice", Re: anchor.Event, Thread: anchor.Event, To: []tr.PrincipalID{p.ID}, Attachments: []tr.MediaRef{ref}, Extensions: f.Extensions}
		r, e := f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: marker(), Message: m})
		if e != nil || r.Outcome != tr.ACK {
			t.Fatalf("message round trip send refused: %v", e)
		}
		b := syncBatch(t, f, start, false)
		one(t, b, r.Event)
		for _, ev := range b.Events {
			if ev.ID != r.Event {
				continue
			}
			if ev.Message.Text != m.Text || ev.Message.Kind != m.Kind || ev.Message.Re != m.Re || ev.Message.Thread != m.Thread || !reflect.DeepEqual(ev.Message.To, m.To) || !reflect.DeepEqual(ev.Message.Attachments, m.Attachments) {
				t.Fatal("message payload changed")
			}
			for k, v := range m.Extensions {
				if !bytes.Equal(ev.Message.Extensions[k], v) {
					t.Fatal("message extension changed")
				}
			}
			direct, e := f.B.Event(ctx(t), f.Conversation, r.Event)
			if e != nil || direct.Sender != ev.Sender || direct.Conversation != ev.Conversation || !reflect.DeepEqual(direct.Message, ev.Message) {
				t.Fatal("Event differs from Sync")
			}
		}
	})
	// Proves definite refusal and transient pre-submit error are distinct beside a successful Send.
	t.Run("outcome_discipline", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "DefiniteRefusal", f.DefiniteRefusal != nil)
		required(t, "PreSubmitFailure", f.PreSubmitFailure != nil)
		required(t, "WireCalls", f.WireCalls != nil)
		if send(t, f, "outcome-good").Outcome != tr.ACK {
			t.Fatal("valid Send refused")
		}
		p, e := f.B.Self(ctx(t))
		if e != nil {
			t.Fatal(e)
		}
		req := tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: marker(), Message: tr.Message{Text: marker(), To: []tr.PrincipalID{p.ID}}}
		f.DefiniteRefusal()
		r, e := f.A.Send(ctx(t), req)
		if e != nil || r.Outcome != tr.REFUSED {
			t.Fatal("definite refusal must give REFUSED")
		}
		checkOutcome(t, r)
		req.IdempotencyKey = marker()
		failureContext := f.PreSubmitFailure()
		if failureContext == nil {
			t.Fatal("PreSubmitFailure returned nil context")
		}
		calls := f.WireCalls()
		_, e = f.A.Send(failureContext, req)
		// Catch transient-as-refusal and post-I/O errors. Wrapped
		// cancellation is allowed only when the supplied context is itself cancelled.
		cancelled := errors.Is(failureContext.Err(), context.Canceled) && errors.Is(e, context.Canceled)
		if e == nil || (tr.Code(e) != tr.Unavailable && !cancelled) || f.WireCalls() != calls {
			t.Fatal("pre-submit failure must give error with zero wire calls")
		}
	})
	// Proves every reported Gap has a closed reason (the gap plant also fails all Sync-using groups).
	t.Run("gap_validity", func(t *testing.T) { f := fixture(t, h); _ = syncBatch(t, f, nil, true) })
	// Proves actual window loss is visible beside a lossless delivered neighbour.
	t.Run("induced_loss", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "InduceLoss", f.InduceLoss != nil)
		start := baseline(t, f)
		good := send(t, f, "loss-good")
		got := syncBatch(t, f, start, false)
		one(t, got, good.Event)
		lost := send(t, f, "loss-lost")
		f.InduceLoss()
		after := syncBatch(t, f, got.Next, false)
		for _, ev := range after.Events {
			if ev.ID == lost.Event {
				t.Fatal("loss hook did not drop the event")
			}
		}
		for _, g := range after.Gaps {
			if g.Conversation == f.Conversation && g.Reason.Valid() {
				return
			}
		}
		t.Fatal("window loss not reported with Gap")
	})
	// Proves the declared Raw bound (including no exposure at zero) on ordinary
	// Sync, Event and History projections beside a delivered valid payload.
	t.Run("raw_bounds", func(t *testing.T) {
		f := fixture(t, h)
		limit := f.B.Caps().MaxRawBytes
		start := baseline(t, f)
		r := send(t, f, "raw")
		b := syncBatch(t, f, start, false)
		one(t, b, r.Event)
		direct, e := f.B.Event(ctx(t), f.Conversation, r.Event)
		if e != nil {
			t.Fatal(e)
		}
		page, e := f.B.History(ctx(t), f.Conversation, nil, 10)
		if e != nil {
			t.Fatal(e)
		}
		// Require the valid neighbour on each path so empty results cannot hide a leak.
		if direct.ID != r.Event {
			t.Fatal("ordinary received Event changed ID")
		}
		one(t, tr.Batch{Events: page.Events}, r.Event)
		for _, ev := range append(append(b.Events, direct), page.Events...) {
			if len(ev.Raw) > limit {
				t.Fatal("Event.Raw exceeds MaxRawBytes declared by Caps")
			}
		}
		// Proves exact N is present and N+1 is dropped/countable on all read paths.
		// At zero, proves the ordinary neighbour has no Raw and an absolute-ceiling+1
		// wire event is still dropped. Nil hooks leave this receive check unverified.
		t.Run("received", func(t *testing.T) {
			if f.ReceivedRawAtSize == nil {
				t.Skip("harness missing optional hook ReceivedRawAtSize")
			}
			exact, wireLimit := r.Event, limit
			if limit > 0 {
				exact = f.ReceivedRawAtSize(limit)
			} else {
				wireLimit = tr.MaxRawBytes
			}
			over := f.ReceivedRawAtSize(wireLimit + 1)
			if exact == "" || over == "" || exact == over {
				t.Fatal("ReceivedRawAtSize must return distinct nonempty IDs")
			}
			b := syncBatch(t, f, start, false)
			page, e := f.B.History(ctx(t), f.Conversation, nil, 10)
			if e != nil {
				t.Fatal(e)
			}
			for _, events := range [][]tr.Event{b.Events, page.Events} {
				one(t, tr.Batch{Events: events}, r.Event)
				one(t, tr.Batch{Events: events}, exact)
				for _, ev := range events {
					if ev.ID == over {
						t.Fatal("oversized received event delivered")
					}
					if len(ev.Raw) > limit {
						t.Fatal("Event.Raw exceeds MaxRawBytes declared by Caps")
					}
					if ev.ID == exact && len(ev.Raw) != limit {
						t.Fatal("exact-bound received Raw changed")
					}
				}
			}
			if b.Dropped < 1 || page.Dropped < 1 {
				t.Fatal("oversized received event not counted on Sync and History")
			}
			direct, e := f.B.Event(ctx(t), f.Conversation, exact)
			if e != nil || direct.ID != exact || len(direct.Raw) != limit {
				t.Fatal("exact-bound received Event unavailable or changed")
			}
			if _, e = f.B.Event(ctx(t), f.Conversation, over); tr.Code(e) != tr.InvalidReference {
				t.Fatal("oversized received Event not refused")
			}
		})
	})

	// Proves idempotent Close and closed-code operations; pure metadata remains usable.
	t.Run("close", func(t *testing.T) {
		f := fixture(t, h)
		good := send(t, f, "before-close")
		if e := f.A.Close(); e != nil {
			t.Fatal(e)
		}
		if e := f.A.Close(); e != nil {
			t.Fatal(e)
		}
		check := func(e error) {
			t.Helper()
			if tr.Code(e) != tr.Unavailable {
				t.Fatal("operation after Close must give unavailable")
			}
		}
		_, e := f.A.Self(ctx(t))
		check(e)
		_, e = f.A.ConversationCaps(ctx(t), f.Conversation)
		check(e)
		_, e = f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: marker()})
		check(e)
		_, e = f.A.Sync(ctx(t), nil, tr.SyncOptions{Initial: true})
		check(e)
		_, e = f.A.History(ctx(t), f.Conversation, nil, 1)
		check(e)
		_, e = f.A.Event(ctx(t), f.Conversation, good.Event)
		check(e)
		_, e = f.A.Members(ctx(t), f.Conversation)
		check(e)
		_, e = f.A.Upload(ctx(t), strings.NewReader("x"), tr.MediaMeta{Size: 1})
		check(e)
		_, e = f.A.Download(ctx(t), tr.NewMediaRef("opaque"), &bytes.Buffer{}, 1)
		check(e)
		_, e = f.A.Addressing().Principal("invalid")
		check(e)
		_, e = f.A.Addressing().Conversation(ctx(t), string(f.Conversation))
		check(e)
		if f.A.Caps().MaxContentBytes <= 0 {
			t.Fatal("Caps unavailable after Close")
		}
	})

}

func implements[T any](v any) bool { _, ok := v.(T); return ok }
