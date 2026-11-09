// Package conformancetest tests the public transport contract without internal imports.
package conformancetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/relux-works/a2a-swarm/transport"
)

// Harness creates a fresh isolated world for each group. Fault hooks must act at
// the actual I/O boundary. Missing required hooks fail instead of skipping.
type Harness struct{ New func(*testing.T) *Fixture }
type Fixture struct {
	A, B         tr.Transport
	Conversation tr.ConversationID
	// RestartB reopens the same identity without resetting the world.
	RestartB              func() tr.Transport
	FailAfterSubmit       func()
	PartialAck            func()
	UnverifiableSender    func(string) tr.EventID
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
	required(t, "StateDir", f.StateDir != "")
	required(t, "SentinelDir", f.SentinelDir != "")
	before := snapshot(t, f.SentinelDir)
	t.Cleanup(func() {
		checkState(t, f.StateDir)
		if after := snapshot(t, f.SentinelDir); after != before {
			t.Error("sentinel sibling changed")
		}
	})
	t.Cleanup(func() { _ = f.A.Close(); _ = f.B.Close() })
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
	r, e := f.A.Send(ctx(t), tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: key, Message: tr.Message{Text: "valid neighbour", To: []tr.PrincipalID{p.ID}}})
	if e != nil {
		t.Fatal(e)
	}
	return r
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
		page := []byte(nil)
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
		one(t, syncBatch(t, f, c, false), r.Event)
	})
	t.Run("partial_ack", func(t *testing.T) {
		f := fixture(t, h)
		required(t, "PartialAck", f.PartialAck != nil)
		c := baseline(t, f)
		self, e := f.A.Sync(ctx(t), nil, tr.SyncOptions{Initial: true})
		if e != nil {
			t.Fatal(e)
		}
		checkGaps(t, self)
		f.PartialAck()
		r := send(t, f, "partial")
		if r.Outcome != tr.UNKNOWN {
			t.Fatal("partial acknowledgement must give UNKNOWN")
		}
		before, e := f.A.Sync(ctx(t), self.Next, tr.SyncOptions{})
		if e != nil {
			t.Fatal(e)
		}
		checkGaps(t, before)
		if len(before.Events) != 1 {
			t.Fatal("partial publication missing self copy")
		}
		r = send(t, f, "partial")
		if r.Outcome != tr.ACK || r.Event != before.Events[0].ID {
			t.Fatal("partial retry changed logical EventID")
		}
		one(t, syncBatch(t, f, c, false), r.Event)
		a, e := f.A.Sync(ctx(t), self.Next, tr.SyncOptions{})
		if e != nil {
			t.Fatal(e)
		}
		checkGaps(t, a)
		one(t, a, r.Event)
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
		bad := f.UnverifiableSender("received-SECRET-marker")
		b := syncBatch(t, f, c, false)
		one(t, b, good.Event)
		for _, e := range b.Events {
			if e.ID == bad {
				t.Fatal("unverifiable sender delivered")
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
		marker := "received-SECRET-marker"
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
		f.UnverifiableSender(marker)
		_ = syncBatch(t, f, c, false)
		for _, line := range f.Logs() {
			if strings.Contains(line, marker) {
				t.Fatal("received marker in log")
			}
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		f := fixture(t, h)
		if _, ok := f.A.(tr.Registration); ok {
			t.Fatal("harness must select an unsupported extension")
		}
		if tr.Code(tr.Unsupported()) != tr.UnsupportedByTransport {
			t.Fatal("unsupported extension not refused")
		}
		r := send(t, f, "supported")
		if r.Outcome != tr.ACK {
			t.Fatal("supported neighbour refused")
		}
	})
}
