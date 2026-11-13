//go:build !darwin && !linux

package cli

import (
	"context"
	"os"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type terminalRefresh struct{}

func newTerminalRefresh(context.Context) *terminalRefresh                       { return &terminalRefresh{} }
func (*terminalRefresh) poll(context.Context, *App, *session, string, *os.File) {}
func (*terminalRefresh) pending() bool                                          { return false }
func (*terminalRefresh) aliasStamps() [2]config.FileStamp {
	return [2]config.FileStamp{{Known: true}, {Known: true}}
}

// Without the native custody checks no launch alias is admitted.
func admittedLaunchAlias(string, string) (string, bool) { return "", false }
