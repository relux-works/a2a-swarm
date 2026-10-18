//go:build !darwin

package cli

import "os"

// Used by isolated tests only; production Keychain selection requires Darwin.
func keychainHostBinding() (string, error) { return os.Hostname() }
