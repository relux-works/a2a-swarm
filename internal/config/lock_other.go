//go:build !unix

package config

import (
	"context"
	"time"
)

// lockFile is a no-op where flock is unavailable.
func lockFile(string) (func(), error) { return func() {}, nil }

// lockFileBy is a no-op where flock is unavailable.
func lockFileBy(string, time.Time) (func(), error) { return func() {}, nil }

// openNoFollow is unavailable here; OpenMailbox still refuses a symlink by Lstat.
const openNoFollow = 0

func lockFileContext(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lockFile(path)
}
