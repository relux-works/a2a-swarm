//go:build !unix

package config

import "os"

// Without a portable file identity every stamp is unknown, so a consumer
// never treats an unidentified file as unchanged.
func fileIdentity(os.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }
