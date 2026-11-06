//go:build !darwin && !linux

package cli

import "fmt"

func selectHookExecutable(hookExecutableChoice) (hookExecutableSelection, error) {
	return hookExecutableSelection{}, fmt.Errorf("hook executable validation is unavailable on this platform")
}
