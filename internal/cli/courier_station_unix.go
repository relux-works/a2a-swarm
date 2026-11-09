//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

func (a *App) stationInvocation(ctx context.Context, args []string) (bool, int) {
	if a.Getenv == nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: enrollment_invalid; re-run enroll\n")
		return true, ExitError
	}
	paths, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: enrollment_invalid; re-run enroll\n")
		return true, ExitError
	}
	e, present, err := courierenroll.Read(filepath.Join(paths.Base, ".a2a"))
	if !present {
		return false, 0
	}
	defer clear(e.Key)
	event := stationHookEvent(args)
	if event != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, courier.HookBudget-courier.HookStateReserve)
		defer cancel()
	}
	refuse := func(class courier.Diagnostic) (bool, int) {
		if event != "" {
			if err := updateHookFailures(paths.Base, event, class, a.Stderr); err != nil {
				_, _ = io.WriteString(a.Stderr, "a2a: courier: courier_unavailable\n")
			}
			return true, ExitOK
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return true, 130
		}
		if class == courier.EnrollmentInvalid {
			_, _ = io.WriteString(a.Stderr, "a2a: courier: enrollment_invalid; re-run enroll\n")
		} else {
			_, _ = fmt.Fprintf(a.Stderr, "a2a: courier: %s\n", class)
		}
		return true, ExitError
	}
	if err != nil {
		return refuse(courier.EnrollmentInvalid)
	}
	p, err := courier.Authorize(args, e.Identity)
	if err != nil {
		class := courier.NotAllowed
		if err == courier.IdentityMismatch {
			class = courier.IdentityMismatch
		}
		return refuse(class)
	}
	r := courier.Request{Argv: append([]string(nil), args...), Files: map[int]courier.InputFile{}}
	// Read stdin only for commands that consume it. An interactive read/wait
	// must not block on a terminal simply to send an unused empty stdin field.

	if p.Stdin && a.Stdin != nil {
		r.Stdin, err = readStationInput(ctx, a.Stdin, event != "")
		if err != nil || len(r.Stdin) > courier.MaxFrameBytes {
			if ctx.Err() != nil {
				return refuse(courier.IdleTimeout)
			}
			return refuse(courier.NotAllowed)
		}
	}
	for index, attachment := range p.Inputs {
		path := args[index]
		if _, value, eq := strings.Cut(path, "="); !attachment && eq && strings.HasPrefix(path, "-") {
			path = value
		}
		if !attachment {
			path = strings.TrimPrefix(path, "@")
		}
		f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return refuse(courier.NotAllowed)
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return refuse(courier.NotAllowed)
		}
		data, err := io.ReadAll(io.LimitReader(f, courier.MaxFrameBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || len(data) > courier.MaxFrameBytes {
			return refuse(courier.NotAllowed)
		}
		name := filepath.Base(path)
		if attachment && safeFileName(name) != nil {
			return refuse(courier.NotAllowed)
		}
		r.Files[index] = courier.InputFile{Name: name, Data: data}
	}
	var hookOut, hookErr boundedHookOutput
	stdout, stderr := a.Stdout, a.Stderr
	if event != "" {
		stdout, stderr = &hookOut, &hookErr
	}
	code, err := courier.Client(ctx, e.Socket, e.Station, e.Identity, e.Key, r, stdout, stderr)
	if err != nil || event != "" && (code != 0 || ctx.Err() != nil) {
		class := courier.Class(err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			class = courier.IdleTimeout
		}
		return refuse(class)
	}
	if event != "" {
		if err := updateHookFailures(paths.Base, event, "", a.Stderr); err != nil {
			_, _ = io.WriteString(a.Stderr, "a2a: courier: courier_unavailable\n")
		}
		if _, err := a.Stdout.Write(hookOut.Bytes()); err != nil {
			return true, ExitOK
		}
		_, _ = a.Stderr.Write(hookErr.Bytes())
	}
	return true, code
}

// Buffer until the hook succeeds; a partial failed response never reaches the runtime.
type boundedHookOutput struct{ bytes.Buffer }

func (b *boundedHookOutput) Write(p []byte) (int, error) {
	if len(p) > courier.HookOutputBytes-b.Len() {
		return 0, courier.Unavailable
	}
	return b.Buffer.Write(p)
}
