package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"github.com/relux-works/a2a-swarm/internal/wakediag"
)

func requireWakeDiagnostic(t *testing.T, r *wakediag.Record, kind wakediag.Kind) {
	t.Helper()
	if r == nil || !r.Valid() || r.Kind != kind {
		t.Fatal("missing or wrong retained wake diagnostic")
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(b, &fields) != nil || len(fields) != 2 || fields["kind"] != string(kind) || fields["at"] == nil {
		t.Fatal("diagnostic contains more than closed kind and time")
	}
}

// Actual private tmux parser refusals with known eligible pending mail retain
// one pre-Send record without typing, attempt/rate reservation, turn or receipt.
// Human status must explain that retained pre-Send layout rejection, rather
// than merely display its kind. Ready/no-mail and valid same-pane submission
// below are the positive controls.
func TestWakeDiagnosticTerminalPreSend(t *testing.T) {
	for _, mode := range []string{"unsupported", "clipped"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			control := map[string]any{"unknown": true}
			if mode == "clipped" {
				control = map[string]any{"row": 23}
			}
			f.control(control)
			r := f.a.terminalEffects(*f.binding()).ObserveReadiness(context.Background(), "")
			if !r.InputObserved || r.Input.Reason != tmuxwake.InputReasonLayoutUnrecognized {
				t.Fatal("fixture did not exercise parser layout refusal")
			}
			f.dispatch()
			b := f.binding()
			requireWakeDiagnostic(t, b.WakeDiagnostic, wakediag.TerminalLayoutRefused)
			first := *b.WakeDiagnostic
			logPath := filepath.Join(f.e.dir, "state", "alice.binding.log")
			logBefore, _ := os.ReadFile(logPath)
			for i := 0; i < 3; i++ {
				f.step()
			}
			logAfter, _ := os.ReadFile(logPath)
			if !bytes.Equal(logBefore, logAfter) {
				t.Fatal("identical poll appended diagnostic logs")
			}
			b = f.binding()
			if *b.WakeDiagnostic != first || len(b.AttemptIDs) != 0 || len(b.Reservations) != 0 || b.SendRefusal != nil || b.Typed || f.e.received("alice", id) {
				t.Fatal("polling refreshed history or changed delivery/mail/rate state")
			}
			for _, args := range [][]string{{"bind", "status"}, {"bind", "status", "--json"}} {
				out := readOnlyWakeStatus(t, f.e, args...)
				if !strings.Contains(out, string(wakediag.TerminalLayoutRefused)) || strings.Contains(out, "send_refusal") {
					t.Fatal("pre-Send history needs later Send evidence")
				}
				if args[len(args)-1] != "--json" {
					// The public contract identifies the observation and stage, not
					// the cause of the unrecognized layout or a later Send result.
					prefix := "wake_diagnostic=" + string(wakediag.TerminalLayoutRefused) + " at=" + first.At.Format(time.RFC3339Nano) + "; retained observation: "
					var explanation string
					for _, line := range strings.Split(out, "\n") {
						if strings.HasPrefix(line, prefix) {
							explanation = strings.TrimPrefix(line, prefix)
						}
					}
					if !strings.Contains(explanation, "terminal readiness refused an unrecognized layout") ||
						!strings.Contains(explanation, "before Send with pending mail") {
						t.Fatal("human wake diagnostic must explain the retained pre-Send layout refusal with pending mail")
					}
				}
			}
			// Later generic readiness observations cannot erase the retained record.
			f.control(map[string]any{})
			f.dispatch()
			if *f.binding().WakeDiagnostic != first {
				t.Fatal("ready observation erased historical refusal")
			}
			screen := f.wait(func(s orphanScreen) bool { return true })
			if screen.Typed != 0 || screen.Erased != 0 || len(screen.Submitted) != 0 {
				t.Fatal("diagnostics changed input")
			}
		})
	}
}

// Neither no mail (even on an unsupported layout) nor healthy same-pane
// submission fabricates a refusal; delivery retains unread mail as before.
func TestWakeDiagnosticTerminalPositive(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.dispatch()
	f.control(map[string]any{"unknown": true})
	f.step()
	if f.binding().WakeDiagnostic != nil {
		t.Fatal("no mail manufactured a refusal")
	}
	f.control(map[string]any{})
	id := f.mail()
	f.step()
	f.step()
	f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	b := f.binding()
	if b.WakeDiagnostic != nil || b.SendRefusal != nil || len(b.Rung) != 1 || f.e.received("alice", id) {
		t.Fatal("valid same-pane delivery diagnostic or receipt changed")
	}
}

