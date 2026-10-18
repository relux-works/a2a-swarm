package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// App.Run -> cmdWait -> runWithRelogin must share one finite budget. These
// fixtures exercise actual subscription GET and password-login paths, using
// fake credentials, a controlled transport and real cancellation/deadlines.
func TestWaitFiniteDeadlineAcrossRelogin(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, shape := range []string{"fast-control", "slow-login", "reset-budget", "cancel-login", "cancel-sync", "parent-deadline-login", "early-login-deadline"} {
			t.Run(fmt.Sprintf("%s/json-%t", shape, asJSON), func(t *testing.T) {
				e, _, body := retryWorld(t)
				base := e.hs.srv.Client().Transport
				originalSubs := string(e.hs.accountDataOf("@alice:"+testServer, SubsType))
				subs, logins, syncs, pauses := 0, 0, 0, 0
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if shape == "parent-deadline-login" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 200*time.Millisecond)
					defer stop()
				}
				pause := func(r *http.Request, d time.Duration) error {
					timer := time.NewTimer(d)
					defer timer.Stop()
					select {
					case <-r.Context().Done():
						return r.Context().Err()
					case <-timer.C:
						return nil
					}
				}
				e.configure = func(a *App) {
					a.deliveryPause = func(context.Context, time.Duration) error { pauses++; return nil }
					a.HTTP = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) && r.Method == "GET" {
							subs++
							if subs == 1 {
								if shape == "reset-budget" {
									if err := pause(r, 300*time.Millisecond); err != nil {
										return nil, err
									}
								}
								return deliveryResponse(401, `{"errcode":"M_UNKNOWN_TOKEN"}`)
							}
						}
						if strings.HasSuffix(r.URL.Path, "/login") {
							logins++
							switch shape {
							case "early-login-deadline":
								return nil, context.DeadlineExceeded
							case "cancel-login":
								cancel()
								return nil, r.Context().Err()
							case "slow-login", "parent-deadline-login":
								if err := pause(r, 800*time.Millisecond); err != nil {
									return nil, err
								}
							}
						}
						if strings.HasSuffix(r.URL.Path, "/sync") {
							syncs++
							if shape == "cancel-sync" {
								cancel()
								return nil, r.Context().Err()
							}
							if shape == "reset-budget" {
								if err := pause(r, 400*time.Millisecond); err != nil {
									return nil, err
								}
							}
							return deliveryResponse(200, body)
						}
						return base.RoundTrip(r)
					})}
				}
				args := []string{"--as", "alice", "wait", "--timeout", "500ms"}
				if asJSON {
					args = append(args, "--json")
				}
				start := time.Now()
				result := e.runCtx(ctx, args...)
				elapsed := time.Since(start)
				if shape == "fast-control" {
					result.ok(t)
					if logins != 1 || syncs != 1 || subs != 3 || pauses != 0 || strings.Count(result.stdout, "$retry") != 1 || e.state("alice").NextBatch != "delivered" || e.addLines("alice", "$retry") != 1 {
						t.Fatalf("fast renewal control lost delivery: %+v logins=%d syncs=%d subs=%d pauses=%d", result, logins, syncs, subs, pauses)
					}
					if entry, ok := e.pendingEntry("alice", "$retry"); !ok || !entry.Shown {
						t.Fatal("fast renewal did not retain shown, unreceipted mail")
					}
				} else {
					wantSyncs, wantSubs := 0, 1
					if shape == "reset-budget" || shape == "cancel-sync" {
						wantSyncs, wantSubs = 1, 2
					}
					if elapsed > 750*time.Millisecond || logins != 1 || syncs != wantSyncs || subs != wantSubs || pauses != 0 || e.state("alice").NextBatch != "resume" || len(e.mailbox("alice").Pending()) != 0 {
						t.Fatalf("finite deadline escaped through renewal: elapsed=%v result=%+v logins=%d syncs=%d subs=%d pauses=%d cursor=%s", elapsed, result, logins, syncs, subs, pauses, e.state("alice").NextBatch)
					}
					data, err := os.ReadFile(e.paths().MailboxFile("alice"))
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "$retry") {
						t.Fatal("renewal deadline allowed durable mailbox append")
					}
					if shape == "slow-login" || shape == "reset-budget" {
						result.ok(t)
						want := "timeout\n"
						if asJSON {
							want = "{\"type\":\"timeout\"}\n"
						}
						if result.stdout != want {
							t.Fatalf("finite renewal timeout output = %q, want %q; stderr=%q", result.stdout, want, result.stderr)
						}
					} else {
						message := "context deadline exceeded"
						if strings.HasPrefix(shape, "cancel-") {
							message = "context canceled"
						}
						result.failsWith(t, ExitError, message)
						if result.stdout != "" {
							t.Fatalf("external/early deadline or cancellation became successful timeout: %+v", result)
						}
					}
				}
				if string(e.hs.accountDataOf("@alice:"+testServer, SubsType)) != originalSubs || e.lease("alice").Holder != "retry-owner" {
					t.Fatal("renewal changed subscriptions or consumer holder")
				}
			})
		}
	}
}
