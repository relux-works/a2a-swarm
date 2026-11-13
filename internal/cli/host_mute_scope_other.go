//go:build !unix

package cli

import (
	"os"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
)

func openWakeScope(string) (*os.File, error) { return nil, wakemute.ErrUnsupported }
