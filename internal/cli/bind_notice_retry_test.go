package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Retained reconciliation tests now publish at persisted eligibility. Restore
// their frozen observation clock afterward, keeping their exact-equality claims.
// Network/lock deadlines continue to use real time and are never extended here.
func publishNoticeAtEligibility(t *testing.T, a *App, s *session, ctx context.Context, generation string) {
	t.Helper()
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		t.Fatal(err)
	}
	original := a.Now
	if st.NoticeRetry != nil {
		now := original()
		if now.Before(st.NoticeRetry.Next) {
			now = st.NoticeRetry.Next
		}
		a.Now = func() time.Time { return now }
		defer func() { a.Now = original }()
	}
	a.publishTerminalNotice(ctx, s, generation)
}

type noticeRetryFixture struct {
	e        *env
	a        *App
	s        *session
	now      time.Time
	mode     atomic.Int32 // 0 ACK, 1 applied-but-500, 2 forbidden
	requests atomic.Int32
	puts     atomic.Int32
}

func newNoticeRetryFixture(t *testing.T) *noticeRetryFixture {
	t.Helper()
	e, a, s := noticeDeadlineFixture(t)
	f := &noticeRetryFixture{e: e, a: a, s: s, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	a.Now = func() time.Time { return f.now }
	a.as = "alice"
	a.Stdout = io.Discard
	a.Stderr = io.Discard
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodPut {
			f.puts.Add(1)
			switch f.mode.Load() {
			case 1:
				rec := httptest.NewRecorder()
				e.hs.srv.Config.Handler.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Errorf("setup: apply returned %d", rec.Code)
				}
				writeJSON(w, 500, map[string]string{"errcode": "M_UNKNOWN", "error": "synthetic lost ACK"})
				return
			case 2:
				writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN", "error": "synthetic refused"})
				return
			}
		}
		e.hs.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
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
	a.publishTerminalNotice(context.Background(), s, "deadline")
	if e.state("alice").Terminal.NoticePublication != "published" {
		t.Fatal("setup: initial ACK failed")
	}
	return f
}

func (f *noticeRetryFixture) update(t *testing.T, fn func(*config.State)) {
	t.Helper()
	_, err := config.UpdateState(f.s.paths, f.s.name, func(st *config.State) error { fn(st); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

// An earlier applied-but-unacknowledged write survives a later definitive
// refusal, reloading and replacement. Latest desired data and current routing
// win only at eligibility; rollback never grants an early send. No crash-atomic
// local/remote transaction or independently racing remote writer is claimed.
func TestNoticeRetryEarlierUncertaintyRebind(t *testing.T) {
	f := newNoticeRetryFixture(t)
	ack := f.e.state("alice").Liveness
	f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("false") })
	f.mode.Store(1)
	if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
		t.Fatal("missing synthetic ambiguity")
	}
	first := f.e.state("alice")
	if !first.NoticeWriteUncertain || !reflect.DeepEqual(first.Liveness, ack) {
		t.Fatal("possibly-applied send lost prior ACK/uncertainty")
	}
	f.now = f.now.Add(2 * time.Second)
	f.mode.Store(2)
	if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
		t.Fatal("missing synthetic refusal")
	}
	after := f.e.state("alice")
	if !after.NoticeWriteUncertain || !reflect.DeepEqual(after.Liveness, ack) {
		t.Fatal("later forbidden erased earlier uncertainty")
	}
	if after.NoticeRetry == nil || after.NoticeRetry.Delay != 4*time.Second {
		t.Fatal("second eligible failure did not double cooldown")
	}
	// Replace only the binding, then open a fresh session as a process reload.
	f.update(t, func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "replacement", Interval: 2 * time.Second, RetryMax: 30 * time.Second, OwnNotice: ack.OwnNotice}
	})
	reloaded, err := f.a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	f.s = reloaded
	before := f.requests.Load()
	f.now = f.now.Add(-time.Hour)
	if err = f.s.publishTerminalNotice(context.Background(), "replacement"); err == nil {
		t.Fatal("cooldown must report pending")
	}
	f.now = after.NoticeRetry.Next.Add(-time.Nanosecond)
	if err = f.s.publishTerminalNotice(context.Background(), "replacement"); err == nil {
		t.Fatal("pending equality falsely reconciled prior uncertainty")
	}
	if f.requests.Load() != before || !f.e.state("alice").NoticeWriteUncertain || !reflect.DeepEqual(f.e.state("alice").Liveness, ack) {
		t.Fatal("rebind/rollback bypassed identity cooldown or knowledge")
	}
	status := f.e.as("alice", "bind", "status", "--json").ok(t)
	if !strings.Contains(status.stdout, "retry pending") {
		t.Fatal("pending publication not observable")
	}
	replacement := f.e.hs.addRoom("retry-current-status", f.s.creds.UserID)
	f.e.hs.mu.Lock()
	f.e.hs.aliases["#status:"+f.e.hs.server] = replacement
	f.e.hs.mu.Unlock()
	f.mode.Store(0)
	f.now = after.NoticeRetry.Next
	if err = f.s.publishTerminalNotice(context.Background(), "replacement"); err != nil {
		t.Fatal(err)
	}
	st := f.e.state("alice")
	if f.requests.Load() != before+2 || st.NoticeRetry != nil || st.NoticeWriteUncertain || st.Terminal.NoticePublication != "published" {
		t.Fatal("due current-room ACK did not reconcile")
	}
	var remote config.AgentStatus
	if err = f.s.client.GetState(context.Background(), replacement, StatusEventType, f.s.creds.UserID, &remote); err != nil || !reflect.DeepEqual(remote, *st.Liveness) {
		t.Fatalf("current-room remote ACK mismatch: %v", err)
	}
}

