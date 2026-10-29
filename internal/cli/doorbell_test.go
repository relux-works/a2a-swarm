package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func doorbellFixture(t *testing.T) (*env, string, string) {
	t.Helper()
	e, room, _, generation := runtimeWorld(t)
	return e, room, generation
}

func doorbellCommand(t *testing.T, e *env, ctx context.Context, generation string, once bool, timeout time.Duration) result {
	t.Helper()
	a, _ := notificationApp(t, e)
	var out, errout bytes.Buffer
	a.Stdout, a.Stderr = &out, &errout
	args := []string{"--generation", generation, "--timeout", timeout.String()}
	if once {
		args = append(args, "--once")
	}
	code := a.fail(cmdDoorbell(ctx, a, args))
	r := result{code: code, stdout: out.String(), stderr: errout.String()}
	e.assertNoLeak(r)
	return r
}

func doorbellAppend(t *testing.T, e *env, entries ...config.MailEntry) {
	t.Helper()
	_, s := notificationApp(t, e)
	_, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		_, err := mb.Append(entries)
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func doorbellEntry(id, room, sender, rel, body string, h Header) config.MailEntry {
	content := msg(body, map[string]any{ContentKey: h})
	raw, _ := json.Marshal(content)
	ev, _ := json.Marshal(matrix.Event{EventID: id, Type: "m.room.message", Sender: sender, Content: raw})
	return config.MailEntry{Kind: config.MailEvent, ID: id, Room: room, Rel: rel, Label: "HOSTILE_ROOM_LABEL", Event: ev}
}

// Both modes produce exactly the existing fixed template, byte-for-byte; peer
// body/header/topic/room/display-like content and control bytes never reach it.
// A real joined sender remains named; spoofed From and foreign/nonmember senders
// cannot name a principal. This is CLI output evidence, not native model receipt.
func TestDoorbellBodyIsolationAndModes(t *testing.T) {
	var reference string
	for _, once := range []bool{true, false} {
		for _, hostile := range []bool{false, true} {
			e, room, g := doorbellFixture(t)
			body, metadata := "hello", "unlisted"
			if hostile {
				body = "PEER_SENTINEL\n\x1b[31m" + strings.Repeat("OVERSIZE", 2000)
				metadata = body
			}
			h := Header{V: 1, Type: "message", From: metadata, To: []string{"alice"}, Kind: metadata, Subject: metadata, Slug: metadata, Title: metadata, Subtype: metadata, Re: metadata, ReplyBy: metadata}
			doorbellAppend(t, e,
				doorbellEntry("$one", room, "@bob:test.local", "to", body, h),
				doorbellEntry("$two", room, "@carol:test.local", "mention", body, Header{}),
				doorbellEntry("$three", room, "@mallory:foreign.example", "mention", body, Header{}))
			lifetime := time.Second
			if !once {
				lifetime = 300 * time.Millisecond
			}
			r := doorbellCommand(t, e, context.Background(), g, once, lifetime)
			want := "a2a: 3 new peer events (to: 1, mention: 2; kinds: other) from bob, unknown sender" + bellTail + "\n"
			if r.stdout != want || (once && r.code != 0) || (!once && (r.code != 1 || !strings.Contains(r.stderr, "doorbell: timeout"))) {
				t.Fatalf("once=%t hostile=%t: got %+v; want %q", once, hostile, r, want)
			}
			if len(r.stdout) > doorbellMax+1 || strings.Count(r.stdout, "\n") != 1 {
				t.Fatal("unbounded or multiline summary")
			}
			for _, forbidden := range []string{"PEER_SENTINEL", "OVERSIZE", "\x1b", "HOSTILE_ROOM_LABEL", "mallory", "carol", "foreign.example", "$one"} {
				if strings.Contains(r.stdout, forbidden) {
					t.Fatalf("forbidden peer text in notice: %q", forbidden)
				}
			}
			if reference != "" && r.stdout != reference {
				t.Fatal("modes/hostile fields changed shared encoding")
			}
			reference = r.stdout
		}
	}
}

// A detector re-exposes shown/rung pending mail without modifying any durable
// delivery marker. Once normal intake receipts it, unread=0 is quiet; this
// absence does NOT establish that an external intake obligation was completed.
func TestDoorbellPendingRecoveryAndReceiptBoundary(t *testing.T) {
	e, room, g := doorbellFixture(t)
	id := runtimeMail(e, room, "retained obligation")
	_, s := notificationApp(t, e)
	_, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(st *config.State, mb *config.Mailbox) ([]string, error) {
		// Model an already-migrated historical notification ledger, rather
		// than asking this test to bypass the separate legacy cooldown.
		if err := st.Notification.Prepare(time.Now().Add(-time.Minute), time.Time{}); err != nil {
			return nil, err
		}
		st.Wake.Rung = []string{id}
		h := st.HookSession("owned-thread")
		h.Rung = []string{id}
		st.PutHookSession(h)
		return nil, mb.MarkShown([]string{id})
	})
	if err != nil {
		t.Fatal(err)
	}
	before := e.state("alice")
	r := doorbellCommand(t, e, context.Background(), g, true, time.Second).ok(t)
	if !strings.Contains(r.stdout, "1 new peer event") || e.received("alice", id) || !e.mailbox("alice").Pending()[0].Shown {
		t.Fatal("shown/rung obligation was hidden or receipted")
	}
	after := e.state("alice")
	if !reflect.DeepEqual(before.Wake.Rung, after.Wake.Rung) || !reflect.DeepEqual(before.HookSessions, after.HookSessions) || !reflect.DeepEqual(before.Lease, after.Lease) {
		t.Fatal("detector modified rung/history/lease")
	}
	e.as("alice", "inbox").ok(t)
	if !e.received("alice", id) {
		t.Fatal("valid intake control did not receipt")
	}
	empty := doorbellCommand(t, e, context.Background(), g, true, 5*time.Millisecond)
	if empty.stdout != "" || empty.code != 1 || !strings.Contains(empty.stderr, "doorbell: timeout") {
		t.Fatalf("unread=0 emitted a false obligation: %+v", empty)
	}
}

