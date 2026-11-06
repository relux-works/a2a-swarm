package cli

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func readinessSample(b config.TerminalBinding) tmuxwake.ReadinessAssessment {
	return tmuxwake.ReadinessAssessment{
		Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeReady, Reason: tmuxwake.RuntimeNone},
		Input:   tmuxwake.InputAssessment{State: tmuxwake.InputEmpty, Reason: tmuxwake.InputReasonNone},
		Key:     tmuxwake.ReadinessKey{Route: tmuxwake.LegacyCodex, Target: b.Target, Generation: b.Generation, SessionID: b.SessionID, ComposerRow: 0, ComposerColumn: 2, PaneWidth: 120},
	}
}

// Two identical coherent samples admit only after the policy interval. Each
// key component below independently restarts qualification, including shell
// at the same timestamp. These are state-machine tests, not live observations.
func TestReadinessIdleContinuity(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"codex", "registry", "status", "status-at", "route", "row", "column", "width", "version", "root", "session", "generation", "daemon", "invalid", "too-soon"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			a, s := notificationApp(t, e)
			clock := e.clock()
			a.Now = clock.now
			b := config.TerminalBinding{Generation: "g", SessionID: "session", Target: tmuxwake.Target{Runtime: "codex", OwnerPID: 42, OwnerBirth: "birth", Pane: "%0", SocketID: "socket"}, Interval: time.Second}
			if mode != "codex" && mode != "too-soon" {
				b.Target.Runtime = "claude"
				b.ClaudeRegistryRoot = "/synthetic/root"
			}
			r := readinessSample(b)
			if b.Target.Runtime == "claude" {
				r.Key.Route = tmuxwake.RegistryNamed
				r.Key.ClaudeRegistryRoot = b.ClaudeRegistryRoot
				r.Key.RegistrySessionID = b.SessionID
				r.Key.RegistryVersion = "2.1.310"
				r.Key.RegistryStatus = "idle"
				r.Key.RegistryStatusAt = 100
			}
			notificationState(t, e, func(st *config.State) { copy := b; st.Terminal = &copy })
			if ok, err := a.qualifyTerminalIdle(s, b.Generation, b, r); err != nil || ok {
				t.Fatalf("first sample admitted: %v %v", ok, err)
			}
			b = *e.state("alice").Terminal
			clock.add(time.Second)
			switch mode {
			case "status":
				r.Key.RegistryStatus = "shell"
			case "status-at":
				r.Key.RegistryStatusAt++
			case "route":
				r.Key.Route = tmuxwake.RegistryLegacy
			case "row":
				r.Key.ComposerRow++
			case "column":
				r.Key.ComposerColumn++
			case "width":
				r.Key.PaneWidth++
			case "version":
				r.Key.RegistryVersion = "2.1.309"
			case "root":
				b.ClaudeRegistryRoot += "-next"
				r.Key.ClaudeRegistryRoot = b.ClaudeRegistryRoot
			case "session":
				b.SessionID = "next"
				r.Key.SessionID = b.SessionID
				r.Key.RegistrySessionID = b.SessionID
			case "generation":
				b.Generation = "next"
				r.Key.Generation = b.Generation
			case "daemon":
				b.DaemonBirth = "next"
			case "invalid":
				r.Key = tmuxwake.ReadinessKey{}
			case "too-soon":
				clock.add(-time.Nanosecond)
			}
			notificationState(t, e, func(st *config.State) { copy := b; st.Terminal = &copy })
			ok, err := a.qualifyTerminalIdle(s, b.Generation, b, r)
			want := mode == "codex" || mode == "registry"
			if err != nil || ok != want {
				t.Fatalf("continuity mode=%s got=%v err=%v", mode, ok, err)
			}
			if !want && mode != "invalid" {
				b = *e.state("alice").Terminal
				clock.add(time.Second)
				if ok, err := a.qualifyTerminalIdle(s, b.Generation, b, r); err != nil || !ok {
					t.Fatalf("fresh matching neighbor refused: %v %v", ok, err)
				}
			}
		})
	}
}

