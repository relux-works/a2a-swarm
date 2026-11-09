//go:build unix && !linux && !darwin

package config

const courierPublishSupported = false

func courierPublishDirectory(string, string) error { return ErrCourierMigration }
