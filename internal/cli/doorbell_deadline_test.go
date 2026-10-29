package cli

// Retained independent ISSUE-123 review-r1 regressions. Each test
// asserts the documented contract; a FAIL is the reproduction, the paired
// "control" subtest is the valid neighbor that must stay green.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type reviewSlowWriter struct {
	delay time.Duration
	buf   bytes.Buffer
}

type deadlineOutcomeWriter struct {
	write func([]byte) (int, error)
	flush func() error
}

func (w deadlineOutcomeWriter) Write(p []byte) (int, error) { return w.write(p) }
func (w deadlineOutcomeWriter) Flush() error {
	if w.flush != nil {
		return w.flush()
	}
	return nil
}

// Cancellation must not replace an actual complete, failed or uncertain local
// transport result. Partial/flush ambiguity stays fenced, proven-zero output
// can cancel only the exact own token, and foreign scope/token stays untouched.
// Cancellation here is deterministic; the retained reviewer tests cover real
// deadlines with a returning slow writer and a delayed pre-write reservation.
func TestDoorbellDeadlineOutcomes(t *testing.T) {
	for _, mode := range []string{"complete", "complete-canceled", "partial-canceled", "flush-canceled", "zero-error-canceled", "reserve-error-canceled", "generation-before-write", "token-before-write", "generation-after-write", "token-after-write", "canceled-before-write"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			clock := e.clock()
			a, _ := notificationApp(t, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out, errout bytes.Buffer
			var foreign *config.NotificationReservation
			mutate := func(generation bool) {
				notificationState(t, e, func(st *config.State) {
					if generation {
						st.Runtime.Generation = "successor"
					} else {
						token := st.Notification.Reservation.Token
						head := "a"
						if token[0] == 'a' {
							head = "b"
						}
						st.Notification.Reservation.Token = head + token[1:]
					}
					copy := *st.Notification.Reservation
					foreign = &copy
				})
			}
			w := deadlineOutcomeWriter{write: func(p []byte) (int, error) {
				switch mode {
				case "partial-canceled":
					cancel()
					n, _ := out.Write(p[:3])
					return n, io.ErrShortWrite
				case "zero-error-canceled":
					cancel()
					return 0, errors.New("HOSTILE_WRITER_ERROR")
				case "complete-canceled":
					cancel()
				case "generation-after-write", "token-after-write":
					mutate(mode == "generation-after-write")
					cancel()
				}
				return out.Write(p)
			}}
			if mode == "flush-canceled" {
				w.flush = func() error { cancel(); return errors.New("HOSTILE_FLUSH_ERROR") }
			}
			switch mode {
			case "reserve-error-canceled", "generation-before-write", "token-before-write", "canceled-before-write":
				a.afterNotificationReserve = func() error {
					if mode == "generation-before-write" || mode == "token-before-write" {
						mutate(mode == "generation-before-write")
					}
					cancel()
					if mode == "reserve-error-canceled" {
						return errors.New("HOSTILE_RESERVE_ERROR")
					}
					return nil
				}
			}
			a.Stdout, a.Stderr = w, &errout
			code := a.fail(cmdDoorbell(ctx, a, []string{"--generation", g, "--once", "--timeout", "2s"}))
			st := e.state("alice")
			r := result{code: code, stdout: out.String(), stderr: errout.String()}
			e.assertNoLeak(r)
			complete := mode == "complete" || mode == "complete-canceled"
			zero := mode == "zero-error-canceled" || mode == "canceled-before-write"
			if complete {
				if code != 0 || r.stderr != "" || !strings.HasSuffix(r.stdout, "\n") || st.Notification.Reservation != nil {
					t.Fatalf("completed local result masked or fenced: %+v reservation=%v", r, st.Notification.Reservation != nil)
				}
			} else if zero {
				want := "doorbell: unavailable; no receipt recorded"
				if mode == "canceled-before-write" {
					want = "doorbell: canceled"
				}
				if code != 1 || !strings.Contains(r.stderr, want) || r.stdout != "" || st.Notification.Reservation != nil {
					t.Fatalf("proven-zero result masked or fenced: %+v reservation=%v", r, st.Notification.Reservation != nil)
				}
			} else {
				if code != 1 || !strings.Contains(r.stderr, "retained uncertain attempt") || st.Notification.Reservation == nil {
					t.Fatalf("uncertainty masked or released: %+v reservation=%v", r, st.Notification.Reservation != nil)
				}
				if foreign != nil && !reflect.DeepEqual(foreign, st.Notification.Reservation) {
					t.Fatal("foreign reservation was changed")
				}
			}
			if e.received("alice", id) || e.mailbox("alice").Pending()[0].Shown || len(st.Wake.Rung) != 0 || st.Wake.LastTurn != "" || strings.Contains(r.stderr, "HOSTILE_") {
				t.Fatal("deadline path receipted, announced native acceptance or leaked error text")
			}
			clock.add(31 * time.Second)
			runtimeMail(e, room, "new event after spacing")
			next := doorbellCommand(t, e, context.Background(), g, true, 2*time.Second)
			if complete || zero {
				if next.code != 0 || !strings.Contains(next.stdout, "2 new peer events") {
					t.Fatalf("settled local result prevented valid new-event recovery: %+v", next)
				}
			} else if next.code != 1 || next.stdout != "" {
				t.Fatalf("uncertain/foreign scope retried: %+v", next)
			}
		})
	}
}

