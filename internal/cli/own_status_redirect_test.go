package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Retained reviewer assertions through notice/manual/public/hook/heartbeat writers.
// No redirect target may be contacted, even if it would return an ACK.
// Real notice/manual writers must accept direct ACKs and recovery but must not
// promote an unrelated redirect target's JSON into an ACK for an unapplied PUT.
// Isolated HTTP and state fixtures test provenance, not server honesty.
func exerciseOwnStatusRedirectACK(t *testing.T, writers, kinds []string) {
	for _, writer := range writers {
		for _, kind := range kinds {
			for _, prior := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prior-%t", writer, kind, prior), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					start := a.Now()
					retry := &config.NoticeRetry{FailedAt: start.Add(-2 * time.Second), Next: start, Delay: 2 * time.Second}
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; st.NoticeRetry = retry; return nil })
					if err != nil {
						t.Fatal(err)
					}
					var puts, redirects, applied atomic.Int32
					var recovery atomic.Bool
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/unrelated-ack" {
							redirects.Add(1)
							want := http.MethodPut
							if kind == "301" || kind == "302" || kind == "303" {
								want = http.MethodGet
							}
							if r.Method != want {
								t.Errorf("redirect method=%s want=%s", r.Method, want)
							}
							writeJSON(w, 200, map[string]string{"event_id": "$unrelated"})
							return
						}
						if r.Method == http.MethodPut {
							puts.Add(1)
							if kind != "direct" && !recovery.Load() {
								w.Header().Set("Location", "/unrelated-ack")
								code, _ := strconv.Atoi(kind)
								w.WriteHeader(code)
								return
							}
							applied.Add(1)
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer srv.Close()
					s.client.BaseURL = srv.URL
					record := *ack
					record.OwnNotice = e.state("alice").Terminal.OwnNotice
					if writer == "notice" {
						err = s.publishTerminalNotice(context.Background(), "deadline")
					} else if writer == "manual" {
						err = s.writeLiveness(context.Background(), record)
					} else if writer == "wait-heartbeat" {
						err = s.writeLivenessSource(context.Background(), record, "wait-heartbeat")
					} else {
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
						a.Stdout = io.Discard
						a.Stderr = io.Discard
						if writer == "hook" {
							a.as = "alice"
							err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
						} else {
							exit := a.Run(context.Background(), append([]string{"--as", "alice"}, statusArgs("working")...))
							if exit != 0 {
								err = fmt.Errorf("public status exit %d", exit)
							}
						}
					}
					st := e.state("alice")
					if kind == "direct" {
						if err != nil || applied.Load() != 1 || redirects.Load() != 0 || st.NoticeWriteUncertain || st.NoticeRetry != nil || !reflect.DeepEqual(st.Liveness.OwnNotice, record.OwnNotice) {
							t.Fatalf("direct ACK control: err=%v applied=%d state=%+v", err, applied.Load(), st)
						}
						return
					}
					if puts.Load() != 1 || redirects.Load() != 0 || applied.Load() != 0 {
						t.Fatal("redirect was followed or original PUT was not driven")
					}
					var remote config.AgentStatus
					current, _, resolveErr := s.resolveRoom(context.Background(), StatusRoom)
					if resolveErr != nil {
						t.Fatal(resolveErr)
					}
					if readErr := s.client.GetState(context.Background(), current, StatusEventType, s.creds.UserID, &remote); readErr != nil {
						t.Fatal(readErr)
					}
					if !reflect.DeepEqual(remote, *ack) {
						t.Fatal("setup remote changed without original PUT")
					}
					if err == nil || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || st.Terminal.NoticePublication == "published" {
						t.Errorf("unrelated ACK trusted: err=%v uncertain=%t priorACK=%t retry=%+v publication=%q originalApplied=%d", err, st.NoticeWriteUncertain, reflect.DeepEqual(st.Liveness, ack), st.NoticeRetry, st.Terminal.NoticePublication, applied.Load())
					}
					reloaded, openErr := a.sessionAs("alice")
					if openErr != nil {
						t.Fatal(openErr)
					}
					reloaded.client.BaseURL = srv.URL
					before := puts.Load()
					_ = reloaded.publishTerminalNotice(context.Background(), "deadline")
					if after := e.state("alice"); after.Terminal.NoticePublication == "published" && !after.NoticeWriteUncertain && puts.Load() == before {
						t.Errorf("false published equality survived reload without original application")
					}
					recovery.Store(true)
					after := e.state("alice")
					when := start.Add(30 * time.Second)
					if after.NoticeRetry != nil {
						when = after.NoticeRetry.Next
					}
					a.Now = func() time.Time { return when }
					// A changed observation forces a real recovery even on the broken candidate.
					_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = schemaNotice("false"); return nil })
					if err != nil {
						t.Fatal(err)
					}
					if err = reloaded.publishTerminalNotice(context.Background(), "deadline"); err != nil {
						t.Fatal(err)
					}
					final := e.state("alice")
					remote = config.AgentStatus{}
					if err = reloaded.client.GetState(context.Background(), current, StatusEventType, s.creds.UserID, &remote); err != nil || final.NoticeWriteUncertain || final.NoticeRetry != nil || !reflect.DeepEqual(remote, *final.Liveness) {
						t.Fatalf("real recovery ACK control: %v", err)
					}
				})
			}
		}
	}
}

