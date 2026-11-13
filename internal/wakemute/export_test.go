//go:build linux || darwin

package wakemute

import "context"

// Test-only fault seam, absent from every production command and binary.
func FaultMuteForTest(ctx context.Context, o Operation, point func(string) error) (Result, error) {
	return mute(ctx, o, point)
}
func FaultUnmuteForTest(ctx context.Context, s Scope, run, id string, point func(string) error) (Result, error) {
	return unmute(ctx, s, run, id, point)
}
