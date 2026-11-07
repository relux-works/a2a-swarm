package voicetranscribe

import (
	"context"
	"errors"
)

var errOutputLimit = errors.New("local ASR output limit")

type limitedOutput struct {
	data     []byte
	overflow bool
	cancel   context.CancelFunc
}

func (w *limitedOutput) Write(b []byte) (int, error) {
	if len(b) > MaxStdoutBytes-len(w.data) {
		w.overflow = true
		w.cancel()
		return 0, errOutputLimit
	}
	w.data = append(w.data, b...)
	return len(b), nil
}
