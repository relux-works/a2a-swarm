package cli

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Recovery geometry is constrained by the filled record, never the earlier
// empty row/column. Target/session/route/version/width remain refusal boundaries.
func TestNoticeOriginNeighbors(t *testing.T) {
	for _, runtime := range []string{"codex", "claude", "claude-named"} {
		for _, axis := range []string{"row", "column", "both"} {
			for _, mode := range []string{"recorded", "no-record", "moved-again", "origin-row", "origin-column", "width", "version", "target", "generation", "session", "unrecorded"} {
				t.Run(runtime+"/"+axis+"/"+mode, func(t *testing.T) {
					k := tmuxwake.ReadinessKey{Generation: "g", SessionID: "s", Target: tmuxwake.Target{Runtime: "codex"}, Route: tmuxwake.LegacyCodex, ComposerRow: 19, ComposerColumn: 2, PaneWidth: 120}
					if runtime != "codex" {
						k.Target.Runtime = "claude"
						k.Route = tmuxwake.RegistryLegacy
						if runtime == "claude-named" {
							k.Route = tmuxwake.RegistryNamed
						}
						k.RegistryVersion = "2.1.310"
						k.RegistrySessionID = "s"
					}
					b := config.TerminalBinding{Generation: "g", SessionID: "s", Target: k.Target, Host: "synthetic", NoticeOrigin: &config.TerminalIdle{Generation: "g", SessionID: "s", Host: "synthetic", Target: k.Target, ReadinessRoute: k.Route, RegistryVersion: k.RegistryVersion, RegistrySessionID: k.RegistrySessionID, ComposerRow: 19, ComposerColumn: 2, PaneWidth: 120}}
					if axis != "column" {
						k.ComposerRow = 16
					}
					if axis != "row" {
						k.ComposerColumn = 5
					}
					b.NoticePosition = &tmuxwake.ComposerPosition{Row: k.ComposerRow, Column: k.ComposerColumn}
					switch mode {
					case "no-record":
						b.NoticePosition = nil // Parsing must independently anchor this; F2 retains the Codex E2E refusal.
					case "moved-again":
						k.ComposerRow--
						k.ComposerColumn++
					case "origin-row":
						b.NoticeOrigin.ComposerRow--
					case "origin-column":
						b.NoticeOrigin.ComposerColumn++
					case "width":
						k.PaneWidth++
					case "version":
						k.RegistryVersion = "other"
					case "target":
						k.Target.SocketID = "other"
					case "generation":
						k.Generation = "other"
					case "session":
						k.SessionID = "other"
					case "unrecorded":
						b.NoticeOrigin = nil
					}
					want := mode == "recorded" || mode == "no-record" || mode == "origin-row" || mode == "origin-column"
					if sameNoticeOrigin(b, k) != want {
						t.Fatal("wrong origin constraint")
					}
				})
			}
		}
	}
}

// The canceled probe makes any attempted process inspection observable without
// launching ps or tmux. Valid policy reaches it; each single-gate refusal must
// not. This proves early policy short-circuiting, not successful Enter (covered
// by TestTerminalOwnedNoticeRecovery/valid). A dead target cannot mask a bypass.
type recoveryInspectionProbe struct {
	context.Context
	calls atomic.Int32
}

func (p *recoveryInspectionProbe) Err() error {
	p.calls.Add(1)
	return p.Context.Err()
}