// Retained reviewer claim: unrelated success must not become a status ACK.
func TestReviewCR2RedirectedACK(t *testing.T) {
	exerciseOwnStatusRedirectACK(t, []string{"notice", "manual", "public-status"}, []string{"direct", "301", "302", "307"})
}

// Retained reviewer claim: legitimate heartbeat/hook writers share this boundary.
func TestReviewCR2HeartbeatRedirectedACK(t *testing.T) {
	exerciseOwnStatusRedirectACK(t, []string{"wait-heartbeat", "hook"}, []string{"direct", "302", "307"})
}

// Retained reviewer claim: method-preserving and method-changing redirects agree.
func TestReviewCR2OtherRedirectCodes(t *testing.T) {
	exerciseOwnStatusRedirectACK(t, []string{"notice", "manual", "public-status"}, []string{"direct", "303", "308"})
}

// A Location naming the exact original URL still must not be followed.
// Scoped no-follow overrides caller following/refusal callbacks without mutation;
// direct HTTP ACK and real later recovery remain valid. Only isolated HTTP.
func TestReviewCR2ACKPolicy(t *testing.T) {
	for _, kind := range []string{"direct", "return-original", "stop", "refuse"} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior-%t", kind, prior), func(t *testing.T) {
				e, _, s := noticeDeadlineFixture(t)
				ack := e.state("alice").Liveness
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
				if err != nil {
					t.Fatal(err)
				}
				var calls, other, applied, callbacks atomic.Int32
				var originalURI, sentURL atomic.Value
				var recovering atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/other" {
						other.Add(1)
						w.Header().Set("Location", originalURI.Load().(string))
						w.WriteHeader(307)
						return
					}
					if r.Method == http.MethodPut {
						n := calls.Add(1)
						if n == 1 {
							originalURI.Store(r.URL.RequestURI())
						}
						if kind != "direct" && !recovering.Load() {
							if n == 1 {
								location := "http://" + r.Host + r.URL.RequestURI()
								if location != sentURL.Load().(string) {
									t.Error("setup: Location differs from exact original request URL")
								}
								w.Header().Set("Location", location)
								w.WriteHeader(307)
							} else {
								writeJSON(w, 200, map[string]string{"event_id": "$from-second-request"})
							}
							return
						}
						applied.Add(1)
					}
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				s.client.BaseURL = srv.URL
				policyErr := errors.New("synthetic caller redirect refusal")
				s.client.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut {
						sentURL.Store(r.URL.String())
					}
					return http.DefaultTransport.RoundTrip(r)
				}), CheckRedirect: func(next *http.Request, via []*http.Request) error {
					callbacks.Add(1)
					if kind == "stop" {
						return http.ErrUseLastResponse
					}
					if kind == "refuse" {
						return policyErr
					}
					if len(via) == 2 && next.URL.String() != via[0].URL.String() {
						t.Error("setup final URL differs from original")
					}
					return nil
				}}
				err = s.publishTerminalNotice(context.Background(), "deadline")
				st := e.state("alice")
				if kind == "direct" {
					if err != nil || st.NoticeWriteUncertain || st.NoticeRetry != nil || applied.Load() != 1 || callbacks.Load() != 0 {
						t.Fatal("direct ACK policy control failed")
					}
					return
				}
				if calls.Load() != 1 || other.Load() != 0 || callbacks.Load() != 0 || applied.Load() != 0 {
					t.Fatal("own-status no-follow policy was bypassed")
				}
				if err == nil || !st.NoticeWriteUncertain || st.NoticeRetry == nil || !reflect.DeepEqual(st.Liveness, ack) || st.Terminal.NoticePublication == "published" {
					t.Errorf("redirect ACK policy knowledge: kind=%s err=%v uncertain=%t priorACK=%t retry=%+v publication=%q originalApplied=%d", kind, err, st.NoticeWriteUncertain, reflect.DeepEqual(st.Liveness, ack), st.NoticeRetry, st.Terminal.NoticePublication, applied.Load())
				}
				if s.client.HTTP.CheckRedirect == nil {
					t.Fatal("shared caller policy was mutated")
				}
				if !errors.Is(s.client.HTTP.CheckRedirect(&http.Request{}, nil), func() error {
					if kind == "refuse" {
						return policyErr
					}
					if kind == "stop" {
						return http.ErrUseLastResponse
					}
					return nil
				}()) {
					t.Fatal("shared caller policy changed")
				}
				// A genuine manual ACK remains available during automatic cooldown.
				recovering.Store(true)
				s.client.HTTP = &http.Client{}
				if err = s.writeLiveness(context.Background(), *ack); err != nil {
					t.Fatal(err)
				}
				st = e.state("alice")
				if st.NoticeWriteUncertain || st.NoticeRetry != nil || applied.Load() != 1 {
					t.Fatal("recovery policy control failed")
				}
			})
		}
	}
}

