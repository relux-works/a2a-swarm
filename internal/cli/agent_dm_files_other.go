//go:build !unix

package cli

import (
	"errors"
	"os"
)

func agentOwned(os.FileInfo) bool { return false }
func agentOpenPrivate(string) (*os.File, error) {
	return nil, errors.New("agent-dm requires Unix private files and locking")
}
