//go:build !unix

package voice

import "os"

// Native voice releases are Unix-only; fail closed on other platforms.
func openReleaseFile(string) (*os.File, error) { return nil, errHelper }
