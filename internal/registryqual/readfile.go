package registryqual

import (
	"errors"
	"fmt"
	"io"
)

// readRegular reads at most max bytes from a regular file. The file is
// opened without following a final symlink and without blocking on a FIFO;
// the opened descriptor, not the pathname, is checked, and the read itself
// is limited, so a file replaced or grown after the check cannot exceed the
// bound.
func readRegular(path, what string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, fmt.Errorf("%s must be a regular file of at most %d bytes", what, max)
	}
	return readLimited(f, what, max)
}

// readLimited refuses a reader that yields more than max bytes.
func readLimited(r io.Reader, what string, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s grew beyond %d bytes while being read", what, max)
	}
	return b, nil
}

var errSymlink = errors.New("refusing a symlink")
