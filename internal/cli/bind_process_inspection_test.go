package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBindingInspectionDiagnostics(t *testing.T) {
	for _, c := range []struct {
		name, class string
		cause       error
		errno       syscall.Errno
	}{
		{"permission", "permission", &os.PathError{Op: "read", Path: "/private/SYNTHETIC_SECRET", Err: syscall.EACCES}, syscall.EACCES},
		{"missing", "missing", syscall.ENOENT, syscall.ENOENT},
		{"gone", "missing", syscall.ESRCH, syscall.ESRCH},
		{"system", "system", syscall.EIO, syscall.EIO},
		{"deadline", "deadline", context.DeadlineExceeded, 0},
		{"canceled", "canceled", context.Canceled, 0},
		{"invalid", "invalid", nil, 0},
		{"unavailable", "unavailable", errors.New("SYNTHETIC_SECRET argv env"), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			low := bindingInspectionFailure("operation", c.cause)
			err := bindingInspectionFailure("candidate-arguments", low)
			var detail *bindingInspectionError
			if !errors.Is(err, ErrBindingProcessUnknown) || !errors.As(err, &detail) || detail.phase != "candidate-arguments" || detail.operation != "operation" || detail.class != c.class || detail.errno != c.errno {
				t.Fatal("diagnostic metadata mismatch")
			}
			if c.errno != 0 && !errors.Is(err, c.errno) {
				t.Fatal("concrete errno lost")
			}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(rendered, "SYNTHETIC_SECRET") {
					t.Fatal("private error payload escaped")
				}
			}
			if err.Error() != ErrBindingProcessUnknown.Error() {
				t.Fatal("public refusal changed")
			}
		})
	}
}

// Use the existing PATH/OS-command seam to distinguish enumeration failure
// from malformed metadata without exposing subprocess stderr or arguments.
func TestBindingInspectionEnumeration(t *testing.T) {
	for _, c := range []struct {
		name, script, phase, class string
		exit                       int
	}{
		{"exit", "echo SYNTHETIC_SECRET >&2; exit 23", "enumerate", "exit", 23},
		{"record", "echo bad 1 1 a2a", "enumerate-record", "invalid", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\n"+c.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			_, err := bindingProcesses(context.Background())
			var detail *bindingInspectionError
			if !errors.As(err, &detail) || detail.phase != c.phase || detail.operation != c.phase || detail.class != c.class || detail.exitCode != c.exit {
				t.Fatal("enumeration diagnostic mismatch")
			}
			if err.Error() != ErrBindingProcessUnknown.Error() || strings.Contains(fmt.Sprintf("%#v", err), "SYNTHETIC_SECRET") {
				t.Fatal("enumeration leaked private output")
			}
		})
	}
	// A missing executable is a concrete lookup failure, not a parse error.
	t.Setenv("PATH", t.TempDir())
	_, err := bindingProcesses(context.Background())
	var detail *bindingInspectionError
	if !errors.As(err, &detail) || detail.class != "not-found" || !errors.Is(err, ErrBindingProcessUnknown) {
		t.Fatal("lookup failure class lost")
	}
}
