package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Counts every HTTP request on the actual dispatch -> unknown -> session route.
// Manual gate avoids any terminal/process fixture; it does not replace publication.
func TestNoticeDispatchLocalNoop(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"missing", "unchanged", "recovered", "changed"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.HooksPaused = true
				if kind == "missing" {
					st.Liveness = nil
				}
				if kind == "unchanged" || kind == "recovered" {
					st.Terminal.OwnNotice = st.Liveness.OwnNotice
					st.Terminal.NoticePublication = "published"
					if kind == "recovered" {
						st.Terminal.NoticePublication = "unavailable: no liveness record (a2a status set)"
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			requests, puts := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				if r.Method == http.MethodPut {
					puts++
				}
				mu.Unlock()
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			before := e.state("alice")
			mail := e.mailbox("alice").Pending()
			for i := 0; i < 3; i++ {
				if err := a.dispatchTerminal(context.Background(), s, "deadline"); err != nil {
					t.Fatal(err)
				}
				b := e.state("alice").Terminal
				if kind == "missing" && (b.NoticePublication != "unavailable: no liveness record (a2a status set)" || e.state("alice").Liveness != nil || e.state("alice").NoticeStatus != nil) {
					t.Errorf("missing-liveness path changed: %q", b.NoticePublication)
				}
				if kind == "unchanged" && b.NoticePublication != before.Terminal.NoticePublication {
					t.Errorf("no-op publication metadata changed: %q", b.NoticePublication)
				}
				if kind == "recovered" && b.NoticePublication != "published" {
					t.Errorf("acknowledged notice retains missing cause: %q", b.NoticePublication)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if kind != "changed" && requests != 0 {
				t.Fatalf("local no-op made network requests: kind=%s requests=%d", kind, requests)
			}
			if kind == "changed" && (requests != 2 || puts != 1) {
				t.Fatalf("changed publication count: requests=%d puts=%d", requests, puts)
			}
			if !reflect.DeepEqual(mail, e.mailbox("alice").Pending()) {
				t.Fatal("mail changed")
			}
		})
	}
}

func TestNoticePublicationLocalOwnerGates(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"stopped", "stale", "foreign", "valid"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				if kind == "stopped" {
					st.Terminal.Stopped = true
				}
				if kind == "stale" {
					st.Terminal.Generation = "replacement"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "foreign" {
				s.creds.UserID = "@bob:test.local"
			}
			before := e.state("alice")
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); e.hs.srv.Config.Handler.ServeHTTP(w, r) }))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if kind == "valid" {
				if requests.Load() != 2 || e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("valid changed control refused")
				}
				return
			}
			if requests.Load() != 0 {
				t.Fatalf("local owner gate made network requests: %d", requests.Load())
			}
			if !reflect.DeepEqual(before, e.state("alice")) {
				t.Fatal("local owner refusal mutated state")
			}
		})
	}
}

