package cli

import (
	"context"
	"encoding/json"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Same-identity production hook/wait writers carry OwnNotice on the same event.
// Their uncertain acknowledgements must not bypass publication reconciliation.
func TestNoticeSharedWriterUncertainty(t *testing.T) {
	for _, kind := range []string{"hook-cancelled", "wait-cancelled", "hook-acknowledged", "wait-acknowledged", "hook-not-applied", "wait-not-applied", "hook-error", "wait-error"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			original := schemaNotice("true")
			at := time.Now().UTC()
			original.ObservedAt = at.Format(time.RFC3339Nano)
			original.Since = at.Add(-time.Minute).Format(time.RFC3339Nano)
			a.Now = func() time.Time { return at }
			a.as = "alice"
			a.Stdout = io.Discard
			a.Stderr = io.Discard
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal.OwnNotice = original
				st.Terminal.Typed = true
				st.Terminal.NoticeCount = 1
				st.Terminal.Notice = tmuxwake.Notice(1)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var partial atomic.Bool
			var puts atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					puts.Add(1)
					if partial.Load() {
						if strings.HasSuffix(kind, "error") {
							w.WriteHeader(500)
							_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"synthetic refusal"}`))
							return
						}
						rec := httptest.NewRecorder()
						if !strings.HasSuffix(kind, "not-applied") {
							e.hs.srv.Config.Handler.ServeHTTP(rec, r)
						}
						if rec.Code != 200 {
							t.Errorf("setup: apply %d", rec.Code)
							w.WriteHeader(rec.Code)
							return
						}
						w.WriteHeader(200)
						_, _ = w.Write([]byte(`{"event_id":"`))
						w.(http.Flusher).Flush()
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer func() { close(release); srv.Close() }()
			target, _ := url.Parse(srv.URL)
			a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				u := *r.URL
				clone.URL = &u
				clone.URL.Scheme = target.Scheme
				clone.URL.Host = target.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}
			s.client.HTTP = a.HTTP
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if puts.Load() != 1 || e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("setup: initial ack missing")
			}
			st := e.state("alice")
			if err = a.observeTerminalNotice(s, "deadline", *st.Terminal, "unknown"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			cancel := func() {}
			if !strings.HasSuffix(kind, "acknowledged") {
				partial.Store(true)
				ctx, cancel = context.WithTimeout(ctx, 120*time.Millisecond)
			}
			if strings.HasPrefix(kind, "hook") {
				err = a.hookLiveness(ctx, hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
			} else {
				profile := *st.Liveness
				profile.Doorbell = "armed"
				err = s.writeLivenessSource(ctx, profile, "wait-heartbeat")
			}
			cancel()
			partial.Store(false)
			if !strings.HasSuffix(kind, "acknowledged") && err == nil {
				t.Fatal("setup: writer cancellation absent")
			}
			if strings.HasSuffix(kind, "acknowledged") && err != nil {
				t.Fatal(err)
			}
			mid := e.state("alice")
			if err = a.observeTerminalNotice(s, "deadline", *mid.Terminal, "nonempty"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(e.state("alice").Terminal.OwnNotice, original) {
				t.Fatal("setup: return observation differs")
			}
			before := puts.Load()
			a.publishTerminalNotice(context.Background(), s, "deadline")
			after := e.state("alice")
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), room, StatusEventType, s.creds.UserID, &remote); err != nil {
				t.Fatal(err)
			}
			status := e.as("alice", "bind", "status", "--json").ok(t)
			who := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t)
			payload, _ := json.Marshal(map[string]any{"kind": kind, "puts": puts.Load(), "publication": after.Terminal.NoticePublication, "uncertain": after.NoticeWriteUncertain, "desired": after.Terminal.OwnNotice, "remote": remote.OwnNotice, "status": status.stdout, "who": who.stdout})
			t.Log(string(payload))
			if puts.Load() != before+1 || after.Terminal.NoticePublication != "published" || !reflect.DeepEqual(remote.OwnNotice, original) {
				t.Fatal("same-identity unacknowledged writer bypassed required reconciliation and fabricated certainty")
			}
			for i := 0; i < 3; i++ {
				a.publishTerminalNotice(context.Background(), s, "deadline")
			}
			if puts.Load() != before+1 {
				t.Fatal("acknowledged equality repeated PUT")
			}
		})
	}
}