// Legitimate own writers still send during notice cooldown. A real ACK from
// each entry point clears obsolete retry metadata and resets the next failure
// to Interval. Failed writers retain the fence and exact ACK instead.
func TestNoticeRetryOwnWriterReset(t *testing.T) {
	for _, writer := range []string{"hook", "wait", "manual"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ack-%t", writer, success), func(t *testing.T) {
				f := newNoticeRetryFixture(t)
				f.mode.Store(2)
				f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("false") })
				_ = f.s.publishTerminalNotice(context.Background(), "deadline")
				f.now = f.now.Add(2 * time.Second)
				_ = f.s.publishTerminalNotice(context.Background(), "deadline")
				before := f.e.state("alice")
				count := f.puts.Load()
				if before.NoticeRetry == nil || before.NoticeRetry.Delay != 4*time.Second {
					t.Fatal("setup: retry missing")
				}
				if success {
					f.mode.Store(0)
				}
				var err error
				switch writer {
				case "hook":
					err = f.a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
				case "wait":
					err = f.s.writeLivenessSource(context.Background(), *before.Liveness, "wait-heartbeat")
				case "manual":
					err = f.s.writeLiveness(context.Background(), *before.Liveness)
				}
				st := f.e.state("alice")
				if f.puts.Load() != count+1 {
					t.Fatal("notice cooldown rate-limited legitimate writer")
				}
				if !success {
					if err == nil || !reflect.DeepEqual(st.NoticeRetry, before.NoticeRetry) || !reflect.DeepEqual(st.Liveness, before.Liveness) || st.NoticeWriteUncertain != before.NoticeWriteUncertain {
						t.Fatal("failed writer cleared retry/ACK knowledge")
					}
					return
				}
				if err != nil || st.NoticeRetry != nil || st.NoticeWriteUncertain || st.Terminal.NoticePublication != "published" {
					t.Fatal("actual ACK did not clear retry state")
				}
				count = f.requests.Load()
				_ = f.s.publishTerminalNotice(context.Background(), "deadline")
				if f.requests.Load() != count {
					t.Fatal("ACK reset equality made HTTP")
				}
				f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("true") })
				f.mode.Store(2)
				if err = f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
					t.Fatal("setup: missing refusal after ACK")
				}
				if f.e.state("alice").NoticeRetry.Delay != 2*time.Second {
					t.Fatal("actual ACK failed to reset next failure to Interval")
				}
			})
		}
	}
}

