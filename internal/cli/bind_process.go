package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrBindingOldImage = errors.New("binding refused: same identity has a different executable image; stop own old process first")
var ErrBindingParallelWait = errors.New("binding refused: same identity has a live wait/watch; stop own waiter first")
var ErrBindingProcessUnknown = errors.New("binding refused: process inspection unavailable")

type bindingProcess struct {
	PID, PPID, UID int
	Name           string
}

// Only process metadata is listed. Arguments/environment are inspected in RAM
// for a2a candidates owned by our UID; only identity/config selectors survive.
func bindingProcesses(ctx context.Context) ([]bindingProcess, error) {
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,uid=,comm=").Output()
	if err != nil {
		return nil, ErrBindingProcessUnknown
	}
	var rows []bindingProcess
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		uid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, ErrBindingProcessUnknown
		}
		rows = append(rows, bindingProcess{pid, ppid, uid, strings.Join(f[3:], " ")})
	}
	return rows, nil
}

func bindingSelectors(args, env []string) (identity, root, command string) {
	for _, v := range env {
		k, value, ok := strings.Cut(v, "=")
		if !ok {
			continue
		}
		switch k {
		case "A2A_AGENT":
			identity = value
		case "A2A_CONFIG_DIR":
			root = value
		}
	}
	var rest []string
	for i := 1; i < len(args); i++ {
		v := args[i]
		switch {
		case v == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case v == "--as" || v == "-as":
			if i+1 < len(args) {
				i++
				identity = args[i]
			}
		case strings.HasPrefix(v, "--as=") || strings.HasPrefix(v, "-as="):
			_, identity, _ = strings.Cut(v, "=")
		default:
			rest = append(rest, v)
		}
	}
	if len(rest) > 0 {
		command = rest[0]
	}
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".config", "a2a-swarm")
	}
	root = filepath.Clean(root)
	return
}

func (a *App) guardBindingProcesses(ctx context.Context, name, root string) error {
	rows, err := bindingProcesses(ctx)
	if err != nil {
		return err
	}
	own, err := bindingImage(ctx, os.Getpid())
	if err != nil {
		return ErrBindingProcessUnknown
	}
	for _, p := range rows {
		if p.PID == os.Getpid() || p.UID != os.Getuid() || !strings.HasPrefix(filepath.Base(p.Name), "a2a") {
			continue
		}
		args, env, err := bindingArguments(p.PID)
		if err != nil {
			if processGone(p.PID) {
				continue
			}
			return ErrBindingProcessUnknown
		}
		id, base, cmd := bindingSelectors(args, env)
		if id != name {
			continue
		}
		if !filepath.IsAbs(base) {
			return ErrBindingProcessUnknown
		}
		currentRoot, _ := filepath.Abs(root)
		canonical := func(v string) string {
			if real, err := filepath.EvalSymlinks(v); err == nil {
				return real
			}
			return filepath.Clean(v)
		}
		if canonical(base) != canonical(currentRoot) {
			continue
		}
		image, err := bindingImage(ctx, p.PID)
		if err != nil {
			if processGone(p.PID) {
				continue
			}
			return ErrBindingProcessUnknown
		}
		if err := bindingProcessConflict(own, image, cmd, p.PID); err != nil {
			return err
		}
	}
	return nil
}

// Pin the closest matching runtime ancestor, never the hook's shell parent.
// Every ancestor must be ours; tmuxwake.Birth and bind revalidate UID/birth.
func hookRuntimeOwner(ctx context.Context, runtime string) (int, error) {
	rows, err := bindingProcesses(ctx)
	if err != nil {
		return 0, err
	}
	return runtimeOwnerPID(rows, os.Getppid(), runtime, os.Getuid())
}
func runtimeOwnerPID(rows []bindingProcess, pid int, runtime string, uid int) (int, error) {
	byPID := make(map[int]bindingProcess)
	for _, p := range rows {
		byPID[p.PID] = p
	}
	for i := 0; i < 64 && pid > 1; i++ {
		p, ok := byPID[pid]
		if !ok || p.UID != uid {
			break
		}
		if filepath.Base(p.Name) == runtime {
			return pid, nil
		}
		if p.PPID == pid {
			break
		}
		pid = p.PPID
	}
	return 0, errors.New("auto-bind refused: own runtime ancestor unavailable")
}

func bindingProcessConflict(own, image, command string, pid int) error {
	if image != own {
		return fmt.Errorf("%w (pid %d)", ErrBindingOldImage, pid)
	}
	if command == "wait" || command == "watch" {
		return fmt.Errorf("%w (pid %d)", ErrBindingParallelWait, pid)
	}
	return nil
}
