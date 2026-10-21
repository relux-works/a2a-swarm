package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A server may apply an unacknowledged unknown update. Returning to the old
// acknowledged observation must not turn that remote uncertainty into a claim
// that the old observation is currently published. A frozen legitimate clock
// reuses the exact observation; fresh timestamps remain covered separately.
func TestNoticeReturnToAcknowledgedAfterCancellation(t *testing.T) {
	for _, kind := range []string{"applied-cancelled", "acknowledged-control", "fresh-observation-control", "not-applied-cancelled", "not-applied-error", "reconciliation-error", "certain-equal-control"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			original := schemaNotice("true")
			at := time.Now().UTC()
			original.ObservedAt = at.Format(time.RFC3339Nano)
			original.Since = at.Add(-time.Minute).Format(time.RFC3339Nano)
			a.Now = func() time.Time { return at }
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
			var requests atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPut {
					puts.Add(1)
					if partial.Load() {
						if kind == "not-applied-error" {
							w.WriteHeader(500)
							_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"synthetic rejected write"}`))
							return
						}
						if kind != "not-applied-cancelled" {
							rec := httptest.NewRecorder()
							e.hs.srv.Config.Handler.ServeHTTP(rec, r)
							if rec.Code != http.StatusOK {
								t.Errorf("setup: apply response %d", rec.Code)
								w.WriteHeader(rec.Code)
								return
							}
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
			if e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("setup: initial positive failed")
			}
			ctx := context.Background()
			cancel := func() {}
			if kind != "acknowledged-control" && kind != "certain-equal-control" {
				partial.Store(true)
				ctx, cancel = context.WithTimeout(ctx, 120*time.Millisecond)
			}
			if kind != "certain-equal-control" {
				a.unknownTerminalNotice(ctx, s, "deadline", false)
			}
			cancel()
			partial.Store(false)
			mid := e.state("alice")
			uncertain := kind != "acknowledged-control" && kind != "certain-equal-control"
			if uncertain {
				status := e.as("alice", "bind", "status", "--json").ok(t)
				if !strings.Contains(status.stdout, "unavailable: own liveness publication failed") {
					t.Fatal("product status fabricated visibility after unacknowledged PUT")
				}
			}
			if kind != "acknowledged-control" && kind != "certain-equal-control" && (mid.Terminal.NoticePublication == "published" || !reflect.DeepEqual(mid.Liveness.OwnNotice, original)) {
				t.Fatal("setup: expected uncertain remote with retained local acknowledgement")
			}
			if kind == "fresh-observation-control" {
				a.Now = func() time.Time { return at.Add(time.Nanosecond) }
			}
			if err = a.observeTerminalNotice(s, "deadline", *mid.Terminal, "nonempty"); err != nil {
				t.Fatal(err)
			}
			desired := e.state("alice").Terminal.OwnNotice
			if kind != "fresh-observation-control" && !reflect.DeepEqual(desired, original) {
				t.Fatal("setup: production observation did not return to acknowledged value")
			}
			beforeRetry := puts.Load()
			if kind == "reconciliation-error" {
				partial.Store(true)
				retryCtx, retryCancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
				publishNoticeAtEligibility(t, a, s, retryCtx, "deadline")
				retryCancel()
				partial.Store(false)
				failed := e.state("alice")
				if failed.Terminal.NoticePublication == "published" || puts.Load() != beforeRetry+1 {
					t.Fatal("uncertain equality skipped retry or fabricated acknowledgement on retry failure")
				}
				beforeRetry = puts.Load()
			}
			publishNoticeAtEligibility(t, a, s, context.Background(), "deadline")
			if kind != "certain-equal-control" && puts.Load() != beforeRetry+1 {
				t.Fatal("uncertain equality did not publish exactly once")
			}
			st := e.state("alice")
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), room, StatusEventType, s.creds.UserID, &remote); err != nil {
				t.Fatal(err)
			}
			remoteJSON, _ := json.Marshal(remote.OwnNotice)
			desiredJSON, _ := json.Marshal(desired)
			status := e.as("alice", "bind", "status", "--json").ok(t)
			who := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t)
			t.Logf("kind=%s puts=%d remote=%s desired=%s product-status=%s product-who=%s", kind, puts.Load(), remoteJSON, desiredJSON, status.stdout, who.stdout)
			if st.Terminal.NoticePublication == "published" && !reflect.DeepEqual(remote.OwnNotice, desired) {
				t.Fatal("published metadata does not match remote own notice after cancelled write")
			}
			if st.Terminal.NoticePublication != "published" {
				t.Fatal("valid acknowledgement failed to restore published")
			}
			var reported []struct {
				Record config.AgentStatus `json:"record"`
			}
			if err = json.Unmarshal([]byte(who.stdout), &reported); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(status.stdout, "published") || len(reported) != 1 || !reflect.DeepEqual(reported[0].Record.OwnNotice, desired) {
				t.Fatal("product readback does not confirm acknowledged rendered notice")
			}
			// Real acknowledgement restores certainty, so identical subsequent production
			// observations/publications are network-free; reads above are excluded.
			afterAck := puts.Load()
			afterAckRequests := requests.Load()
			for i := 0; i < 3; i++ {
				current := e.state("alice")
				if err = a.observeTerminalNotice(s, "deadline", *current.Terminal, "nonempty"); err != nil {
					t.Fatal(err)
				}
				a.publishTerminalNotice(context.Background(), s, "deadline")
				if e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("certain equal cycle lost published")
				}
			}
			if puts.Load() != afterAck {
				t.Fatal("certain equal cycles repeated PUT after acknowledgement")
			}
			if requests.Load() != afterAckRequests {
				t.Fatal("certain equal cycles made network requests after acknowledgement")
			}
			if kind == "certain-equal-control" && puts.Load() != 1 {
				t.Fatal("certain-equal positive control made extra PUT")
			}
		})
	}
}

// A failed resolve or already-cancelled call cannot have sent a PUT. The old
// acknowledgement still establishes visibility when a real observation returns
// to it; conservative uncertainty starts only at the PUT boundary.
func TestNoticeAcknowledgementSurvivesPrePutFailure(t *testing.T) {
	for _, kind := range []string{"cancelled", "resolve-error"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			var requests, puts atomic.Int32
			var rejectResolve atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPut {
					puts.Add(1)
				}
				if rejectResolve.Load() && strings.Contains(r.URL.Path, "/directory/room/") {
					w.WriteHeader(500)
					_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"synthetic resolve failure"}`))
					return
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			a.publishTerminalNotice(context.Background(), s, "deadline")
			before := e.state("alice")
			if before.Terminal.NoticePublication != "published" {
				t.Fatal("setup: initial acknowledgement failed")
			}
			at, err := time.Parse(time.RFC3339Nano, before.Terminal.OwnNotice.ObservedAt)
			if err != nil {
				t.Fatal(err)
			}
			a.Now = func() time.Time { return at }
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal.Typed = true
				st.Terminal.NoticeCount = 1
				st.Terminal.Notice = tmuxwake.Notice(1)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			} else {
				rejectResolve.Store(true)
			}
			a.unknownTerminalNotice(ctx, s, "deadline", false)
			rejectResolve.Store(false)
			if puts.Load() != 1 {
				t.Fatal("pre-PUT failure unexpectedly wrote server state")
			}
			mid := e.state("alice")
			if err = a.observeTerminalNotice(s, "deadline", *mid.Terminal, "nonempty"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(e.state("alice").Terminal.OwnNotice, before.Terminal.OwnNotice) {
				t.Fatal("setup: observation differs from old acknowledgement")
			}
			requestCount := requests.Load()
			for i := 0; i < 3; i++ {
				a.publishTerminalNotice(context.Background(), s, "deadline")
			}
			if requests.Load() != requestCount || e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("pre-PUT failure invalidated an established acknowledgement")
			}
		})
	}
}
