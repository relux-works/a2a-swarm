//go:build !unix

package config

import (
	"testing"
	"time"
)

// A legacy raw waiter remains available where native/monitor are unsupported;
// this fallback must never let a monitor obtain a lifetime authority lock.
func TestRawConsumerLockUnsupportedNeighbor(t *testing.T) {
	p := testPaths(t)
	release, err := AcquireRawConsumerLock(p, "a", time.Now())
	if err != nil || release == nil {
		t.Fatal("legacy raw delivery refused", err)
	}
	release()
	if release, err := AcquireConsumerLock(p, "a", time.Now()); err == nil || release != nil {
		t.Fatal("unsupported monitor authority admitted")
	}
}
