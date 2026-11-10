//go:build unix && !linux && !darwin

package config

const daemonPublishSupported = false

func daemonPublishDirectory(string, string) error { return ErrDaemonMigration }
