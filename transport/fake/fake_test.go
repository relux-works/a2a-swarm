package fake_test

import (
	"context"
	"encoding/json"
	"fmt"
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
			PrincipalReferences:    func() (string, string) { return "bob", "not-a-principal" },
			ConversationReferences: func() (string, string) { return "room", "not-a-conversation" },
			ReceivedRawAtSize:      func(n int) tr.EventID { return w.InjectRaw("alice", n) },
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
		if defect == "empty-Extensions" {
			result.Extensions = map[string]json.RawMessage{}
		}
		if defect == "state-file-mode" {
			if e := os.Chmod(filepath.Join(state, "a", "identity"), 0644); e != nil {
				t.Fatal(e)
			}
		}
		if defect == "reference-grammar" || defect == "conversation-addressing" {
			result.A = referenceTransport{Transport: a, acceptInvalid: defect == "conversation-addressing"}
			result.PrincipalReferences = func() (string, string) { return "principal:bob", "bad principal" }
			result.ConversationReferences = func() (string, string) { return "conversation:room", "bad conversation" }
		}
		if strings.HasPrefix(defect, "cancel-") {
			wrapped := &cancelledTransport{Transport: a, defect: defect}
			result.PreSubmitFailure = func() context.Context {
				if defect == "cancel-live" {
					wrapped.failLive = true
					return context.Background()
				}
				c, cancel := context.WithCancel(context.Background())
				cancel()
				return c
			}
			result.A = wrapped
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
		// Catches ID-rewriting impersonators which evaded the old wire-ID-only check.
		{"sender-rewrite", "unverifiable_sender", "unauthenticated claim of B delivered"},
		{"resend-all", "partial_ack", "retry resent an acknowledged part"},
		{"early-ack", "partial_ack", "partial acknowledgement must give UNKNOWN"},
		{"advance-pending", "partial_window", "cursor advanced past pending event without Gap"},
		{"addressing", "addressing/principal_references", "invalid principal reference accepted"},
		// Catches a resolver that accepts every conversation reference.
		{"conversation-addressing", "addressing/conversation_references", "invalid conversation reference accepted"},
		{"caps", "conversation_caps", "known conversation refused"},
		{"members", "members", "shared identity missing from Members"},
		{"event", "event_message", "Event differs from Sync"},
		{"message", "event_message", "message payload changed"},
		{"transient-refused", "outcome_discipline", "pre-submit failure must give error with zero wire calls"},
		// Catch unrelated errors, cancellation without a cancelled context, and a
		// cancelled Send which submits anyway; wrapped cancellation is the neighbour.
		{"cancel-unrelated", "outcome_discipline", "pre-submit failure must give error with zero wire calls"},
		{"cancel-live", "outcome_discipline", "pre-submit failure must give error with zero wire calls"},
		{"cancel-wire", "outcome_discipline", "pre-submit failure must give error with zero wire calls"},
		{"error-after-submit", "unknown_retry", "transport: unavailable"},
		{"silent-loss", "induced_loss", "window loss not reported with Gap"},
		{"raw", "raw_bounds", "Event.Raw exceeds MaxRawBytes"},
		// Catches missing receive validation even when ordinary sent events are small.
		{"accept-raw", "raw_bounds/received", "oversized received event delivered"},
		// Catch clamping instead of dropping, and rejection of the valid boundary.
		{"clamp-raw", "raw_bounds/received", "oversized received event delivered"},
		{"reject-raw-bound", "raw_bounds/received", "logical event"},
		// Catches a transport writing a non-private file inside its otherwise private tree.
		{"state-file-mode", "self", "StateDir mode: identity"},
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
	// Catch incomplete fixtures which could previously omit meaningful coverage;
	// each control is paired with a complete-fixture run of the same group.
	for _, name := range []string{"C", "Extensions", "StateDir", "SentinelDir"} {
		out = append(out, control{"missing-" + name, "self", "harness missing fixture field " + name})
	}
	out = append(out, control{"empty-Extensions", "self", "harness missing fixture field Extensions"})
	return out
}
func runControl(t *testing.T, defect, group, reason string, wantFail bool) {
	t.Helper()
	action := "PASS"
	if wantFail {
		action = "FAIL"
	}
	runVerdict(t, defect, group, reason, action)
}

func runVerdict(t *testing.T, defect, group, reason, action string) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(group, "/")
	for i, part := range parts {
		parts[i] = "^" + part + "$"
	}
	c := exec.Command(exe, "-test.run=^TestFakeConformance$/"+strings.Join(parts, "/"), "-test.v")
	c.Env = append(os.Environ(), "TRANSPORT_DEFECT="+defect)
	out, e := c.CombinedOutput()
	// Scope diagnostics to the named subtest: a failure elsewhere cannot satisfy a control.
	name := "TestFakeConformance/" + group
	lines := strings.Split(string(out), "\n")
	inGroup, diagnostic, verdict := false, false, false
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
	if (e != nil) != (action == "FAIL") || !verdict || !diagnostic {
		t.Fatalf("control %s expected %s %s with %q: %s", defect, action, name, reason, out)
	}
	t.Logf("plant=%s expected=%s observed=%s %s reason=%s", defect, name, action, name, reason)
}

