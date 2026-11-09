//go:build !linux && !darwin

package cli

import "github.com/relux-works/a2a-swarm/internal/courier"

func courierStationStatus(a *App) error { return courier.Unavailable }