// Missing/foreign/stale enrollment cannot output, change a lease or receipt.
// Each invalid world has the same real positive same-session control nearby.
func TestDoorbellEnrollmentRefusals(t *testing.T) {
	for _, mode := range []string{"valid", "session-missing", "session-foreign", "native-mismatch", "generation", "identity", "lease", "expired", "owner", "retired", "no-binding", "endpoint"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			switch mode {
			case "session-missing":
				delete(e.extra, EnvSessionID)
			case "session-foreign":
				e.extra[EnvSessionID] = "foreign"
			case "native-mismatch":
				e.extra["CODEX_THREAD_ID"] = "foreign"
			case "generation":
				g = "foreign"
			default:
				notificationState(t, e, func(st *config.State) {
					switch mode {
					case "identity":
						st.Runtime.Identity = "bob"
					case "lease":
						st.Lease.Holder = "foreign"
					case "expired":
						st.Lease.Expires = time.Now().Add(-time.Second)
					case "owner":
						st.Runtime.OwnerStart = "foreign-birth"
					case "retired":
						st.Runtime.Retired = true
					case "no-binding":
						st.Runtime = nil
					case "endpoint":
						st.Runtime.EndpointID = "replaced"
					}
				})
			}
			before := e.state("alice")
			r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
			if mode == "valid" {
				r.ok(t)
				if r.stdout == "" {
					t.Fatal("positive control rejected")
				}
			} else if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "own enrolled session") {
				t.Fatalf("invalid enrollment admitted: %+v", r)
			}
			if e.received("alice", id) || !reflect.DeepEqual(before.Lease, e.state("alice").Lease) {
				t.Fatal("enrollment refusal changed custody")
			}
		})
	}
}

// Real source gates retain pending work and budget. Removing each gate permits
// the valid same-generation transport; no provider/model or terminal is used.
func TestDoorbellHoldsAndBudget(t *testing.T) {
	for _, mode := range []string{"pause", "human", "blocked", "stopping", "budget-gate", "hourly", "native-busy"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			clock := e.clock()
			notificationState(t, e, func(st *config.State) {
				switch mode {
				case "pause":
					st.HooksPaused = true
				case "human", "blocked", "stopping":
					state := mode
					if state == "human" {
						state = "waiting-human"
					}
					st.Liveness = &config.AgentStatus{State: state}
					st.LivenessSource = "status"
				case "budget-gate":
					st.Runtime.Gate = "budget"
				case "hourly":
					st.Wake.Reservations = []time.Time{clock.now(), clock.now()}
				case "native-busy":
					st.Wake.LastTurn, st.Wake.LastTurnStatus = "turn", "inProgress"
				}
			})
			r := doorbellCommand(t, e, context.Background(), g, true, 5*time.Millisecond)
			if r.stdout != "" || r.code != 1 || e.received("alice", id) {
				t.Fatalf("hold lost work or emitted: %+v", r)
			}
			notificationState(t, e, func(st *config.State) {
				st.HooksPaused = false
				st.Liveness, st.LivenessSource = nil, ""
				st.Runtime.Gate = "ready"
				st.Wake.Reservations = nil
				st.Wake.LastTurn, st.Wake.LastTurnStatus = "", ""
			})
			r = doorbellCommand(t, e, context.Background(), g, true, time.Second).ok(t)
			if r.stdout == "" || len(e.state("alice").Wake.Reservations) != 1 || e.received("alice", id) {
				t.Fatal("gate release failed positive control")
			}
		})
	}
}

