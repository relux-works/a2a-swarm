//go:build !darwin && !linux

package cli

import "context"

func bindingArguments(int) ([]string, []string, error)  { return nil, nil, ErrBindingProcessUnknown }
func bindingImage(context.Context, int) (string, error) { return "", ErrBindingProcessUnknown }
