//go:build !unix

package config

import (
	"errors"
	"os"
	"time"
)

func WithConsumerLock(Paths, string, time.Time, int, func(*os.File) error) error {
	return errors.New("terminal consumer requires Unix file locking")
}

func AcquireConsumerLock(Paths, string, time.Time) (func(), error) {
	return nil, errors.New("consumer requires Unix file locking")
}

// Native/monitor admission still refuses without Unix locking. Raw delivery
// predates those backends and retains its existing behavior on these platforms.
func AcquireRawConsumerLock(Paths, string, time.Time) (func(), error) {
	return func() {}, nil
}
