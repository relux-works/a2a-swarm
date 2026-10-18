package cli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type deliveryRoundTrip func(*http.Request) (*http.Response, error)

func (f deliveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func deliveryResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func retryArgs(command string) []string {
	args := []string{"--as", "alice", command, "--json"}
	if command == "wait" {
		args = append(args, "--timeout", "0")
	}
	return args
}

// retryWorld uses actual account_data reads and a resumed cursor. No live
// credentials or HTTP endpoints are used. Only /sync is scripted; all other
// successful requests go to the existing isolated Matrix fake.
func retryWorld(t *testing.T) (*env, string, string) {
	t.Helper()
	e, room := world(t)
	e.extra[EnvSessionID] = "retry-owner"
	e.hs.setAccountData("@alice:"+testServer, SubsType, SubList{V: 1, Subs: []Sub{{
		ID: "sub-retry", SubSelector: SubSelector{Room: room, Subject: "BUG-retry"}, CreatedAt: legacyCreatedAt,
	}}})
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = "resume"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(msg("BUG-retry recovery", nil))
	// Construct through JSON to avoid depending on anonymous SyncResponse types.
	body, _ := json.Marshal(map[string]any{"next_batch": "delivered", "rooms": map[string]any{"join": map[string]any{
		room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{{EventID: "$retry", Type: "m.room.message", Sender: "@bob:" + testServer, OriginServerTS: time.Now().UnixMilli(), Content: content}}}},
	}}})
	return e, room, string(body)
}