// A stale observer cannot overwrite a replacement scope or a newer idle
// sample, including on the refusal path. The unchanged scope is its control.
func TestReadinessIdleCAS(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"valid", "root", "sample", "invalid-root"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			a, s := notificationApp(t, e)
			b := config.TerminalBinding{Generation: "g", SessionID: "s", Target: tmuxwake.Target{Runtime: "codex", OwnerPID: 42, OwnerBirth: "birth", Pane: "%0", SocketID: "sock"}, Interval: time.Second}
			r := readinessSample(b)
			notificationState(t, e, func(st *config.State) {
				copy := b
				st.Terminal = &copy
				if mode == "root" || mode == "invalid-root" {
					st.Terminal.ClaudeRegistryRoot = "/replacement"
				}
				if mode == "sample" {
					st.Terminal.Idle = &config.TerminalIdle{At: a.Now()}
				}
			})
			if mode == "invalid-root" {
				r.Key = tmuxwake.ReadinessKey{}
			}
			before, _ := json.Marshal(e.state("alice"))
			ok, err := a.qualifyTerminalIdle(s, "g", b, r)
			if ok || ((err != nil) != (mode != "valid")) {
				t.Fatalf("CAS result %v %v", ok, err)
			}
			if mode != "valid" {
				after, _ := json.Marshal(e.state("alice"))
				if string(before) != string(after) {
					t.Fatal("stale observer changed state")
				}
			}
		})
	}
}

// Only a valid submitted pair with no transport error completes the channel;
// unknown, inconsistent, draft, uncertain and refused results never credit mail.
func TestReadinessTypedCompletion(t *testing.T) {
	isolateBindTest(t)
	type sample struct {
		name                     string
		r                        tmuxwake.SendResult
		err                      error
		complete, draft, refused bool
	}
	for _, c := range []sample{
		{"submitted", tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil, true, false, false},
		{"zero", tmuxwake.SendResult{}, nil, false, false, false},
		{"unknown", tmuxwake.SendResult{Disposition: "future", Outcome: "private-data"}, nil, false, false, false},
		{"inconsistent", tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendDraftChanged}, nil, false, false, false},
		{"uncertain", tmuxwake.SendResult{Disposition: tmuxwake.SendUncertain, Outcome: tmuxwake.SendEnterUncertain}, nil, false, false, false},
		{"draft", tmuxwake.SendResult{Disposition: tmuxwake.SendDraft, Outcome: tmuxwake.SendDraftChanged}, nil, false, true, false},
		{"refused", tmuxwake.SendResult{Disposition: tmuxwake.SendRefused, Outcome: tmuxwake.SendSuppressedInput}, nil, false, false, true},
		{"submitted-error", tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, errors.New("private-error"), false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, room := world(t)
			clock := e.clock()
			id := runtimeMail(e, room, "retained mail")
			a, s := notificationApp(t, e)
			a.Now = clock.now
			var expected config.TerminalBinding
			notificationState(t, e, func(st *config.State) {
				token, err := st.Notification.Reserve("terminal", "s", "g", clock.now(), false)
				if err != nil {
					t.Fatal(err)
				}
				expected = config.TerminalBinding{Generation: "g", SessionID: "s", NotificationToken: token, AttemptAt: clock.now(), AttemptIDs: []string{id}, Notice: "notice"}
				copy := expected
				st.Terminal = &copy
			})
			if err := a.finishTerminal(s, "g", c.r, c.err, false, expected); err != nil {
				t.Fatal(err)
			}
			st := e.state("alice")
			b := st.Terminal
			if (len(b.Rung) == 1) != c.complete || b.Typed != c.draft || (len(b.AttemptIDs) == 0) != (c.complete || c.refused) || (st.Notification.Reservation == nil) != (c.complete || c.refused) || e.received("alice", id) {
				t.Fatalf("wrong completion effects: %#v %#v", b, st.Notification)
			}
			if strings.Contains(b.Outcome, "private") || (!c.complete && b.Outcome == string(tmuxwake.SendWakeSubmitted)) {
				t.Fatal("unsafe/misleading diagnostic", b.Outcome)
			}
		})
	}
}

