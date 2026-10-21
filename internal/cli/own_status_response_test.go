package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Retained reviewer body-boundary regression through production writers and synthetic HTTP.
// A complete body BELOW the cap permits certainty; a limit hit is ambiguous.
// The valid-cap expectation follows the keeper clarification after CR1.
// A bounded valid JSON prefix is not evidence that the entire response decoded.
func TestReviewN1ErrorBodyCap(t *testing.T) {
	for _, kind := range []string{"valid-short", "valid-below-cap", "valid-cap", "trailing-malformed", "late-read-error"} {
		for _, prior := range []bool{false, true} {
			for _, writer := range []string{"notice", "manual"} {
				t.Run(fmt.Sprintf("%s/prior-%t/%s", kind, prior, writer), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
					if err != nil {
						t.Fatal(err)
					}
					prefix := `{"errcode":"M_FORBIDDEN","error":"denied"}`
					body := prefix
					if kind != "valid-short" {
						size := 64 << 10
						if kind == "valid-below-cap" {
							size--
						}
						body += strings.Repeat(" ", size-len(prefix))
					}
					if kind == "trailing-malformed" {
						body += "!malformed"
					}
					var puts atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut {
							puts.Add(1)
							if kind == "late-read-error" {
								w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
							}
							w.WriteHeader(403)
							_, _ = w.Write([]byte(body))
							return
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer srv.Close()
					s.client.BaseURL = srv.URL
					if writer == "notice" {
						err = s.publishTerminalNotice(context.Background(), "deadline")
					} else {
						err = s.writeLiveness(context.Background(), *ack)
					}
					st := e.state("alice")
					want := prior
					if kind == "valid-cap" || kind == "trailing-malformed" || kind == "late-read-error" {
						want = true
					}
					if err == nil || puts.Load() != 1 || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) {
						t.Errorf("complete-body knowledge: kind=%s prior=%t writer=%s PUT=%d uncertainty=%t want=%t err=%v", kind, prior, writer, puts.Load(), st.NoticeWriteUncertain, want, err)
					}
					_ = a
				})
			}
		}
	}
}

// Retained reviewer provenance regression: a redirected endpoint's refusal cannot establish
// non-application of the original own-status PUT. Targets are never contacted;
// the synthetic original endpoint may already have applied the write.
func TestReviewN1RedirectedRefusal(t *testing.T) {
	for _, kind := range []string{"direct403", "redirect302", "redirect307"} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior-%t", kind, prior), func(t *testing.T) {
				e, _, s := noticeDeadlineFixture(t)
				ack := e.state("alice").Liveness
				_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
				if err != nil {
					t.Fatal(err)
				}
				var applied, redirected atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirect-refusal" {
						redirected.Add(1)
						writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN", "error": "other endpoint"})
						return
					}
					if r.Method == http.MethodPut && kind != "direct403" {
						rec := httptest.NewRecorder()
						e.hs.srv.Config.Handler.ServeHTTP(rec, r)
						if rec.Code != 200 {
							t.Errorf("setup apply: %d", rec.Code)
						}
						applied.Add(1)
						w.Header().Set("Location", "/redirect-refusal")
						code := 302
						if kind == "redirect307" {
							code = 307
						}
						w.WriteHeader(code)
						return
					}
					if r.Method == http.MethodPut {
						writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN"})
						return
					}
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				s.client.BaseURL = srv.URL
				err = s.publishTerminalNotice(context.Background(), "deadline")
				st := e.state("alice")
				want := prior || kind != "direct403"
				if kind != "direct403" && (applied.Load() != 1 || redirected.Load() != 0) {
					t.Fatalf("setup redirect: applied=%d redirected=%d", applied.Load(), redirected.Load())
				}
				if err == nil || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) {
					t.Errorf("redirect provenance knowledge: kind=%s prior=%t applied=%d redirected=%d uncertain=%t want=%t err=%v", kind, prior, applied.Load(), redirected.Load(), st.NoticeWriteUncertain, want, err)
				}
			})
		}
	}
}

