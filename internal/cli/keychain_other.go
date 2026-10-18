//go:build !unix

package cli

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func boundSecurityProcess(*exec.Cmd) error       { return errors.New("keychain requires macOS") }
func runBoundSecurityProcess(*exec.Cmd) error    { return errors.New("keychain requires macOS") }
func openKeychainState(string) (*os.File, error) { return nil, errors.New("keychain requires macOS") }
func keychainLock(string, time.Time) (func(), error) {
	return nil, errors.New("keychain requires macOS")
}
