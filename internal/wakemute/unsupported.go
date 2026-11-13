//go:build !linux && !darwin

package wakemute

import (
	"context"
	"os"
)

const Supported = false

func Confirm(context.Context, *os.File, *os.File, Operation, string) (Operation, error) {
	return Operation{}, ErrUnsupported
}
func Enroll(context.Context, Operation) error                             { return ErrUnsupported }
func Mute(context.Context, Operation) (Result, error)                     { return Result{}, ErrUnsupported }
func Unmute(context.Context, Scope, string, string) (Result, error)       { return Result{}, ErrUnsupported }
func Status(context.Context, Scope) (Result, error)                       { return Result{}, ErrUnsupported }
func Admission(context.Context, string, string, func(uint64) error) error { return ErrUnsupported }

func AdmissionRequired(context.Context, string, string, int, func(uint64) error) error {
	return ErrUnsupported
}

func ttyWrite(context.Context, *os.File, []byte) (int, error, bool) { return 0, nil, false }
