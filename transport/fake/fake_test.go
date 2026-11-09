package fake_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		w.SetDefect(defect)
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
		c, e := w.New("carol", tr.Config{StateDir: filepath.Join(state, "c")})
		if e != nil {
			t.Fatal(e)
		}
		result := &conformancetest.Fixture{A: a, B: b, C: c, Extensions: map[string]json.RawMessage{"opaque.test": json.RawMessage(`"valid extension"`)}, Conversation: "room", StateDir: state, SentinelDir: sentinel,
			RestartB: func() tr.Transport {
				next, e := w.New("bob", tr.Config{StateDir: filepath.Join(state, "b")})
				if e != nil {
					t.Fatal(e)
				}
				return next
			},
			FailAfterSubmit: w.FailAfterSubmit, PartialAck: w.PartialAck,
			PartWireCalls: w.PartWireCalls, DefiniteRefusal: w.DefiniteRefusal, PreSubmitFailure: w.PreSubmitFailure, InduceLoss: w.InduceLoss,
			UnverifiableSender: func(id tr.PrincipalID, s string) tr.EventID { return w.InjectUnverified(id, s) },
			BackdatedPublish:   func(at time.Time) tr.EventID { return w.Inject("alice", "backdated", at) },
			ForeignCursor:      func() tr.Cursor { return fake.NewWorld().Cursor() },
			OversizeReceived: func(marker string) error {
				id := w.Inject("alice", marker+strings.Repeat("x", 4096), time.Now())
				_, e := b.Event(context.Background(), "room", id)
				return e
			},
			WireCalls: w.WireCalls, MessageAtSize: fake.MessageAtSize, Logs: func() []string { return nil },
		}
		if strings.HasPrefix(defect, "missing-") {
			name := strings.TrimPrefix(defect, "missing-")
			value := reflect.ValueOf(result).Elem().FieldByName(name)
			if value.IsValid() {
				value.Set(reflect.Zero(value.Type()))
			}
		}
		if defect == "gap" {
			result.B = gapTransport{Transport: result.B}
		}
		if defect == "self-gap" {
			result.A = gapTransport{Transport: result.A}
		}
		if defect == "all-extensions" {
			result.A = allExtensions{result.A}
		}
		if defect == "registration-extension" {
			result.A = registration{result.A}
		}
		return result
	}}
}

// Proves the generic contract with real in-memory state transitions; no live service is exercised.
func TestFakeConformance(t *testing.T) {
	h := harness(os.Getenv("TRANSPORT_DEFECT"))
	if os.Getenv("TRANSPORT_DEFECT") == "missing-New" {
		h.New = nil
	}
	conformancetest.Run(t, h)
}

// Proves each named plant fails the intended subtest with its own diagnostic;
// clean neighbours pass the same group. This validates detection, not a malicious harness.
func TestPlantedDefects(t *testing.T) {
	for _, tc := range controls() {
		t.Run(tc.defect+"_"+tc.group, func(t *testing.T) {
			runControl(t, "", tc.group, "", false)
			runControl(t, tc.defect, tc.group, tc.reason, true)
		})
	}
}

type control struct{ defect, group, reason string }