// A returning full writer followed by a held fixture state lock remains
// uncertain, but settlement terminates at its own finite budget despite caller
// cancellation. The available-lock neighbor completes and leaves no fence.
func TestDoorbellDeadlineSettlementBounded(t *testing.T) {
	for _, hold := range []bool{false, true} {
		name := "available"
		if hold {
			name = "held-state-lock"
		}
		t.Run(name, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			id := runtimeMail(e, room, "pending")
			a, _ := notificationApp(t, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release, locked, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var out, errout bytes.Buffer
			var writeReturned time.Time
			started := false
			defer func() {
				if started {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
				}
			}()
			a.Stdout = deadlineOutcomeWriter{write: func(p []byte) (int, error) {
				if hold {
					started = true
					go func() {
						_, err := config.UpdateStateBy(e.paths(), "alice", time.Now().Add(time.Second), func(_ *config.State) error {
							close(locked)
							<-release
							return nil
						})
						done <- err
					}()
					select {
					case <-locked:
					case <-time.After(2 * time.Second):
						return 0, errors.New("fixture did not acquire state lock")
					}
				}
				cancel()
				writeReturned = time.Now()
				return out.Write(p)
			}}
			a.Stderr = &errout
			code := a.fail(cmdDoorbell(ctx, a, []string{"--generation", g, "--once", "--timeout", "2s"}))
			elapsed := time.Since(writeReturned)
			if writeReturned.IsZero() || !strings.HasSuffix(out.String(), "\n") {
				t.Fatal("setup did not complete stdout")
			}
			if hold {
				if code != 1 || !strings.Contains(errout.String(), "retained uncertain attempt") || elapsed < nativeRPCTimeout/2 || elapsed > nativeRPCTimeout+2*time.Second {
					t.Fatalf("settlement did not honor its finite uncertainty bound: code=%d elapsed=%s budget=%s stderr=%q", code, elapsed, nativeRPCTimeout, errout.String())
				}
			} else if code != 0 || errout.Len() != 0 {
				t.Fatalf("valid canceled-writer completion failed: code=%d stderr=%q", code, errout.String())
			}
			if started {
				close(release)
				started = false
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if (e.state("alice").Notification.Reservation != nil) != hold || e.received("alice", id) {
				t.Fatal("settlement lost uncertainty or receipted mail")
			}
		})
	}
}

// A canceled call that cannot enter the already-owned consumer must use the
// same fixed non-notice classification, without receipts or a second owner.
func TestDoorbellDeadlineConsumerCancellation(t *testing.T) {
	e, room, g := doorbellFixture(t)
	id := runtimeMail(e, room, "pending")
	stop := notificationConsumerLock(t, e)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := doorbellCommand(t, e, ctx, g, true, time.Second)
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "doorbell: canceled") || e.received("alice", id) {
		t.Fatalf("consumer-lock cancellation lost its fixed outcome: %+v", r)
	}
}

func (w *reviewSlowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.buf.Write(p)
}