// A real draft with pending mail reaches pre-Send suppression, keeps its exact
// parser class, and gains no layout history or delivery effects. The existing
// TerminalPositive test is the paired healthy same-pane dispatch control.
func TestWakeDiagnosticTerminalDraft(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	id := f.mail()
	f.control(map[string]any{"text": "synthetic draft", "hint": true})
	r := f.a.terminalEffects(*f.binding()).ObserveReadiness(context.Background(), "")
	want := tmuxwake.InputAssessment{State: tmuxwake.InputNonempty, Reason: tmuxwake.InputReasonInputNonempty}
	if !r.InputObserved || r.Input != want || r.Eligible() {
		t.Fatalf("fixture did not exercise draft refusal: %+v", r.Input)
	}
	before := f.wait(func(s orphanScreen) bool { return s.Text == "synthetic draft" })
	f.dispatch()
	b := f.binding()
	if b.Outcome != "suppressed: busy, draft or unknown input" || b.Idle != nil || b.WakeDiagnostic != nil || b.SendRefusal != nil || len(b.AttemptIDs) != 0 || len(b.Reservations) != 0 || len(b.Rung) != 0 || b.Typed || f.e.received("alice", id) {
		t.Fatal("draft suppression wrote layout history or changed delivery/mail/rate state")
	}
	after := f.wait(func(s orphanScreen) bool { return true })
	if after.Text != before.Text || after.Typed != before.Typed || after.Erased != before.Erased || len(after.Submitted) != len(before.Submitted) {
		t.Fatal("draft suppression changed terminal input")
	}
}

// A supported loaded-session handover ends diagnostic custody even if the
// pane and consumer generation continue; it preserves existing handover rules.
func TestWakeDiagnosticTerminalHandover(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"unknown": true})
	f.dispatch()
	b := f.binding()
	requireWakeDiagnostic(t, b.WakeDiagnostic, wakediag.TerminalLayoutRefused)
	st, err := f.a.handoverTerminalSession(f.s, b, b.Target, "next-owned-thread")
	if err != nil {
		t.Fatal(err)
	}
	if st.Terminal.WakeDiagnostic != nil || st.Terminal.Generation != b.Generation || st.Terminal.SessionID != "next-owned-thread" {
		t.Fatal("handover retained another session's history or changed generation")
	}
}

// Full dispatch never records a layout diagnosis through manual, foreign or
// retired-generation gates. Input, unread mail and rate remain untouched.
func TestWakeDiagnosticTerminalGates(t *testing.T) {
	for _, mode := range []string{"manual", "foreign", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			f.control(map[string]any{"unknown": true})
			f.update(func(st *config.State) {
				switch mode {
				case "manual":
					st.HooksPaused = true
				case "foreign":
					st.Terminal.Host = "foreign.invalid"
				case "replaced":
					st.Terminal.Generation = "replacement"
				}
			})
			err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
			if mode == "replaced" && (err == nil || !strings.Contains(err.Error(), "binding retired")) {
				t.Fatal("missing intended retired-generation rejection")
			}
			if mode != "replaced" && err != nil {
				t.Fatal(err)
			}
			b := f.binding()
			if b.WakeDiagnostic != nil || b.SendRefusal != nil || len(b.AttemptIDs) != 0 || len(b.Reservations) != 0 || f.e.received("alice", id) {
				t.Fatal("gated dispatch wrote diagnosis or spent authority")
			}
			screen := f.wait(func(s orphanScreen) bool { return true })
			if screen.Typed != 0 || screen.Erased != 0 || len(screen.Submitted) != 0 {
				t.Fatal("gated dispatch changed input")
			}
		})
	}
}

