package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Claim: an already expired budget or canceled caller makes no poll, including
// when cancellation delivery lags expiry. The adjacent live-budget control polls
// successfully; unlimited waits retain the Matrix client's ordinary bound.
func TestWaitDeadlineAdmission(t *testing.T) {
	for _, mode := range []string{"expired", "canceled", "live", "unlimited"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				a, _, _, _ := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					calls++
					return waitResponse(r, 200, waitValidEvent)
				})
				a.Now = time.Now
				a.as = "alice"
				s, err := a.session()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deadline := time.Now().Add(time.Second)
				wantErr, wantCalls := error(nil), 1
				switch mode {
				case "expired":
					deadline = time.Now() // No context timer has fired.
					wantErr, wantCalls = errWaitTimeout, 0
				case "canceled":
					cancel()
					wantErr, wantCalls = context.Canceled, 0
				case "unlimited":
					deadline = time.Time{}
				}
				resp, err := s.waitSyncUntil(ctx, "seed", maxPoll, syncFilter, deadline)
				if !errors.Is(err, wantErr) || calls != wantCalls || wantCalls == 1 && resp.NextBatch != "delivered" {
					t.Fatalf("mode=%s calls=%d response=%q err=%v", mode, calls, resp.NextBatch, err)
				}
			})
		})
	}
}

// Claim: every poll attempt shares the original budget, even after a retry;
// adjacent controls deliver an event one millisecond before that deadline.
// All sleeps in this file advance synctest's virtual clock, never wall time.
func TestWaitDeadlinePoll(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, deliver := range []bool{false, true} {
			t.Run(strconv.FormatBool(retry)+"/deliver="+strconv.FormatBool(deliver), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					deadline := start.Add(10 * time.Second)
					calls := 0
					a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
						calls++
						if !time.Now().Before(deadline) || r.URL.Query().Get("since") != "seed" {
							t.Fatal("poll started after expiry or changed the retry cursor")
						}
						remaining := deadline.Sub(time.Now())
						if got := r.URL.Query().Get("timeout"); got != strconv.FormatInt(remaining.Milliseconds(), 10) {
							t.Fatalf("server poll=%s, remaining=%v", got, remaining)
						}
						if got, ok := r.Context().Deadline(); !ok || !got.Equal(deadline) {
							t.Fatalf("request deadline=%v, want %v", got, deadline)
						}
						if retry && calls == 1 {
							return nil, waitRefused()
						}
						if deliver {
							time.Sleep(remaining - time.Millisecond)
							return waitResponse(r, 200, waitValidEvent)
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					})
					if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "10s"}); code != ExitOK {
						t.Fatalf("code=%d stderr=%q", code, errb)
					}
					wantCalls, wantElapsed, wantCursor := 1, 10*time.Second, "seed"
					if retry {
						wantCalls++
					}
					if deliver {
						wantElapsed -= time.Millisecond
						wantCursor = "delivered"
						if strings.Count(out.String(), "$neighbor") != 1 {
							t.Fatalf("event lost or duplicated: %q", out)
						}
					} else if out.String() != "timeout\n" {
						t.Fatalf("timeout output=%q", out)
					}
					// Zero intentional timer slack: neither the 30s HTTP allowance
					// nor a stale pre-retry poll extends the virtual deadline.
					if calls != wantCalls || time.Since(start) != wantElapsed {
						t.Fatalf("calls=%d elapsed=%v; want %d, %v", calls, time.Since(start), wantCalls, wantElapsed)
					}
					waitRetryState(t, p, wantCursor)
				})
			})
		}
	}
}

// Claim: expiry in either retry pause prevents another request; the adjacent
// unlimited-wait control retains legitimate 5s/30s retries and delivers once.
func TestWaitDeadlineBackoff(t *testing.T) {
	for _, timeout := range []string{"2s", "8s", "0"} {
		t.Run(timeout, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var attempts []time.Duration
				a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					attempts = append(attempts, time.Since(start))
					if len(attempts) < 3 {
						return nil, waitRefused()
					}
					return waitResponse(r, 200, waitValidEvent)
				})
				if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", timeout}); code != ExitOK {
					t.Fatalf("code=%d stderr=%q", code, errb)
				}
				if timeout == "0" {
					if len(attempts) != 3 || attempts[1] != 5*time.Second || attempts[2] != 35*time.Second || strings.Count(out.String(), "$neighbor") != 1 {
						t.Fatalf("valid retry control: attempts=%v output=%q", attempts, out)
					}
					waitRetryState(t, p, "delivered")
					return
				}
				budget, _ := time.ParseDuration(timeout)
				wantCalls := 1
				if budget > 5*time.Second {
					wantCalls = 2
				}
				if len(attempts) != wantCalls || time.Since(start) != budget || out.String() != "timeout\n" {
					t.Fatalf("backoff extended deadline: attempts=%v elapsed=%v output=%q", attempts, time.Since(start), out)
				}
				waitRetryState(t, p, "seed")
			})
		})
	}
}