func reviewOnce(t *testing.T, e *env, g string, timeout time.Duration, w *reviewSlowWriter, afterReserve func() error) result {
	t.Helper()
	a, _ := notificationApp(t, e)
	var errout bytes.Buffer
	a.Stdout, a.Stderr = w, &errout
	a.afterNotificationReserve = afterReserve
	code := a.fail(cmdDoorbell(context.Background(), a, []string{"--generation", g, "--once", "--timeout", timeout.String()}))
	return result{code: code, stdout: w.buf.String(), stderr: errout.String()}
}

// Claim: docs/doorbell.md says callers distinguish NON-notice outcomes by the
// fixed stderr classification and that "timeout is not a bell". A --once run
// whose complete notice reached stdout must therefore not be classified as
// `doorbell: timeout`, and must not leave a retained token that fences every
// later run. Limit: the slow writer models a slow-but-returning stdout reader
// (the doc requires writers to return; it does not require them to be fast).
func TestReviewR1OnceNoticeNearDeadline(t *testing.T) {
	for _, mode := range []string{"control", "slow-writer"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			clock := e.clock()
			runtimeMail(e, room, "pending")
			w := &reviewSlowWriter{}
			timeout := 2 * time.Second
			if mode == "slow-writer" {
				timeout, w.delay = 500*time.Millisecond, 900*time.Millisecond
			}
			r := reviewOnce(t, e, g, timeout, w, nil)
			st := e.state("alice")
			t.Logf("first: code=%d stdout=%q stderr=%q reservation=%v", r.code, r.stdout, r.stderr, st.Notification.Reservation != nil)
			complete := strings.HasPrefix(r.stdout, "a2a: 1 new peer event") && strings.HasSuffix(r.stdout, "\n")
			if !complete {
				t.Fatalf("setup: no complete notice: %+v", r)
			}
			if strings.Contains(r.stderr, "doorbell: timeout") {
				t.Errorf("complete notice on stdout classified as non-notice timeout: code=%d stderr=%q", r.code, r.stderr)
			}
			if mode == "control" && r.code != 0 {
				t.Fatalf("control failed: %+v", r)
			}
			// Later bell for a new event after spacing must not be fenced by
			// the earlier, fully written notice.
			clock.add(31 * time.Second)
			runtimeMail(e, room, "second")
			next := reviewOnce(t, e, g, 2*time.Second, &reviewSlowWriter{}, nil)
			t.Logf("next: code=%d stdout=%q stderr=%q", next.code, next.stdout, next.stderr)
			if next.code != 0 || next.stdout == "" {
				t.Errorf("fully written notice left a permanent fence: %+v", next)
			}
		})
	}
}

// Claim: a run that reserved but wrote ZERO bytes (deadline reached before
// the write) has proven no output; the doc lets a proven no-output token be
// canceled. It must not permanently fence later runs. Limit: the reserve
// hook is a test seam modeling scheduling/IO latency at the deadline.
func TestReviewR1DeadlineBeforeWrite(t *testing.T) {
	for _, mode := range []string{"control", "deadline-before-write"} {
		t.Run(mode, func(t *testing.T) {
			e, room, g := doorbellFixture(t)
			clock := e.clock()
			runtimeMail(e, room, "pending")
			w := &reviewSlowWriter{}
			timeout := 2 * time.Second
			var hook func() error
			if mode == "deadline-before-write" {
				timeout = 500 * time.Millisecond
				hook = func() error { time.Sleep(700 * time.Millisecond); return nil }
			}
			r := reviewOnce(t, e, g, timeout, w, hook)
			st := e.state("alice")
			t.Logf("first: code=%d stdout=%q stderr=%q reservation=%v", r.code, r.stdout, r.stderr, st.Notification.Reservation != nil)
			if mode == "control" && (r.code != 0 || r.stdout == "") {
				t.Fatalf("control failed: %+v", r)
			}
			if mode != "control" && r.stdout != "" {
				t.Fatalf("setup: expected zero output: %+v", r)
			}
			clock.add(31 * time.Second)
			runtimeMail(e, room, "second")
			next := reviewOnce(t, e, g, 2*time.Second, &reviewSlowWriter{}, nil)
			t.Logf("next: code=%d stdout=%q stderr=%q", next.code, next.stdout, next.stderr)
			if next.code != 0 || next.stdout == "" {
				t.Errorf("zero-output deadline left a permanent uncertain fence: %+v", next)
			}
		})
	}
}
