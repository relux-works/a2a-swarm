//go:build !darwin && !linux

package cli

import (
	"context"
	"github.com/relux-works/a2a-swarm/internal/daemon"
)

func cmdDaemonService(context.Context, *App, []string) error { return daemon.NotAllowed }
func (a *App) serveDaemonService(context.Context, string, string, string, string, string, uint) error {
	return daemon.NotAllowed
}