// Every recovery case drives App.Run -> cmdWait/cmdWatch -> waitingSync /
// waitingSubs / startWaiting / ingestWaiting. A successful nearby control,
// the original unexpected EOF, and repeated failures share the same assertions.
func TestDeliveryRetryRecovery(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		for _, source := range []string{"sync", "setup-subs", "loop-subs", "close-subs", "initial-sync"} {
			for _, failures := range []int{0, 1, 8} {
				t.Run(command+"/"+source+"/"+strconv.Itoa(failures), func(t *testing.T) {
					e, _, body := retryWorld(t)
					cursor := "resume"
					if source == "initial-sync" {
						cursor = ""
						_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = ""; return nil })
						if err != nil {
							t.Fatal(err)
						}
					}
					if source == "close-subs" {
						list := e.serverSubs("alice")
						list.Subs[0].UntilKind = "system"
						e.hs.setAccountData("@alice:"+testServer, SubsType, list)
						body = strings.Replace(body, `"BUG-retry recovery"`, `"BUG-retry recovery","ai.relux.a2a":{"kind":"system"}`, 1)
					}
					originalSubs := string(e.hs.accountDataOf("@alice:"+testServer, SubsType))
					syncs, gets, puts, injected := 0, 0, 0, 0
					var delays []time.Duration
					base := e.hs.srv.Client().Transport
					assertUnchanged := func() {
						t.Helper()
						wantCursor := cursor
						if source == "initial-sync" && syncs > failures+1 {
							wantCursor = "position"
						}
						if got := e.state("alice").NextBatch; got != wantCursor {
							t.Fatalf("failed read advanced cursor to %q, want %q", got, wantCursor)
						}
						if len(e.mailbox("alice").Pending()) != 0 || string(e.hs.accountDataOf("@alice:"+testServer, SubsType)) != originalSubs {
							t.Fatal("failed read changed mailbox/subscriptions")
						}
					}
					e.configure = func(a *App) {
						a.deliveryPause = func(ctx context.Context, d time.Duration) error {
							assertUnchanged() // also proves locks are released before backoff
							delays = append(delays, d)
							if lease := e.lease("alice"); lease == nil || lease.Holder != "retry-owner" {
								t.Fatalf("lease = %+v", lease)
							}
							// A second public consumer cannot steal this lease or poll.
							other := App{Stdout: io.Discard, Stderr: io.Discard, HTTP: a.HTTP, Now: a.Now}
							other.Getenv = func(k string) string {
								if k == EnvSessionID {
									return "competitor"
								}
								return a.Getenv(k)
							}
							if code := other.Run(ctx, retryArgs("wait")); code != ExitError {
								t.Fatalf("second consumer admitted: %d", code)
							}
							return ctx.Err()
						}
						a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
							isSync := strings.HasSuffix(r.URL.Path, "/sync")
							isSub := strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType)
							if isSync {
								syncs++
								want := cursor
								if source == "initial-sync" && syncs > failures+1 {
									want = "position"
								}
								if e.state("alice").NextBatch == "delivered" {
									want = "delivered"
								}
								if got := r.URL.Query().Get("since"); got != want {
									t.Fatalf("since = %q, want %q", got, want)
								}
							}
							if isSub && r.Method == "GET" {
								gets++
							}
							if isSub && r.Method == "PUT" {
								puts++
							}
							fail := isSync && (source == "sync" || source == "initial-sync")
							fail = fail || (isSub && r.Method == "GET" && ((source == "setup-subs") || (source == "loop-subs" && syncs == 1) || (source == "close-subs" && gets >= 3)))
							if fail && injected < failures {
								assertUnchanged()
								injected++
								return nil, io.ErrUnexpectedEOF
							}
							if isSync {
								if source == "initial-sync" && syncs == failures+1 {
									return deliveryResponse(200, `{"next_batch":"position"}`)
								}
								if e.state("alice").NextBatch == "delivered" {
									// Repeated batch exercises mailbox dedup and shown semantics.
									if syncs == failures+3 || (source != "sync" && source != "initial-sync" && syncs == 3) {
										return deliveryResponse(403, `{"errcode":"M_FORBIDDEN","error":"stop fixture"}`)
									}
								}
								return deliveryResponse(200, body)
							}
							return base.RoundTrip(r)
						})}
					}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					r := e.runCtx(ctx, retryArgs(command)...)
					if command == "wait" {
						r.ok(t)
					} else {
						r.failsWith(t, ExitError, "stop fixture")
					}
					if strings.Count(r.stdout, `"event_id":"$retry"`) != 1 || !strings.Contains(r.stdout, `"type":"sub"`) {
						t.Fatalf("expected exactly one subscribed output, got %q", r.stdout)
					}
					if e.state("alice").NextBatch != "delivered" || e.addLines("alice", "$retry") != 1 {
						t.Fatal("delivery not durable/deduplicated")
					}
					if en, ok := e.pendingEntry("alice", "$retry"); !ok || !en.Shown || e.received("alice", "$retry") {
						t.Fatal("shown/receipt contract changed")
					}
					if len(delays) != failures {
						t.Fatalf("backoffs = %v, want %d", delays, failures)
					}
					for i, got := range delays {
						want := min(100*time.Millisecond*time.Duration(1<<i), 5*time.Second)
						if got != want {
							t.Fatalf("backoff %d = %v, want %v", i, got, want)
						}
					}
					if source == "close-subs" {
						if puts != 1 || len(e.serverSubs("alice").Subs) != 0 {
							t.Fatalf("until-kind not removed once: puts=%d", puts)
						}
					} else if puts != 0 || string(e.hs.accountDataOf("@alice:"+testServer, SubsType)) != originalSubs {
						t.Fatal("subscriptions overwritten")
					}
				})
			}
		}
	}
}

