package cli

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// Darwin can report EPERM for a group containing only exiting/zombie members.
// A successful Wait reaps our leader; ESRCH from signal 0 then proves the group
// has disappeared. EPERM alone never proves cleanup, including for zombies
// awaiting their parent's wait. Observe synchronously within the existing bound;
// never reissue a destructive signal to a potentially reusable PGID.
func completedSecurityGroupCleanup(pgid int, reaped bool, wait time.Duration, err error) error {
	return reconcileSecurityGroupCleanup(pgid, reaped, wait, err, syscall.Kill)
}

func reconcileSecurityGroupCleanup(pgid int, reaped bool, wait time.Duration, err error, probe func(int, syscall.Signal) error) error {
	if !reaped || !errors.Is(err, syscall.EPERM) {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		observed := probe(-pgid, 0)
		if errors.Is(observed, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if !errors.Is(observed, syscall.EPERM) {
			return err // A live group or an unknown result cannot certify cleanup.
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		time.Sleep(min(time.Millisecond, remaining))
	}
}
