//go:build !darwin && !linux

package cli

import (
	"context"
	"os"
)

type terminalRefresh struct{}

func newTerminalRefresh(context.Context) *terminalRefresh                       { return &terminalRefresh{} }
func (*terminalRefresh) poll(context.Context, *App, *session, string, *os.File) {}

// Without the native custody checks no launch alias is admitted.
func admittedLaunchAlias(string, string) (string, bool) { return "", false }