func controls() []control {
	out := []control{
		{"drop-key", "send_idempotency", "idempotency key retry created a second logical event"},
		{"unknown-new-id", "unknown_retry", "retry created multiple distinct logical EventIDs"},
		{"sender", "unverifiable_sender", "unverifiable sender delivered"},
		{"resend-all", "partial_ack", "retry resent an acknowledged part"},
		{"early-ack", "partial_ack", "partial acknowledgement must give UNKNOWN"},
		{"advance-pending", "partial_window", "cursor advanced past pending event without Gap"},
		{"addressing", "addressing", "invalid principal reference accepted"},
		{"caps", "conversation_caps", "known conversation refused"},
		{"members", "members", "shared identity missing from Members"},
		{"event", "event_message", "Event differs from Sync"},
		{"message", "event_message", "message payload changed"},
		{"transient-refused", "outcome_discipline", "pre-submit failure must give error with zero wire calls"},
		{"error-after-submit", "unknown_retry", "transport: unavailable"},
		{"silent-loss", "induced_loss", "window loss not reported with Gap"},
		{"raw", "raw_bounds", "Event.Raw exceeds MaxRawBytes"},
		{"history-cursor", "cursors", "invalid History cursor accepted"},
		{"backdated", "resume_backdated_history", "logical event"},
		// A bad Gap fails every Sync-using group. Assert the resume group AND the dedicated validity check.
		{"gap", "resume_backdated_history", "invalid Gap reason"},
		{"gap", "gap_validity", "invalid Gap reason"},
		{"self-gap", "partial_ack", "invalid Gap reason"},
	}
	hooks := []struct{ name, group string }{
		{"New", "self"}, {"RestartB", "resume_backdated_history"}, {"BackdatedPublish", "resume_backdated_history"},
		{"FailAfterSubmit", "unknown_retry"}, {"PartialAck", "partial_ack"}, {"PartWireCalls", "partial_ack"},
		{"DefiniteRefusal", "outcome_discipline"}, {"PreSubmitFailure", "outcome_discipline"},
		{"InduceLoss", "induced_loss"}, {"UnverifiableSender", "unverifiable_sender"},
		{"ForeignCursor", "cursors"}, {"OversizeReceived", "media_redaction_errors"},
		{"WireCalls", "bounds"}, {"MessageAtSize", "bounds"}, {"Logs", "media_redaction_errors"},
	}
	for _, hook := range hooks {
		out = append(out, control{"missing-" + hook.name, hook.group, "harness missing fault hook " + hook.name})
	}
	return out
}
func runControl(t *testing.T, defect, group, reason string, wantFail bool) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	c := exec.Command(exe, "-test.run=^TestFakeConformance$/^"+group+"$", "-test.v")
	c.Env = append(os.Environ(), "TRANSPORT_DEFECT="+defect)
	out, e := c.CombinedOutput()
	// Scope diagnostics to the named subtest: a failure elsewhere cannot satisfy a control.
	name := "TestFakeConformance/" + group
	lines := strings.Split(string(out), "\n")
	inGroup, diagnostic, verdict := false, false, false
	action := "PASS"
	if wantFail {
		action = "FAIL"
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "=== RUN   ") {
			inGroup = strings.TrimPrefix(line, "=== RUN   ") == name
		}
		if inGroup && strings.Contains(line, reason) {
			diagnostic = true
		}
		if strings.HasPrefix(strings.TrimSpace(line), "--- "+action+": "+name+" (") {
			verdict = true
		}
	}
	if (e != nil) != wantFail || !verdict || wantFail && !diagnostic {
		t.Fatalf("control %s expected %s %s with %q: %s", defect, action, name, reason, out)
	}
	t.Logf("plant=%s expected=%s observed=%s %s reason=%s", defect, name, action, name, reason)
}

// Proves implemented Registration and all implemented extensions pass discovery.
func TestImplementedExtensions(t *testing.T) {
	for _, defect := range []string{"registration-extension", "all-extensions"} {
		runControl(t, defect, "unsupported", "", false)
	}
}

type registration struct{ tr.Transport }

func (registration) Register(context.Context, map[string]string) (tr.Principal, error) {
	return tr.Principal{}, nil
}

type allExtensions struct{ tr.Transport }

