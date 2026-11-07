//go:build !linux && !darwin

package voiceaudio

type scratch struct{}

func newScratch(string) (*scratch, Reason) { return nil, PlatformUnqualified }
func (*scratch) cleanup() bool             { return true }
func (*scratch) writeInput([]byte) bool    { return false }