type doorbellFaultWriter struct {
	n          int
	err, flush error
}

func (w doorbellFaultWriter) Write(p []byte) (int, error) {
	if w.n < 0 {
		return len(p), w.err
	}
	return min(w.n, len(p)), w.err
}
func (w doorbellFaultWriter) Flush() error { return w.flush }

// Zero writes permit a proven-no-output cancellation; partial/flush/crash and
// prior native uncertainty retain their exact fences across restart and due.
// A successful write completes transport only, never native acceptance/receipt.
func TestDoorbellOutputFaultsAndUncertainRestart(t *testing.T) {
	for _, mode := range []string{"zero", "short", "error", "flush", "crash", "reserve-crash", "native-uncertain"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "retained")
			clock := e.clock()
			a, s := notificationApp(t, e)
			w := doorbellFaultWriter{n: -1}
			switch mode {
			case "zero":
				w.n, w.err = 0, errors.New("HOSTILE_ERROR\n\x1b[31m")
			case "short":
				w.n = 3
			case "error":
				w.n, w.err = 3, errors.New("HOSTILE_ERROR")
			case "flush":
				w.flush = errors.New("HOSTILE_ERROR")
			case "crash":
				a.afterPrint = func() error { return errors.New("HOSTILE_ERROR") }
			case "reserve-crash":
				a.afterNotificationReserve = func() error { return errors.New("HOSTILE_ERROR") }
			case "native-uncertain":
				notificationState(t, e, func(st *config.State) {
					_, err := st.Wake.Begin(*st.Runtime, []string{id}, "fixed", clock.now())
					if err != nil {
						t.Fatal(err)
					}
				})
			}
			a.Stdout = w
			printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, map[string]bool{})
			if printed || err == nil || e.received("alice", id) {
				t.Fatal("fault treated as successful/received")
			}
			before := e.state("alice")
			if mode == "zero" {
				if before.Notification.Reservation != nil {
					t.Fatal("proven zero output not canceled")
				}
			} else if mode != "native-uncertain" && before.Notification.Reservation == nil {
				t.Fatal("uncertain transport fence lost")
			}
			clock.add(31 * time.Second)
			r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
			if mode == "zero" {
				r.ok(t)
				if r.stdout == "" {
					t.Fatal("proven-no-output recovery blocked")
				}
			} else {
				if r.stdout != "" || r.code != 1 || !strings.Contains(r.stderr, "retained uncertain attempt") {
					t.Fatalf("blind uncertain retry: %+v", r)
				}
				after := e.state("alice")
				if !reflect.DeepEqual(before.Notification.Reservation, after.Notification.Reservation) || !reflect.DeepEqual(before.Wake.Attempt, after.Wake.Attempt) {
					t.Fatal("restart changed same-attempt evidence")
				}
			}
		})
	}
}

