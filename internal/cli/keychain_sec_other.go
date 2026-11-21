//go:build !(darwin && cgo)

package cli

import "context"

// Keychain mode needs the macOS Security.framework, built with cgo. Every
// other build refuses it at store selection; file mode remains available. The
// backend below refuses too, for a caller that reaches it another way.
const keychainBuildSupported = false

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