// Permanent errors must stop at the failing public read, without a second
// attempt, mutation, cursor advance, or output. Transient classes reach a
// nearby valid recovery using the same real Matrix client source paths.
func TestDeliveryRetryClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		body   string
		retry  bool
	}{
		{name: "EOF", err: io.EOF, retry: true},
		{name: "unexpectedEOF", err: io.ErrUnexpectedEOF, retry: true},
		{name: "reset", err: syscall.ECONNRESET, retry: true},
		{name: "aborted", err: syscall.ECONNABORTED, retry: true},
		{name: "pipe", err: syscall.EPIPE, retry: true},
		{name: "temporary", err: &net.DNSError{Err: "fixture temporary", IsTemporary: true}, retry: true},
		{name: "transport-timeout", err: &net.DNSError{Err: "fixture timeout", IsTimeout: true}, retry: true},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "dns-config", err: &net.DNSError{Err: "fixture missing", IsNotFound: true}},
		{name: "certificate", err: x509.UnknownAuthorityError{}},
		{name: "generic", err: errors.New("permanent fixture")},
		{name: "bad-json", status: 200, body: `{"broken":!}`},
		{name: "truncated-response", status: 200, body: `{"v":`, retry: true},
		{name: "auth", status: 401, body: `{"errcode":"M_UNKNOWN_TOKEN"}`},
		{name: "forbidden", status: 403, body: `{"errcode":"M_FORBIDDEN"}`},
		{name: "config", status: 400, body: `{"errcode":"M_INVALID_PARAM"}`},
		{name: "permanent-code-503", status: 503, body: `{"errcode":"M_FORBIDDEN"}`},
		{name: "not-implemented", status: 501, body: `{}`},
	}
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		cases = append(cases, struct {
			name   string
			err    error
			status int
			body   string
			retry  bool
		}{name: http.StatusText(status), status: status, body: `{}`, retry: true})
	}
	for _, command := range []string{"wait", "watch"} {
		for _, source := range []string{"sync", "subs"} {
			for _, tc := range cases {
				t.Run(command+"/"+source+"/"+tc.name, func(t *testing.T) {
					e, _, body := retryWorld(t)
					attempts, pauses, syncs := 0, 0, 0
					base := e.hs.srv.Client().Transport
					e.configure = func(a *App) {
						a.deliveryPause = func(context.Context, time.Duration) error { pauses++; return nil }
						a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
							isSync := strings.HasSuffix(r.URL.Path, "/sync")
							if isSync {
								syncs++
							}
							target := isSync && source == "sync" || strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) && source == "subs" && syncs == 1
							if target {
								attempts++
								if attempts == 1 {
									if tc.err != nil {
										return nil, tc.err
									}
									return deliveryResponse(tc.status, tc.body)
								}
							}
							if isSync {
								if e.state("alice").NextBatch == "delivered" {
									return deliveryResponse(403, `{"error":"stop fixture"}`)
								}
								return deliveryResponse(200, body)
							}
							return base.RoundTrip(r)
						})}
					}
					r := e.run(retryArgs(command)...)
					if tc.retry {
						if command == "wait" {
							r.ok(t)
						} else {
							r.failsWith(t, ExitError, "stop fixture")
						}
						if pauses != 1 || !strings.Contains(r.stdout, `"event_id":"$retry"`) {
							t.Fatalf("transient not recovered: attempts=%d pauses=%d output=%q", attempts, pauses, r.stdout)
						}
					} else {
						if r.code != ExitError || attempts != 1 || pauses != 0 || r.stdout != "" || e.state("alice").NextBatch != "resume" || len(e.mailbox("alice").Pending()) != 0 {
							t.Fatalf("permanent retried/changed state: %+v attempts=%d pauses=%d", r, attempts, pauses)
						}
					}
				})
			}
		}
	}
}