// Actual arbiter spacing coalesces a second event arriving15s later, retains a
// fixed due time, and releases at30s without a new arrival. Just-after is valid.
func TestDoorbellSpacingTrailingAndCoalescing(t *testing.T) {
	e, room, g := doorbellFixture(t)
	clock := e.clock()
	first := runtimeMail(e, room, "first")
	a, s := notificationApp(t, e)
	var out bytes.Buffer
	a.Stdout = &out
	announced := map[string]bool{}
	if printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, announced); err != nil || !printed {
		t.Fatalf("leading control: %t %v", printed, err)
	}
	due := e.state("alice").Notification.NotBefore
	clock.add(15 * time.Second)
	second := runtimeMail(e, room, "second")
	for _, delta := range []time.Duration{0, 14999 * time.Millisecond} {
		clock.add(delta)
		if printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, announced); err != nil || printed || !e.state("alice").Notification.NotBefore.Equal(due) {
			t.Fatal("early output/sliding due")
		}
	}
	clock.add(time.Millisecond)
	if printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, announced); err != nil || !printed || !strings.Contains(out.String(), "2 new peer events") {
		t.Fatalf("trailing event dropped: %t %v %q", printed, err, out.String())
	}
	// The fixture's two-slot budget is full: advance to its real next-hour
	// boundary without erasing reservations, and keep its lease independently live.
	clock.add(time.Hour + time.Nanosecond)
	notificationState(t, e, func(st *config.State) { st.Lease.Expires = clock.now().Add(config.LeaseTTL) })
	third := runtimeMail(e, room, "third")
	if printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, announced); err != nil || !printed {
		t.Fatalf("just-after positive: %t %v", printed, err)
	}
	if e.received("alice", first) || e.received("alice", second) || e.received("alice", third) || len(e.mailbox("alice").Pending()) != 3 {
		t.Fatal("spacing receipted/lost obligations")
	}
}

// The real exclusive consumer lock rejects a concurrent detector. A control
// holder and a canceled or empty one-shot emit no stdout, without receipting.
func TestDoorbellEmptyCancellationAndConcurrentConsumer(t *testing.T) {
	e, room, g := doorbellFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := doorbellCommand(t, e, ctx, g, true, time.Second)
	if r.stdout != "" || r.code != 1 || !strings.Contains(r.stderr, "doorbell: canceled") {
		t.Fatalf("cancellation became a notice: %+v", r)
	}
	id := runtimeMail(e, room, "pending")
	stop := notificationConsumerLock(t, e)
	r = doorbellCommand(t, e, context.Background(), g, true, time.Second)
	if r.stdout != "" || r.code != 1 || e.received("alice", id) {
		t.Fatal("second consumer admitted")
	}
	stop()
	doorbellCommand(t, e, context.Background(), g, true, time.Second).ok(t)
}

// A crash after fsync and before cursor save retains one deduplicated pending
// event; a fresh detector recovers it. Network/error text stays off stdout.
func TestDoorbellPersistBeforeCursorAndFailureIsolation(t *testing.T) {
	e, room, g := doorbellFixture(t)
	before := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "SECRET_PEER_BODY"))
	prior := e.configure
	e.configure = func(a *App) {
		prior(a)
		a.afterAppend = func() error { return errors.New("HOSTILE_ERROR") }
	}
	r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
	if r.stdout != "" || strings.Contains(r.stderr, "HOSTILE_ERROR") || r.code != 1 || e.state("alice").NextBatch != before || len(e.mailbox("alice").Pending()) != 1 {
		t.Fatalf("persist/cursor failure boundary broken: %+v", r)
	}
	e.configure = prior
	doorbellCommand(t, e, context.Background(), g, true, time.Second).ok(t)
	if len(e.mailbox("alice").Pending()) != 1 || e.received("alice", id) {
		t.Fatal("replay duplicated/lost obligation")
	}
}

// Malformed durable metadata is retained and refused, adjacent to a valid
// control. A corrupt entry cannot smuggle a relevance label through the encoder.
func TestDoorbellMalformedMetadataClosed(t *testing.T) {
	for _, field := range []string{"kind", "rel", "event", "id"} {
		t.Run(field, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			entry := doorbellEntry("$bad", room, "@bob:test.local", "to", "peer", Header{})
			switch field {
			case "kind":
				entry.Kind = "PEER\nLABEL"
			case "rel":
				entry.Rel = "PEER\nLABEL"
			case "event":
				entry.Event = json.RawMessage(`{"type":"m.room.member","event_id":"$bad"}`)
			case "id":
				entry.ID = "$mismatch"
			}
			doorbellAppend(t, e, entry)
			r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
			if r.stdout != "" || r.code != 1 || !strings.Contains(r.stderr, "invalid mailbox metadata") || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatalf("bad metadata emitted/erased: %+v", r)
			}
		})
	}
}