// Proves new hooks are optional with named SKIP verdicts, beside complete-hook
// passes. Catches a harness that silently passes an unexercised new check.
func TestOptionalHookSkips(t *testing.T) {
	for _, tc := range []struct{ hook, group string }{
		{"PrincipalReferences", "addressing/principal_references"},
		{"ConversationReferences", "addressing/conversation_references"},
		{"ReceivedRawAtSize", "raw_bounds/received"},
	} {
		t.Run(tc.hook, func(t *testing.T) {
			runControl(t, "", tc.group, "", false)
			runVerdict(t, "missing-"+tc.hook, tc.group, "harness missing optional hook "+tc.hook, "SKIP")
		})
	}
}

// Proves the addressing group handles a grammar where IDs are not references
// and the old invalid-hex probes are valid. It proves no external adapter grammar.
func TestReferenceGrammar(t *testing.T) {
	runControl(t, "reference-grammar", "addressing", "", false)
}

type referenceTransport struct {
	tr.Transport
	acceptInvalid bool
}

func (r referenceTransport) Addressing() tr.Addressing {
	return referenceAddressing{Addressing: r.Transport.Addressing(), acceptInvalid: r.acceptInvalid}
}

type referenceAddressing struct {
	tr.Addressing
	acceptInvalid bool
}

func (r referenceAddressing) Format(p tr.PrincipalID) string { return "principal:" + string(p) }
func (r referenceAddressing) Principal(s string) (tr.PrincipalID, error) {
	if strings.HasPrefix(s, "invalid-") {
		return "bob", nil
	}
	if !strings.HasPrefix(s, "principal:") {
		return "", tr.NewError(tr.InvalidReference)
	}
	return r.Addressing.Principal(strings.TrimPrefix(s, "principal:"))
}
func (r referenceAddressing) Conversation(c context.Context, s string) (tr.ConversationID, error) {
	if r.acceptInvalid || strings.HasPrefix(s, "invalid-") {
		return "room", nil
	}
	if !strings.HasPrefix(s, "conversation:") {
		return "", tr.NewError(tr.InvalidReference)
	}
	return r.Addressing.Conversation(c, strings.TrimPrefix(s, "conversation:"))
}

// Proves a wrapped cancellation from a cancelled hook context is accepted beside
// successful live-context sends. Internal unavailable has the main fake neighbour.
func TestWrappedPreSubmitCancellation(t *testing.T) {
	runControl(t, "cancel-wrapped", "outcome_discipline", "", false)
}

