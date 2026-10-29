package cli

import (
	"encoding/binary"
	"errors"
	"syscall"
)

// Stable host UUID, read from the kernel without provisioning or live config.
// Only its hash is persisted; a shared config directory must not conflate hosts.
func keychainHostBinding() (string, error) { return syscall.Sysctl("kern.uuid") }

// keychainBootSession identifies the current boot by its kern.boottime
// seconds. A different value than a fence recorded means the host, and with
// it securityd, has restarted since that fence's write was submitted.
func keychainBootSession() (int64, error) {
	raw, err := syscall.Sysctl("kern.boottime")
	if err != nil || len(raw) < 8 {
		return 0, errors.New("boot session unavailable")
	}
	sec := int64(binary.LittleEndian.Uint64([]byte(raw[:8])))
	if sec <= 0 {
		return 0, errors.New("boot session unavailable")
	}
	return sec, nil
}
