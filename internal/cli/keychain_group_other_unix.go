//go:build unix && !darwin

package cli

import "time"

func completedSecurityGroupCleanup(_ int, _ bool, _ time.Duration, err error) error { return err }