// Proves History holds at an unacknowledged recipient and later resumes through
// that same event, beside delivered neighbours. Catches the old History which
// advanced past pending copies, and a replacement which rejects everything.
func TestHistoryPendingAndLoss(t *testing.T) {
	f := harness("").New(t)
	t.Cleanup(func() { _ = f.A.Close(); _ = f.B.Close(); _ = f.C.Close() })
	c := context.Background()
	bp, e := f.B.Self(c)
	if e != nil {
		t.Fatal(e)
	}
	cp, e := f.C.Self(c)
	if e != nil {
		t.Fatal(e)
	}
	send := func(key string, to ...tr.PrincipalID) tr.SendResult {
		t.Helper()
		r, e := f.A.Send(c, tr.SendRequest{Conversation: f.Conversation, IdempotencyKey: key, Message: tr.Message{Text: key, To: to}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	before := send("before", bp.ID)
	f.PartialAck()
	if r := send("pending", cp.ID, bp.ID); r.Outcome != tr.UNKNOWN {
		t.Fatal("pending control was not partially published")
	}
	later := send("later", bp.ID)
	page, e := f.B.History(c, f.Conversation, nil, 10)
	if e != nil || len(page.Events) != 1 || page.Events[0].ID != before.Event || len(page.Next) == 0 || page.Dropped != 0 {
		t.Fatal("History skipped pending copy or counted it as dropped")
	}
	held, e := f.B.History(c, f.Conversation, page.Next, 10)
	if e != nil || len(held.Events) != 0 || string(held.Next) != string(page.Next) || held.Dropped != 0 {
		t.Fatal("History did not hold its pending cursor")
	}
	resumed := send("pending", cp.ID, bp.ID)
	if resumed.Outcome != tr.ACK {
		t.Fatal("pending retry did not ACK")
	}
	after, e := f.B.History(c, f.Conversation, held.Next, 1)
	if e != nil || len(after.Events) != 1 || after.Events[0].ID != resumed.Event || len(after.Next) == 0 {
		t.Fatal("History lost pending event after acknowledgement")
	}
	tail, e := f.B.History(c, f.Conversation, after.Next, 1)
	if e != nil || len(tail.Events) != 1 || tail.Events[0].ID != later.Event || len(tail.Next) != 0 {
		t.Fatal("History pagination lost the delivered neighbour")
	}
	// Proves induced loss also bounds History from both a fresh and retained
	// cursor. Catches the old History which resurrected events that Sync had lost.
	f.InduceLoss()
	fresh := send("after-loss", bp.ID)
	for _, cursor := range []tr.Cursor{nil, page.Next, after.Next} {
		p, e := f.B.History(c, f.Conversation, cursor, 10)
		if e != nil || len(p.Events) != 1 || p.Events[0].ID != fresh.Event || len(p.Next) != 0 || p.Dropped != 0 {
			t.Fatal("History resurrected lost events or lost the retained neighbour")
		}
	}
}

// Proves fake creation writes a regular 0600 file and restart preserves it;
// catches a vacuous private-file check when StateDir contains only directories.
func TestPrivateStateFile(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	w := fake.NewWorld()
	a, e := w.New("alice", tr.Config{StateDir: root})
	if e != nil {
		t.Fatal(e)
	}
	_ = a.Close()
	path := filepath.Join(root, "identity")
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("fake did not write a regular private state file")
	}
	if e := os.WriteFile(path, []byte("retained private state"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := w.New("alice", tr.Config{StateDir: root})
	if e != nil {
		t.Fatal(e)
	}
	_ = b.Close()
	if data, e := os.ReadFile(path); e != nil || string(data) != "retained private state" {
		t.Fatal("restart rewrote the private state file")
	}
	// Catch reuse of an existing public or symlink state file; neither refusal
	// may alter the file or its outside target. The regular private neighbour passed above.
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := w.New("alice", tr.Config{StateDir: root}); tr.Code(e) != tr.InvalidReference {
		t.Fatal("public existing state file accepted")
	}
	if data, e := os.ReadFile(path); e != nil || string(data) != "retained private state" {
		t.Fatal("refusal changed existing state")
	}
	if e := os.Remove(path); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if e := os.WriteFile(target, []byte("untouched"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, path); e != nil {
		t.Fatal(e)
	}
	if _, e := w.New("alice", tr.Config{StateDir: root}); tr.Code(e) != tr.InvalidReference {
		t.Fatal("symlink existing state file accepted")
	}
	if data, e := os.ReadFile(target); e != nil || string(data) != "untouched" {
		t.Fatal("refusal changed the symlink target")
	}
}

type cancelledTransport struct {
	tr.Transport
	defect   string
	failLive bool
}

func (c *cancelledTransport) Send(ctx context.Context, req tr.SendRequest) (tr.SendResult, error) {
	if c.failLive {
		c.failLive = false
		return tr.SendResult{}, fmt.Errorf("spurious cancellation: %w", context.Canceled)
	}
	if ctx.Err() == nil {
		return c.Transport.Send(ctx, req)
	}
	if c.defect == "cancel-wire" {
		_, _ = c.Transport.Send(context.Background(), req)
	}
	if c.defect == "cancel-unrelated" {
		return tr.SendResult{}, fmt.Errorf("unrelated pre-submit error")
	}
	return tr.SendResult{}, fmt.Errorf("cancelled: %w", context.Canceled)
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
