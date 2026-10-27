//go:build !unix

package config

import "os"

const presenceOpenFlags = os.O_RDONLY

// No owner/permission equivalence is asserted on unsupported platforms.
func presenceOwned(os.FileInfo) bool { return false }
