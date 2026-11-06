//go:build !darwin && !linux

package cli

import "context"

func bindingArguments(int) ([]string, []string, error) {
	return nil, nil, bindingInspectionFailure("unsupported-arguments", ErrBindingProcessUnknown)
}
func bindingImage(context.Context, int) (string, error) {
	return "", bindingInspectionFailure("unsupported-image", ErrBindingProcessUnknown)
}
func bindingCandidateExited(int) bool { return false }
