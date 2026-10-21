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

// Probe actual own writers with a direct status response that is incomplete or
// does not contain an event ACK. A real direct write/recovery is the control.
// This does not claim detection of a server fabricating a valid event_id.
func TestReviewCR3ACKBoundary(t *testing.T) {
	for _, kind := range []string{"direct", "empty-object", "null", "empty-event", "short-body", "malformed-tail"} {
		for _, writer := range []string{"notice", "manual", "public-status", "hook", "wait-heartbeat"} {
			for _, prior := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prior-%t", kind, writer, prior), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					start := a.Now()
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.NoticeWriteUncertain = prior
						st.NoticeRetry = &config.NoticeRetry{FailedAt: start.Add(-2 * time.Second), Next: start, Delay: 2 * time.Second}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					var puts, applied atomic.Int32
					var recovery atomic.Bool
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut {
							puts.Add(1)
							if kind != "direct" && !recovery.Load() {
								body := map[string]string{"empty-object": "{}", "null": "null", "empty-event": `{"event_id":""}`, "short-body": `{"event_id":"$partial"}`, "malformed-tail": `{"event_id":"$partial"}!`}[kind]
								if kind == "short-body" {
									w.Header().Set("Content-Length", fmt.Sprint(len(body)+20))
								}
								w.WriteHeader(200)
								fmt.Fprint(w, body)
								return
							}
							applied.Add(1)
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer srv.Close()
					s.client.BaseURL = srv.URL
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
					a.as = "alice"
					a.Stdout = io.Discard
					a.Stderr = io.Discard
					call := func() error {
						switch writer {
						case "notice":
							return s.publishTerminalNotice(context.Background(), "deadline")
						case "manual":
							return s.writeLiveness(context.Background(), *ack)
						case "wait-heartbeat":
							return s.writeLivenessSource(context.Background(), *ack, "wait-heartbeat")
						case "hook":
							return a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
						default:
							if code := a.Run(context.Background(), append([]string{"--as", "alice"}, statusArgs("working")...)); code != 0 {
								return fmt.Errorf("status exit %d", code)
							}
							return nil
						}
					}
					err = call()
					st := e.state("alice")
					if kind == "direct" {
						if err != nil || st.NoticeWriteUncertain || st.NoticeRetry != nil || applied.Load() != 1 {
							t.Fatal("direct ACK control")
						}
						return
					}
					if err == nil || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || st.Terminal.NoticePublication == "published" {
						t.Errorf("incomplete ACK promoted: kind=%s writer=%s err=%v uncertain=%t exactPrior=%t retry=%v publication=%s applied=%d", kind, writer, err, st.NoticeWriteUncertain, reflect.DeepEqual(st.Liveness, ack), st.NoticeRetry, st.Terminal.NoticePublication, applied.Load())
					}
					reloaded, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					reloaded.client.BaseURL = srv.URL
					persisted := e.state("alice")
					if !persisted.NoticeWriteUncertain || !reflect.DeepEqual(persisted.Liveness, ack) {
						t.Error("incomplete ACK persisted false knowledge")
					}
					recovery.Store(true)
					if err = reloaded.writeLiveness(context.Background(), *ack); err != nil {
						t.Fatal(err)
					}
					final := e.state("alice")
					if final.NoticeWriteUncertain || final.NoticeRetry != nil || applied.Load() != 1 {
						t.Error("genuine recovery control")
					}
				})
			}
		}
	}
}

type reviewCR3ReadBody struct {
	data   string
	cancel context.CancelFunc
}

func (b *reviewCR3ReadBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	b.cancel()
	return n, context.Canceled
}
func (b *reviewCR3ReadBody) Close() error { return nil }

// Cancellation delivered with the success bytes must not silently become a
// successful own write. Transport delegates resolver to the isolated HTTP server.
func TestReviewCR3ACKCancelledRead(t *testing.T) {
	for _, writer := range []string{"notice", "manual"} {
		t.Run(writer, func(t *testing.T) {
			e, _, s := noticeDeadlineFixture(t)
			ack := e.state("alice").Liveness
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.client.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/") {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: &reviewCR3ReadBody{data: `{"event_id":"$interrupted"}`, cancel: cancel}}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			var err error
			if writer == "notice" {
				err = s.publishTerminalNotice(ctx, "deadline")
			} else {
				err = s.writeLiveness(ctx, *ack)
			}
			st := e.state("alice")
			if ctx.Err() == nil {
				t.Fatal("cancel did not run")
			}
			if err == nil || !st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness, ack) {
				t.Errorf("cancelled ACK promoted: err=%v uncertain=%t exactPrior=%t publication=%s", err, st.NoticeWriteUncertain, reflect.DeepEqual(st.Liveness, ack), st.Terminal.NoticePublication)
			}
		})
	}
}
