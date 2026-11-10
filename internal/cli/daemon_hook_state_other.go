//go:build !linux && !darwin

package cli

import "github.com/relux-works/a2a-swarm/internal/daemon"

func daemonStationStatus(a *App) error { return daemon.Unavailable }
