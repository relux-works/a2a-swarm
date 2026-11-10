//go:build !unix

package config

import "context"

func MigrateDaemon(context.Context, Paths, string, DaemonMigrationOptions) error {
	return ErrDaemonMigration
}