// Changed observations at one clock instant must not convert unrelated ACK
// responses into a cooldown bypass. Direct genuine ACKs may still publish each
// changed observation. This counts actual synthetic resolve/PUT/redirect calls.
func TestReviewCR2RedirectACKTraffic(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct-%t", direct), func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			ack := e.state("alice").Liveness
			now := a.Now()
			a.Now = func() time.Time { return now }
			var requests, puts, applied atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/unrelated-ack" {
					writeJSON(w, 200, map[string]string{"event_id": "$unrelated"})
					return
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
					if !direct {
						w.Header().Set("Location", "/unrelated-ack")
						w.WriteHeader(302)
						return
					}
					applied.Add(1)
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			for cycle := 0; cycle < 4; cycle++ {
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
					st.Terminal.OwnNotice.ObservedAt = now.Add(time.Duration(cycle) * time.Nanosecond).UTC().Format(time.RFC3339Nano)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				err = s.publishTerminalNotice(context.Background(), "deadline")
				st := e.state("alice")
				if !direct {
					if err == nil || requests.Load() != 2 || puts.Load() != 1 || applied.Load() != 0 || st.NoticeRetry == nil || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) {
						t.Errorf("redirect ACK cooldown traffic: cycle=%d RPC=%d originalPUT=%d applied=%d err=%v retry=%+v uncertain=%t", cycle, requests.Load(), puts.Load(), applied.Load(), err, st.NoticeRetry, st.NoticeWriteUncertain)
					}
				} else if err != nil || requests.Load() != int32(2*(cycle+1)) || applied.Load() != int32(cycle+1) || st.NoticeRetry != nil || st.NoticeWriteUncertain {
					t.Errorf("real ACK changed-observation control: cycle=%d RPC=%d applied=%d err=%v", cycle, requests.Load(), applied.Load(), err)
				}
			}
		})
	}
}

