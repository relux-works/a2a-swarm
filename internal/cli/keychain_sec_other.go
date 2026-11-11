//go:build !(darwin && cgo)

package cli

import (
	"context"
	"errors"
)

// Keychain mode needs the macOS Security.framework, built with cgo. Every
// other build refuses it; file mode remains available.
var errKeychainUnsupported = errors.New("keychain requires a macOS cgo build")

type unsupportedBackend struct{}

func defaultKeychainBackend() keychainBackend { return unsupportedBackend{} }

func (unsupportedBackend) find(context.Context, string, string, string) ([]byte, error) {
	return nil, errKeychainUnsupported
}

func (unsupportedBackend) add(context.Context, string, string, string, []byte) error {
	return errKeychainUnsupported
}

func (unsupportedBackend) update(context.Context, string, string, string, []byte) error {
	return errKeychainUnsupported
}
