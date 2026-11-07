package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// These tests use the real App/session publisher and real HTTP sockets. The
// server consumes the status PUT body before withholding every response byte.
// Safety releases happen only after a named property assertion has failed.
func noticeDeadlineFixture(t *testing.T) (*env, *App, *session) {
	t.Helper()
	e, room := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	e.hs.inject(room, "@bob:test.local", msg("@alice:test.local pending B1 mail", nil))
	e.as("alice", "wait", "--timeout", "2s").ok(t)
	if len(e.mailbox("alice").Pending()) != 1 {
		t.Fatal("setup: pending mail missing")
	}
	a := eApp(e)
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "deadline", OwnNotice: schemaNotice("true"), Interval: 2 * time.Second, RetryMax: 30 * time.Second}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, a, s
}

// Stalled resolve/PUT requests remain bounded while same-identity state writers
// can acquire the lock; completion preserves their edits, notice evidence and mail.
func TestNoticePublicationNetworkBound(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"put-default", "resolve-default", "put-deadline", "resolve-deadline", "put-cancel", "resolve-cancel", "consumer-deadline"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			before := e.state("alice")
			mail := e.mailbox("alice").Pending()
			entered, release := make(chan struct{}), make(chan struct{})
			requestFinished := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				statusPUT := r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/"+StatusEventType+"/")
				resolve := strings.Contains(r.URL.Path, "/directory/room/")
				stall := statusPUT
				if strings.HasPrefix(kind, "resolve-") {
					stall = resolve
				}
				if !stall {
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
					return
				}
				defer close(requestFinished)
				if statusPUT {
					var record config.AgentStatus
					if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
						t.Errorf("setup: PUT body: %v", err)
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/"+s.creds.UserID) {
						t.Error("foreign status key")
					}
				}
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				// A finite safety release makes a missing deadline an assertion failure,
				// rather than relying on the Go test process timing out to kill a mutant.
				if statusPUT {
					writeJSON(w, 200, map[string]string{"event_id": "$synthetic"})
				} else {
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}
			}))
			// Cleanup must release a deliberately broken publisher before Server.Close.
			t.Cleanup(srv.Close)
			s.client.BaseURL = srv.URL
			ctx := context.Background()
			cancel := func() {}
			budget := nativeRPCTimeout
			if strings.Contains(kind, "deadline") {
				budget = 350 * time.Millisecond
				ctx, cancel = context.WithTimeout(ctx, budget)
			}
			if strings.Contains(kind, "cancel") {
				budget = 350 * time.Millisecond
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			done := make(chan struct{})
			// Join the publisher and competing writer after releasing safety,
			// before temporary config cleanup, including expected-red plants.
			var acquired chan error
			writerReturned := false
			defer func() {
				close(release)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("setup: publisher cleanup failed")
				}
				if acquired != nil && !writerReturned {
					select {
					case <-acquired:
					case <-time.After(3 * time.Second):
						t.Error("setup: writer cleanup failed")
					}
				}
			}()
			start := time.Now()
			go func() {
				defer close(done)
				if kind == "consumer-deadline" {
					a.unknownTerminalNotice(ctx, s, "deadline", true)
				} else {
					a.publishTerminalNotice(ctx, s, "deadline")
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("setup: server never accepted request")
			}
			// HTTP runs outside this identity's actual file lock. Require a
			// successful mutation before the stalled request finishes.
			acquired = make(chan error, 1)
			writerStarted := make(chan struct{})
			go func() {
				close(writerStarted)
				_, err := config.UpdateStateBy(s.paths, s.name, start.Add(nativeRPCTimeout+4*time.Second), func(st *config.State) error { st.HooksPaused = true; return nil })
				acquired <- err
			}()
			<-writerStarted
			select {
			case err := <-acquired:
				writerReturned = true
				if err != nil {
					t.Errorf("state mutation failed during publication HTTP: %v", err)
				}
				select {
				case <-requestFinished:
					t.Error("state mutation completed only after publication HTTP")
				default:
				}
			case <-time.After(150 * time.Millisecond):
				t.Error("state lock unavailable during publication HTTP")
			}
			if strings.Contains(kind, "cancel") {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Until(start.Add(budget + 2*time.Second))):
				t.Error("publication exceeded network bound")
				return
			}
			elapsed := time.Since(start)
			if strings.HasSuffix(kind, "default") && elapsed < nativeRPCTimeout-time.Second {
				t.Errorf("default stall ended too early: %v", elapsed)
			}
			if !writerReturned {
				select {
				case err := <-acquired:
					writerReturned = true
					if err != nil {
						t.Errorf("released state lock unavailable: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("state lock not released after publication bound")
				}
			}
			st := e.state("alice")
			wantNotice := before.Terminal.OwnNotice
			if kind == "consumer-deadline" {
				wantNotice.PossiblyOrphaned = nil
			}
			if !strings.HasPrefix(st.Terminal.NoticePublication, "unavailable:") {
				t.Errorf("failed publication claimed visibility: %q", st.Terminal.NoticePublication)
			}
			if !st.HooksPaused || !reflect.DeepEqual(st.Liveness, before.Liveness) || !reflect.DeepEqual(st.Terminal.OwnNotice, wantNotice) || !reflect.DeepEqual(e.mailbox("alice").Pending(), mail) {
				t.Fatal("bounded publication lost concurrent state, heartbeat, notice evidence or mail")
			}
			if kind == "consumer-deadline" && st.Terminal.Outcome != "consumer unavailable; retrying" {
				t.Fatal("consumer error path did not resume")
			}
			t.Logf("accepted %s returned unavailable in %v; same-identity mutation and evidence preserved", kind, elapsed)
		})
	}
}