// Resolution is pre-send: a redirect must retain both prior certainty states,
// the exact ACK and the notice cooldown without sending a PUT. Recovery is a
// direct real write; isolated HTTP counters prove there is no target contact.
func TestOwnStatusResolveRedirects(t *testing.T) {
	for _, code := range []int{0, 301, 302, 303, 307, 308} {
		for _, writer := range []string{"notice", "manual", "wait-heartbeat"} {
			for _, prior := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/prior-%t", code, writer, prior), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					start := a.Now()
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.NoticeWriteUncertain = prior
						st.NoticeRetry = &config.NoticeRetry{FailedAt: start.Add(-2 * time.Second), Next: start, Delay: 2 * time.Second}
						if writer != "notice" {
							st.NoticeRetry.Next = start.Add(30 * time.Second)
							st.NoticeRetry.Delay = 30 * time.Second
							st.NoticeRetry.FailedAt = start
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					var resolves, puts, targets atomic.Int32
					var recovery atomic.Bool
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirect-resolve" {
							targets.Add(1)
							writeJSON(w, 200, map[string]string{"room_id": "!wrong:synthetic"})
							return
						}
						if strings.Contains(r.URL.Path, "/directory/room/") {
							resolves.Add(1)
							if code != 0 && !recovery.Load() {
								w.Header().Set("Location", "/redirect-resolve")
								w.WriteHeader(code)
								return
							}
						}
						if r.Method == http.MethodPut {
							puts.Add(1)
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer srv.Close()
					s.client.BaseURL = srv.URL
					before := e.state("alice")
					switch writer {
					case "notice":
						err = s.publishTerminalNotice(context.Background(), "deadline")
					case "manual":
						err = s.writeLiveness(context.Background(), *ack)
					default:
						err = s.writeLivenessSource(context.Background(), *ack, "wait-heartbeat")
					}
					st := e.state("alice")
					if code == 0 {
						if err != nil || resolves.Load() != 1 || puts.Load() != 1 || targets.Load() != 0 || st.NoticeWriteUncertain || st.NoticeRetry != nil {
							t.Fatal("direct resolution ACK control failed")
						}
						return
					}
					if err == nil || resolves.Load() != 1 || puts.Load() != 0 || targets.Load() != 0 || st.NoticeWriteUncertain != prior || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || st.Terminal.NoticePublication == "published" {
						t.Fatalf("redirected resolution changed pre-send knowledge: err=%v resolve=%d PUT=%d target=%d prior=%t uncertainty=%t", err, resolves.Load(), puts.Load(), targets.Load(), prior, st.NoticeWriteUncertain)
					}
					if writer != "notice" && !reflect.DeepEqual(st.NoticeRetry, before.NoticeRetry) {
						t.Fatal("failed explicit writer lost automatic retry")
					}
					reloaded, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					reloaded.client.BaseURL = srv.URL
					_ = reloaded.publishTerminalNotice(context.Background(), "deadline")
					if resolves.Load() != 1 || puts.Load() != 0 || targets.Load() != 0 {
						t.Fatal("resolve redirect cooldown bypassed after reload")
					}
					recovery.Store(true)
					if err = reloaded.writeLiveness(context.Background(), *ack); err != nil {
						t.Fatal(err)
					}
					st = e.state("alice")
					if st.NoticeWriteUncertain || st.NoticeRetry != nil || puts.Load() != 1 || resolves.Load() != 2 {
						t.Fatal("direct recovery failed after resolution redirect")
					}
				})
			}
		}
	}
}