// A default layout reason is not parser evidence. A stale same-generation
// scope/lease or manual gate cannot attach history to its replacement. Draft
// and cursor observations are not layout refusals; valid is the paired control.
// Own-target cases keep the expected scope equal, independently testing local
// host and binding-to-target ownership rather than only scope equality.
func TestWakeDiagnosticTerminalWriteFence(t *testing.T) {
	for _, mode := range []string{"valid", "not-observed", "draft", "cursor", "session", "target", "daemon", "lease", "lease-expired", "lease-host", "own-host", "own-pid", "own-birth", "paused", "manual", "native"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			expected := *f.binding()
			r := tmuxwake.ReadinessAssessment{InputObserved: true, Input: tmuxwake.InputAssessment{State: tmuxwake.InputUnavailable, Reason: tmuxwake.InputReasonLayoutUnrecognized}}
			f.update(func(st *config.State) {
				switch mode {
				case "not-observed":
					r.InputObserved = false
				case "draft":
					r.Input = tmuxwake.InputAssessment{State: tmuxwake.InputNonempty, Reason: tmuxwake.InputReasonInputNonempty}
				case "cursor":
					r.Input = tmuxwake.InputAssessment{State: tmuxwake.InputUnavailable, Reason: tmuxwake.InputReasonCursorMismatch}
				case "session":
					st.Terminal.SessionID = "other"
				case "target":
					st.Terminal.Target.Pane = "%99"
				case "daemon":
					st.Terminal.DaemonBirth = "new-consumer"
				case "lease":
					st.Lease.Holder = "other"
				case "lease-expired":
					st.Lease.Expires = f.now
				case "lease-host":
					st.Lease.Host = "foreign.invalid"
				case "own-host":
					st.Terminal.Host = "foreign.invalid"
					expected.Host = st.Terminal.Host
				case "own-pid":
					st.Terminal.OwnerPID++
					expected.OwnerPID = st.Terminal.OwnerPID
				case "own-birth":
					st.Terminal.OwnerBirth = "other-birth"
					expected.OwnerBirth = st.Terminal.OwnerBirth
				case "paused":
					st.HooksPaused = true
				case "manual":
					st.Liveness = &config.AgentStatus{State: "waiting-human"}
					st.LivenessSource = "manual"
				case "native":
					st.Runtime = &codexwake.Binding{Generation: "native"}
				}
			})
			before := f.e.state("alice")
			if strings.HasPrefix(mode, "own-") && (!sameTerminalScope(*before.Terminal, expected) || ownTerminalTarget(before.Terminal, expected.Target)) {
				t.Fatal("own-target negative did not preserve equal scope with invalid ownership")
			}
			if err := f.a.terminalPreSendSuppressed(f.s, expected.Generation, expected, r); err != nil {
				t.Fatal(err)
			}
			after := f.e.state("alice")
			if mode == "valid" {
				requireWakeDiagnostic(t, after.Terminal.WakeDiagnostic, wakediag.TerminalLayoutRefused)
			} else if after.Terminal.WakeDiagnostic != nil {
				t.Fatalf("%s observation gained layout diagnosis: %+v", mode, after.Terminal.WakeDiagnostic)
			}
			if after.Terminal.Idle != nil || after.Terminal.Outcome != "suppressed: busy, draft or unknown input" || f.e.received("alice", id) {
				t.Fatal("suppression reason, idle invalidation or unread mail changed")
			}
			screen := f.wait(func(s orphanScreen) bool { return true })
			if screen.Typed != 0 || screen.Erased != 0 || len(screen.Submitted) != 0 {
				t.Fatal("suppression wrote terminal input")
			}
			// Only the pre-existing suppression fields and new diagnostic may differ.
			after.Terminal.Idle, before.Terminal.Idle = nil, nil
			after.Terminal.Outcome, before.Terminal.Outcome = "", ""
			after.Terminal.WakeDiagnostic, before.Terminal.WakeDiagnostic = nil, nil
			before.Writer, after.Writer = nil, nil
			oldJSON, _ := json.Marshal(before)
			newJSON, _ := json.Marshal(after)
			if !bytes.Equal(oldJSON, newJSON) {
				t.Fatal("diagnostic changed unrelated state")
			}
		})
	}
}

// Native diagnostics follow actual typed Wake errors only. Definite refusal
// releases the attempt; uncertain transport keeps its fence. Neither receipts
// mail or changes rate semantics; success/no-mail never invents a refusal.
func TestWakeDiagnosticNativeResults(t *testing.T) {
	for _, mode := range []string{"definite", "uncertain", "internal-error", "success", "no-mail"} {
		t.Run(mode, func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			id := ""
			if mode != "no-mail" {
				id = runtimeMail(e, room, "PRIVATE_SYNTHETIC_BODY")
			}
			switch mode {
			case "definite":
				n.wakeErr = &codexwake.RPCError{Method: "turn/start", Code: -32602}
			case "uncertain":
				n.wakeErr = errors.New("PRIVATE_SYNTHETIC_ERROR")
			case "internal-error":
				n.wakeErr = &codexwake.RPCError{Method: "turn/start", Code: -32603}
			}
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			st := e.state("alice")
			switch mode {
			case "definite":
				requireWakeDiagnostic(t, st.Runtime.WakeDiagnostic, wakediag.NativeDefiniteRefusal)
			case "uncertain", "internal-error":
				requireWakeDiagnostic(t, st.Runtime.WakeDiagnostic, wakediag.NativeTransportUncertain)
			default:
				if st.Runtime.WakeDiagnostic != nil {
					t.Fatal("healthy native observation invented failure")
				}
			}
			if id != "" && (e.received("alice", id) || len(st.Wake.Reservations) != 1 || len(n.wakes) != 1) {
				t.Fatal("native diagnostic changed receipt/rate/submit semantics")
			}
			if (mode == "uncertain" || mode == "internal-error") != (st.Wake.Attempt != nil) {
				t.Fatal("native error classification changed attempt fence")
			}
			if st.Runtime.WakeDiagnostic != nil {
				first := *st.Runtime.WakeDiagnostic
				for _, args := range [][]string{{"bind", "status"}, {"bind", "status", "--json"}, {"runtime", "status"}} {
					out := readOnlyWakeStatus(t, e, args...)
					if !strings.Contains(out, string(first.Kind)) || strings.Contains(out, "PRIVATE_SYNTHETIC") {
						t.Fatal("native history missing or private error projected")
					}
				}
				if mode != "definite" {
					e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
					if *e.state("alice").Runtime.WakeDiagnostic != first || len(n.wakes) != 1 {
						t.Fatal("repeat uncertain attempt refreshed diagnostic or retried Wake")
					}
				}
			}
		})
	}
}

