//go:build !linux && !darwin

package voicetranscribe

import (
	"context"
	"os"
)

func safeDirectory(os.FileInfo, bool) bool            { return false }
func safeArtifact(os.FileInfo) bool                   { return false }
func openReadOnly(*os.Root, string) (*os.File, error) { return nil, errUnsafe }
func runEngine(context.Context, string) ([]byte, *Refusal) {
	return nil, &Refusal{BackendUnavailable, BackendStage}
}
