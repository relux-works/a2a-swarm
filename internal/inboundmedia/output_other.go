//go:build !linux && !darwin

package inboundmedia

import "os"

// Platforms without the qualified directory-relative no-follow primitives
// refuse; they never use the legacy checked-rename fallback.
type outputTarget struct{ file *os.File }

func openTarget(string) (*outputTarget, Code) { return nil, PublicationFailed }
func (*outputTarget) publish() Code           { return PublicationFailed }
func (*outputTarget) cleanup()                {}
