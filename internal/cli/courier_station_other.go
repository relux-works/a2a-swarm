//go:build !linux && !darwin

package cli

import "context"

func (a *App) stationInvocation(context.Context, []string) (bool, int) { return false, 0 }
