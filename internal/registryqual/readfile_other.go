//go:build !unix

package registryqual

import "os"

func refuseSymlink(path string) error {
	st, err := os.Lstat(path)
	if err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	return nil
}

// openNoFollow without O_NOFOLLOW checks the final path component first.
// This leaves a replacement race that the unix variant does not have; the
// descriptor check and limited read in readRegular still bound what is read.
func openNoFollow(path string) (*os.File, error) {
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	return os.Open(path)
}
