//go:build !unix

package config

import "os"

func fileChangeTimeNS(os.FileInfo) (int64, bool) { return 0, false }
