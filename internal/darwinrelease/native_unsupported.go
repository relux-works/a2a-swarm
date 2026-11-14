//go:build !darwin || !arm64 || !cgo

package darwinrelease

import "errors"

func CompileRequirement(string) ([]byte, error) { return nil, errors.New("darwin-arm64-cgo-required") }
func nativeVerify(string, string) (string, []byte, error) {
	return "", nil, errors.New("darwin-arm64-cgo-required")
}

type signingKeychain struct{ path, selector string }

func openSigningKeychain(string, map[string]string) (*signingKeychain, error) {
	return nil, errors.New("darwin-arm64-cgo-required")
}
func (k *signingKeychain) close() error { return nil }

type unsupportedSearchList struct{}

func releaseSearchList() searchListOps { return unsupportedSearchList{} }

func (unsupportedSearchList) Get() ([]string, error) { return nil, errSearchListUnsupported }
func (unsupportedSearchList) Set([]string) error     { return errSearchListUnsupported }
