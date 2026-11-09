//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

func (a *App) stationInvocation(ctx context.Context, args []string) (bool, int) {
	event := stationHookEvent(args)
	if event != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, courier.HookBudget)
		defer cancel()
		type outcome struct {
			handled bool
			code    int
		}
		done := make(chan outcome, 1)
		go func() { handled, code := a.stationInvocationRun(ctx, args, event); done <- outcome{handled, code} }()
		select {
		case result := <-done:
			return result.handled, result.code
		case <-ctx.Done():
			return true, ExitOK
		}
	}
	return a.stationInvocationRun(ctx, args, "")
}

func (a *App) stationInvocationRun(ctx context.Context, args []string, event string) (bool, int) {
	totalCtx := ctx
	if event != "" && ctx.Err() != nil {
		return true, ExitOK
	}
	if a.Getenv == nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: enrollment_invalid; re-run enroll\n")
		if event != "" {
			return true, ExitOK
		}
		return true, ExitError
	}
	paths, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: enrollment_invalid; re-run enroll\n")
		if event != "" {
			return true, ExitOK
		}
		return true, ExitError
	}
	if event != "" && ctx.Err() != nil {
		return true, ExitOK
	}
	e, present, err := courierenroll.Read(filepath.Join(paths.Base, ".a2a"))
	if !present {
		return false, 0
	}
	defer clear(e.Key)
	if event != "" {
		var cancel context.CancelFunc
		deadline, _ := ctx.Deadline()
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-courier.HookStateReserve))
		defer cancel()
	}
	refuse := func(class courier.Diagnostic) (bool, int) {
		if event != "" {
			if err := updateHookFailuresContext(totalCtx, paths.Base, event, class, hookContextWriter{totalCtx, a.Stderr}); err != nil {
				_, _ = io.WriteString(hookContextWriter{totalCtx, a.Stderr}, "a2a: courier: courier_unavailable\n")
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
	r.Session, err = stationSession(p, r.Stdin, a.Getenv)
	if err != nil {
		return refuse(courier.NotAllowed)
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
	stdout, stderr := a.Stdout, a.Stderr
	if event != "" {
		stdout = hookContextWriter{ctx, stdout}
		stderr = hookContextWriter{ctx, stderr}
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
		if err := updateHookFailuresContext(totalCtx, paths.Base, event, "", hookContextWriter{totalCtx, a.Stderr}); err != nil {
			_, _ = io.WriteString(hookContextWriter{totalCtx, a.Stderr}, "a2a: courier: courier_unavailable\n")
		}
	}
	return true, code
}

// Output begins only while the budget is live. Pollable stdout files also
// receive a write deadline, so an undrained runtime pipe cannot pin a hook.
type hookContextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w hookContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if f, ok := w.w.(*os.File); ok {
		return writeHookFile(w.ctx, f, p)
	}
	return w.w.Write(p)
}
func (w hookContextWriter) Flush() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if f, ok := w.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// A private invocation owns stdout writes. Nonblocking attempts bound a raw
// pipe even when Go cannot attach a write deadline to an inherited descriptor.
// Restore the original file-description flags before returning.
func writeHookFile(ctx context.Context, f *os.File, p []byte) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	written := 0
	controlErr := raw.Control(func(fd uintptr) {
		flags, e := unix.FcntlInt(fd, unix.F_GETFL, 0)
		if e != nil {
			err = e
			return
		}
		if e = unix.SetNonblock(int(fd), true); e != nil {
			err = e
			return
		}
		defer unix.FcntlInt(fd, unix.F_SETFL, flags)
		for written < len(p) {
			if e := ctx.Err(); e != nil {
				err = e
				return
			}
			n, e := unix.Write(int(fd), p[written:])
			if n > 0 {
				written += n
			}
			if e != nil && e != unix.EAGAIN && e != unix.EINTR {
				err = e
				return
			}
			if e == unix.EAGAIN {
				timer := time.NewTimer(stationInputPoll)
				select {
				case <-ctx.Done():
					timer.Stop()
					err = ctx.Err()
					return
				case <-timer.C:
				}
			}
		}
	})
	if controlErr != nil {
		return written, controlErr
	}
	return written, err
}
