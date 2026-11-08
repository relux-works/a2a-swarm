//go:build !darwin && !linux

package cli

import "context"

func oldLibexecStateWriter(string, string) bool { return false }

func oldLoadedLibexecStateWriter(context.Context, int, string) bool { return false }
