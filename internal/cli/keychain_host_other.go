//go:build !darwin

package cli

import (
	"errors"
	"os"
)

// Used by isolated tests only; production Keychain selection requires Darwin.
func keychainHostBinding() (string, error) { return os.Hostname() }

// Without a boot session, reconcile never treats a restart as quiescence.
func keychainBootSession() (int64, error) { return 0, errors.New("boot session unavailable") }