// Stable diagnostic pairs retain their original since and cause no file
// replacement. A transition writes only readiness and clears qualification;
// attempts, receipts, notification tokens and reservation history survive.
func TestReadinessDiagnosticPersistence(t *testing.T) {
	isolateBindTest(t)
	e, _ := world(t)
	a, s := notificationApp(t, e)
	clock := e.clock()
	a.Now = clock.now
	b := config.TerminalBinding{Generation: "g", SessionID: "s", AttemptIDs: []string{"mail"}, NotificationToken: "token", Reservations: []time.Time{clock.now()}, Readiness: &config.TerminalReadiness{Reason: tmuxwake.RuntimeBusy, Version: "2.1.310", Since: clock.now()}, Idle: &config.TerminalIdle{At: clock.now()}}
	notificationState(t, e, func(st *config.State) { copy := b; st.Terminal = &copy })
	b = *e.state("alice").Terminal
	// A successful no-op must not even enter the state writer (read-only paths).
	before, _ := json.Marshal(e.state("alice"))
	info, err := os.Stat(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	r := tmuxwake.ReadinessAssessment{Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeRefused, Reason: tmuxwake.RuntimeBusy, Version: "2.1.310"}}
	clock.add(time.Second)
	if err := a.observeTerminalReadiness(s, b, r); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(e.state("alice"))
	if !os.SameFile(info, afterInfo) || string(before) != string(after) {
		t.Fatal("stable diagnostic pair caused write")
	}
	r.Runtime.Reason = tmuxwake.RuntimeHeadless
	if err := a.observeTerminalReadiness(s, b, r); err != nil {
		t.Fatal(err)
	}
	got := e.state("alice").Terminal
	if got.Readiness.Reason != tmuxwake.RuntimeHeadless || !got.Readiness.Since.Equal(clock.now()) || got.Idle != nil {
		t.Fatal("transition not recorded", got)
	}
	got.Readiness = b.Readiness
	got.Idle = b.Idle
	if !reflect.DeepEqual(*got, b) {
		t.Fatal("diagnostic transition changed unrelated journal")
	}
}

// A same-pair invalid timestamp is repaired at the first consumer observation,
// once. Later stable observations do no write; stale replacement scopes cannot
// receive that repair. Malformed JSON is exercised through the real decoder.
func TestReadinessDiagnosticSinceRepair(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"zero", "future", "malformed", "stale", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			a, s := notificationApp(t, e)
			clock := e.clock()
			a.Now = clock.now
			old := &config.TerminalReadiness{Reason: tmuxwake.RuntimeBusy, Version: "2.1.310"}
			if mode == "future" {
				old.Since = clock.now().Add(time.Hour)
			}
			if mode == "malformed" {
				if err := json.Unmarshal([]byte(`{"reason":"busy","version":"2.1.310","since":"PRIVATE_INVALID_TIME"}`), old); err != nil {
					t.Fatal(err)
				}
			}
			b := config.TerminalBinding{Generation: "g", SessionID: "s", Readiness: old, NotificationToken: "token", AttemptIDs: []string{"mail"}}
			notificationState(t, e, func(st *config.State) {
				copy := b
				st.Terminal = &copy
				if mode == "concurrent" {
					st.Terminal.Readiness = &config.TerminalReadiness{Reason: old.Reason, Version: old.Version, Since: clock.now().Add(-time.Minute)}
				}
				if mode == "stale" {
					st.Terminal.SessionID = "new"
				}
			})
			r := tmuxwake.ReadinessAssessment{Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeRefused, Reason: tmuxwake.RuntimeBusy, Version: "2.1.310"}}
			before, _ := json.Marshal(e.state("alice"))
			err := a.observeTerminalReadiness(s, b, r)
			if mode == "stale" {
				after, _ := json.Marshal(e.state("alice"))
				if err == nil || string(before) != string(after) {
					t.Fatal("stale diagnostic repair wrote")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b = *e.state("alice").Terminal
			wantSince := clock.now()
			if mode == "concurrent" {
				wantSince = wantSince.Add(-time.Minute)
			}
			if !b.Readiness.Since.Equal(wantSince) || b.NotificationToken != "token" || len(b.AttemptIDs) != 1 {
				t.Fatal("invalid since not repaired at first observation", b.Readiness)
			}
			first := b.Readiness.Since
			info, err := os.Stat(s.paths.StateFile(s.name))
			if err != nil {
				t.Fatal(err)
			}
			clock.add(time.Second)
			if err := a.observeTerminalReadiness(s, b, r); err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(s.paths.StateFile(s.name))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(info, afterInfo) || !e.state("alice").Terminal.Readiness.Since.Equal(first) {
				t.Fatal("stable repaired since rewritten")
			}
		})
	}
}

