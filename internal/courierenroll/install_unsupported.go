//go:build !linux && !darwin

package courierenroll

import "io"

func Install(dir string, r io.Reader, rotate bool) error {
	return errWrite
}
