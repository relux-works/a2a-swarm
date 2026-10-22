package cli

import (
	"context"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
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

// Every own-status writer reaches the shared Matrix interpreter. These are
// real in-memory HTTP/state fixtures, with persisted state and genuine recovery.
func TestStrictWireAllOwnWriters(t *testing.T) {
	for _, writer := range []string{"notice", "manual", "public-status", "wait-heartbeat", "hook"} {
		for _, prior := range []bool{false, true} {
			for _, kind := range []string{"forbidden", "utf8", "case", "duplicate", "partial", "ack"} {
				t.Run(fmt.Sprintf("%s/prior-%t/%s", writer, prior, kind), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					start := a.Now()
					retry := &config.NoticeRetry{FailedAt: start.Add(-2 * time.Second), Next: start, Delay: 2 * time.Second}
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; st.NoticeRetry = retry; return nil })
					if err != nil {
						t.Fatal(err)
					}
					var puts atomic.Int32
					var recovering atomic.Bool
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut {
							puts.Add(1)
							if kind != "ack" && !recovering.Load() {
								body := `{"errcode":"M_FORBIDDEN","error":"denied"}`
								switch kind {
								case "utf8":
									body = "{\"errcode\":\"M_FORBIDDEN\",\"error\":\"\xff\"}"
								case "case":
									body = `{"ERRCODE":"M_FORBIDDEN","error":"denied"}`
								case "duplicate":
									body = `{"errcode":"M_UNKNOWN","errcode":"M_FORBIDDEN"}`
								case "partial":
									body = `{"errcode":"M_FORBIDDEN",`
								}
								w.WriteHeader(403)
								fmt.Fprint(w, body)
								return
							}
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
						resp, err := http.DefaultTransport.RoundTrip(c)
						if resp != nil {
							resp.Request = r
						}
						return resp, err
					})}
					s.client.HTTP = a.HTTP
					a.Stdout = io.Discard
					a.Stderr = io.Discard
					a.as = "alice"
					record := *ack
					record.OwnNotice = e.state("alice").Terminal.OwnNotice
					switch writer {
					case "notice":
						err = s.publishTerminalNotice(context.Background(), "deadline")
					case "manual":
						err = s.writeLiveness(context.Background(), record)
					case "wait-heartbeat":
						err = s.writeLivenessSource(context.Background(), record, "wait-heartbeat")
					case "hook":
						err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
					case "public-status":
						if exit := a.Run(context.Background(), append([]string{"--as", "alice"}, statusArgs("working")...)); exit != 0 {
							err = fmt.Errorf("status exit %d", exit)
						}
					}
					st := e.state("alice")
					if puts.Load() != 1 {
						t.Fatalf("writer did not send exactly once: %d", puts.Load())
					}
					if kind == "ack" {
						if err != nil || st.NoticeWriteUncertain || st.NoticeRetry != nil {
							t.Fatalf("genuine ACK lost: %v %+v", err, st)
						}
						return
					}
					want := true
					if kind == "forbidden" {
						want = prior
					}
					if err == nil || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || !strings.HasPrefix(st.Terminal.NoticePublication, "unavailable:") {
						t.Fatalf("strict refusal knowledge: err=%v uncertain=%v want=%v retry=%+v priorACK=%v", err, st.NoticeWriteUncertain, want, st.NoticeRetry, reflect.DeepEqual(st.Liveness, ack))
					}
					// All writers retain the bounded publication cooldown; no silent reset.
					if st.NoticeRetry.Delay < retry.Delay || st.NoticeRetry.Delay > 30*time.Second || st.NoticeRetry.Next.Before(start) {
						t.Fatalf("cooldown changed: %+v", st.NoticeRetry)
					}
					reloaded, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					reloaded.client.HTTP = a.HTTP
					if e.state("alice").NoticeWriteUncertain != want {
						t.Fatal("uncertainty did not persist")
					}
					recovering.Store(true)
					a.Now = func() time.Time { return st.NoticeRetry.Next }
					// Force a changed observation so recovery exercises the real send.
					_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = schemaNotice("false"); return nil })
					if err != nil {
						t.Fatal(err)
					}
					if err = reloaded.publishTerminalNotice(context.Background(), "deadline"); err != nil {
						t.Fatal(err)
					}
					recovered := e.state("alice")
					if recovered.NoticeWriteUncertain || recovered.NoticeRetry != nil || puts.Load() != 2 {
						t.Fatalf("genuine recovery lost: %+v puts=%d", recovered, puts.Load())
					}
				})
			}
		}
	}
}
