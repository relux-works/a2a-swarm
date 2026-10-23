//go:build !unix

package cli

import (
	"context"
	"errors"
)

func (f ownerDMFiles) lock(context.Context) (func(), error) {
	return nil, errors.New("owner-dm requires OS file locking on this platform")
}