// Claim: caller cancellation (including an earlier caller deadline) interrupts
// polls and backoff without advancing the cursor. Each phase has an adjacent
// live-caller control that recovers and delivers the same event.
func TestWaitDeadlineCaller(t *testing.T) {
	for _, phase := range []string{"poll", "backoff"} {
		for _, mode := range []string{"canceled", "deadline", "live"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if mode == "canceled" {
						go func() { time.Sleep(time.Second); cancel() }()
					} else if mode == "deadline" {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeout(ctx, time.Second)
						defer stop()
					}
					calls := 0
					a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
						calls++
						if phase == "backoff" && calls == 1 {
							return nil, waitRefused()
						}
						select {
						case <-r.Context().Done():
							return nil, r.Context().Err()
						case <-time.After(2 * time.Second):
							return waitResponse(r, 200, waitValidEvent)
						}
					})
					start := time.Now()
					code := a.Run(ctx, []string{"--as", "alice", "wait", "--timeout", "20s"})
					if mode == "live" {
						if code != ExitOK || strings.Count(out.String(), "$neighbor") != 1 {
							t.Fatalf("live control: code=%d output=%q stderr=%q", code, out, errb)
						}
						waitRetryState(t, p, "delivered")
					} else {
						if code != ExitError || out.Len() != 0 || calls != 1 || time.Since(start) != time.Second {
							t.Fatalf("cancellation: code=%d calls=%d elapsed=%v output=%q stderr=%q", code, calls, time.Since(start), out, errb)
						}
						waitRetryState(t, p, "seed")
					}
				})
			})
		}
	}
}

// Claim: a transient retry preserves selected-identity keywords AND explicit
// predicates. Adjacent near misses remain unshown and unreceived; the valid
// control arrives just before expiry, is shown once and remains unreceived.
func TestWaitDeadlineKeywordsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		a, p, out, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, waitRefused()
			}
			if r.URL.Query().Get("since") != "seed" {
				t.Fatal("retry changed cursor")
			}
			time.Sleep(5*time.Second - time.Millisecond)
			return waitResponse(r, 200, `{"next_batch":"delivered","rooms":{"join":{"!proj:test.local":{"timeline":{"events":[
				{"event_id":"$keyword-miss","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local TASK-70 ready"}},
				{"event_id":"$text-miss","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local TASK-7 waiting"}},
				{"event_id":"$sender-miss","type":"m.room.message","sender":"@carol:test.local","content":{"msgtype":"m.text","body":"@alice:test.local TASK-7 ready"}},
				{"event_id":"$match","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local TASK-7 ready"}}
			]}}}}}`)
		})
		if _, err := config.UpdateCredentials(p, "alice", func(c *config.Credentials) error {
			c.WaitKeywords = config.WaitKeywords{"TASK-7"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if code := a.Run(context.Background(), []string{"--as", "alice", "wait", "--timeout", "10s", "--from", "bob", "--match-text", "ready$", "--to-me-only", "--json"}); code != ExitOK {
			t.Fatalf("code=%d stderr=%q", code, errb)
		}
		if calls != 2 || time.Since(start) != 10*time.Second-time.Millisecond || strings.Count(out.String(), "$match") != 1 || strings.Contains(out.String(), "-miss") {
			t.Fatalf("combined delivery: calls=%d elapsed=%v output=%q", calls, time.Since(start), out)
		}
		waitRetryState(t, p, "delivered")
		mb, err := config.OpenMailbox(p, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if len(mb.Pending()) != 4 {
			t.Fatalf("lost filtered or shown mail: %#v", mb.Pending())
		}
		for _, entry := range mb.Pending() {
			var event struct {
				EventID string `json:"event_id"`
			}
			if err := json.Unmarshal(entry.Event, &event); err != nil || entry.Shown != (event.EventID == "$match") {
				t.Fatalf("incorrect shown state: id=%q shown=%v error=%v", event.EventID, entry.Shown, err)
			}
		}
	})
}