// Early policy refusals preserve the journal except the explicit held outcome.
// A distinct in-hour reservation exhausts the cap; the attempt's own reservation
// (including duplicate timestamps) is excluded by the documented budget rule.
func TestNoticeRecoveryPolicyHeld(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"valid", "not-typed", "retry-cap", "retry-delay", "cooldown", "budget", "invalid-count", "zero-count", "missing-token", "interval", "retry-short", "retry-long"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			now := a.Now()
			b := *e.state("alice").Terminal
			b.Notice = tmuxwake.Notice(1)
			b.NoticeCount = 1
			b.NotificationToken = "synthetic-token"
			b.Target.OwnerPID = os.Getpid() // Inspect consults the canceled context before starting ps.
			b.TypingStarted = true
			b.AttemptAt = now.Add(-30 * time.Second)
			b.MaxWakes = 1
			b.Reservations = []time.Time{b.AttemptAt}
			switch mode {
			case "missing-token":
				b.NotificationToken = ""
			case "interval":
				b.Interval = 0
			case "retry-short":
				b.RetryMax = 0
			case "retry-long":
				b.RetryMax = 6 * time.Minute
				b.AttemptAt = now.Add(-7 * time.Minute) // Isolate validation from the later retry-delay gate.
				b.Reservations = []time.Time{b.AttemptAt}
			case "not-typed":
				b.TypingStarted = false
			case "retry-cap":
				b.Retried = true
			case "retry-delay":
				b.RetryMax = time.Minute
			case "cooldown":
				b.AttemptAt = now.Add(-time.Second)
				b.Reservations = []time.Time{b.AttemptAt}
			case "budget":
				b.Reservations = append(b.Reservations, b.AttemptAt.Add(time.Second))
			case "zero-count":
				b.NoticeCount = 0
				b.Notice = tmuxwake.Notice(0)
			case "invalid-count":
				b.NoticeCount = 2
			}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = &b; return nil })
			if err != nil {
				t.Fatal(err)
			}
			before := *e.state("alice").Terminal
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			probe := &recoveryInspectionProbe{Context: canceled}
			if err := a.resumeTerminalNotice(probe, s, "gen", before); err != nil {
				t.Fatal(err)
			}
			after := *e.state("alice").Terminal
			if (probe.calls.Load() > 0) != (mode == "valid") {
				t.Fatal("policy inspection boundary bypassed", mode, probe.calls.Load())
			}
			before.Idle = nil
			before.Outcome = "suppressed: owned notice recovery held"
			if !reflect.DeepEqual(before, after) {
				t.Fatal("held policy changed journal")
			}
		})
	}
	b := config.TerminalBinding{MaxWakes: 1, AttemptAt: time.Unix(100, 0), Reservations: []time.Time{time.Unix(100, 0), time.Unix(100, 0)}}
	if !noticeRecoveryBudget(b, time.Unix(200, 0)) {
		t.Fatal("own reservation counted as another wake")
	}
	b.Reservations = append(b.Reservations, time.Unix(101, 0))
	if noticeRecoveryBudget(b, time.Unix(200, 0)) {
		t.Fatal("distinct reservation ignored")
	}
}

