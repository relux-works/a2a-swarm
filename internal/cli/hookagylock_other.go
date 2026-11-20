//go:build !darwin && !linux

package cli

import (
	"errors"
	"os"
)

func lockAgySettingsDir(string, bool) (func(), error) {
	return nil, errors.New("agy settings installation unavailable on this platform")
}

func agyBridgePipe(*os.File) (*os.File, func(), error) {
	return nil, nil, errors.New("agy bridge unavailable on this platform")
}

func agyPipeWrite(*os.File, []byte) (int, error) { return 0, errors.New("unsupported agy transport") }

func agyPipePending(*os.File) (bool, error) { return false, errors.New("unsupported agy transport") }
