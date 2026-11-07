//go:build !darwin || !cgo || !sherpa

package voice

import "io"

const nativeSynthesis = false

func Worker(_ []string, _ io.Reader) int { return 1 }
