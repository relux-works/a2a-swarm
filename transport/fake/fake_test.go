package fake_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/relux-works/a2a-swarm/transport"
	"github.com/relux-works/a2a-swarm/transport/conformancetest"
	"github.com/relux-works/a2a-swarm/transport/fake"
)

func harness(defect string) conformancetest.Harness {
	return conformancetest.Harness{New: func(t *testing.T) *conformancetest.Fixture {
		root, e := filepath.EvalSymlinks(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		state, sentinel := filepath.Join(root, "state"), filepath.Join(root, "sentinel")
		if e = os.Mkdir(sentinel, 0755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(sentinel, "keep"), []byte("untouched"), 0644); e != nil {
			t.Fatal(e)
		}
		w := fake.NewWorld()
		w.DropKeyOnRetry = defect == "drop-key"
		w.ReturnUnverified = defect == "sender"
		a, e := w.New("alice", tr.Config{StateDir: filepath.Join(state, "a")})
		if e != nil {
			t.Fatal(e)
		}
		// MkdirAll can leave the parent subject to umask; explicitly claim the state root.
		if e = os.Chmod(state, 0700); e != nil {
			t.Fatal(e)
		}
		b, e := w.New("bob", tr.Config{StateDir: filepath.Join(state, "b")})
		if e != nil {
			t.Fatal(e)
		}
		result := &conformancetest.Fixture{A: a, B: b, Conversation: "room", StateDir: state, SentinelDir: sentinel,
			RestartB: func() tr.Transport {
				next, e := w.New("bob", tr.Config{StateDir: filepath.Join(state, "b")})
				if e != nil {
					t.Fatal(e)
				}
				return next
			},
			FailAfterSubmit: w.FailAfterSubmit, PartialAck: w.PartialAck,
			UnverifiableSender: func(s string) tr.EventID { return w.Inject("unverifiable", s, time.Now()) },
			BackdatedPublish:   func(at time.Time) tr.EventID { return w.Inject("alice", "backdated", at) },
			ForeignCursor:      func() tr.Cursor { return fake.NewWorld().Cursor() },
			OversizeReceived: func(marker string) error {
				id := w.Inject("alice", marker+strings.Repeat("x", 4096), time.Now())
				_, e := b.Event(context.Background(), "room", id)
				return e
			},
			WireCalls: w.WireCalls, MessageAtSize: fake.MessageAtSize, Logs: func() []string { return nil },
		}
		if defect == "missing-hook" {
			result.FailAfterSubmit = nil
		}
		if defect == "gap" {
			result.B = gapTransport{Transport: result.B}
		}
		if defect == "self-gap" {
			result.A = gapTransport{Transport: result.A}
		}
		return result
	}}
}

// Proves the generic contract with real in-memory state transitions; no live service is exercised.
func TestFakeConformance(t *testing.T) {
	conformancetest.Run(t, harness(os.Getenv("TRANSPORT_DEFECT")))
}

// Proves the suite detects the specific retry and sender defects, not merely any failure.
func TestPlantedDefects(t *testing.T) {
	for _, tc := range []struct{ defect, reason string }{{"drop-key", "idempotency key retry created a second logical event"}, {"sender", "unverifiable sender delivered"}, {"missing-hook", "harness missing fault hook FailAfterSubmit"}, {"gap", "invalid Gap reason"}, {"self-gap", "invalid Gap reason"}} {
		t.Run(tc.defect, func(t *testing.T) {
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			c := exec.Command(exe, "-test.run=^TestFakeConformance$", "-test.v")
			c.Env = append(os.Environ(), "TRANSPORT_DEFECT="+tc.defect)
			out, e := c.CombinedOutput()
			if e == nil || !strings.Contains(string(out), tc.reason) {
				t.Fatalf("defect did not fail for intended reason %q: %s", tc.reason, out)
			}
		})
	}
}

// Proves a real other-world cursor is refused beside its own cursor, and a
// partial retry sends only the unacknowledged recipient while preserving ID.
func TestCursorScopeAndPartialRecipients(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w := fake.NewWorld()
	a, e := w.New("alice", tr.Config{StateDir: filepath.Join(root, "a")})
	if e != nil {
		t.Fatal(e)
	}
	b, e := w.New("bob", tr.Config{StateDir: filepath.Join(root, "b")})
	if e != nil {
		t.Fatal(e)
	}
	c := context.Background()
	own := w.Cursor()
	if _, e = b.Sync(c, own, tr.SyncOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Sync(c, fake.NewWorld().Cursor(), tr.SyncOptions{}); tr.Code(e) != tr.InvalidCursor {
		t.Fatal("actual foreign-world cursor accepted")
	}
	r := tr.SendRequest{Conversation: "room", IdempotencyKey: "partial", Message: tr.Message{Text: "valid", To: []tr.PrincipalID{"bob"}}}
	w.PartialAck()
	first, e := a.Send(c, r)
	if e != nil || first.Outcome != tr.UNKNOWN || w.PartCalls("alice") != 1 || w.PartCalls("bob") != 0 {
		t.Fatal("partial publication did not stop after one recipient")
	}
	second, e := a.Send(c, r)
	if e != nil || second.Outcome != tr.ACK || w.PartCalls("alice") != 1 || w.PartCalls("bob") != 1 {
		t.Fatal("retry resent an acknowledged part")
	}
	for _, identity := range []tr.Transport{a, b} {
		batch, e := identity.Sync(c, own, tr.SyncOptions{})
		if e != nil || len(batch.Events) != 1 || batch.Events[0].ID != second.Event {
			t.Fatal("partial retry changed event or duplicated recipient")
		}
	}
}

// Proves stored payloads and returned slices do not alias the caller, including
// opaque attachment references; this is memory ownership, not media persistence.
func TestMessageOwnership(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w := fake.NewWorld()
	a, e := w.New("alice", tr.Config{StateDir: root})
	if e != nil {
		t.Fatal(e)
	}
	r := tr.SendRequest{Conversation: "room", IdempotencyKey: "ownership", Message: tr.Message{To: []tr.PrincipalID{"alice"}, Attachments: []tr.MediaRef{tr.NewMediaRef("opaque-value")}, Extensions: map[string]json.RawMessage{"key": json.RawMessage(`"value"`)}}}
	ack, e := a.Send(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	r.Message.To[0] = "changed"
	r.Message.Attachments[0] = tr.NewMediaRef("changed")
	r.Message.Extensions["key"][1] = 'X'
	ev, e := a.Event(context.Background(), "room", ack.Event)
	if e != nil || ev.Message.To[0] != "alice" || ev.Message.Attachments[0].Opaque() != "opaque-value" || string(ev.Message.Extensions["key"]) != `"value"` {
		t.Fatal("stored message aliases caller")
	}
	ev.Message.To[0] = "changed"
	again, e := a.Event(context.Background(), "room", ack.Event)
	if e != nil || again.Message.To[0] != "alice" {
		t.Fatal("returned message aliases storage")
	}
}

type gapTransport struct{ tr.Transport }

func (g gapTransport) Sync(c context.Context, from tr.Cursor, opt tr.SyncOptions) (tr.Batch, error) {
	b, e := g.Transport.Sync(c, from, opt)
	b.Gaps = append(b.Gaps, tr.Gap{Conversation: "room", Reason: "unlisted"})
	return b, e
}
