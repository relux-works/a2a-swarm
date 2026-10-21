package cli

import (
	"context"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Independent nonstandard/no-Location 3xx neighbors must remain ambiguous.
// This probes real HTTP and both prior uncertainty states, without live services.
func TestReviewCR3RedirectNeighbors(t *testing.T) {
	for _, code := range []int{300, 302, 304, 305, 306, 399} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/prior-%t", code, prior), func(t *testing.T) {
				e, _, s := noticeDeadlineFixture(t)
				ack := e.state("alice").Liveness
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
				if err != nil {
					t.Fatal(err)
				}
				var puts, other atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/target" {
						other.Add(1)
						writeJSON(w, 200, map[string]string{"event_id": "$unrelated"})
						return
					}
					if r.Method == http.MethodPut {
						puts.Add(1)
						w.WriteHeader(code)
						fmt.Fprint(w, `{"errcode":"M_FORBIDDEN"}`)
						return
					}
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				s.client.BaseURL = srv.URL
				err = s.publishTerminalNotice(context.Background(), "deadline")
				st := e.state("alice")
				if err == nil || puts.Load() != 1 || other.Load() != 0 || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || st.Terminal.NoticePublication == "published" {
					t.Fatalf("redirect neighbor accepted: %d %v", code, err)
				}
			})
		}
	}
}

// A missing event ACK must not reset notice retry on every changed observation.
// A real direct ACK is permitted to publish each change; no arbitrary throughput
// promise applies to successful writes.
func TestReviewCR3MalformedACKTraffic(t *testing.T) {
	for _, kind := range []string{"missing", "known-error", "direct"} {
		t.Run(kind, func(t *testing.T) {
			direct := kind == "direct"
			e, a, s := noticeDeadlineFixture(t)
			ack := e.state("alice").Liveness
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			a.Now = func() time.Time { return now }
			var requests, puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPut {
					puts.Add(1)
					if !direct {
						code := 200
						if kind == "known-error" {
							code = 500
						}
						writeJSON(w, code, map[string]string{"errcode": "M_FORBIDDEN"})
						return
					}
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			for i := 0; i < 4; i++ {
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					st.Terminal.OwnNotice.ObservedAt = now.Add(time.Duration(i) * time.Nanosecond).Format(time.RFC3339Nano)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				err = s.publishTerminalNotice(context.Background(), "deadline")
				st := e.state("alice")
				if direct {
					if err != nil || requests.Load() != int32(2*(i+1)) || st.NoticeRetry != nil || st.NoticeWriteUncertain {
						t.Error("real ACK traffic control")
					}
				} else if err == nil || requests.Load() != 2 || puts.Load() != 1 || st.NoticeRetry == nil || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) {
					t.Errorf("missing ACK bypasses bounded traffic: cycle=%d RPC=%d PUT=%d err=%v retry=%v uncertain=%t", i, requests.Load(), puts.Load(), err, st.NoticeRetry, st.NoticeWriteUncertain)
				}
			}
		})
	}
}
