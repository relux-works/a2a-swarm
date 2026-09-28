//go:build !unix

package config

// lockFile is a no-op where flock is unavailable.
func lockFile(string) (func(), error) { return func() {}, nil }
