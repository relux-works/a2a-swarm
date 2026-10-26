package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const bindDiagnosticMarker = "SYNTHETIC_SECRET_SCREEN_PATH_SENDER_BODY"

// Typed facts classify without formatting arbitrary errors. An outer fallback
// cannot replace an already observed stage; malformed persisted codes stay unknown.
func TestBindDiagnosticPrivacy(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, class string
		err         error
	}{
		{"generic", "unavailable", errors.New(bindDiagnosticMarker)},
		{"deadline", "timeout", fmt.Errorf("%s: %w", bindDiagnosticMarker, context.DeadlineExceeded)},
		{"lock", "locked", fmt.Errorf("%s: %w", bindDiagnosticMarker, config.ErrLockTimeout)},
		{"permission", "permission", &os.PathError{Op: bindDiagnosticMarker, Path: bindDiagnosticMarker, Err: os.ErrPermission}},
		{"lease", "lease_conflict", &config.LeaseHeldError{Name: bindDiagnosticMarker, Lease: config.Lease{Holder: bindDiagnosticMarker}}},
		{"auth", "authentication", fmt.Errorf("%s: %w", bindDiagnosticMarker, matrix.ErrAuth)},
		{"remote", "remote", &matrix.Error{Status: 503, ErrCode: bindDiagnosticMarker, Message: bindDiagnosticMarker, Raw: json.RawMessage(`"` + bindDiagnosticMarker + `"`)}},
		{"json", "invalid", &json.UnmarshalTypeError{Value: bindDiagnosticMarker}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := terminalAt("dispatch", terminalAt("delivery_sync", tc.err))
			d := terminalErrorDiagnostic("state", err, now)
			if d.Phase != "delivery_sync" || d.Class != tc.class || d.Result != "error" || !d.At.Equal(now) {
				t.Fatal(d)
			}
			b, _ := json.Marshal(d)
			if strings.Contains(string(b)+err.Error(), bindDiagnosticMarker) {
				t.Fatal("unsafe error projection")
			}
		})
	}
	invalid := config.TerminalDiagnostic{Phase: bindDiagnosticMarker, Class: bindDiagnosticMarker, Result: bindDiagnosticMarker, At: &now}
	if got := invalid.Safe(); got.Phase != "unknown" || got.Class != "invalid" || got.At != nil {
		t.Fatal(got)
	}
}

func bindDiagnosticEnv(t *testing.T) (*env, *App, *session) {
	t.Helper()
	e := newEnv(t)
	e.agent("alice")
	e.now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: e.now, HTTP: e.hs.srv.Client(), Stdout: io.Discard, Stderr: io.Discard}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	if err = config.SaveState(e.paths(), "alice", config.State{Terminal: &config.TerminalBinding{Generation: "diagnostic-generation", Interval: time.Second, RetryMax: 2 * time.Second}}); err != nil {
		t.Fatal(err)
	}
	return e, a, s
}

// The real retry/publication handlers cannot hide the failed delivery phase.
// Unknown send outcomes/errors retain the ambiguous attempt, never claim a send.
func TestBindDiagnosticRetention(t *testing.T) {
	e, a, s := bindDiagnosticEnv(t)
	var log bytes.Buffer
	a.Stderr = &log
	a.recordTerminalFailure(s, "diagnostic-generation", "delivery_sync", errors.New(bindDiagnosticMarker))
	a.unknownTerminalNotice(context.Background(), s, "diagnostic-generation", true)
	b := e.state("alice").Terminal
	if b.Diagnostic == nil || b.Diagnostic.Phase != "delivery_sync" || b.Diagnostic.Result != "error" {
		t.Fatal(b.Diagnostic)
	}
	if strings.Contains(log.String(), bindDiagnosticMarker) || !strings.Contains(log.String(), "phase=delivery_sync class=unavailable") {
		t.Fatal(log.String())
	}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.AttemptIDs = []string{"one"}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finishTerminal(s, "diagnostic-generation", bindDiagnosticMarker, errors.New(bindDiagnosticMarker), false); err != nil {
		t.Fatal(err)
	}
	b = e.state("alice").Terminal
	if b.Diagnostic.Result != "uncertain" || len(b.Rung) != 0 || len(b.AttemptIDs) != 1 || strings.Contains(b.Outcome, bindDiagnosticMarker) {
		t.Fatal(b)
	}
	if err = a.finishTerminal(s, "diagnostic-generation", "wake submitted", nil, false); err != nil {
		t.Fatal(err)
	}
	b = e.state("alice").Terminal
	if b.Diagnostic.Result != "submitted" || len(b.Rung) != 1 || len(b.AttemptIDs) != 0 {
		t.Fatal(b)
	}
}

