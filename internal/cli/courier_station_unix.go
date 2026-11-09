//go:build linux || darwin

package cli

import (
	"context"
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
		return false, 0
	}
	paths, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		return false, 0
	}
	e, present, err := courierenroll.Read(filepath.Join(paths.Base, ".a2a"))
	if !present {
		return false, 0
	}
	defer clear(e.Key)
	refuse := func() (bool, int) {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: refused; re-run enroll if enrollment is incomplete\n")
		return true, ExitError
	}
	if err != nil {
		return refuse()
	}
	p, err := courier.Authorize(args, e.Identity)
	if err != nil {
		return refuse()
	}
	r := courier.Request{Argv: append([]string(nil), args...), Files: map[int]courier.InputFile{}}
	// Read stdin only for commands that consume it. An interactive read/wait
	// must not block on a terminal simply to send an unused empty stdin field.

	if p.Stdin && a.Stdin != nil {
		r.Stdin, err = io.ReadAll(io.LimitReader(a.Stdin, courier.MaxFrameBytes+1))
		if err != nil || len(r.Stdin) > courier.MaxFrameBytes {
			return refuse()
		}
	}
	for index, attachment := range p.Inputs {
		path := args[index]
		if !attachment {
			path = strings.TrimPrefix(path, "@")
		}
		f, err := os.Open(path)
		if err != nil {
			return refuse()
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			return refuse()
		}
		data, err := io.ReadAll(io.LimitReader(f, courier.MaxFrameBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || len(data) > courier.MaxFrameBytes {
			return refuse()
		}
		name := filepath.Base(path)
		if attachment && safeFileName(name) != nil {
			return refuse()
		}
		r.Files[index] = courier.InputFile{Name: name, Data: data}
	}
	code, err := courier.Client(ctx, e.Socket, e.Station, e.Identity, e.Key, r, a.Stdout, a.Stderr)
	if err != nil {
		return refuse()
	}
	return true, code
}