// Bad semantic metadata heals to one bounded wait, then permits recovery.
// Malformed JSON is a read failure (no mutation/HTTP), not absent metadata.
// Internally coherent future times follow the stated rollback contract.
func TestNoticeRetryMalformedState(t *testing.T) {
	for _, kind := range []string{"zero", "negative", "oversize", "missing-time", "mismatch", "json", "absent"} {
		t.Run(kind, func(t *testing.T) {
			f := newNoticeRetryFixture(t)
			f.update(t, func(st *config.State) {
				st.Terminal.OwnNotice = schemaNotice("false")
				r := &config.NoticeRetry{FailedAt: f.now, Next: f.now.Add(2 * time.Second), Delay: 2 * time.Second}
				switch kind {
				case "zero":
					r.Delay = 0
					r.Next = f.now
				case "negative":
					r.Delay = -1
				case "oversize":
					r.Delay = 6 * time.Minute
					r.Next = f.now.Add(r.Delay)
				case "missing-time":
					r.FailedAt = time.Time{}
				case "mismatch":
					r.Next = f.now.AddDate(10, 0, 0)
				case "absent":
					r = nil
				}
				st.NoticeRetry = r
			})
			before := f.requests.Load()
			if kind == "json" {
				path := f.s.paths.StateFile(f.s.name)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte(`{"notice_retry":`), 0600); err != nil {
					t.Fatal(err)
				}
				if err = f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil || f.requests.Load() != before {
					t.Fatal("malformed state treated as absence")
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				f.now = f.now.Add(2 * time.Second)
			} else if kind != "absent" {
				for i := 0; i < 3; i++ {
					if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
						t.Fatal("invalid retry did not report pending")
					}
				}
				if f.requests.Load() != before {
					t.Fatal("invalid retry generated a storm")
				}
				f.now = f.now.Add(2 * time.Second)
			}
			if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err != nil {
				t.Fatal(err)
			}
			if f.requests.Load() != before+2 || f.e.state("alice").NoticeRetry != nil {
				t.Fatal("repaired retry permanently blocked ACK")
			}
		})
	}
}

// Invalid explicit binding policy refuses before HTTP; a corrected policy
// remains usable. This does not infer a new implicit interval for old metadata.
func TestNoticeRetryPolicy(t *testing.T) {
	for _, kind := range []string{"zero", "short", "long", "cap-small", "cap-large", "valid"} {
		t.Run(kind, func(t *testing.T) {
			f := newNoticeRetryFixture(t)
			f.update(t, func(st *config.State) {
				st.Terminal.OwnNotice = schemaNotice("false")
				switch kind {
				case "zero":
					st.Terminal.Interval = 0
				case "short":
					st.Terminal.Interval = time.Millisecond
				case "long":
					st.Terminal.Interval = time.Minute + time.Second
				case "cap-small":
					st.Terminal.RetryMax = time.Second
				case "cap-large":
					st.Terminal.RetryMax = 6 * time.Minute
				}
			})
			before := f.e.state("alice")
			requests := f.requests.Load()
			err := f.s.publishTerminalNotice(context.Background(), "deadline")
			if kind != "valid" {
				st := f.e.state("alice")
				if err == nil || f.requests.Load() != requests || !reflect.DeepEqual(st.Liveness, before.Liveness) || st.NoticeWriteUncertain != before.NoticeWriteUncertain || st.NoticeRetry != nil {
					t.Fatal("invalid retry policy made HTTP or mutated knowledge")
				}
				f.update(t, func(st *config.State) {
					st.Terminal.Interval = 2 * time.Second
					st.Terminal.RetryMax = 30 * time.Second
				})
				err = f.s.publishTerminalNotice(context.Background(), "deadline")
			}
			if err != nil || f.requests.Load() != requests+2 || f.e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("valid retry policy refused recovery")
			}
		})
	}
}