func TestNoticePublicationCacheLifecycle(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"reuse", "M_NOT_FOUND", "M_FORBIDDEN", "rebind", "unbind", "resolve-M_NOT_FOUND", "resolve-M_FORBIDDEN"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			resolves, puts := 0, 0
			fail := ""
			var mu sync.Mutex
			setFail := func(value string) { mu.Lock(); fail = value; mu.Unlock() }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				resolve := strings.Contains(r.URL.Path, "/directory/room/")
				if resolve {
					resolves++
				}
				if r.Method == http.MethodPut {
					puts++
				}
				if fail != "" && (resolve == strings.HasPrefix(kind, "resolve-")) {
					writeJSON(w, 403, map[string]string{"errcode": fail, "error": "synthetic wrong room"})
					return
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			if strings.HasPrefix(kind, "resolve-") {
				setFail(strings.TrimPrefix(kind, "resolve-"))
			}
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if !strings.HasPrefix(kind, "resolve-") && e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("initial control failed")
			}
			if kind == "unbind" {
				e.extra[EnvSessionID] = "load-owner"
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					st.Terminal.SessionID = "load-owner"
					st.Terminal.Host = machineLabel()
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				e.as("alice", "unbind").ok(t)
				b := e.state("alice").Terminal
				if !b.Stopped {
					t.Fatal("unbind did not stop binding")
				}
			}
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal.OwnNotice = schemaNotice("false")
				if kind == "rebind" || kind == "unbind" {
					st.Terminal = &config.TerminalBinding{Generation: "replacement", Interval: 2 * time.Second, RetryMax: 30 * time.Second, OwnNotice: schemaNotice("false")}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			gen := "deadline"
			if kind == "rebind" || kind == "unbind" {
				gen = "replacement"
			}
			if kind == "M_NOT_FOUND" || kind == "M_FORBIDDEN" {
				setFail(kind)
			}
			publishNoticeAtEligibility(t, a, s, context.Background(), gen)
			if fail != "" {
				if e.state("alice").Terminal.NoticePublication == "published" {
					t.Fatal("failed write acknowledged")
				}
				setFail("")
				publishNoticeAtEligibility(t, a, s, context.Background(), gen)
			}
			mu.Lock()
			defer mu.Unlock()
			wantResolve := 2
			if kind == "M_NOT_FOUND" || kind == "M_FORBIDDEN" {
				wantResolve = 3
			}
			if strings.HasPrefix(kind, "resolve-") {
				wantResolve = 3
			}
			if resolves != wantResolve {
				t.Fatalf("current-room resolve count: got=%d want=%d", resolves, wantResolve)
			}
			wantPut := 2
			if kind == "M_NOT_FOUND" || kind == "M_FORBIDDEN" {
				wantPut = 3
			}
			if strings.HasPrefix(kind, "resolve-") {
				wantPut = 1
			}
			if puts != wantPut {
				t.Fatalf("current-room PUT count: got=%d want=%d", puts, wantPut)
			}
			if e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("current-room retry control failed")
			}
		})
	}
}