func TestDeliveryRetryStopsDuringBackoff(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		for _, source := range []string{"sync", "setup-subs", "loop-subs"} {
			for _, stop := range []string{"cancel", "parent-deadline", "finite"} {
				if stop == "finite" && command == "watch" {
					continue
				}
				t.Run(command+"/"+source+"/"+stop, func(t *testing.T) {
					e, _, _ := retryWorld(t)
					var ctx context.Context
					var cancel context.CancelFunc
					if stop == "parent-deadline" {
						ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
					} else {
						ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
					}
					defer cancel()
					calls, syncs := 0, 0
					base := e.hs.srv.Client().Transport
					e.configure = func(a *App) {
						a.deliveryPause = func(ctx context.Context, delay time.Duration) error {
							if delay != 100*time.Millisecond {
								t.Fatalf("initial backoff=%v", delay)
							}
							return deliveryPause(ctx, 5*time.Second)
						}
						a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
							isSync := strings.HasSuffix(r.URL.Path, "/sync")
							if isSync {
								syncs++
							}
							if isSync && source == "sync" || strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) && (source == "setup-subs" || source == "loop-subs" && syncs == 1) {
								calls++
								if stop == "cancel" {
									time.AfterFunc(15*time.Millisecond, cancel)
								}
								return nil, io.ErrUnexpectedEOF
							}
							if isSync {
								return deliveryResponse(200, `{"next_batch":"next"}`)
							}
							return base.RoundTrip(r)
						})}
					}
					args := retryArgs(command)
					if stop == "finite" {
						args[len(args)-1] = "500ms"
					}
					start := time.Now()
					r := e.runCtx(ctx, args...)
					if stop == "finite" {
						r.ok(t)
						if r.stdout != "{\"type\":\"timeout\"}\n" {
							t.Fatalf("timeout output=%q", r.stdout)
						}
					} else if command == "watch" {
						r.ok(t)
					} else {
						r.failsWith(t, ExitError, "context")
					}
					if calls != 1 || time.Since(start) > 3*time.Second || e.state("alice").NextBatch != "resume" || len(e.mailbox("alice").Pending()) != 0 {
						t.Fatalf("stop did not bound backoff: calls=%d elapsed=%v", calls, time.Since(start))
					}
				})
			}
		}
	}
}

// Transient-shaped local/persistence/output failures are not read failures.
// Replay would admit duplicate mutations/output. A fresh invocation recovers
// the same pending batch without advancing past an unpersisted entry.
func TestDeliveryRetryPersistenceIsTerminal(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		for _, fault := range []string{"before-append", "after-append", "output", "subscription-write", "bad-state", "bad-subs-version"} {
			t.Run(command+"/"+fault, func(t *testing.T) {
				e, _, body := retryWorld(t)
				if fault == "subscription-write" {
					list := e.serverSubs("alice")
					list.Subs[0].UntilKind = "system"
					e.hs.setAccountData("@alice:"+testServer, SubsType, list)
					body = strings.Replace(body, `"BUG-retry recovery"`, `"BUG-retry recovery","ai.relux.a2a":{"kind":"system"}`, 1)
				}
				if fault == "bad-subs-version" {
					e.hs.setAccountData("@alice:"+testServer, SubsType, SubList{V: 2})
				}
				if fault == "bad-state" {
					if err := os.WriteFile(e.paths().StateFile("alice"), []byte(`!`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				syncs, pauses, writes, appendCalls := 0, 0, 0, 0
				base := e.hs.srv.Client().Transport
				e.configure = func(a *App) {
					a.deliveryPause = func(context.Context, time.Duration) error { pauses++; return nil }
					if fault == "before-append" {
						a.beforeAppend = func() error {
							appendCalls++
							if appendCalls == 1 {
								return io.ErrUnexpectedEOF
							}
							return nil
						}
					}
					if fault == "after-append" {
						a.afterAppend = func() error {
							appendCalls++
							if appendCalls == 1 {
								return io.ErrUnexpectedEOF
							}
							return nil
						}
					}
					if fault == "output" {
						a.Stdout = deliveryFailWriter{}
					}
					a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/sync") {
							syncs++
							if syncs > 1 {
								return deliveryResponse(403, `{"error":"stop fixture"}`)
							}
							return deliveryResponse(200, body)
						}
						if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
							writes++
							if writes == 1 {
								return nil, io.ErrUnexpectedEOF
							}
							return base.RoundTrip(r)
						}
						return base.RoundTrip(r)
					})}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				r := e.runCtx(ctx, retryArgs(command)...)
				if r.code != ExitError || pauses != 0 || syncs > 1 || writes > 1 || appendCalls > 1 {
					t.Fatalf("terminal failure retried: %+v syncs=%d writes=%d append=%d pauses=%d", r, syncs, writes, appendCalls, pauses)
				}
				if fault == "bad-state" {
					return
				}
				wantCursor := "resume"
				if fault == "output" {
					wantCursor = "delivered"
				}
				if e.state("alice").NextBatch != wantCursor {
					t.Fatal("failed persistence advanced cursor")
				}
				_, present := e.pendingEntry("alice", "$retry")
				if want := fault == "after-append" || fault == "output" || fault == "subscription-write"; present != want {
					t.Fatalf("durable mailbox present=%v want=%v", present, want)
				}
				if fault == "bad-subs-version" {
					return
				}
				if en, ok := e.pendingEntry("alice", "$retry"); ok && en.Shown {
					t.Fatal("failed output marked shown")
				}
				// Fresh run: same entry, same cursor source, exactly one durable add.
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/sync") {
							return deliveryResponse(200, body)
						}
						return base.RoundTrip(r)
					})}
				}
				r = e.run(retryArgs("wait")...).ok(t)
				if strings.Count(r.stdout, `"event_id":"$retry"`) != 1 || e.addLines("alice", "$retry") != 1 {
					t.Fatalf("resume lost/duplicated batch: %q", r.stdout)
				}
			})
		}
	}
}