func (allExtensions) Register(context.Context, map[string]string) (tr.Principal, error) {
	return tr.Principal{}, nil
}
func (allExtensions) Create(context.Context, string) (tr.ConversationID, error)       { return "", nil }
func (allExtensions) Join(context.Context, string) (tr.ConversationID, error)         { return "", nil }
func (allExtensions) Leave(context.Context, tr.ConversationID) error                  { return nil }
func (allExtensions) Invite(context.Context, tr.ConversationID, tr.PrincipalID) error { return nil }
func (allExtensions) Alias(context.Context, tr.ConversationID, string) error          { return nil }
func (allExtensions) GetState(context.Context, tr.ConversationID, string, string) (json.RawMessage, error) {
	return nil, nil
}
func (allExtensions) PutState(context.Context, tr.ConversationID, string, string, json.RawMessage) (tr.EventID, error) {
	return "", nil
}
func (allExtensions) GetAccountData(context.Context, string) (json.RawMessage, error) {
	return nil, nil
}
func (allExtensions) PutAccountData(context.Context, string, json.RawMessage) error { return nil }
func (allExtensions) SetPresence(context.Context, string) error                     { return nil }
func (allExtensions) Redact(context.Context, tr.ConversationID, tr.EventID, string) (tr.SendResult, error) {
	return tr.SendResult{}, nil
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
	third, e := w.New("carol", tr.Config{StateDir: filepath.Join(root, "c")})
	if e != nil {
		t.Fatal(e)
	}
	defer third.Close()
	c := context.Background()
	own := w.Cursor()
	if _, e = b.Sync(c, own, tr.SyncOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Sync(c, fake.NewWorld().Cursor(), tr.SyncOptions{}); tr.Code(e) != tr.InvalidCursor {
		t.Fatal("actual foreign-world cursor accepted")
	}
	r := tr.SendRequest{Conversation: "room", IdempotencyKey: "partial", Message: tr.Message{Text: "valid", To: []tr.PrincipalID{"bob", "carol"}}}
	w.PartialAck()
	first, e := a.Send(c, r)
	if e != nil || first.Outcome != tr.UNKNOWN || w.PartCalls("alice") != 1 || w.PartCalls("bob") != 1 || w.PartCalls("carol") != 0 {
		t.Fatal("partial publication did not stop after two recipients")
	}
	second, e := a.Send(c, r)
	if e != nil || second.Outcome != tr.ACK || w.PartCalls("alice") != 1 || w.PartCalls("bob") != 1 || w.PartCalls("carol") != 1 {
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

// Proves concurrent same-key submissions converge on one event, and locked defect
// setters and evidence snapshots are safe alongside Send/Sync. It proves no live I/O.
func TestConcurrentIdentityAndFaultState(t *testing.T) {
	f := harness("").New(t)
	defer f.A.Close()
	defer f.B.Close()
	defer f.C.Close()
	// A second isolated world exercises setter/evidence access under concurrent sends.
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w := fake.NewWorld()
	a, e := w.New("alice", tr.Config{StateDir: root})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	type result struct {
		send tr.SendResult
		err  error
	}
	done := make(chan result, 16)
	for i := 0; i < 16; i++ {
		go func() {
			w.SetDefect("")
			r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "room", IdempotencyKey: "concurrent", Message: tr.Message{Text: "valid"}})
			calls := w.PartWireCalls("concurrent")
			calls["alice"] = 999
			done <- result{r, e}
		}()
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var id tr.EventID
	for i := 0; i < 16; i++ {
		select {
		case r := <-done:
			if r.err != nil || r.send.Outcome != tr.ACK {
				t.Fatal("concurrent send failed")
			}
			if id != "" && r.send.Event != id {
				t.Fatal("concurrent retry minted a new ID")
			}
			id = r.send.Event
		case <-deadline.C:
			t.Fatal("concurrent sends did not finish")
		}
	}
	if w.PartWireCalls("concurrent")["alice"] != 1 {
		t.Fatal("evidence snapshot aliases storage or part resent")
	}
	b, e := a.Sync(context.Background(), nil, tr.SyncOptions{Initial: true})
	if e != nil || len(b.Events) != 1 || b.Events[0].ID != id {
		t.Fatal("concurrent send lost or duplicated event")
	}
}
