//go:build unix && !darwin && !linux

package config

import "os"

func fileChangeTimeNS(os.FileInfo) (int64, bool) { return 0, false }