// Unknown reasons and hostile version strings project to bounded enums;
// malformed/future stored timestamps never supply a status since or permission.
func TestReadinessStatusProjection(t *testing.T) {
	isolateBindTest(t)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"valid", "unknown", "inconsistent", "version", "future", "zero", "old-shell"} {
		t.Run(mode, func(t *testing.T) {
			r := tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeReady, Reason: tmuxwake.RuntimeNone, Version: "2.1.310"}
			old := &config.TerminalReadiness{Reason: r.Reason, Version: r.Version, Since: now.Add(-time.Hour)}
			switch mode {
			case "unknown":
				r.Reason = "secret\nreason"
			case "inconsistent":
				r.State = tmuxwake.RuntimeRefused
			case "version":
				r.Version = "/private/root"
			case "future":
				old.Since = now.Add(time.Second)
			case "zero":
				old.Since = time.Time{}
			case "old-shell":
				old.Reason = "shell"
			}
			got := readinessStatus(r, old, now)
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") {
				t.Fatal("raw diagnostic leaked")
			}
			if (mode == "unknown" || mode == "inconsistent") && (got.State != tmuxwake.RuntimeUnavailable || got.Reason != tmuxwake.RuntimeRegistryUnavailable) {
				t.Fatal(got)
			}
			want := now
			if mode == "valid" {
				want = old.Since
			}
			if !got.Since.Equal(want) {
				t.Fatal("wrong since", got)
			}
		})
	}
}

// Root association is scope-CAS guarded and invalidates only readiness. A
// session handover drops the previous provider root before any trusted rebind.
func TestReadinessRootAssociationCAS(t *testing.T) {
	isolateBindTest(t)
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "stale"}[stale], func(t *testing.T) {
			e, _ := world(t)
			a, s := notificationApp(t, e)
			b := config.TerminalBinding{Generation: "g", SessionID: "s", ClaudeRegistryRoot: "/old", AttemptIDs: []string{"mail"}, NotificationToken: "token", Idle: &config.TerminalIdle{}, Readiness: &config.TerminalReadiness{Reason: tmuxwake.RuntimeBusy}}
			notificationState(t, e, func(st *config.State) {
				copy := b
				st.Terminal = &copy
				if stale {
					st.Terminal.SessionID = "replacement"
				}
			})
			before, _ := json.Marshal(e.state("alice"))
			err := a.associateTerminalRoot(s, b, "/new")
			if (err != nil) != stale {
				t.Fatal(err)
			}
			got := e.state("alice")
			if stale {
				after, _ := json.Marshal(got)
				if string(before) != string(after) {
					t.Fatal("stale root association wrote")
				}
				return
			}
			if got.Terminal.ClaudeRegistryRoot != "/new" || got.Terminal.Idle != nil || got.Terminal.Readiness != nil || len(got.Terminal.AttemptIDs) != 1 || got.Terminal.NotificationToken != "token" {
				t.Fatal("association lost journal")
			}
		})
	}
}

// Name-only drift changes are persisted once, retain the reason/version since,
// and leave the binding/journal intact. Repeating the observation is a no-op.
func TestReadinessUnknownKeysPersistence(t *testing.T) {
	isolateBindTest(t)
	e, _ := world(t)
	a, s := notificationApp(t, e)
	now := a.Now()
	b := config.TerminalBinding{Generation: "g", SessionID: "s", NotificationToken: "token", Readiness: &config.TerminalReadiness{Reason: tmuxwake.RuntimeBusy, Version: "3.0.0", Since: now.Add(-time.Hour)}}
	notificationState(t, e, func(st *config.State) { copy := b; st.Terminal = &copy })
	r := tmuxwake.ReadinessAssessment{Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeRefused, Reason: tmuxwake.RuntimeBusy, Version: "3.0.0", UnknownKeys: "future"}}
	if err := a.observeTerminalReadiness(s, b, r); err != nil {
		t.Fatal(err)
	}
	got := e.state("alice").Terminal
	if got.Readiness.UnknownKeys != "future" || !got.Readiness.Since.Equal(b.Readiness.Since) || got.NotificationToken != "token" {
		t.Fatal("drift not persisted safely", got)
	}
	info, err := os.Stat(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.observeTerminalReadiness(s, *got, r); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(s.paths.StateFile(s.name))
	if err != nil || !os.SameFile(info, after) {
		t.Fatal("stable drift caused write", err)
	}
}
