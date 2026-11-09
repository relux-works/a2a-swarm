//go:build !darwin && !linux

package admission

import (
	"context"
	"os"
)

func lock(context.Context, *os.Root) (func(), error) { return nil, ErrStorage }
