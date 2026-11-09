//go:build !linux && !darwin

package courier

import (
	"context"
	"io"
)

func Register(home, station, identity string, uid uint32, rotate bool) ([]byte, error) {
	return nil, errRefused
}
func Unregister(home, station string) error                               { return errRefused }
func Serve(ctx context.Context, home, socket string, log io.Writer) error { return errRefused }

func ServeWithHandler(ctx context.Context, home, socket string, log io.Writer, handler Handler) error {
	return errRefused
}

func IdentityHome(home, identity string) (string, error) { return "", errRefused }