// Pending retry state cannot authorize stopped/foreign/stale/cancelled calls.
// Each refusal is zeroHTTP and preserves the entire identity state, including
// the persisted retry fence; a restored valid caller can ACK at eligibility.
func TestNoticeRetryPendingRefusals(t *testing.T) {
	for _, kind := range []string{"stopped", "foreign", "stale", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newNoticeRetryFixture(t)
			f.mode.Store(2)
			f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("false") })
			_ = f.s.publishTerminalNotice(context.Background(), "deadline")
			if f.e.state("alice").NoticeRetry == nil {
				t.Fatal("setup: retry missing")
			}
			f.update(t, func(st *config.State) {
				if kind == "stopped" {
					st.Terminal.Stopped = true
				}
				if kind == "stale" {
					st.Terminal.Generation = "replacement"
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner := f.s.creds.UserID
			if kind == "foreign" {
				f.s.creds.UserID = "@bob:test.local"
			}
			if kind == "cancelled" {
				cancel()
			}
			before := f.e.state("alice")
			count := f.requests.Load()
			if err := f.s.publishTerminalNotice(ctx, "deadline"); err == nil || f.requests.Load() != count || !reflect.DeepEqual(before, f.e.state("alice")) {
				t.Fatal("pending local refusal made HTTP or mutated state")
			}
			f.s.creds.UserID = owner
			f.update(t, func(st *config.State) { st.Terminal.Stopped = false; st.Terminal.Generation = "deadline" })
			f.now = before.NoticeRetry.Next
			f.mode.Store(0)
			if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err != nil || f.e.state("alice").NoticeRetry != nil {
				t.Fatal("restored pending caller failed real ACK")
			}
		})
	}
}

// Hook/wait failures remain legitimate writes. Their unresolved knowledge feeds
// real dispatch; repeated automatic notice work is fenced, while a later own
// heartbeat/manual ACK remains available (covered separately by writer reset).
func TestNoticeRetryFailedWriterDispatch(t *testing.T) {
	for _, writer := range []string{"hook", "wait"} {
		for _, mode := range []int32{1, 2} {
			t.Run(fmt.Sprintf("%s/mode-%d", writer, mode), func(t *testing.T) {
				f := newNoticeRetryFixture(t)
				f.mode.Store(mode)
				f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("false") })
				var err error
				for i := 0; i < 2; i++ {
					if writer == "hook" {
						err = f.a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
					} else {
						err = f.s.writeLivenessSource(context.Background(), *f.e.state("alice").Liveness, "wait-heartbeat")
					}
					if err == nil {
						t.Fatal("setup: failed writer unexpectedly ACKed")
					}
				}
				if f.puts.Load() != 3 {
					t.Fatal("legitimate persistent writer unexpectedly rate-limited")
				}
				f.update(t, func(st *config.State) { st.HooksPaused = true })
				base := f.puts.Load()
				for i := 0; i < 4; i++ {
					if err = f.a.dispatchTerminal(context.Background(), f.s, "deadline"); err != nil {
						t.Fatal(err)
					}
				}
				if f.puts.Load() != base+1 {
					t.Fatal("failed writer dispatch bypassed notice cooldown")
				}
				f.now = f.e.state("alice").NoticeRetry.Next
				f.mode.Store(0)
				if err = f.a.dispatchTerminal(context.Background(), f.s, "deadline"); err != nil || f.e.state("alice").Terminal.NoticePublication != "published" {
					t.Fatal("failed writer dispatch did not recover at eligibility")
				}
			})
		}
	}
}

