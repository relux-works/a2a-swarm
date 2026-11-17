//go:build !unix

package envelopeobserve

import "os"

const rosterOpenNoFollow = 0

type hostRosterProtection struct{}

// No qualified ownership adapter exists here; the reader refuses every roster
// on these platforms, so nothing publishable can be produced.
func (hostRosterProtection) CallerMayPublish() bool { return false }

func (hostRosterProtection) Protected(st os.FileInfo, directory bool) bool {
	return protectedStorageInfo(st, directory)
}
