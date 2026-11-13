//go:build !linux && !darwin

package fixture

import (
	"errors"
	"os"
)

func openPTY() (*os.File, *os.File, error) { return nil, nil, errors.New("fixture PTY unsupported") }