// Failed non-notice writers must preserve the existing cooldown while changing
// only uncertainty; later definitive failures cannot reconcile that ambiguity.
func TestReviewN1ManualFailureSequence(t *testing.T) {
	f := newNoticeRetryFixture(t)
	f.update(t, func(st *config.State) { st.Terminal.OwnNotice = schemaNotice("false") })
	f.mode.Store(2)
	if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
		t.Fatal("setup refusal missing")
	}
	before := f.e.state("alice")
	f.mode.Store(1)
	if err := f.s.writeLiveness(context.Background(), *before.Liveness); err == nil {
		t.Fatal("setup ambiguity missing")
	}
	middle := f.e.state("alice")
	if !middle.NoticeWriteUncertain || !reflect.DeepEqual(middle.NoticeRetry, before.NoticeRetry) || !reflect.DeepEqual(middle.Liveness, before.Liveness) {
		t.Fatal("failed manual writer lost cooldown/knowledge")
	}
	f.mode.Store(2)
	if err := f.s.writeLiveness(context.Background(), *before.Liveness); err == nil {
		t.Fatal("setup later refusal missing")
	}
	after := f.e.state("alice")
	if !after.NoticeWriteUncertain || !reflect.DeepEqual(after.NoticeRetry, before.NoticeRetry) || !reflect.DeepEqual(after.Liveness, before.Liveness) {
		t.Fatal("definitive manual refusal erased earlier uncertainty or fence")
	}
	count := f.requests.Load()
	_ = f.s.publishTerminalNotice(context.Background(), "deadline")
	if f.requests.Load() != count {
		t.Fatal("manual failure bypassed cooldown")
	}
	f.mode.Store(0)
	if err := f.s.writeLiveness(context.Background(), *before.Liveness); err != nil {
		t.Fatal(err)
	}
	end := f.e.state("alice")
	if end.NoticeRetry != nil || end.NoticeWriteUncertain || end.Terminal.NoticePublication != "published" {
		t.Fatal("manual recovery ACK failed to clear fence")
	}
}

// Coherent future metadata is the specified rollback case, not a freshness
// assertion. Cross-generation policy changes preserve the existing fence.
func TestReviewN1FutureMetadataAndPolicyChange(t *testing.T) {
	f := newNoticeRetryFixture(t)
	future := f.now.Add(365 * 24 * time.Hour)
	next := future.Add(5 * time.Minute)
	f.update(t, func(st *config.State) {
		st.Terminal.OwnNotice = schemaNotice("false")
		st.Terminal.Interval = time.Second
		st.Terminal.RetryMax = 3 * time.Second
		st.NoticeRetry = &config.NoticeRetry{FailedAt: future, Next: next, Delay: 5 * time.Minute}
	})
	count := f.requests.Load()
	ack := f.e.state("alice").Liveness
	for _, at := range []time.Time{f.now, future, next.Add(-time.Nanosecond)} {
		f.now = at
		if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err == nil || f.requests.Load() != count || !reflect.DeepEqual(f.e.state("alice").Liveness, ack) {
			t.Fatal("coherent future retry bypassed stored fence")
		}
	}
	f.now = next
	f.mode.Store(2)
	_ = f.s.publishTerminalNotice(context.Background(), "deadline")
	st := f.e.state("alice")
	if f.requests.Load() != count+2 || st.NoticeRetry.Delay != 3*time.Second || !st.NoticeRetry.Next.Equal(next.Add(3*time.Second)) {
		t.Fatal("due retry failed current policy cap")
	}
	f.now = st.NoticeRetry.Next
	f.mode.Store(0)
	if err := f.s.publishTerminalNotice(context.Background(), "deadline"); err != nil || f.e.state("alice").NoticeRetry != nil {
		t.Fatal("future/cap recovery ACK refused")
	}
}

