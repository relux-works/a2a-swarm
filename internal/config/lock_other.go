//go:build !unix

package config

import "time"

// lockFile is a no-op where flock is unavailable.
func lockFile(string) (func(), error) { return func() {}, nil }

// lockFileBy is a no-op where flock is unavailable.
func lockFileBy(string, time.Time) (func(), error) { return func() {}, nil }
