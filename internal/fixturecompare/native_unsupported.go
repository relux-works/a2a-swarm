//go:build !darwin || !arm64 || !cgo

package fixturecompare

import "errors"

func inspect(string) (signature, error) {
	return signature{}, errors.New("fixture-darwin-arm64-cgo-required")
}
