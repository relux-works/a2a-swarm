//go:build !darwin && !linux

package cli

import "errors"

func lockAgySettingsDir(string, bool) (func(), error) {
	return nil, errors.New("agy settings installation unavailable on this platform")
}
