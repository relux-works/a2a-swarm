//go:build !darwin && !linux

package cli

import "errors"

// Without a fork-free process identity the quiescent wait is unavailable and
// the consumer keeps its ordinary active loop.
func processStart(int) (string, error) { return "", errors.New("process start unavailable") }