type noticeContextTransport struct {
	next     http.RoundTripper
	contexts []context.Context
}

func (p *noticeContextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.contexts = append(p.contexts, r.Context())
	return p.next.RoundTrip(r)
}

func TestNoticePublicationSuccessCancelsBudget(t *testing.T) {
	isolateBindTest(t)
	e, a, s := noticeDeadlineFixture(t)
	before := e.state("alice")
	mail := e.mailbox("alice").Pending()
	p := &noticeContextTransport{next: http.DefaultTransport}
	s.client.HTTP = &http.Client{Transport: p}
	a.publishTerminalNotice(context.Background(), s, "deadline")
	st := e.state("alice")
	if st.Terminal.NoticePublication != "published" || !reflect.DeepEqual(st.Liveness.OwnNotice, st.Terminal.OwnNotice) {
		t.Fatal("successful own publication unavailable")
	}
	got := *st.Liveness
	got.OwnNotice = before.Liveness.OwnNotice
	if !reflect.DeepEqual(got, *before.Liveness) || !reflect.DeepEqual(mail, e.mailbox("alice").Pending()) {
		t.Fatal("successful publication changed heartbeat, other fields or mail")
	}
	if len(p.contexts) != 2 {
		t.Fatalf("expected resolve and PUT, got %d requests", len(p.contexts))
	}
	for _, ctx := range p.contexts {
		d, ok := ctx.Deadline()
		if !ok || time.Until(d) > nativeRPCTimeout || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("network budget missing or cancellation resources not released")
		}
	}
	// Read back the actual self-keyed synthetic Matrix event.
	room, _, err := s.resolveRoom(context.Background(), StatusRoom)
	if err != nil {
		t.Fatal(err)
	}
	var remote config.AgentStatus
	if err = s.client.GetState(context.Background(), room, StatusEventType, s.creds.UserID, &remote); err != nil || !reflect.DeepEqual(remote, *st.Liveness) {
		t.Fatalf("successful own status not published: %v", err)
	}
}

func TestNoticePublicationEarlierLockDeadline(t *testing.T) {
	isolateBindTest(t)
	_, _, s := noticeDeadlineFixture(t)
	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := config.UpdateStateBy(s.paths, s.name, time.Now().Add(time.Second), func(*config.State) error { close(held); <-release; return nil })
		holderDone <- err
	}()
	<-held
	defer func() {
		close(release)
		if err := <-holderDone; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.publishTerminalNotice(ctx, "deadline")
	if !errors.Is(err, config.ErrLockTimeout) || time.Since(start) > time.Second {
		t.Fatalf("lock wait ignored earlier caller deadline: elapsed=%v err=%v", time.Since(start), err)
	}
}
