package cli

import "syscall"

// Stable host UUID, read from the kernel without provisioning or live config.
// Only its hash is persisted; a shared config directory must not conflate hosts.
func keychainHostBinding() (string, error) { return syscall.Sysctl("kern.uuid") }