// A successful same-identity liveness PUT acknowledges OwnNotice too. Retaining
// uncertainty after that acknowledgement must not induce a redundant notice PUT.
func TestNoticeSharedWriterAcknowledgement(t *testing.T) {
	for _, kind := range []string{"hook-after-cancel", "wait-after-cancel", "hook-certain-control", "wait-certain-control"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			a.as = "alice"
			a.Stdout = io.Discard
			a.Stderr = io.Discard
			var partial atomic.Bool
			var puts atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					puts.Add(1)
					if partial.Load() {
						rec := httptest.NewRecorder()
						e.hs.srv.Config.Handler.ServeHTTP(rec, r)
						if rec.Code != 200 {
							t.Errorf("setup: apply %d", rec.Code)
							w.WriteHeader(rec.Code)
							return
						}
						w.WriteHeader(200)
						_, _ = w.Write([]byte(`{"event_id":"`))
						w.(http.Flusher).Flush()
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer func() { close(release); srv.Close() }()
			target, _ := url.Parse(srv.URL)
			a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				u := *r.URL
				clone.URL = &u
				clone.URL.Scheme = target.Scheme
				clone.URL.Host = target.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}
			s.client.HTTP = a.HTTP
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("setup: initial ack missing")
			}
			if strings.HasSuffix(kind, "after-cancel") {
				partial.Store(true)
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
				a.unknownTerminalNotice(ctx, s, "deadline", false)
				cancel()
				partial.Store(false)
				if !e.state("alice").NoticeWriteUncertain {
					t.Fatal("setup: missing publisher uncertainty")
				}
			}
			st := e.state("alice")
			var err error
			if strings.HasPrefix(kind, "hook") {
				err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
			} else {
				profile := *st.Liveness
				profile.Doorbell = "armed"
				err = s.writeLivenessSource(context.Background(), profile, "wait-heartbeat")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := puts.Load()
			st = e.state("alice")
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), room, StatusEventType, s.creds.UserID, &remote); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(remote.OwnNotice, st.Terminal.OwnNotice) || !reflect.DeepEqual(remote, *st.Liveness) {
				t.Fatal("setup: actual writer acknowledgement/readback missing")
			}
			for i := 0; i < 3; i++ {
				a.publishTerminalNotice(context.Background(), s, "deadline")
			}
			t.Logf("kind=%s actualAcknowledgedPuts=%d afterCertainEqual=%d uncertainAfterWriter=%t", kind, before, puts.Load(), st.NoticeWriteUncertain)
			if puts.Load() != before {
				t.Fatal("successful same-identity writer acknowledgement did not restore certainty; certain-equal publication repeated PUT")
			}
		})
	}
}