type deliveryFailWriter struct{}

func (deliveryFailWriter) Write([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDeliveryRetryEarlyRequestDeadlineIsTerminal(t *testing.T) {
	e, _, _ := retryWorld(t)
	calls, pauses := 0, 0
	base := e.hs.srv.Client().Transport
	e.configure = func(a *App) {
		a.deliveryPause = func(context.Context, time.Duration) error { pauses++; return nil }
		a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/sync") {
				calls++
				return nil, context.DeadlineExceeded
			}
			return base.RoundTrip(r)
		})}
	}
	r := e.as("alice", "wait", "--timeout", "500ms").failsWith(t, ExitError, "context deadline exceeded")
	if calls != 1 || pauses != 0 || r.stdout != "" || e.state("alice").NextBatch != "resume" {
		t.Fatalf("early request deadline became success/retry: %+v calls=%d pauses=%d", r, calls, pauses)
	}
}

// The production timer, rather than only the injectable schedule, prevents a
// hot retry loop. A successful empty poll resets the next failure's delay.
func TestDeliveryRetryRealPacingAndReset(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		t.Run(command, func(t *testing.T) {
			e, _, body := retryWorld(t)
			calls := 0
			var at []time.Time
			var delays []time.Duration
			base := e.hs.srv.Client().Transport
			e.configure = func(a *App) {
				a.deliveryPause = func(ctx context.Context, delay time.Duration) error {
					delays = append(delays, delay)
					return deliveryPause(ctx, delay)
				}
				a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(r.URL.Path, "/sync") {
						return base.RoundTrip(r)
					}
					calls++
					at = append(at, time.Now())
					switch calls {
					case 1, 2, 4:
						return nil, io.ErrUnexpectedEOF
					case 3:
						return deliveryResponse(200, `{"next_batch":"resume"}`)
					case 5:
						return deliveryResponse(200, body)
					default:
						return deliveryResponse(403, `{"error":"stop fixture"}`)
					}
				})}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r := e.runCtx(ctx, retryArgs(command)...)
			if command == "wait" {
				r.ok(t)
			} else {
				r.failsWith(t, ExitError, "stop fixture")
			}
			if strings.Count(r.stdout, `"event_id":"$retry"`) != 1 || len(at) < 5 {
				t.Fatalf("paced recovery output=%q calls=%d", r.stdout, calls)
			}
			if len(delays) != 3 || delays[0] != 100*time.Millisecond || delays[1] != 200*time.Millisecond || delays[2] != 100*time.Millisecond {
				t.Fatalf("backoff did not reset: %v", delays)
			}
			for _, pair := range []struct {
				from, to int
				floor    time.Duration
			}{{0, 1, deliveryRetryInitial}, {1, 2, 2 * deliveryRetryInitial}, {3, 4, deliveryRetryInitial}} {
				if elapsed := at[pair.to].Sub(at[pair.from]); elapsed < pair.floor {
					t.Fatalf("unpaced retries %d..%d: %v < %v", pair.from, pair.to, elapsed, pair.floor)
				}
			}
		})
	}
}