// Confirm externally observable knowledge loss: an applied write with an
// untrustworthy refusal must not later turn local ACK equality into published.
func TestReviewN1FalsePublishedEquality(t *testing.T) {
	for _, kind := range []string{"cap-tail", "redirect302", "redirect307", "direct-valid-control"} {
		t.Run(kind, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			ack := e.state("alice").Liveness
			var sends atomic.Int32
			var recovery atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				if r.URL.Path == "/redirect-refusal" {
					writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN"})
					return
				}
				if r.Method == http.MethodPut && !recovery.Load() {
					if kind == "direct-valid-control" {
						writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN"})
						return
					}
					rec := httptest.NewRecorder()
					e.hs.srv.Config.Handler.ServeHTTP(rec, r)
					if rec.Code != 200 {
						t.Errorf("setup apply: %d", rec.Code)
					}
					if kind == "cap-tail" {
						prefix := `{"errcode":"M_FORBIDDEN","error":"denied"}`
						w.WriteHeader(403)
						_, _ = w.Write([]byte(prefix + strings.Repeat(" ", (64<<10)-len(prefix)) + "malformed"))
						return
					}
					w.Header().Set("Location", "/redirect-refusal")
					code := 302
					if kind == "redirect307" {
						code = 307
					}
					w.WriteHeader(code)
					return
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			s.client.BaseURL = srv.URL
			if err := s.publishTerminalNotice(context.Background(), "deadline"); err == nil {
				t.Fatal("setup error missing")
			}
			current, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), current, StatusEventType, s.creds.UserID, &remote); err != nil {
				t.Fatal(err)
			}
			if kind != "direct-valid-control" && reflect.DeepEqual(remote.OwnNotice, ack.OwnNotice) {
				t.Fatal("setup: remote write was not applied")
			}
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = ack.OwnNotice; return nil })
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			reloaded.client.BaseURL = srv.URL
			before := sends.Load()
			_ = reloaded.publishTerminalNotice(context.Background(), "deadline")
			after := e.state("alice")
			if kind != "direct-valid-control" && (after.Terminal.NoticePublication == "published" || !after.NoticeWriteUncertain) {
				t.Errorf("false published equality: kind=%s publication=%q uncertain=%t remoteDiffers=true", kind, after.Terminal.NoticePublication, after.NoticeWriteUncertain)
			}
			if sends.Load() != before {
				t.Fatal("equality/cooldown made HTTP")
			}
			if kind == "direct-valid-control" && after.Terminal.NoticePublication != "published" {
				t.Fatal("valid definitive refusal rejected known equality")
			}
			// At the due time a real ACK can repair the discrepancy in every branch.
			when := after.NoticeRetry.Next
			a.Now = func() time.Time { return when }
			recovery.Store(true)
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = schemaNotice("false"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err = reloaded.publishTerminalNotice(context.Background(), "deadline"); err != nil {
				t.Fatal(err)
			}
			final := e.state("alice")
			if final.NoticeRetry != nil || final.NoticeWriteUncertain || final.Terminal.NoticePublication != "published" {
				t.Fatal("real recovery ACK did not reset knowledge/fence")
			}
			remote = config.AgentStatus{}
			if err = reloaded.client.GetState(context.Background(), current, StatusEventType, s.creds.UserID, &remote); err != nil || !reflect.DeepEqual(remote, *final.Liveness) {
				t.Fatalf("recovery remote ACK mismatch: %v", err)
			}
		})
	}
}

// Retained reviewer body-boundary regression through production writers and synthetic HTTP.
// A complete body BELOW the cap permits certainty; a limit hit is ambiguous.
// The valid-cap expectation follows the keeper clarification after CR1.
// A bounded valid JSON prefix is not evidence that the entire response decoded.
func TestReviewN1PublicStatusErrorBodyCap(t *testing.T) {
	for _, kind := range []string{"valid-short", "valid-below-cap", "valid-cap", "trailing-malformed", "late-read-error"} {
		for _, prior := range []bool{false, true} {
			for _, writer := range []string{"notice", "manual"} {
				t.Run(fmt.Sprintf("%s/prior-%t/%s", kind, prior, writer), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					ack := e.state("alice").Liveness
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil })
					if err != nil {
						t.Fatal(err)
					}
					prefix := `{"errcode":"M_FORBIDDEN","error":"denied"}`
					body := prefix
					if kind != "valid-short" {
						size := 64 << 10
						if kind == "valid-below-cap" {
							size--
						}
						body += strings.Repeat(" ", size-len(prefix))
					}
					if kind == "trailing-malformed" {
						body += "!malformed"
					}
					var puts atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut {
							puts.Add(1)
							if kind == "late-read-error" {
								w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
							}
							w.WriteHeader(403)
							_, _ = w.Write([]byte(body))
							return
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
					s.client.HTTP = a.HTTP
					a.Stdout = io.Discard
					a.Stderr = io.Discard
					if writer == "notice" {
						err = s.publishTerminalNotice(context.Background(), "deadline")
					} else {
						code := a.Run(context.Background(), append([]string{"--as", "alice"}, statusArgs("working")...))
						err = nil
						if code != 0 {
							err = fmt.Errorf("public status exit %d", code)
						}
					}
					st := e.state("alice")
					want := prior
					if kind == "valid-cap" || kind == "trailing-malformed" || kind == "late-read-error" {
						want = true
					}
					if err == nil || puts.Load() != 1 || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) {
						t.Errorf("complete-body knowledge: kind=%s prior=%t writer=%s PUT=%d uncertainty=%t want=%t err=%v", kind, prior, writer, puts.Load(), st.NoticeWriteUncertain, want, err)
					}
					_ = a
				})
			}
		}
	}
}

