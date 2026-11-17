//go:build !unix

package envelopeobserve

import "os"

func protectedInfo(st os.FileInfo, directory bool) bool {
	return protectedStorageInfo(st, directory)
}

// No qualified protected-file ownership adapter exists on these platforms.
func protectedStorageInfo(os.FileInfo, bool) bool { return false }
