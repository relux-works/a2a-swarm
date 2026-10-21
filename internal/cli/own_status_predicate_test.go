package cli

import (
	"context"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type predicateTransport func(*http.Request) (*http.Response, error)

func (f predicateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type predicateReadError struct {
	data     string
	cancel   context.CancelFunc
	cleanEOF bool
}

func (b *predicateReadError) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.cancel != nil {
		b.cancel()
	}
	if b.cleanEOF {
		return n, nil
	}
	return n, io.ErrUnexpectedEOF
}
func (b *predicateReadError) Close() error { return nil }

// Both shape adapters run through writeLiveness -> putOwnStatus, including
// persisted prior knowledge and real HTTP recovery. Synthetic body faults are
// deterministic; this cannot detect a server lying with a valid response ID.
func TestOwnStatusEvidencePredicate(t *testing.T) {
	for _, shape := range []string{"put", "resolve"} {
		for _, kind := range []string{
			"direct", "created", "whitespace", "server", "missing", "null", "empty", "wrong-type", "wrong-shape", "trailing-value", "trailing-garbage", "array", "short", "read-error", "cancel", "cancel-eof", "different-url", "different-method", "informational",
		} {
			for _, prior := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/prior-%t", shape, kind, prior), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.NoticeWriteUncertain = prior
						st.NoticeRetry = &config.NoticeRetry{FailedAt: a.Now(), Next: a.Now().Add(time.Second), Delay: time.Second}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					puts := 0
					recovery := false
					s.client.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						isPut := r.Method == http.MethodPut
						if isPut {
							puts++
						}
						resp, err := http.DefaultTransport.RoundTrip(r)
						if err != nil {
							return nil, err
						}
						if recovery || isPut != (shape == "put") {
							return resp, nil
						}
						raw, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						if err != nil {
							return nil, err
						}
						field := "event_id"
						other := "room_id"
						if shape == "resolve" {
							field, other = other, field
						}
						data := string(raw)
						switch kind {
						case "created":
							resp.StatusCode = 201
						case "whitespace":
							data = " \n" + data + " \t"
						case "server":
							resp.StatusCode = 500
							data = `{"errcode":"M_FORBIDDEN"}`
						case "missing":
							data = "{}"
						case "null":
							data = "null"
						case "empty":
							data = fmt.Sprintf(`{"%s":""}`, field)
						case "wrong-type":
							data = fmt.Sprintf(`{"%s":3}`, field)
						case "wrong-shape":
							data = fmt.Sprintf(`{"%s":"other"}`, other)
						case "trailing-value":
							data += " {}"
						case "trailing-garbage":
							data += "!"
						case "array":
							data = "[" + data + "]"
						case "different-url":
							rr := r.Clone(r.Context())
							u := *r.URL
							u.Path = "/other"
							rr.URL = &u
							resp.Request = rr
						case "different-method":
							rr := r.Clone(r.Context())
							rr.Method = http.MethodPost
							resp.Request = rr
						case "informational":
							resp.StatusCode = 101
						}
						resp.ContentLength = int64(len(data))
						resp.Body = io.NopCloser(strings.NewReader(data))
						if kind == "short" {
							resp.ContentLength++
						}
						if kind == "read-error" {
							resp.Body = &predicateReadError{data: data}
						}
						if kind == "cancel" || kind == "cancel-eof" {
							resp.Body = &predicateReadError{data: data, cancel: cancel, cleanEOF: kind == "cancel-eof"}
						}
						return resp, nil
					})}
					err = s.writeLiveness(ctx, *ack)
					st := e.state("alice")
					valid := kind == "direct" || kind == "created" || kind == "whitespace"
					if valid {
						if err != nil || st.NoticeWriteUncertain || st.NoticeRetry != nil || puts != 1 {
							t.Fatalf("genuine predicate control: %v", err)
						}
						return
					}
					uncertain := prior || shape == "put"
					if err == nil || st.NoticeWriteUncertain != uncertain || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeRetry == nil || st.Terminal.NoticePublication == "published" || shape == "resolve" && puts != 0 {
						t.Fatalf("predicate evidence accepted: shape=%s kind=%s err=%v uncertain=%t want=%t puts=%d retry=%v", shape, kind, err, st.NoticeWriteUncertain, uncertain, puts, st.NoticeRetry)
					}
					reloaded, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					reloaded.client = s.client
					persisted := e.state("alice")
					if persisted.NoticeWriteUncertain != uncertain || !reflect.DeepEqual(persisted.Liveness, ack) {
						t.Fatal("predicate knowledge lost on reload")
					}
					recovery = true
					if err = reloaded.writeLiveness(context.Background(), *ack); err != nil {
						t.Fatal(err)
					}
					st = e.state("alice")
					if st.NoticeWriteUncertain || st.NoticeRetry != nil {
						t.Fatal("real predicate recovery failed")
					}
				})
			}
		}
	}
}

// A decoded direct403 still needs complete framing before it can restore prior
// certainty. A synthetic clean EOF with a mismatched declared length is unknown.
func TestOwnStatusRefusalDeclaredLength(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("short-%t/prior-%t", short, prior), func(t *testing.T) {
				e, _, s := noticeDeadlineFixture(t)
				ack := e.state("alice").Liveness
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
				if err != nil {
					t.Fatal(err)
				}
				s.client.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodPut {
						return http.DefaultTransport.RoundTrip(r)
					}
					body := `{"errcode":"M_FORBIDDEN"}`
					length := int64(len(body))
					if short {
						length++
					}
					return &http.Response{StatusCode: 403, Header: make(http.Header), Request: r, ContentLength: length, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				err = s.writeLiveness(context.Background(), *ack)
				st := e.state("alice")
				if err == nil || st.NoticeWriteUncertain != (prior || short) || !reflect.DeepEqual(st.Liveness, ack) {
					t.Fatal("refusal length evidence accepted")
				}
			})
		}
	}
}