// Retained reviewer policy endpoint traces count due attempts across reload and
// generation changes; fixture time advances without changing RPC deadlines.
func TestReviewN1PolicyEndpointTraces(t *testing.T) {
	for _, policy := range []struct{ interval, cap time.Duration }{{time.Second, time.Second}, {time.Second, 5 * time.Minute}, {time.Minute, time.Minute}, {time.Minute, 5 * time.Minute}} {
		for _, mode := range []int32{1, 2} {
			t.Run(fmt.Sprintf("%s-%s/mode-%d", policy.interval, policy.cap, mode), func(t *testing.T) {
				f := newNoticeRetryFixture(t)
				ack := f.e.state("alice").Liveness
				gen := "deadline"
				f.update(t, func(st *config.State) {
					st.Terminal.OwnNotice = schemaNotice("false")
					st.Terminal.Interval = policy.interval
					st.Terminal.RetryMax = policy.cap
				})
				f.mode.Store(mode)
				delay := policy.interval
				for attempt := 0; attempt < 5; attempt++ {
					before := f.requests.Load()
					if err := f.s.publishTerminalNotice(context.Background(), gen); err == nil {
						t.Fatal("setup failure missing")
					}
					st := f.e.state("alice")
					if f.requests.Load() != before+2 || st.NoticeRetry.Delay != delay || !st.NoticeRetry.Next.Equal(f.now.Add(delay)) || !reflect.DeepEqual(st.Liveness, ack) || st.NoticeWriteUncertain != (mode == 1) {
						t.Fatalf("policy endpoint trace: attempt=%d policy=%+v state=%+v", attempt, policy, st.NoticeRetry)
					}
					next := st.NoticeRetry.Next
					before = f.requests.Load()
					gen = fmt.Sprintf("replacement-%d", attempt)
					f.update(t, func(st *config.State) {
						st.Terminal.Generation = gen
						st.Terminal.OwnNotice.ObservedAt = f.now.Format(time.RFC3339Nano)
					})
					reloaded, err := f.a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					f.s = reloaded
					for _, at := range []time.Time{f.now.Add(-time.Hour), next.Add(-time.Nanosecond)} {
						f.now = at
						if err = f.s.publishTerminalNotice(context.Background(), gen); err == nil || f.requests.Load() != before || !reflect.DeepEqual(f.e.state("alice").NoticeRetry, st.NoticeRetry) {
							t.Fatal("policy endpoint cooldown bypass or mutation")
						}
					}
					f.now = next
					delay = min(policy.cap, delay*2)
				}
				f.mode.Store(0)
				if err := f.s.publishTerminalNotice(context.Background(), gen); err != nil {
					t.Fatal(err)
				}
				st := f.e.state("alice")
				if st.NoticeRetry != nil || st.NoticeWriteUncertain || st.Terminal.NoticePublication != "published" {
					t.Fatal("policy endpoint recovery reset failed")
				}
			})
		}
	}
}

// Public status set must keep exact ACK and uncertainty when the original PUT
// applies but another endpoint refuses. Direct403 and earlier-uncertain siblings
// prevent reject-all; request counts prove no redirect target is contacted.
func TestOwnStatusPublicRedirectKnowledge(t *testing.T) {
	for _, kind := range []string{"direct", "302", "307"} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior-%t", kind, prior), func(t *testing.T) {
				e, a, s := noticeDeadlineFixture(t)
				ack := e.state("alice").Liveness
				if _, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.NoticeWriteUncertain = prior; return nil }); err != nil {
					t.Fatal(err)
				}
				var puts, redirects atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/refusal" {
						redirects.Add(1)
						want := http.MethodGet
						if kind == "307" {
							want = http.MethodPut
						}
						if r.Method != want {
							t.Errorf("redirect method=%s want=%s", r.Method, want)
						}
						writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN"})
						return
					}
					if r.Method == http.MethodPut {
						puts.Add(1)
						if kind == "direct" {
							writeJSON(w, 403, map[string]string{"errcode": "M_FORBIDDEN"})
							return
						}
						rec := httptest.NewRecorder()
						e.hs.srv.Config.Handler.ServeHTTP(rec, r)
						if rec.Code != 200 {
							t.Errorf("setup apply: %d", rec.Code)
						}
						w.Header().Set("Location", "/refusal")
						code := 302
						if kind == "307" {
							code = 307
						}
						w.WriteHeader(code)
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
				a.Stdout = io.Discard
				a.Stderr = io.Discard
				exit := a.Run(context.Background(), append([]string{"--as", "alice"}, statusArgs("working")...))
				st := e.state("alice")
				want := prior || kind != "direct"
				count := int32(0) // Own-status redirects are refused before target contact.
				if exit == 0 || puts.Load() != 1 || redirects.Load() != count || st.NoticeWriteUncertain != want || !reflect.DeepEqual(st.Liveness, ack) || !strings.HasPrefix(st.Terminal.NoticePublication, "unavailable:") {
					t.Fatalf("public redirect knowledge: exit=%d PUT=%d redirect=%d uncertain=%t want=%t", exit, puts.Load(), redirects.Load(), st.NoticeWriteUncertain, want)
				}
			})
		}
	}
}
