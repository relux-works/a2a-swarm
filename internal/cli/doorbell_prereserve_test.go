package cli

// Retained independent ISSUE-123 review-r2 regression. docs/doorbell.md "Output and outcomes": timeout and
// cancellation return the fixed classifications `doorbell: timeout` /
// `doorbell: canceled`. A FAIL is the reproduction; "loop-top" is the valid
// neighbor where the same injection is already classified correctly.

import (
	"bytes"
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func reviewR2Stack(parts ...string) bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var names []string
	for {
		f, more := frames.Next()
		names = append(names, f.Function)
		if !more {
			break
		}
	}
	joined := strings.Join(names, "|")
	for _, p := range parts {
		neg := strings.HasPrefix(p, "!")
		if strings.Contains(joined, strings.TrimPrefix(p, "!")) == neg {
			return false
		}
	}
	return true
}

// Claim: cancellation or deadline expiry that lands while emitDoorbell runs its
// pre-reserve scope proof is a non-notice timeout/cancel outcome and leaves no
// reservation. Limit: deterministic injection via the Now seam, runtime-binding
// fixture only; it does not prove other ctx-bound helpers classify correctly.
func TestReviewR2ScopeProofCancellation(t *testing.T) {
	cases := []struct {
		name   string
		where  []string
		expire bool
		want   string
	}{
		{"loop-top-cancel", []string{"runDoorbell", "doorbellScope", "!emitDoorbell"}, false, "doorbell: canceled"},
		{"emit-prereserve-cancel", []string{"emitDoorbell", "doorbellScope", "!reserveNotification", "!settleDoorbell"}, false, "doorbell: canceled"},
		{"emit-select-cancel", []string{"emitDoorbell", "reserveNotification", "doorbellScope"}, false, "doorbell: canceled"},
		{"emit-prereserve-deadline", []string{"emitDoorbell", "doorbellScope", "!reserveNotification", "!settleDoorbell"}, true, "doorbell: timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			runtimeMail(e, room, "pending")
			a, _ := notificationApp(t, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := a.Now
			fired := false
			a.Now = func() time.Time {
				if !fired && reviewR2Stack(c.where...) {
					fired = true
					if c.expire {
						time.Sleep(700 * time.Millisecond)
					} else {
						cancel()
					}
				}
				return now()
			}
			var out, errout bytes.Buffer
			a.Stdout, a.Stderr = &out, &errout
			code := a.fail(cmdDoorbell(ctx, a, []string{"--generation", g, "--once", "--timeout", "500ms"}))
			st := e.state("alice")
			if !fired {
				t.Fatal("setup: injection point not reached")
			}
			if code != 1 || out.String() != "" || !strings.Contains(errout.String(), c.want) || st.Notification.Reservation != nil {
				t.Fatalf("want %q, no stdout, no reservation; got code=%d stdout=%q stderr=%q reservation=%v", c.want, code, out.String(), errout.String(), st.Notification.Reservation != nil)
			}
		})
	}
}

// Claim: these two pre-reservation sites retain live-context scope refusals,
// classify actual cancellation/deadline expiry, and leave no output, token,
// receipt, shown/rung/native-acceptance marker or lease change. A fresh valid
// call still emits after each refusal; the Now seam does not mutate enrollment.
// Limit: runtime-binding fixture, not generic/native enrollment qualification.
func TestDoorbellPrereserveScopeControls(t *testing.T) {
	for _, mode := range []string{"valid", "initial-scope", "select-scope", "initial-cancel", "select-cancel", "initial-deadline", "select-deadline"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			before := e.state("alice")
			a, _ := notificationApp(t, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := a.Now
			fired := false
			where := []string{"emitDoorbell", "doorbellScope", "!reserveNotification", "!settleDoorbell"}
			if strings.HasPrefix(mode, "select-") {
				where = []string{"emitDoorbell", "reserveNotification", "doorbellScope"}
			}
			a.Now = func() time.Time {
				if mode != "valid" && !fired && reviewR2Stack(where...) {
					fired = true
					switch {
					case strings.HasSuffix(mode, "-scope"):
						return before.Lease.Expires.Add(time.Second)
					case strings.HasSuffix(mode, "-deadline"):
						time.Sleep(700 * time.Millisecond)
					default:
						cancel()
					}
				}
				return now()
			}
			var out, errout bytes.Buffer
			a.Stdout, a.Stderr = &out, &errout
			code := a.fail(cmdDoorbell(ctx, a, []string{"--generation", g, "--once", "--timeout", "500ms"}))
			st := e.state("alice")
			if mode == "valid" {
				if code != 0 || !strings.Contains(out.String(), "1 new peer event") || errout.String() != "" {
					t.Fatalf("valid enrollment rejected: code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
				}
			} else {
				want := "doorbell: canceled"
				if strings.HasSuffix(mode, "-scope") {
					want = "own enrolled session and generation required"
					if ctx.Err() != nil {
						t.Fatal("live-context scope control canceled")
					}
				} else if strings.HasSuffix(mode, "-deadline") {
					want = "doorbell: timeout"
				}
				if !fired || code != 1 || out.String() != "" || !strings.Contains(errout.String(), want) {
					t.Fatalf("pre-reserve outcome: injected=%t code=%d stdout=%q stderr=%q want=%q", fired, code, out.String(), errout.String(), want)
				}
			}
			if st.Notification.Reservation != nil || e.received("alice", id) || e.mailbox("alice").Pending()[0].Shown || len(st.Wake.Rung) != 0 || st.Wake.LastTurn != "" || !reflect.DeepEqual(before.Lease, st.Lease) {
				t.Fatal("pre-reserve path changed custody or left a reservation")
			}
			if mode != "valid" {
				r := doorbellCommand(t, e, context.Background(), g, true, time.Second)
				if r.code != 0 || !strings.Contains(r.stdout, "1 new peer event") {
					t.Fatalf("refusal spent a slot or invalidated enrollment: %+v", r)
				}
			}
		})
	}
}
