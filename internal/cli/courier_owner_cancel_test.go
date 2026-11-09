//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"golang.org/x/sys/unix"
)

// A real station disconnect cancels the invocation while owner recording is
// behind a real State flock. Release only after daemon cancellation is observed;
// the cancelled invocation must finish without replacing the owner. A subsequent
// live hook must still replace it normally.
func TestCourierMonitorOwnerDisconnect(t *testing.T) {
	var capture atomic.Bool
	entered := make(chan context.Context, 1)
	finished := make(chan struct{}, 1)
	f := newCourierFixtureHandler(t, func(next courier.Handler) courier.Handler {
		return func(ctx context.Context, inv courier.Invocation) int {
			if capture.Load() {
				entered <- ctx
				defer func() { finished <- struct{}{} }()
			}
			return next(ctx, inv)
		}
	})
	p := f.paths["alice"]
	if _, err := config.UpdateState(p, "alice", func(s *config.State) error { s.HooksPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	start := func(id string) string { return `{"session_id":"` + id + `","hook_event_name":"SessionStart"}` }
	r := f.run("alice", start("old-owner"), "--as", "alice", "hook", "session-start", "--runtime", "muse")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatal("owner prerequisite", r)
	}
	before, err := os.ReadFile(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(filepath.Join(filepath.Dir(p.StateFile("alice")), "alice.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	capture.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		var out, diag flushBuffer
		code := f.app("alice", start("cancelled-owner"), &out, &diag).Run(ctx, []string{"--as", "alice", "hook", "session-start", "--runtime", "muse"})
		stdout, _ := out.snapshot()
		stderr, _ := diag.snapshot()
		done <- result{code: code, stdout: stdout, stderr: stderr}
	}()
	var invocation context.Context
	select {
	case invocation = <-entered:
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("hook never entered engine")
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	bound := time.NewTimer(courier.LockReleaseBound)
	defer bound.Stop()
	for {
		err := config.WithRuntimeLock(p, "alice", "control", time.Now(), func() error { return nil })
		if errors.Is(err, config.ErrLockTimeout) {
			break
		}
		if err != nil {
			t.Fatal("control probe", err)
		}
		select {
		case r := <-done:
			t.Fatal("hook ended before contention", r)
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-bound.C:
			t.Fatal("owner writer did not acquire control lock")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case <-invocation.Done():
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-bound.C:
		t.Fatal("disconnect did not reach engine")
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("owner hook retained after cancellation")
	}
	select {
	case r := <-done:
		if r.code != 0 || r.stdout != "" {
			t.Fatal("cancelled hook exit", r)
		}
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("station retained")
	}
	after, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("disconnected hook completed an owner write", err)
	}
	capture.Store(false)
	// The live neighbor may report the cancelled hook's legitimate prior gap.
	r = f.run("alice", start("live-owner"), "--as", "alice", "hook", "session-start", "--runtime", "muse")
	st, err := config.LoadState(p, "alice")
	if r.code != 0 || r.stdout != "" || err != nil || st.MonitorOwner == nil || st.MonitorOwner.SessionIDHash != cliOwnerHash("live-owner") {
		t.Fatal("live owner hook neighbor", r, err)
	}
}
