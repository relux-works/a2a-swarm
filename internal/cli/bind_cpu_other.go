//go:build !darwin && !linux

package cli

import (
	"errors"
	"time"
)

// Without getrusage the CPU self-alarm samples nothing and stays silent.
func processCPUTime() (time.Duration, error) {
	return 0, errors.New("process cpu time unavailable")
}
