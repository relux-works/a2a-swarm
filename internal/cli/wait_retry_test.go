package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type waitRoundTrip func(*http.Request) (*http.Response, error)

func (f waitRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitResponse(r *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
}

func waitRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// No listener or live account: the existing HTTP seam supplies every response.
func waitRetryApp(t *testing.T, since string, sync waitRoundTrip) (*App, config.Paths, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	p := config.Paths{Base: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(p.AgentFile("alice")), 0o700); err != nil {
		t.Fatal(err)
	}
	creds, err := json.Marshal(config.Credentials{UserID: "@alice:test.local", AccessToken: "synthetic-token", Homeserver: "http://wait.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		p.AgentFile("alice"): creds,
		p.EnvFile():          []byte("A2A_MATRIX_URL=http://wait.invalid\nA2A_MATRIX_SERVER_NAME=test.local\n"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.SaveState(p, "alice", config.State{NextBatch: since}); err != nil {
		t.Fatal(err)
	}
	out, errb := new(bytes.Buffer), new(bytes.Buffer)
	a := NewApp()
	a.Stdout, a.Stderr = out, errb
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return p.Base
		}
		return ""
	}
	a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/sync":
			return sync(r)
		case strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType):
			return waitResponse(r, 200, `{"v":1,"subs":[]}`)
		case strings.Contains(r.URL.Path, "/state/m.room.canonical_alias"):
			return waitResponse(r, 200, `{"alias":"#proj:test.local"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return waitResponse(r, 400, `{"errcode":"M_INVALID_PARAM"}`)
		}
	})}
	return a, p, out, errb
}

const waitValidEvent = `{"next_batch":"delivered","rooms":{"join":{"!proj:test.local":{"timeline":{"events":[{"event_id":"$neighbor","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local recovered"}}]}}}}}`

func waitRetryState(t *testing.T, p config.Paths, want string) {
	t.Helper()
	st, err := config.LoadState(p, "alice")
	if err != nil || st.NextBatch != want {
		t.Fatalf("cursor = %q, error %v; want %q", st.NextBatch, err, want)
	}
}

// A short wait still takes its initial position after successful setup, without replaying history.
func TestWaitRetryInitialPositionBeforeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		a, p, out, errb := waitRetryApp(t, "", func(r *http.Request) (*http.Response, error) {
			calls++
			return waitResponse(r, 200, `{"next_batch":"positioned"}`)
		})
		base := a.HTTP.Transport
		a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
				time.Sleep(2 * time.Millisecond)
			}
			return base.RoundTrip(r)
		})
		if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "1ms"}); code != ExitOK || out.String() != "timeout\n" || calls != 1 {
			t.Fatalf("initial position: code=%d sync=%d stdout=%q stderr=%q", code, calls, out, errb)
		}
		waitRetryState(t, p, "positioned")
	})
}

// Two outages must reach the valid neighbor in this invocation, from the same cursor.
func TestWaitRetryTransientNeighbor(t *testing.T) {
	for _, cold := range []bool{false, true} {
		name := "positioned"
		if cold {
			name = "initial-position"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				rearming := false
				var at []time.Time
				since := "seed"
				if cold {
					since = ""
				}
				a, p, out, errb := waitRetryApp(t, since, func(r *http.Request) (*http.Response, error) {
					if rearming {
						if got := r.URL.Query().Get("since"); got != "delivered" {
							t.Fatalf("re-arm since = %q", got)
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					calls++
					at = append(at, time.Now())
					if got := r.URL.Query().Get("since"); got != since {
						t.Fatalf("request %d since = %q, want %q", calls, got, since)
					}
					switch calls {
					case 1:
						return nil, waitRefused()
					case 2:
						return waitResponse(r, 503, `{"errcode":"M_UNAVAILABLE"}`)
					case 3:
						if cold {
							since = "seed"
							return waitResponse(r, 200, `{"next_batch":"seed"}`)
						}
						return waitResponse(r, 200, waitValidEvent)
					default:
						return waitResponse(r, 200, waitValidEvent)
					}
				})
				if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "2m"}); code != ExitOK {
					t.Fatalf("wait returned before recovery: code %d, %s", code, errb)
				}
				wantCalls := 3
				if cold {
					wantCalls++
				}
				if calls != wantCalls || strings.Count(out.String(), "$neighbor") != 1 || !strings.Contains(errb.String(), shownHint) {
					t.Fatalf("recovery: calls=%d, stdout=%q, stderr=%q", calls, out, errb)
				}
				if at[1].Sub(at[0]) != 5*time.Second || at[2].Sub(at[1]) != 30*time.Second {
					t.Fatalf("retry timing: %v", at)
				}
				waitRetryState(t, p, "delivered")
				mb, err := config.OpenMailbox(p, "alice")
				if err != nil || len(mb.Pending()) != 1 || !mb.Pending()[0].Shown {
					t.Fatalf("wait must retain one shown, unreceived entry: %v, %v", mb, err)
				}
				out.Reset()
				rearming = true
				if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "1s"}); code != ExitOK || out.String() != "timeout\n" {
					t.Fatalf("re-arm re-emitted entry: code=%d stdout=%q stderr=%q", code, out, errb)
				}
			})
		})
	}
}

// The cap is exercised with virtual timers; a successful empty sync resets the next outage.
func TestWaitRetryBoundedTimingAndReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Time
		a, _, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
			at = append(at, time.Now())
			wantSince := "seed"
			if len(at) > 9 {
				wantSince = "after-empty"
			}
			if got := r.URL.Query().Get("since"); got != wantSince {
				t.Fatalf("attempt %d since=%q, want %q", len(at), got, wantSince)
			}
			switch len(at) {
			case 9:
				return waitResponse(r, 200, `{"next_batch":"after-empty"}`)
			case 12:
				return waitResponse(r, 200, waitValidEvent)
			default:
				return nil, waitRefused()
			}
		})
		if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "1h"}); code != ExitOK || !strings.Contains(out.String(), "$neighbor") {
			t.Fatalf("bounded recovery: code=%d stdout=%q stderr=%q", code, out, errb)
		}
		want := []time.Duration{5 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 0, 5 * time.Second, 30 * time.Second}
		if len(at) != len(want)+1 {
			t.Fatalf("attempt count = %d", len(at))
		}
		for i, delay := range want {
			if got := at[i+1].Sub(at[i]); got != delay {
				t.Fatalf("attempt %d delay = %s, want %s", i+2, got, delay)
			}
		}
	})
}

// Temporary HTTP and request timeouts retry, while subscription reads retain the fetched batch.
func TestWaitRetryTemporaryReads(t *testing.T) {
	for _, phase := range []string{"startup", "after-sync", "sync-statuses"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				syncCalls, subCalls := 0, 0
				a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					syncCalls++
					if phase == "sync-statuses" {
						switch syncCalls {
						case 1:
							return waitResponse(r, 429, `{"errcode":"M_LIMIT_EXCEEDED"}`)
						case 2:
							return waitResponse(r, 500, `{"errcode":"M_UNKNOWN"}`)
						case 3:
							return waitResponse(r, 502, `bad gateway`)
						case 4:
							return waitResponse(r, 504, `gateway timeout`)
						case 5:
							return nil, context.DeadlineExceeded // individual request, caller still live
						}
					}
					return waitResponse(r, 200, waitValidEvent)
				})
				base := a.HTTP.Transport
				a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
						subCalls++
						if phase == "startup" && subCalls <= 2 || phase == "after-sync" && subCalls >= 2 && subCalls <= 3 {
							return nil, waitRefused()
						}
					}
					return base.RoundTrip(r)
				})
				if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "1h", "--json"}); code != ExitOK || strings.Count(out.String(), "$neighbor") != 1 {
					t.Fatalf("read recovery: code=%d sync=%d subs=%d stdout=%q stderr=%q", code, syncCalls, subCalls, out, errb)
				}
				wantSync := 1
				if phase == "sync-statuses" {
					wantSync = 6
				}
				if syncCalls != wantSync {
					t.Fatalf("batch was fetched %d times, want %d", syncCalls, wantSync)
				}
				waitRetryState(t, p, "delivered")
			})
		})
	}
}

// Permanent failures refuse before the valid neighbor, without advancing the cursor.
func TestWaitRetryPermanentRefusal(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		err        error
	}{
		{name: "forbidden", status: 403, body: `{"errcode":"M_FORBIDDEN"}`},
		{name: "auth", status: 401, body: `{"errcode":"M_UNKNOWN_TOKEN"}`},
		{name: "auth-on-503", status: 503, body: `{"errcode":"M_UNKNOWN_TOKEN"}`},
		{name: "bad-request", status: 400, body: `{"errcode":"M_INVALID_PARAM"}`},
		{name: "not-implemented", status: 501, body: `{}`},
		{name: "malformed", status: 200, body: `{broken`},
		{name: "truncated-json", status: 200, body: `{"next_batch":`},
		{name: "missing-cursor", status: 200, body: `{}`},
		{name: "wrong-type", status: 200, body: `{"next_batch":3}`},
		{name: "configuration", err: errors.New("permanent transport configuration refusal")},
		{name: "certificate", err: x509.UnknownAuthorityError{Cert: &x509.Certificate{}}},
		{name: "dns-not-found", err: &net.DNSError{Err: "no such host", IsNotFound: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						return waitResponse(r, 200, waitValidEvent)
					}
					if tc.err != nil {
						return nil, tc.err
					}
					return waitResponse(r, tc.status, tc.body)
				})
				start := time.Now()
				code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "1h"})
				if code != ExitError || calls != 1 || out.Len() != 0 || errb.Len() == 0 || time.Since(start) != 0 {
					t.Fatalf("permanent refusal: code=%d calls=%d elapsed=%s stdout=%q stderr=%q", code, calls, time.Since(start), out, errb)
				}
				waitRetryState(t, p, "seed")
			})
		})
	}
}

// Real cancellation while the five-second timer is pending must join the waiter promptly.
func TestWaitRetryCancelDuringBackoff(t *testing.T) {
	first := make(chan struct{})
	var calls atomic.Int32
	a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(first)
			return nil, waitRefused()
		}
		return waitResponse(r, 200, waitValidEvent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- a.Run(ctx, []string{"--as", "alice", "wait", "--timeout", "0"}) }()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("wait did not attempt sync")
	}
	// Let the request return and the real backoff timer arm, then cancel it.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != ExitError || calls.Load() != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "context canceled") {
			t.Fatalf("cancel: code=%d requests=%d stdout=%q stderr=%q", code, calls.Load(), out, errb)
		}
	case <-time.After(time.Second):
		// Join even a broken timer implementation before reporting the causal failure.
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("waiter leaked after cancellation")
		}
		t.Fatal("wait did not cancel during backoff within one second")
	}
	waitRetryState(t, p, "seed")
}

// Own timeout keeps its output schema; a shorter caller deadline remains an error.
func TestWaitRetryDeadlines(t *testing.T) {
	for _, mode := range []string{"text", "json", "caller"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					calls++
					return nil, waitRefused()
				})
				ctx := context.Background()
				args := []string{"--as", "alice", "wait", "--timeout", "2s"}
				want, code := "timeout\n", ExitOK
				if mode == "json" {
					args = append(args, "--json")
					want = "{\"type\":\"timeout\"}\n"
				}
				if mode == "caller" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
					want, code = "", ExitError
				}
				start := time.Now()
				if got := a.Run(ctx, args); got != code || out.String() != want || calls != 1 {
					t.Fatalf("deadline: code=%d calls=%d stdout=%q stderr=%q", got, calls, out, errb)
				}
				duration := 2 * time.Second
				if mode == "caller" {
					duration = time.Second
				}
				if time.Since(start) != duration {
					t.Fatalf("deadline duration = %s", time.Since(start))
				}
				waitRetryState(t, p, "seed")
			})
		})
	}
}