func bindPendingAppend(t *testing.T, e *env, entries ...config.MailEntry) {
	t.Helper()
	_, err := config.UpdateState(e.paths(), "alice", func(*config.State) error {
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		_, err = mb.Append(entries)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
func bindPendingReceive(t *testing.T, e *env, ids ...string) {
	t.Helper()
	_, err := config.UpdateState(e.paths(), "alice", func(*config.State) error {
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		_, err = mb.Receive(ids)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
func bindPendingStatus(t *testing.T, e *env) terminalPending {
	t.Helper()
	state := readFile(t, e.paths().StateFile("alice"))
	mail := readFile(t, e.paths().MailboxFile("alice"))
	syncs := e.hs.CallCount("GET /_matrix/client/v3/sync")
	out := e.as("alice", "bind", "status", "--json").ok(t).stdout
	var status struct {
		Pending    terminalPending
		Diagnostic config.TerminalDiagnostic
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if readFile(t, e.paths().StateFile("alice")) != state || readFile(t, e.paths().MailboxFile("alice")) != mail {
		t.Fatal("status mutated authority")
	}
	if e.hs.CallCount("GET /_matrix/client/v3/sync") != syncs {
		t.Fatal("status synced")
	}
	if strings.Contains(out, bindDiagnosticMarker) {
		t.Fatal("status leaked event data")
	}
	return status.Pending
}

// Zero-attempt mail is visible from durable origin times, even unbound. New
// App invocations/unrelated attempts never reset age; only matching receipts
// remove entries. Origin-time minimum differs deliberately from append order.
func TestBindPendingVisibility(t *testing.T) {
	e, _, _ := bindDiagnosticEnv(t)
	now := e.now()
	bindPendingAppend(t, e, config.MailEntry{ID: "c", Kind: config.MailEvent, TS: now.Add(-time.Hour).UnixMilli(), Sender: bindDiagnosticMarker}, config.MailEntry{ID: "a", Kind: config.MailEvent, TS: now.Add(-3 * time.Hour).UnixMilli(), Event: json.RawMessage(`{"body":"` + bindDiagnosticMarker + `"}`)}, config.MailEntry{ID: "b", Kind: config.MailEvent, TS: now.Add(-2 * time.Hour).UnixMilli()})
	check := func(count int, age time.Duration) {
		t.Helper()
		p := bindPendingStatus(t, e)
		if p.Count == nil || *p.Count != count || p.AgeState != "known" || p.OldestKnownEventAgeMillis == nil || *p.OldestKnownEventAgeMillis != age.Milliseconds() {
			t.Fatal(p)
		}
	}
	check(3, 3*time.Hour)
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal.AttemptAt = now
		st.Terminal.Rung = []string{"unrelated"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return now.Add(time.Minute) }
	check(3, 3*time.Hour+time.Minute)
	bindPendingReceive(t, e, "b")
	check(2, 3*time.Hour+time.Minute)
	bindPendingReceive(t, e, "a")
	check(1, time.Hour+time.Minute)
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	check(1, time.Hour+time.Minute)
	text := e.as("alice", "bind", "status").ok(t).stdout
	if !strings.Contains(text, "pending=1 origin-age=known") || !strings.Contains(text, "unbound") {
		t.Fatal(text)
	}
	bindPendingReceive(t, e, "c")
	p := bindPendingStatus(t, e)
	if p.AgeState != "empty" || p.OldestKnownEventAgeMillis != nil || p.Count == nil || *p.Count != 0 {
		t.Fatal(p)
	}
}

// Unknown/malformed/future timestamps never become a healthy zero age. Valid
// metadata alongside an invalid entry is explicitly only the oldest KNOWN time.
func TestBindPendingMetadataLimits(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		ts                       int64
		state                    string
		missing, invalid, future int
	}{
		{"missing", 0, "unknown", 1, 0, 0}, {"negative", -1, "invalid", 0, 1, 0}, {"future", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), "invalid", 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := bindDiagnosticEnv(t)
			bindPendingAppend(t, e, config.MailEntry{ID: "bad", TS: tc.ts})
			p := bindPendingStatus(t, e)
			if p.AgeState != tc.state || p.OldestKnownEventAgeMillis != nil || p.MissingTimestamps != tc.missing || p.InvalidTimestamps != tc.invalid || p.FutureTimestamps != tc.future {
				t.Fatal(p)
			}
			bindPendingAppend(t, e, config.MailEntry{ID: "valid", TS: e.now().Add(-time.Hour).UnixMilli()})
			p = bindPendingStatus(t, e)
			if p.AgeState != tc.state || p.OldestKnownEventAgeMillis == nil || *p.OldestKnownEventAgeMillis != time.Hour.Milliseconds() {
				t.Fatal(p)
			}
		})
	}
	t.Run("malformed-json", func(t *testing.T) {
		e, _, _ := bindDiagnosticEnv(t)
		if err := os.WriteFile(e.paths().MailboxFile("alice"), []byte(`{"op":"add","entry":{"id":"bad","ts":"`+bindDiagnosticMarker+`"}}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		p := bindPendingStatus(t, e)
		if p.State != "unavailable" || p.Count != nil || p.AgeState != "unknown" || p.OldestKnownEventAgeMillis != nil {
			t.Fatal(p)
		}
	})
}

// Legacy absent diagnostics are unknown, not healthy; hostile unknown enum
// values and old outcome text cannot appear in the safe text/JSON projections.
func TestBindDiagnosticLegacyStatus(t *testing.T) {
	e, _, _ := bindDiagnosticEnv(t)
	read := func() config.TerminalDiagnostic {
		t.Helper()
		out := e.as("alice", "bind", "status", "--json").ok(t).stdout
		var status struct{ Diagnostic config.TerminalDiagnostic }
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, bindDiagnosticMarker) {
			t.Fatal(out)
		}
		return status.Diagnostic
	}
	if d := read(); d.Result != "unknown" || d.At != nil {
		t.Fatal(d)
	}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal.Diagnostic = &config.TerminalDiagnostic{Phase: bindDiagnosticMarker, Class: bindDiagnosticMarker, Result: bindDiagnosticMarker}
		st.Terminal.Outcome = bindDiagnosticMarker
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := read(); d.Class != "invalid" || d.Result != "unknown" {
		t.Fatal(d)
	}
	if text := e.as("alice", "bind", "status").ok(t).stdout; strings.Contains(text, bindDiagnosticMarker) || !strings.Contains(text, "unknown outcome") {
		t.Fatal(text)
	}
}

type bindCancelLog struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *bindCancelLog) Write(b []byte) (int, error) {
	n, err := w.Buffer.Write(b)
	if bytes.Contains(b, []byte("bounded retry")) {
		w.cancel()
	}
	return n, err
}

// Exercise actual source stages with isolated HTTP/filesystem/connection faults.
// Fixed logs survive unavailable state; no stage error types or receipts mail.
// The existing real target fixture is synthetic, not a provider/runtime session.
func TestBindConsumerDiagnosticStages(t *testing.T) {
	for _, phase := range []string{"connection", "state", "authority", "delivery_start", "delivery_position", "delivery_sync"} {
		t.Run(phase, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			initial := *f.binding()
			statePath := f.e.paths().StateFile("alice")
			saved := readFile(t, statePath)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			log := &bindCancelLog{cancel: cancel}
			f.a.Stderr = log
			class := "remote"
			switch phase {
			case "connection":
				class = "unavailable"
				f.s.checkBinding = func() error { return errors.New(bindDiagnosticMarker) }
			case "state":
				class = "invalid"
				if err := os.WriteFile(statePath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "authority":
				class = "lease_conflict"
				f.update(func(st *config.State) {
					st.Lease = &config.Lease{Holder: "foreign", Host: machineLabel(), Expires: f.now.Add(time.Hour)}
				})
			case "delivery_start":
				f.e.hs.fail("GET /_matrix/client/v3/user/{user}/account_data/{type}", 503, "M_UNKNOWN", bindDiagnosticMarker)
			case "delivery_position":
				f.update(func(st *config.State) { st.NextBatch = "" })
				f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", bindDiagnosticMarker)
			case "delivery_sync":
				f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", bindDiagnosticMarker)
			}
			_, runErr := f.a.runTerminal(ctx, f.s, "orphan-gen", initial)
			if phase == "connection" {
				if runErr == nil {
					t.Fatal("connection failure accepted")
				}
			} else if runErr != nil {
				t.Fatal(runErr)
			}
			if !strings.Contains(log.String(), "phase="+phase+" class="+class) || strings.Contains(log.String(), bindDiagnosticMarker) {
				t.Fatal(log.String())
			}
			if phase == "state" {
				if err := os.WriteFile(statePath, []byte(saved), 0600); err != nil {
					t.Fatal(err)
				}
			} else if phase != "connection" {
				d := f.binding().Diagnostic
				if d == nil || d.Phase != phase || d.Class != class {
					t.Fatal(d)
				}
			}
			if f.e.received("alice", id) || len(f.e.mailbox("alice").Pending()) != 1 {
				t.Fatal("failure lost pending mail")
			}
			if screen := f.wait(func(orphanScreen) bool { return true }); len(screen.Submitted) != 0 || screen.Erased != 0 || screen.Text != "" {
				t.Fatal("failure touched target")
			}
		})
	}
}

// Suppression is a successful conservative observation, distinct from errors or
// sends. A secret-like draft/unknown screen remains untouched; repaired input
// still requires both idle samples before an actual submission, without receipt.
func TestBindDiagnosticQualification(t *testing.T) {
	for _, mode := range []string{"draft", "unknown", "pause", "manual", "rate"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			class := "input_unqualified"
			phase := "qualification"
			switch mode {
			case "draft":
				f.control(map[string]any{"text": bindDiagnosticMarker})
			case "unknown":
				f.control(map[string]any{"unknown": true, "text": bindDiagnosticMarker})
			case "pause":
				class = "hook_pause"
				phase = "authority"
				f.update(func(st *config.State) { st.HooksPaused = true })
			case "manual":
				class = "manual_gate"
				phase = "authority"
				f.update(func(st *config.State) {
					st.Liveness = &config.AgentStatus{State: "waiting-human"}
					st.LivenessSource = "manual"
				})
			case "rate":
				class = "hourly_rate"
				phase = "authority"
				f.update(func(st *config.State) { st.Terminal.MaxWakes = 1; st.Terminal.Reservations = []time.Time{f.now} })
			}
			f.dispatch()
			f.step()
			b := f.binding()
			if b.Diagnostic == nil || b.Diagnostic.Phase != phase || b.Diagnostic.Class != class || b.Diagnostic.Result != "suppressed" {
				t.Fatal(b.Diagnostic)
			}
			data, _ := json.Marshal(b.Diagnostic)
			if strings.Contains(string(data), bindDiagnosticMarker) {
				t.Fatal("screen leaked")
			}
			before := f.wait(func(orphanScreen) bool { return true })
			if len(before.Submitted) != 0 || before.Erased != 0 {
				t.Fatal("suppression sent")
			}
			f.control(map[string]any{"text": ""})
			f.update(func(st *config.State) { st.HooksPaused = false; st.Liveness = nil; st.Terminal.MaxWakes = 30 })
			f.step()
			if f.binding().Diagnostic.Result != "qualifying" {
				t.Fatal(f.binding().Diagnostic)
			}
			f.step()
			b = f.binding()
			if b.Diagnostic.Result != "submitted" || !reflect.DeepEqual(b.Rung, []string{id}) || f.e.received("alice", id) {
				t.Fatal(b.Diagnostic)
			}
		})
	}
}

// A transport may only claim submission with its exact successful outcome.
// Unknown/ambiguous outcomes, even without an error, keep the durable fence;
// suppressed draft outcomes remain distinct from both failure and submission.
func TestBindDiagnosticSendOutcomes(t *testing.T) {
	for _, outcome := range []string{bindDiagnosticMarker, "unbound", "uncertain: type", "notice left in draft: changed input", "suppressed: target or width", "wake submitted"} {
		t.Run(outcome, func(t *testing.T) {
			e, a, s := bindDiagnosticEnv(t)
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal.AttemptIDs = []string{"pending"}
				st.Terminal.Notice = "fixed"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = a.finishTerminal(s, "diagnostic-generation", outcome, nil, false); err != nil {
				t.Fatal(err)
			}
			b := e.state("alice").Terminal
			switch outcome {
			case "wake submitted":
				if b.Diagnostic.Result != "submitted" || len(b.Rung) != 1 || len(b.AttemptIDs) != 0 {
					t.Fatal(b)
				}
			case "notice left in draft: changed input":
				if b.Diagnostic.Result != "suppressed" || !b.Typed || len(b.AttemptIDs) != 1 || len(b.Rung) != 0 {
					t.Fatal(b)
				}
			case "suppressed: target or width":
				if b.Diagnostic.Result != "suppressed" || b.Diagnostic.Class != "target_or_width" || len(b.Rung) != 0 {
					t.Fatal(b)
				}
			default:
				if b.Diagnostic.Result != "uncertain" || len(b.Rung) != 0 || len(b.AttemptIDs) != 1 || strings.Contains(b.Outcome, bindDiagnosticMarker) {
					t.Fatal(b)
				}
			}
		})
	}
}
