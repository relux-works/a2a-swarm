//go:build !linux || !amd64

package voiceaudio

import "context"

// No cross-compile is a native qualification. A separate admitted profile and
// native resource/loader audit are required before enabling another platform.
func snapshot(context.Context, *scratch, Profile) Reason   { return PlatformUnqualified }
func runNative(context.Context, *scratch) ([]byte, Reason) { return nil, PlatformUnqualified }
