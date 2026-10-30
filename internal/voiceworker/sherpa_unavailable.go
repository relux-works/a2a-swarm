//go:build !darwin || !cgo || !sherpa

package voiceworker

import "io"

func Worker(_ []string, _ io.Reader) int { return 1 }