// The actual CLI registry dispatches the additive command, and its bounded
// syntax refuses invalid durations/arguments without reflecting their values.
func TestDoorbellRegistryAndSyntax(t *testing.T) {
	e, room, g := doorbellFixture(t)
	runtimeMail(e, room, "body stays in mailbox")
	r := e.as("alice", "doorbell", "--generation", g, "--once", "--timeout", "1s").ok(t)
	if !strings.Contains(r.stdout, "1 new peer event") {
		t.Fatal("registry did not dispatch body-free primitive")
	}
	for _, args := range [][]string{
		{}, {"--generation", g, "--timeout", "0"}, {"--generation", g, "--timeout", "26m"},
		{"--generation", g, "--timeout", "-1s"}, {"--generation", "PEER\nARG"}, {"--PEER_ARG"},
	} {
		r := e.as("alice", append([]string{"doorbell"}, args...)...)
		if r.code != 2 || r.stdout != "" || strings.Contains(r.stderr, "PEER") {
			t.Fatalf("invalid syntax reflected input or ran: %+v", r)
		}
	}
}

// Reuse source classify/ingest: own participation and unrelated/muted thread
// traffic alone are quiet; direct, mention and explicit follow pass. This
// intentionally does not claim the118 implicit-follow cutover is on this base.
func TestDoorbellSourceEligibility(t *testing.T) {
	for _, mode := range []string{"unrelated", "own-participation", "muted", "direct", "mention", "follow", "owner-sub"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			root := "$topic-root"
			notificationState(t, e, func(st *config.State) {
				if mode == "muted" || mode == "direct" || mode == "mention" {
					st.AddThread(root)
					st.Mute(root)
				}
				if mode == "follow" {
					st.Mute(root)
					st.Follow(root) // source API explicitly removes this mute
				}
			})
			if mode == "owner-sub" {
				// This fake server otherwise timestamps events in September;
				// use its supported live clock for the real creation-time gate.
				e.hs.mu.Lock()
				e.hs.liveClock = true
				e.hs.mu.Unlock()
				e.as("alice", "subscribe", "--room", room, "--from", "bob").ok(t)
			}
			body, sender := "unrelated body", "@bob:test.local"
			h := Header{V: 1, Type: "message", Kind: "notice"}
			if mode == "direct" {
				h.To = []string{"alice"}
			}
			if mode == "mention" {
				body = "@alice:test.local hello"
			}
			if mode == "own-participation" {
				sender = "@alice:test.local"
			}
			content := msg(body, map[string]any{ContentKey: h, "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}})
			if mode == "owner-sub" {
				content = msg(body, nil) // ordinary untyped pinned-principal traffic
			}
			e.hs.inject(room, sender, content)
			positive := mode == "direct" || mode == "mention" || mode == "follow" || mode == "owner-sub"
			lifetime := 150 * time.Millisecond
			if positive {
				lifetime = time.Second
			}
			r := doorbellCommand(t, e, context.Background(), g, true, lifetime)
			if positive {
				r.ok(t)
				if r.stdout == "" || len(e.mailbox("alice").Pending()) != 1 {
					t.Fatal("eligible source control missing")
				}
			} else if r.stdout != "" || r.code != 1 || !strings.Contains(r.stderr, "doorbell: timeout") || len(e.mailbox("alice").Pending()) != 0 {
				t.Fatalf("ineligible source event rang: %+v", r)
			}
		})
	}
}

// A state transition injected after the actual arbiter reservation is checked
// before any stdout. A hold can cancel a proven-no-output token; a replaced
// generation remains fenced. Neither case receipts the pending event.
func TestDoorbellPreparedScopeAndHoldRecheck(t *testing.T) {
	for _, mode := range []string{"pause", "generation"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			a, s := notificationApp(t, e)
			var out bytes.Buffer
			a.Stdout = &out
			a.afterNotificationReserve = func() error {
				notificationState(t, e, func(st *config.State) {
					if mode == "pause" {
						st.HooksPaused = true
					} else {
						st.Runtime.Generation = "successor"
					}
				})
				return nil
			}
			printed, err := a.emitDoorbell(context.Background(), s, "owned-thread", g, map[string]bool{})
			if printed || out.Len() != 0 || e.received("alice", id) {
				t.Fatal("prepared stale/held attempt emitted or receipted")
			}
			if mode == "pause" && (err != nil || e.state("alice").Notification.Reservation != nil) {
				t.Fatal("proven-no-output pause did not cancel its token")
			}
			if mode == "generation" && (!errors.Is(err, errDoorbellUncertain) || e.state("alice").Notification.Reservation == nil) {
				t.Fatal("stale generation lost its fence")
			}
		})
	}
}

