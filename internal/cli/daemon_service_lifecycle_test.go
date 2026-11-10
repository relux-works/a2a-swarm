//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/daemonsvc"
)

type lifecycleHeartbeat struct {
	pulse func(time.Time) error
	run   func(context.Context, <-chan time.Time) error
	close func() error
}

func (h lifecycleHeartbeat) Pulse(t time.Time) error { return h.pulse(t) }
func (h lifecycleHeartbeat) Run(ctx context.Context, ticks <-chan time.Time) error {
	return h.run(ctx, ticks)
}
func (h lifecycleHeartbeat) Close() error { return h.close() }

// W-LIFECYCLE/M-HB-CANCEL: drive the exact lifecycle helper called after
// serveDaemonService's unchanged authority checks. Loop failure must cancel the
// server with Canceled (not the fixture deadline), join, and return Unavailable.
// C-LIFECYCLE narrows pre-ready/open/initial-pulse failures and clean cancellation.
// Fake I/O proves wiring only, not policy, socket binding or native signal bounds.
func TestServiceHeartbeatFailureCancelsSupervision(t *testing.T) {
	for _, kind := range []string{"clean", "loop_failure", "parent_cancel", "before_ready", "no_ready", "open_failure", "initial_pulse_failure"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var opens, pulses, runs, closes, stops atomic.Int32
			var exited, badClose atomic.Bool
			now := time.Unix(1700000000, 123456789)
			seenCancel := error(nil)
			h := lifecycleHeartbeat{
				pulse: func(at time.Time) error {
					pulses.Add(1)
					if !at.Equal(now) {
						return errors.New("wrong initial time")
					}
					if kind == "initial_pulse_failure" {
						return daemonsvc.ErrRefused
					}
					return nil
				},
				run: func(child context.Context, _ <-chan time.Time) error {
					runs.Add(1)
					defer exited.Store(true)
					if kind == "loop_failure" {
						return daemonsvc.ErrRefused
					}
					<-child.Done()
					return nil
				},
				close: func() error {
					closes.Add(1)
					if kind != "initial_pulse_failure" && !exited.Load() {
						badClose.Store(true)
					}
					return nil
				},
			}
			rt := serviceRuntime{
				open: func(path string) (serviceHeartbeat, error) {
					opens.Add(1)
					if path != home {
						return nil, errors.New("wrong home")
					}
					if kind == "open_failure" {
						return nil, daemonsvc.ErrRefused
					}
					return h, nil
				},
				now:   func() time.Time { return now },
				clock: func() (<-chan time.Time, func()) { return make(chan time.Time), func() { stops.Add(1) } },
				serve: func(child context.Context, path, socket string, _ io.Writer, handler daemon.Handler, ready func() error) error {
					if path != home || socket != "fixture-socket" || handler == nil {
						return errors.New("wrong serve arguments")
					}
					if kind == "before_ready" {
						return daemonsvc.ErrRefused
					}
					if kind == "no_ready" {
						return nil
					}
					if err := ready(); err != nil {
						return err
					}
					if kind == "loop_failure" || kind == "parent_cancel" {
						if kind == "parent_cancel" {
							cancel()
						}
						<-child.Done()
						seenCancel = child.Err()
					}
					return nil
				},
			}
			a := &App{Stderr: io.Discard}
			err := a.superviseDaemonService(ctx, home, "fixture-socket", daemonsvc.Policy{Owner: "@ivan:fixture.invalid", Identities: []string{"alice"}}, rt)
			switch kind {
			case "before_ready", "open_failure", "initial_pulse_failure":
				if !errors.Is(err, daemonsvc.ErrRefused) {
					t.Fatal("startup error lost", err)
				}
			case "loop_failure":
				if !errors.Is(err, daemon.Unavailable) || seenCancel != context.Canceled {
					t.Fatal("heartbeat did not cancel supervision", err, seenCancel)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "before_ready" || kind == "no_ready" {
				if opens.Load() != 0 || pulses.Load() != 0 || runs.Load() != 0 || closes.Load() != 0 {
					t.Fatal("heartbeat before readiness")
				}
				return
			}
			if kind == "open_failure" {
				if opens.Load() != 1 || pulses.Load() != 0 || runs.Load() != 0 || closes.Load() != 0 {
					t.Fatal("open failure leaked")
				}
				return
			}
			if kind == "initial_pulse_failure" {
				if pulses.Load() != 1 || runs.Load() != 0 || closes.Load() != 1 || stops.Load() != 0 {
					t.Fatal("initial pulse failure launched loop")
				}
				return
			}
			if opens.Load() != 1 || pulses.Load() != 1 || runs.Load() != 1 || closes.Load() != 1 || stops.Load() != 1 || badClose.Load() {
				t.Fatal("lifecycle not joined", opens.Load(), pulses.Load(), runs.Load(), closes.Load(), stops.Load(), badClose.Load())
			}
		})
	}
}

// W-LIFECYCLE-JOIN/C-EARLY-CLOSE: a cancelled loop deliberately waits at its
// exit barrier. The supervisor must wait for that barrier before Close/return.
// A short observation window checks forbidden early effects; completion is
// additionally bounded by the fixture context and no real service is started.
func TestServiceLifecycleJoinsHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sawCancel := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	done := make(chan error, 1)
	var earlyClose atomic.Bool
	h := lifecycleHeartbeat{
		pulse: func(time.Time) error { return nil },
		run: func(child context.Context, _ <-chan time.Time) error {
			<-child.Done()
			close(sawCancel)
			select {
			case <-release:
			case <-ctx.Done():
			}
			close(exited)
			return nil
		},
		close: func() error {
			select {
			case <-exited:
			default:
				earlyClose.Store(true)
			}
			close(closed)
			return nil
		},
	}
	rt := serviceRuntime{
		open: func(string) (serviceHeartbeat, error) { return h, nil }, now: time.Now,
		clock: func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} },
		serve: func(_ context.Context, _ string, _ string, _ io.Writer, _ daemon.Handler, ready func() error) error {
			return ready()
		},
	}
	a := &App{Stderr: io.Discard}
	home := t.TempDir()
	go func() { done <- a.superviseDaemonService(ctx, home, "fixture", daemonsvc.Policy{}, rt) }()
	select {
	case <-sawCancel:
	case <-ctx.Done():
		t.Fatal("service did not cancel loop after serve returned")
	}
	select {
	case <-closed:
		t.Error("heartbeat closed before loop exit")
	case <-done:
		close(release)
		t.Fatal("supervisor returned before loop exit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil || earlyClose.Load() {
			t.Fatal("join violation", err, earlyClose.Load())
		}
	case <-ctx.Done():
		t.Fatal("join did not complete")
	}
}