// A response read that blocks in setup or sync obeys the actual finite
// deadline, and a failed timeout output is never reported as success.
func TestDeliveryRetryFiniteRequestDeadline(t *testing.T) {
	for _, source := range []string{"setup-subs", "sync"} {
		for _, outputFails := range []bool{false, true} {
			t.Run(source+"/output-fails-"+strconv.FormatBool(outputFails), func(t *testing.T) {
				e, _, _ := retryWorld(t)
				calls := 0
				base := e.hs.srv.Client().Transport
				e.configure = func(a *App) {
					if outputFails {
						a.Stdout = deliveryFailWriter{}
					}
					a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
						if source == "sync" && strings.HasSuffix(r.URL.Path, "/sync") || source == "setup-subs" && strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
							calls++
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						return base.RoundTrip(r)
					})}
				}
				start := time.Now()
				r := e.as("alice", "wait", "--timeout", "500ms")
				if outputFails {
					r.failsWith(t, ExitError, "unexpected EOF")
				} else {
					r.ok(t)
					if r.stdout != "timeout\n" {
						t.Fatalf("timeout output=%q", r.stdout)
					}
				}
				if calls != 1 || time.Since(start) > 3*time.Second || e.state("alice").NextBatch != "resume" {
					t.Fatalf("request not deadline bounded: calls=%d", calls)
				}
			})
		}
	}
}

func TestDeliveryRetryObservesSubscriptionUpdate(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		t.Run(command, func(t *testing.T) {
			e, room, body := retryWorld(t)
			e.hs.setAccountData("@alice:"+testServer, SubsType, SubList{V: 1})
			syncs, reads, pauses := 0, 0, 0
			base := e.hs.srv.Client().Transport
			e.configure = func(a *App) {
				a.deliveryPause = func(context.Context, time.Duration) error { pauses++; return nil }
				a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						syncs++
						if syncs > 1 {
							return deliveryResponse(403, `{"error":"stop fixture"}`)
						}
						return deliveryResponse(200, body)
					}
					if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) && r.Method == "GET" {
						reads++
						if reads == 2 {
							// The initial subscription list is stale; the retry must read
							// the new server value rather than treating failure as absence.
							e.hs.setAccountData("@alice:"+testServer, SubsType, SubList{V: 1, Subs: []Sub{{ID: "new-sub", SubSelector: SubSelector{Room: room, Subject: "BUG-retry"}, CreatedAt: legacyCreatedAt}}})
							return nil, io.ErrUnexpectedEOF
						}
					}
					return base.RoundTrip(r)
				})}
			}
			r := e.run(retryArgs(command)...)
			if command == "wait" {
				r.ok(t)
			} else {
				r.failsWith(t, ExitError, "stop fixture")
			}
			if pauses != 1 || reads != 3 || !strings.Contains(r.stdout, `"sub":"new-sub"`) || e.addLines("alice", "$retry") != 1 {
				t.Fatalf("subscription update lost: pauses=%d reads=%d output=%q", pauses, reads, r.stdout)
			}
		})
	}
}

func TestDeliveryRetryLeaseLossIsTerminal(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		t.Run(command, func(t *testing.T) {
			e, _, body := retryWorld(t)
			calls := 0
			base := e.hs.srv.Client().Transport
			e.configure = func(a *App) {
				a.deliveryPause = func(context.Context, time.Duration) error {
					_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
						st.Lease = &config.Lease{Holder: "replacement", Host: "fixture-foreign-host", PID: 1, Expires: time.Now().Add(time.Hour)}
						return nil
					})
					return err
				}
				a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						calls++
						if calls == 1 {
							return nil, io.ErrUnexpectedEOF
						}
						if calls == 2 {
							return deliveryResponse(200, body)
						}
						return deliveryResponse(403, `{"error":"stop fixture"}`)
					}
					return base.RoundTrip(r)
				})}
			}
			r := e.run(retryArgs(command)...).failsWith(t, ExitError, "held by replacement")
			if calls != 1 || r.stdout != "" || e.state("alice").NextBatch != "resume" || e.lease("alice").Holder != "replacement" {
				t.Fatalf("continued without lease: calls=%d output=%q", calls, r.stdout)
			}
		})
	}
}