// Reload actual persisted state in a new session; refused/no-liveness paths
// must retain unresolved knowledge, so the next eligible change reconciles once.
func TestNoticeSharedUncertaintyPersistence(t *testing.T) {
	for _, kind := range []string{"new-session", "missing", "stale", "stopped", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			var partial atomic.Bool
			var puts, requests atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPut {
					puts.Add(1)
					if partial.Load() {
						rec := httptest.NewRecorder()
						e.hs.srv.Config.Handler.ServeHTTP(rec, r)
						if rec.Code != 200 {
							t.Errorf("setup: apply %d", rec.Code)
							w.WriteHeader(rec.Code)
							return
						}
						w.WriteHeader(200)
						_, _ = w.Write([]byte(`{"event_id":"`))
						w.(http.Flusher).Flush()
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer func() { close(release); srv.Close() }()
			s.client.BaseURL = srv.URL
			a.publishTerminalNotice(context.Background(), s, "deadline")
			ack := e.state("alice").Liveness
			partial.Store(true)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			a.unknownTerminalNotice(ctx, s, "deadline", false)
			cancel()
			partial.Store(false)
			if !e.state("alice").NoticeWriteUncertain {
				t.Fatal("setup: uncertainty not persisted")
			}
			next, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			next.client.BaseURL = srv.URL
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal.OwnNotice = ack.OwnNotice
				switch kind {
				case "missing":
					st.Liveness = nil
				case "stale":
					st.Terminal.Generation = "replacement"
				case "stopped":
					st.Terminal.Stopped = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := next.creds.UserID
			if kind == "foreign" {
				next.creds.UserID = "@bob:test.local"
			}
			before := requests.Load()
			if kind != "new-session" {
				a.publishTerminalNotice(context.Background(), next, "deadline")
				if requests.Load() != before {
					t.Fatal("refused unresolved publication made network request")
				}
				if !e.state("alice").NoticeWriteUncertain {
					t.Fatal("refusal erased uncertainty")
				}
			}
			next.creds.UserID = owner
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Liveness = ack
				st.Terminal.Generation = "deadline"
				st.Terminal.Stopped = false
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a.publishTerminalNotice(context.Background(), next, "deadline")
			st := e.state("alice")
			if puts.Load() != 3 || st.NoticeWriteUncertain || st.Terminal.NoticePublication != "published" {
				t.Fatal("persisted uncertain equality did not reconcile once")
			}
			after := requests.Load()
			for i := 0; i < 3; i++ {
				a.publishTerminalNotice(context.Background(), next, "deadline")
			}
			if requests.Load() != after {
				t.Fatal("new session certain-equal made requests")
			}
		})
	}
}

type noticeWriterTransport func(*http.Request) (*http.Response, error)

func (f noticeWriterTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Pre-send failures preserve a real acknowledgement; a possible PUT (even a
// rejected response) instead requires reconciliation. No static source gate.
func TestNoticeSharedWriterPreSendFailure(t *testing.T) {
	for _, writer := range []string{"hook", "wait"} {
		for _, failure := range []string{"cancelled", "resolve", "schema"} {
			t.Run(writer+"/"+failure, func(t *testing.T) {
				e, a, s := noticeDeadlineFixture(t)
				a.as = "alice"
				a.Stdout = io.Discard
				a.Stderr = io.Discard
				var reject atomic.Bool
				var puts, requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method == http.MethodPut {
						puts.Add(1)
					}
					if reject.Load() && strings.Contains(r.URL.Path, "/directory/room/") {
						w.WriteHeader(500)
						_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"synthetic resolve failure"}`))
						return
					}
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				target, _ := url.Parse(srv.URL)
				a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
					c := r.Clone(r.Context())
					u := *r.URL
					c.URL = &u
					c.URL.Scheme = target.Scheme
					c.URL.Host = target.Host
					return http.DefaultTransport.RoundTrip(c)
				})}
				s.client.HTTP = a.HTTP
				a.publishTerminalNotice(context.Background(), s, "deadline")
				ack := e.state("alice").Liveness
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					st.Terminal.OwnNotice = schemaNotice("false")
					if failure == "schema" {
						st.Terminal.OwnNotice.ObservedAt = "invalid"
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				cancel := func() {}
				if failure == "cancelled" {
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if failure == "resolve" {
					reject.Store(true)
				}
				before := puts.Load()
				if writer == "hook" {
					err = a.hookLiveness(ctx, hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
				} else {
					err = s.writeLivenessSource(ctx, *ack, "wait-heartbeat")
				}
				cancel()
				reject.Store(false)
				st := e.state("alice")
				if err == nil || puts.Load() != before || st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) {
					t.Fatal("pre-send failure changed acknowledged knowledge or sent PUT")
				}
				_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = ack.OwnNotice; return nil })
				if err != nil {
					t.Fatal(err)
				}
				noops := requests.Load()
				for i := 0; i < 3; i++ {
					a.publishTerminalNotice(context.Background(), s, "deadline")
				}
				if requests.Load() != noops || e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("pre-send failure prevented certain-equal no-op")
				}
			})
		}
	}
}

// The identity owns uncertainty even when no terminal binding exists. A new
// binding must reconcile once, then acknowledged equality returns to zero HTTP.
func TestNoticeSharedKnowledgeWithoutBinding(t *testing.T) {
	e, a, s := noticeDeadlineFixture(t)
	a.publishTerminalNotice(context.Background(), s, "deadline")
	st := e.state("alice")
	binding := *st.Terminal
	ack := *st.Liveness
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	var reject atomic.Bool
	reject.Store(true)
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			if reject.Load() {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"synthetic uncertain write"}`))
				return
			}
		}
		e.hs.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s.client.BaseURL = srv.URL
	if err = s.writeLivenessSource(context.Background(), ack, "wait-heartbeat"); err == nil {
		t.Fatal("expected writer failure")
	}
	st = e.state("alice")
	if !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, &ack) {
		t.Fatal("unbound writer lost uncertainty or changed acknowledgement")
	}
	binding.Generation = "replacement"
	_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal = &binding; return nil })
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(false)
	a.publishTerminalNotice(context.Background(), s, "replacement")
	st = e.state("alice")
	if puts.Load() != 2 || st.NoticeWriteUncertain || st.Terminal.NoticePublication != "published" {
		t.Fatal("new binding forgot identity publication knowledge")
	}
	for i := 0; i < 3; i++ {
		a.publishTerminalNotice(context.Background(), s, "replacement")
	}
	if puts.Load() != 2 {
		t.Fatal("new binding repeated acknowledged PUT")
	}
}