// The native observation writer cannot cross changed bindings, identity,
// attempt or manual gates; unchanged own context is the paired positive.
func TestWakeDiagnosticNativeWriteFence(t *testing.T) {
	for _, mode := range []string{"valid", "identity", "generation", "thread", "consumer", "attempt", "retired", "manual", "paused"} {
		t.Run(mode, func(t *testing.T) {
			b := codexwake.Binding{Identity: "alice", Generation: "g", ThreadID: "thread", Gate: "ready"}
			st := config.State{Runtime: &b, Wake: codexwake.Ledger{Attempt: &codexwake.Attempt{ID: "attempt"}}}
			expected := b
			switch mode {
			case "identity":
				b.Identity = "bob"
			case "generation":
				b.Generation = "next"
			case "thread":
				b.ThreadID = "other"
			case "consumer":
				b.ConsumerGeneration = "other"
			case "attempt":
				st.Wake.Attempt.ID = "next"
			case "retired":
				b.Retired = true
			case "paused":
				st.HooksPaused = true
			case "manual":
				st.Liveness = &config.AgentStatus{State: "waiting-human"}
				st.LivenessSource = "manual"
			}
			before, _ := json.Marshal(st)
			recordNativeWakeDiagnostic(&st, "alice", expected, "attempt", wakediag.NativeDefiniteRefusal, time.Unix(1700000000, 0))
			if mode == "valid" {
				requireWakeDiagnostic(t, b.WakeDiagnostic, wakediag.NativeDefiniteRefusal)
			} else if b.WakeDiagnostic != nil {
				t.Fatal("foreign/replaced/manual context gained history")
			}
			b.WakeDiagnostic = nil
			after, _ := json.Marshal(st)
			if !bytes.Equal(before, after) {
				t.Fatal("native diagnostic changed authority or ledger")
			}
		})
	}
}

func readOnlyWakeStatus(t *testing.T, e *env, args ...string) string {
	t.Helper()
	paths := []string{e.paths().StateFile("alice"), e.paths().MailboxFile("alice")}
	before := make([][]byte, len(paths))
	info := make([]os.FileInfo, len(paths))
	for i, p := range paths {
		before[i], _ = os.ReadFile(p)
		info[i], _ = os.Stat(p)
	}
	out := e.as("alice", args...).ok(t).stdout
	for i, p := range paths {
		after, _ := os.ReadFile(p)
		next, _ := os.Stat(p)
		if !bytes.Equal(before[i], after) || (info[i] == nil) != (next == nil) || info[i] != nil && (!os.SameFile(info[i], next) || !info[i].ModTime().Equal(next.ModTime())) {
			t.Fatal("status wrote state, cursor, mailbox or authority")
		}
	}
	return out
}

// A real pre-change JSON fixture remains readable and byte-identical after
// both status forms; missing and malicious optional history never create a
// refusal. A valid closed record is exercised by the dispatch tests above.
func TestWakeDiagnosticLegacyStatus(t *testing.T) {
	for _, mode := range []string{"legacy", "unknown", "malformed", "wrong-route"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			raw, err := os.ReadFile("testdata/wake-diagnostic-legacy.json")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "legacy" {
				var doc map[string]any
				if json.Unmarshal(raw, &doc) != nil {
					t.Fatal("bad legacy fixture")
				}
				diagnostic := any(map[string]any{"kind": "PRIVATE_SYNTHETIC_BODY", "at": "2026-01-01T00:00:00Z"})
				if mode == "malformed" {
					diagnostic = 42
				}
				if mode == "wrong-route" {
					diagnostic = map[string]any{"kind": string(wakediag.NativeDefiniteRefusal), "at": "2026-01-01T00:00:00Z"}
				}
				doc["terminal"].(map[string]any)["wake_diagnostic"] = diagnostic
				raw, _ = json.Marshal(doc)
			}
			if err := os.MkdirAll(filepath.Dir(e.paths().StateFile("alice")), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.paths().StateFile("alice"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"bind", "status"}, {"bind", "status", "--json"}} {
				out := readOnlyWakeStatus(t, e, args...)
				if strings.Contains(out, "wake_diagnostic") || strings.Contains(out, "PRIVATE_SYNTHETIC") {
					t.Fatal("missing/invalid history became refusal or leaked")
				}
			}
		})
	}
}
