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
