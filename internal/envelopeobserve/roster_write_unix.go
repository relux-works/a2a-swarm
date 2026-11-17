//go:build unix

package envelopeobserve

import (
	"os"
	"syscall"
)

const rosterOpenNoFollow = syscall.O_NOFOLLOW

type hostRosterProtection struct{}

// Only root can create the root-owned file the reader requires.
func (hostRosterProtection) CallerMayPublish() bool { return os.Geteuid() == 0 }

// The reader and writer share storage facts but have opposite caller policies.
func (hostRosterProtection) Protected(st os.FileInfo, directory bool) bool {
	return protectedStorageInfo(st, directory)
}
