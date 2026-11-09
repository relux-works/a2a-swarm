//go:build !unix

package config

import "context"

func MigrateCourier(context.Context, Paths, string, CourierMigrationOptions) error {
	return ErrCourierMigration
}