// State writers finish while status HTTP is in flight. Publication merges
// only its own result, preserving replacement, notice, heartbeat and mail edits.
// An overlapping automatic writer skips without HTTP; a superseded completion cannot
// clear its successor fence, and expiry permits one acknowledged reconciliation.
func TestNoticePublicationConcurrentMutation(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"resolve-replacement", "put-replacement", "put-notice", "put-heartbeat", "put-missing", "put-fence"} {
		t.Run(kind, noticeDeadlineTest(true, func(t *testing.T, e *env, a *App, s *session) {
			mail := e.mailbox("alice").Pending()
			var puts atomic.Int32
			var once sync.Once
			writerDone := make(chan error, 1)
			s.client.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				put := r.Method == http.MethodPut
				if put {
					puts.Add(1)
				}
				if put == strings.HasPrefix(kind, "put-") {
					once.Do(func() {
						if kind == "put-fence" {
							if err := s.publishTerminalNotice(context.Background(), "deadline"); err != nil || puts.Load() != 1 {
								t.Errorf("overlapping status writer bypassed reservation: %v", err)
							}
						}
						start := time.Now()
						go func() {
							_, err := config.UpdateStateBy(s.paths, s.name, time.Now().Add(150*time.Millisecond), func(st *config.State) error {
								st.HooksPaused = true
								switch kind {
								case "resolve-replacement", "put-replacement":
									st.Terminal = &config.TerminalBinding{Generation: "replacement", NoticePublication: "replacement untouched"}
								case "put-notice":
									st.Terminal.OwnNotice = schemaNotice("false")
									st.Terminal.NoticePublication = "unavailable: own notice changed"
								case "put-heartbeat":
									st.Liveness.HeartbeatAt = st.Liveness.HeartbeatAt.Add(time.Minute)
									st.Liveness.Detail = "concurrent heartbeat"
								case "put-missing":
									st.Liveness = nil
								case "put-fence":
									st.OwnStatusWrite = &config.OwnStatusWrite{Token: "successor", Until: time.Now().Add(time.Minute)}
								}
								return nil
							})
							writerDone <- err
						}()
						// The state edit must complete before the HTTP response, with
						// no timer advance or lock deadline needed to make progress.
						synctest.Wait()
						select {
						case err := <-writerDone:
							writerDone <- err
							if err != nil || !time.Now().Equal(start) {
								t.Errorf("state writer failed during publication: %v", err)
							}
						default:
							t.Error("publication held state lock during network")
						}
					})
				}
				return noticeHandlerTransport(e.hs.srv.Config.Handler).RoundTrip(r)
			})}
			publicationErr := s.publishTerminalNotice(context.Background(), "deadline")
			if err := <-writerDone; err != nil {
				t.Fatal(err)
			}
			st := e.state("alice")
			if puts.Load() != 1 {
				t.Fatalf("serialized publication count=%d", puts.Load())
			}
			if !st.HooksPaused || !reflect.DeepEqual(mail, e.mailbox("alice").Pending()) {
				t.Fatal("queued state/mail lost")
			}
			switch kind {
			case "resolve-replacement", "put-replacement":
				if st.Terminal.Generation != "replacement" || st.Terminal.NoticePublication != "replacement untouched" {
					t.Fatal("queued replacement overwritten")
				}
			case "put-notice":
				if !reflect.DeepEqual(st.Terminal.OwnNotice, schemaNotice("false")) || reflect.DeepEqual(st.Liveness.OwnNotice, st.Terminal.OwnNotice) {
					t.Fatal("queued desired notice lost or falsely acknowledged")
				}
				a.publishTerminalNotice(context.Background(), s, "deadline")
				if puts.Load() != 2 || e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("queued desired notice not retryable")
				}
			case "put-heartbeat":
				if publicationErr == nil || publicationErr.Error() != "own status changed during publication" || st.Liveness.Detail != "concurrent heartbeat" || !st.NoticeWriteUncertain || st.Terminal.NoticePublication == "published" {
					t.Fatalf("concurrent heartbeat lost or stale status acknowledged: %v", publicationErr)
				}
			case "put-fence":
				if publicationErr == nil || publicationErr.Error() != "own status publication superseded" || st.OwnStatusWrite == nil || st.OwnStatusWrite.Token != "successor" || !st.NoticeWriteUncertain || st.Terminal.NoticePublication == "published" {
					t.Fatalf("stale completion changed successor publication: %v", publicationErr)
				}
				if _, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.OwnStatusWrite.Until = time.Now().Add(-time.Second); return nil }); err != nil {
					t.Fatal(err)
				}
				if err := s.publishTerminalNotice(context.Background(), "deadline"); err != nil {
					t.Fatal(err)
				}
				if puts.Load() != 2 || e.state("alice").OwnStatusWrite != nil || e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("expired reservation did not reconcile")
				}

			case "put-missing":
				a.publishTerminalNotice(context.Background(), s, "deadline")
				st = e.state("alice")
				if puts.Load() != 1 || st.Liveness != nil || st.NoticeStatus != nil || st.Terminal.NoticePublication != "unavailable: no liveness record (a2a status set)" {
					t.Fatal("missing liveness caused a remote write")
				}
			}
		}))
	}
}

