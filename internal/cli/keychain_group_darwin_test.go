package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// Real synthetic probe processes retain their successful result and complete
// output after mandatory group cleanup; no Keychain or host probe is invoked.
func TestKeychainCompletedGroupReaped(t *testing.T) {
	for i := 0; i < 128; i++ {
		cmd := unlockedProbe(context.Background())
		var stdout, stderr limitedSecretBuffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := boundSecurityProcess(cmd); err != nil {
			t.Fatal(err)
		}
		if err := runBoundSecurityProcess(cmd); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if cmd.ProcessState == nil || !cmd.ProcessState.Success() || stdout.String() != "unlocked\n" || stderr.Len() != 0 {
			t.Fatalf("probe %d: state=%v stdout=%q stderr=%q", i, cmd.ProcessState, stdout.String(), stderr.String())
		}
	}
}

// Proves that EPERM is accepted only after an explicit ESRCH observation.
// A present group, persistent denial (including unreaped members), or an unknown
// probe result must preserve the original refusal. No signal is reissued.
func TestKeychainGroupTeardownVerdicts(t *testing.T) {
	const pgid = 4242
	const budget = 100 * time.Millisecond
	for _, tc := range []struct {
		name         string
		initial      error
		observations []error
		want         error
		waited       bool
		unreaped     bool
	}{
		{name: "killed", initial: nil, want: nil},
		{name: "already-gone", initial: os.ErrProcessDone, want: os.ErrProcessDone},
		{name: "other-error", initial: syscall.EIO, want: syscall.EIO},
		{name: "unreaped-leader", initial: syscall.EPERM, want: syscall.EPERM, unreaped: true},
		{name: "denied-then-gone", initial: syscall.EPERM, observations: []error{syscall.ESRCH}, want: os.ErrProcessDone},
		{name: "reaping-then-gone", initial: syscall.EPERM, observations: []error{syscall.EPERM, syscall.EPERM, syscall.ESRCH}, want: os.ErrProcessDone},
		{name: "live-group", initial: syscall.EPERM, observations: []error{nil}, want: syscall.EPERM},
		{name: "probe-failure", initial: syscall.EPERM, observations: []error{syscall.EIO}, want: syscall.EPERM},
		{name: "probe-interrupted", initial: syscall.EPERM, observations: []error{syscall.EINTR}, want: syscall.EPERM},
		{name: "denied-or-unreaped", initial: syscall.EPERM, observations: []error{syscall.EPERM}, want: syscall.EPERM, waited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				probe := func(pid int, sig syscall.Signal) error {
					if pid != -pgid || sig != 0 {
						t.Fatalf("cleanup exceeded read-only owned-group observation: pid=%d signal=%d", pid, sig)
					}
					if len(tc.observations) == 0 {
						t.Fatal("successful cleanup or unrelated error probed a group")
					}
					at := calls
					calls++
					if at >= len(tc.observations) {
						at = len(tc.observations) - 1
					}
					return tc.observations[at]
				}
				start := time.Now()
				got := reconcileSecurityGroupCleanup(pgid, !tc.unreaped, budget, tc.initial, probe)
				if !errors.Is(got, tc.want) {
					t.Fatalf("cleanup verdict=%v want=%v", got, tc.want)
				}
				elapsed := time.Since(start)
				if tc.waited {
					if elapsed != budget || calls < 2 {
						t.Fatalf("unresolved group must refuse after bounded observations: elapsed=%s calls=%d", elapsed, calls)
					}
				} else if calls != len(tc.observations) || elapsed > budget {
					t.Fatalf("observations=%d want=%d elapsed=%s", calls, len(tc.observations), elapsed)
				}
				before := calls
				time.Sleep(2 * budget)
				synctest.Wait()
				if calls != before {
					t.Fatal("cleanup retained a reusable PGID after return")
				}
			})
		})
	}
}
