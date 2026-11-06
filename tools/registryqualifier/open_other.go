//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

func openRegular(string) (*os.File, error) { return nil, errors.New("unsupported_platform") }