// The real Enter journal fence refuses changed authority without spending retry
// or notification state. A valid neighbor reserves exactly once, with no tmux.
func TestNoticeRecoveryFenceAuthority(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"valid", "paused", "manual", "stopped", "generation", "session", "target", "lease", "budget", "route", "version", "position-row", "position-column"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			now := a.Now()
			b := *e.state("alice").Terminal
			b.SessionID = "s"
			b.Host = machineLabel()
			b.OwnerPID = 123
			b.OwnerBirth = "synthetic-birth"
			b.Target = tmuxwake.Target{Runtime: "codex", OwnerPID: b.OwnerPID, OwnerBirth: b.OwnerBirth, SocketID: "synthetic", Pane: "%0"}
			b.Notice = tmuxwake.Notice(1)
			b.NoticeCount = 1
			b.TypingStarted = true
			b.AttemptAt = now.Add(-time.Minute)
			b.MaxWakes = 1
			b.Reservations = []time.Time{b.AttemptAt}
			k := tmuxwake.ReadinessKey{Generation: b.Generation, SessionID: b.SessionID, Target: b.Target, Route: tmuxwake.LegacyCodex, PaneWidth: 120, ComposerRow: 19, ComposerColumn: 2}
			b.NoticeOrigin = &config.TerminalIdle{Generation: b.Generation, SessionID: b.SessionID, Host: b.Host, Target: b.Target, ReadinessRoute: k.Route, PaneWidth: 120}
			b.NoticePosition = &tmuxwake.ComposerPosition{Row: 19, Column: 2}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &b
				st.Lease = &config.Lease{Holder: b.SessionID, Host: b.Host, Expires: now.Add(time.Hour)}
				token, err := st.Notification.Reserve("terminal", b.SessionID, b.Generation, b.AttemptAt, false)
				b.NotificationToken = token
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			expected := *e.state("alice").Terminal
			_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				c := st.Terminal
				switch mode {
				case "paused":
					st.HooksPaused = true
				case "manual":
					st.Liveness = &config.AgentStatus{State: "waiting-human"}
				case "stopped":
					c.Stopped = true
				case "generation":
					c.Generation = "other"
				case "session":
					c.SessionID = "other"
				case "target":
					c.Target.SocketID = "other"
				case "lease":
					st.Lease.Holder = "other"
				case "budget":
					c.Reservations = append(c.Reservations, b.AttemptAt.Add(time.Second))
				case "route":
					k.Route = tmuxwake.RegistryNamed
				case "version":
					k.RegistryVersion = "other"
				case "position-row":
					k.ComposerRow--
				case "position-column":
					k.ComposerColumn++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before := e.state("alice")
			// Retry only a proven pre-write fence timeout, preserving the production
			// deadline and requiring unchanged state before another attempt.
			for attempt := 0; attempt < 3; attempt++ {
				_, err = a.reserveTerminalRecovery(context.Background(), s, "gen", expected, k)
				if !errors.Is(err, tmuxwake.ErrEnterFenceTimeout) {
					break
				}
				if !reflect.DeepEqual(before, e.state("alice")) {
					t.Fatal("timed-out fence changed state")
				}
			}
			after := e.state("alice")
			if mode == "valid" {
				if err != nil || !after.Terminal.Retried {
					t.Fatal("valid fence refused", err)
				}
				return
			}
			wantError := "terminal recovery attempt changed"
			if mode == "paused" || mode == "manual" || mode == "lease" || mode == "budget" {
				wantError = "terminal recovery held"
			}
			if err == nil || err.Error() != wantError || !reflect.DeepEqual(before, after) {
				t.Fatal("authority refusal changed state or reason", err)
			}
		})
	}
}

// These legacy detection neighbors deliberately have no pending-origin path:
// it would mask changes to the separate Typed/count/ID eligibility expression.
// Retained pending-origin behavior remains in TestTerminalNoticeDetection.
func TestNoticeDetectionGateNeighbors(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"valid", "valid-single", "partial", "no-journal", "untyped"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			b := *e.state("alice").Terminal
			b.OwnNotice = config.OwnNoticeObservation{}
			b.Typed = true
			b.NoticeCount = 2
			b.Notice = tmuxwake.Notice(2)
			b.AttemptAt = a.Now().Add(-time.Minute)
			switch mode {
			case "valid-single":
				b.NoticeCount = 1
				b.Notice = tmuxwake.Notice(1)
			case "partial":
				b.AttemptIDs = []string{"one"}
			case "no-journal":
				b.Typed = false
				b.NoticeCount = 0
				b.Notice = ""
			case "untyped":
				b.Typed = false
				b.NoticeCount = 1
				b.Notice = tmuxwake.Notice(1)
			}
			if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = &b; return nil }); err != nil {
				t.Fatal(err)
			}
			before := *e.state("alice").Terminal
			if err := a.observeTerminalNotice(s, "gen", before, "nonempty"); err != nil {
				t.Fatal(err)
			}
			after := *e.state("alice").Terminal
			want := "unknown"
			if mode == "valid" || mode == "valid-single" {
				want = "suspected"
			}
			if mode == "no-journal" {
				want = "clear"
			}
			requireNotice(t, after.OwnNotice, want)
			// Detection grants no recovery authority and changes no attempt journal.
			after.OwnNotice = before.OwnNotice
			after.NoticePublication = before.NoticePublication
			after.Idle = before.Idle
			if !reflect.DeepEqual(before, after) {
				t.Fatal("detection changed attempt journal")
			}
		})
	}
}
