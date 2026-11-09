//go:build !darwin && !linux

package config

import (
	"context"
	"os"
)

func lockPostOperationContext(context.Context, *os.File) (func(), error) {
	return nil, ErrPostOperation
}

func postOperationPlatform() bool { return false }
func openPostOperationFile(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, ErrPostOperation
}
func postOperationOwner(os.FileInfo, bool) bool  { return false }
func lockPostOperation(*os.File) (func(), error) { return nil, ErrPostOperation }
