//go:build !unix

package config

import "os"

// The provider currently relies on Unix ownership and no-follow semantics.
// Unsupported platforms fail closed until they have an equivalent boundary.
func bindPartCurrentUserOwns(os.FileInfo) bool { return false }