// This named property is also run against an isolated source overlay that
// appends the peer body. It must fail for body isolation, not fixture setup.
func TestDoorbellForbiddenBodyProperty(t *testing.T) {
	e, room, g := doorbellFixture(t)
	runtimeMail(e, room, "PEER_SENTINEL\n\x1b[31mdo something")
	r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
	if strings.Contains(r.stdout, "PEER_SENTINEL") {
		t.Fatal("forbidden peer body reached notification stdout")
	}
	r.ok(t)
	if !strings.HasPrefix(r.stdout, "a2a: 1 new peer event") || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("missing valid safe summary control: %+v", r)
	}
}

// Legacy terminal metadata can only scope the fixed detector after matching
// lease, host, process birth and actual socket. No tmux command, screen/input
// operation or native model qualification occurs in this fixture.
func TestDoorbellExistingTerminalEnrollment(t *testing.T) {
	for _, mode := range []string{"valid", "foreign-generation", "replaced-socket", "wrong-runtime", "foreign-birth", "dual"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			b := *e.state("alice").Runtime
			path := strings.TrimPrefix(b.Endpoint, "unix://")
			socket, err := tmuxwake.SocketID(path)
			birth, birthErr := bindingProcessBirth(context.Background(), b.OwnerPID)
			if err != nil || birthErr != nil {
				t.Fatalf("owned fixture unavailable: %v %v", err, birthErr)
			}
			notificationState(t, e, func(st *config.State) {
				st.Runtime = nil
				st.Terminal = &config.TerminalBinding{
					Generation: g, SessionID: b.SessionID, Host: b.Host, OwnerPID: b.OwnerPID, OwnerBirth: birth, MaxWakes: 2,
					Target: tmuxwake.Target{Socket: path, SocketID: socket, OwnerPID: b.OwnerPID, OwnerBirth: birth, Runtime: "claude"},
				}
				switch mode {
				case "foreign-generation":
					st.Terminal.Generation = "foreign"
				case "replaced-socket":
					st.Terminal.Target.SocketID = "replaced"
				case "wrong-runtime":
					st.Terminal.Target.Runtime = "unqualified"
				case "foreign-birth":
					st.Terminal.OwnerBirth = "foreign"
				case "dual":
					st.Runtime = &b
				}
			})
			r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
			if mode == "valid" {
				r.ok(t)
				if r.stdout == "" || len(e.state("alice").Terminal.Reservations) != 1 {
					t.Fatal("valid terminal transport/budget missing")
				}
			} else if r.stdout != "" || r.code != 1 || !strings.Contains(r.stderr, "own enrolled session") {
				t.Fatalf("invalid terminal scope admitted: %+v", r)
			}
			if e.received("alice", id) {
				t.Fatal("terminal-scoped detector receipted")
			}
		})
	}
}

// The stream writes a leading batch and a later due batch using its real loop,
// then cancellation ends it. The writer synchronizes observed output; synthetic
// clock advancement replaces a30-second wall wait, not the source arbiter.
func TestDoorbellStreamTrailingLoop(t *testing.T) {
	e, room, g := doorbellFixture(t)
	clock := e.clock()
	first := runtimeMail(e, room, "first")
	a, s := notificationApp(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := &doorbellStreamWriter{lines: make(chan string, 4)}
	a.Stdout = w
	done := make(chan error, 1)
	go func() { done <- a.runDoorbell(ctx, s, "owned-thread", g, false) }()
	select {
	case <-w.lines:
	case err := <-done:
		t.Fatalf("stream no leading notice: %v", err)
	case <-ctx.Done():
		t.Fatal("stream stalled")
	}
	// Write is observed before the transport completion commits. Acquire the
	// same control lock before advancing time so completion anchors at t0.
	if err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	clock.add(30 * time.Second)
	second := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "second"))
	select {
	case line := <-w.lines:
		if !strings.Contains(line, "2 new peer events") {
			t.Fatalf("wrong trailing summary %q", line)
		}
	case err := <-done:
		t.Fatalf("stream no trailing notice: %v", err)
	case <-ctx.Done():
		t.Fatal("stream stalled after due arrival")
	}
	cancel()
	<-done
	if e.received("alice", first) || e.received("alice", second) {
		t.Fatal("stream receipted")
	}
}

type doorbellStreamWriter struct {
	mu    sync.Mutex
	lines chan string
}

func (w *doorbellStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines <- string(p)
	return len(p), nil
}

var _ io.Writer = (*doorbellStreamWriter)(nil)
