//go:build !unix

package cli

import (
	"fmt"
	"os"
)

func openSavedInboxFile(root *os.Root, name string) (*os.File, error) {
	return nil, fmt.Errorf("reading saved inbox frames requires no-follow file support on this platform")
}
