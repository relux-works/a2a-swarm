//go:build !darwin && !linux

package cli

import (
	"context"
	"os"
)

type terminalRefresh struct{}

func newTerminalRefresh(context.Context) *terminalRefresh                       { return &terminalRefresh{} }
func (*terminalRefresh) poll(context.Context, *App, *session, string, *os.File) {}
