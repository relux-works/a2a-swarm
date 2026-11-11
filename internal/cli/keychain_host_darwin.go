package cli

import (
	"errors"
	"syscall"
)

// Stable host UUID, read from the kernel without provisioning or live config.
// Only its hash is persisted; a shared config directory must not conflate hosts.
func keychainHostBinding() (string, error) { return syscall.Sysctl("kern.uuid") }

// keychainBootSession returns the kernel's boot-session UUID
// (kern.bootsessionuuid). It is created once per boot and never changes
// within it, unlike the calendar boot time, which a clock correction can
// move. A different session than a fence recorded means the host, and with
// it securityd, has restarted since that fence's write was submitted.
func keychainBootSession() (string, error) {
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil || !validBootSession(id) {
		return "", errors.New("boot session unavailable")
	}
	return id, nil
}