// Real dispatch/notice paths must bound both definite refusals and ambiguous
// sends. Clock steps count every resolve/PUT without sleeping or a live runtime.
func TestNoticeRetryPersistentFailures(t *testing.T) {
	for _, kind := range []string{"forbidden", "server", "transport", "resolve"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			a.Now = func() time.Time { return now }
			_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.HooksPaused = true
				st.Terminal.Interval = 2 * time.Second
				st.Terminal.RetryMax = 30 * time.Second
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ack := e.state("alice").Liveness
			mail := e.mailbox("alice").Pending()
			var requests, puts, resolves atomic.Int32
			var recover atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.Contains(r.URL.Path, "/directory/room/") {
					resolves.Add(1)
					if kind == "resolve" && !recover.Load() {
						writeJSON(w, 500, map[string]string{"errcode": "M_UNKNOWN"})
						return
					}
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
					if !recover.Load() {
						if kind == "transport" {
							c, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							c.Close()
							return
						}
						code := 500
						if kind == "forbidden" {
							code = 403
						}
						writeJSON(w, code, map[string]string{"errcode": "M_FORBIDDEN", "error": "synthetic"})
						return
					}
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			base := now
			expected := int32(0)
			for second := 0; second <= 60; second++ {
				now = base.Add(time.Duration(second) * time.Second)
				due := second == 0 || second == 2 || second == 6 || second == 14 || second == 30 || second == 60
				if due {
					expected++
				}
				if err := a.dispatchTerminal(context.Background(), s, "deadline"); err != nil {
					t.Fatal(err)
				}
				if resolves.Load() != expected || kind != "resolve" && puts.Load() != expected || kind == "resolve" && puts.Load() != 0 {
					t.Fatalf("cooldown traffic at %ds: resolve=%d PUT=%d want attempts=%d", second, resolves.Load(), puts.Load(), expected)
				}
				st := e.state("alice")
				if st.Terminal.NoticePublication == "published" || !reflect.DeepEqual(st.Liveness, ack) || !reflect.DeepEqual(mail, e.mailbox("alice").Pending()) {
					t.Fatal("failed retry changed ACK/mail or claimed published")
				}
			}
			// Recovery is available before eligibility, but must not bypass the fence.
			recover.Store(true)
			before := requests.Load()
			now = base.Add(89 * time.Second)
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if requests.Load() != before {
				t.Fatal("early recovery bypassed cooldown")
			}
			now = base.Add(90 * time.Second)
			a.publishTerminalNotice(context.Background(), s, "deadline")
			st := e.state("alice")
			if st.Terminal.NoticePublication != "published" || st.NoticeWriteUncertain || !reflect.DeepEqual(st.Liveness.OwnNotice, st.Terminal.OwnNotice) {
				t.Fatal("scheduled recovery did not ACK latest desired")
			}
			before = requests.Load()
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if requests.Load() != before {
				t.Fatal("ACK equality made HTTP")
			}
			t.Logf("kind=%s failed-attempts=%d recovery=90s resolve=%d PUT=%d", kind, expected, resolves.Load(), puts.Load())
		})
	}
}

// Each real writer preserves exact prior ACK and PRE-attempt uncertainty on a
// decoded 403/M_FORBIDDEN only. Other errors cannot assert non-application.
func TestOwnStatusDefinitiveRefusalKnowledge(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		definite bool
	}{
		{"forbidden", 403, `{"errcode":"M_FORBIDDEN","error":"denied"}`, true},
		{"server-code", 500, `{"errcode":"M_FORBIDDEN","error":"denied"}`, false},
		{"other-status", 400, `{"errcode":"M_FORBIDDEN","error":"denied"}`, false},
		{"lookalike", 403, `{"errcode":"M_UNKNOWN","error":"M_FORBIDDEN"}`, false},
		{"malformed", 403, `{"errcode":"M_FORBIDDEN",`, false},
		{"partial-decode", 403, `{"errcode":"M_FORBIDDEN","error":5}`, false},
		{"body-read-error", 403, `{"errcode":"M_FORBIDDEN","error":"denied"}`, false},
		{"status-spoof", 500, `{"Status":403,"errcode":"M_FORBIDDEN","error":"denied"}`, false},
	}
	for _, writer := range []string{"notice", "hook", "wait", "manual"} {
		for _, prior := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/prior-%t/%s", writer, prior, tc.name), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					a.as = "alice"
					a.Stdout = io.Discard
					a.Stderr = io.Discard
					ack := e.state("alice").Liveness
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
					if err != nil {
						t.Fatal(err)
					}
					var puts atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut {
							puts.Add(1)
							if tc.name == "body-read-error" {
								w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+10))
							}
							w.WriteHeader(tc.status)
							_, _ = w.Write([]byte(tc.body))
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
						resp, err := http.DefaultTransport.RoundTrip(c)
						if resp != nil {
							resp.Request = r
						}
						return resp, err
					})}
					s.client.HTTP = a.HTTP
					switch writer {
					case "notice":
						err = s.publishTerminalNotice(context.Background(), "deadline")
					case "hook":
						err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
					case "wait":
						err = s.writeLivenessSource(context.Background(), *ack, "wait-heartbeat")
					case "manual":
						err = s.writeLiveness(context.Background(), *ack)
					}
					st := e.state("alice")
					want := true
					if tc.definite {
						want = prior
					}
					if err == nil || puts.Load() != 1 || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) || !strings.HasPrefix(st.Terminal.NoticePublication, "unavailable:") {
						t.Fatalf("per-attempt knowledge: err=%v PUT=%d uncertain=%t want=%t prior=%t", err, puts.Load(), st.NoticeWriteUncertain, want, prior)
					}
				})
			}
		}
	}
}