func TestNoticeObservationDoesNotMarkPending(t *testing.T) {
	isolateBindTest(t)
	_, a, s := noticeDeadlineFixture(t)
	st, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.NoticePublication = "published"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = a.observeTerminalNotice(s, "deadline", *st.Terminal, "unknown"); err != nil {
		t.Fatal(err)
	}
	st, err = config.LoadState(s.paths, s.name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(st.Terminal.NoticePublication, "pending:") {
		t.Fatal("observation flapped publication pending before eligibility")
	}
	a.unknownTerminalNotice(context.Background(), s, "stale", false)
	st, err = config.LoadState(s.paths, s.name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(st.Terminal.NoticePublication, "pending:") {
		t.Fatal("stale observation flapped pending")
	}
	st, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Liveness = nil
		st.Terminal.NoticePublication = "unavailable: no liveness record (a2a status set)"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = a.observeTerminalNotice(s, "deadline", *st.Terminal, "empty"); err != nil {
			t.Fatal(err)
		}
		st, err = config.LoadState(s.paths, s.name)
		if err != nil {
			t.Fatal(err)
		}
		if st.Terminal.NoticePublication != "unavailable: no liveness record (a2a status set)" {
			t.Fatal("missing liveness cause changed during observation")
		}
	}
}

func TestNoticePublicationSharedBudgetAndAppliedCancellation(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"earlier-partial-body", "later-caller-success"} {
		t.Run(kind, noticeDeadlineTest(true, func(t *testing.T, e *env, a *App, s *session) {
			before := e.state("alice")
			p := &noticeContextTransport{next: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				if strings.Contains(r.URL.Path, "/directory/room/") && kind == "earlier-partial-body" {
					time.Sleep(100 * time.Millisecond)
				}
				if r.Method == http.MethodPut && kind == "earlier-partial-body" {
					rec := httptest.NewRecorder()
					e.hs.srv.Config.Handler.ServeHTTP(rec, r)
					if rec.Code != http.StatusOK {
						t.Fatalf("setup: apply failed: %d", rec.Code)
					}
					return noticePartialResponse(r), nil
				}
				return noticeHandlerTransport(e.hs.srv.Config.Handler).RoundTrip(r)
			})}
			s.client.HTTP = &http.Client{Transport: p}
			budget := 350 * time.Millisecond
			if kind == "later-caller-success" {
				budget = 2 * nativeRPCTimeout
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			start := time.Now()
			a.publishTerminalNotice(ctx, s, "deadline")
			elapsed := time.Since(start)
			if len(p.contexts) != 2 {
				t.Fatalf("shared budget requests: %d", len(p.contexts))
			}
			d0, _ := p.contexts[0].Deadline()
			d1, _ := p.contexts[1].Deadline()
			if !d0.Equal(d1) || d0.After(start.Add(nativeRPCTimeout+20*time.Millisecond)) || !errors.Is(p.contexts[1].Err(), context.Canceled) && !errors.Is(p.contexts[1].Err(), context.DeadlineExceeded) {
				t.Fatal("resolve and PUT did not share capped budget/resources")
			}
			st := e.state("alice")
			if kind == "later-caller-success" {
				if st.Terminal.NoticePublication != "published" {
					t.Fatal("later caller positive refused")
				}
				return
			}
			if elapsed != budget || !reflect.DeepEqual(st.Liveness, before.Liveness) || st.Terminal.NoticePublication == "published" {
				t.Fatalf("partial-body shared budget or acknowledgement violated: elapsed=%v", elapsed)
			}
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), room, StatusEventType, s.creds.UserID, &remote); err != nil || !reflect.DeepEqual(remote.OwnNotice, before.Terminal.OwnNotice) {
				t.Fatalf("applied-before-cancel control missing: %v", err)
			}
			// A complete acknowledgement on the same state restores publication.
			p.next = noticeHandlerTransport(e.hs.srv.Config.Handler)
			publishNoticeAtEligibility(t, a, s, context.Background(), "deadline")
			if e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("valid acknowledgement control refused")
			}
		}))
	}
}

// The ordinary observed dispatch route uses the existing private terminal
// fixture. Frozen observations are no-ops; a genuinely newer ObservedAt is a
// real change and publishes once, resolving the current room for each change.
func TestNoticeDispatchObservedNoop(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"missing", "unchanged"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.e.as("alice", "status", "init").ok(t)
			if kind == "unchanged" {
				f.e.as("alice", statusArgs("working")...).ok(t)
			}
			f.dispatch()
			var requests, puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPut {
					puts.Add(1)
				}
				f.e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			f.s.client.BaseURL = srv.URL
			for i := 0; i < 3; i++ {
				f.dispatch()
				if kind == "missing" && (f.binding().NoticePublication != "unavailable: no liveness record (a2a status set)" || f.e.state("alice").Liveness != nil) {
					t.Fatal("missing liveness changed during observation")
				}
			}
			if requests.Load() != 0 {
				t.Fatalf("observed local no-op made network requests: %d", requests.Load())
			}
			f.now = f.now.Add(time.Second)
			f.dispatch()
			if kind == "missing" {
				f.now = f.now.Add(time.Second)
				f.dispatch()
				if requests.Load() != 0 {
					t.Fatal("missing liveness caused recurring remote writes")
				}
				return
			}
			if requests.Load() != 2 || puts.Load() != 1 {
				t.Fatalf("fresh ObservedAt count: requests=%d puts=%d", requests.Load(), puts.Load())
			}
			f.now = f.now.Add(time.Second)
			f.dispatch()
			if requests.Load() != 4 || puts.Load() != 2 {
				t.Fatalf("observed current-room count: requests=%d puts=%d", requests.Load(), puts.Load())
			}
		})
	}
}
