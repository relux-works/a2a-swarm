//go:build darwin || linux

package cli

import (
	"syscall"
	"time"
)

// processCPUTime returns this process's user+system CPU time (getrusage
// RUSAGE_SELF). It measures only this process, never children or the host.
func processCPUTime() (time.Duration, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	return timevalDuration(ru.Utime) + timevalDuration(ru.Stime), nil
}

func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}
