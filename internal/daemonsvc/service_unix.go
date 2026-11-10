//go:build darwin || linux

package daemonsvc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type State string

const (
	Absent  State = "absent"
	Stopped State = "stopped"
	Running State = "running"
	Unknown State = "unknown"
)

// Supervisor is used only by explicit service install/uninstall/status. Stop
// must join the old process before it reports success. No background tool calls.
type Supervisor interface {
	State(context.Context, Plan, string) (State, error)
	Start(context.Context, Plan, string) error
	Stop(context.Context, Plan, string) error
	Reload(context.Context, Plan) error
}

// Manager has no alternate production root. Tests use a fixture directory and
// fake Supervisor via the package-private constructor fields.
type Manager struct {
	Supervisor Supervisor
	d          disk
	dir        string
}

func NewManager(s Supervisor) *Manager { return &Manager{Supervisor: s, d: realDisk()} }
func (m *Manager) Run(ctx context.Context, p Plan, op string) (map[string]State, error) {
	if p.Validate() != nil || (op != "install" && op != "uninstall" && op != "status") {
		return nil, ErrRefused
	}
	dir := m.dir
	if dir == "" {
		dir = p.UnitDir()
	}
	root, e := m.d.rootDir(dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	files, _ := Render(p)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	// Audit every file and supervisor state before changing any unit.
	present := map[string]bool{}
	states := map[string]State{}
	for _, name := range names {
		var st unix.Stat_t
		e := unix.Fstatat(int(root.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if e == nil {
			b, e := m.d.readAt(root, name, 0, ^uint32(0), 0644, 65536)
			if e != nil || !bytes.Equal(b, files[name]) {
				return nil, ErrUnresolved
			}
			present[name] = true
		} else if e != unix.ENOENT {
			return nil, ErrUnresolved
		}
		state, e := m.Supervisor.State(ctx, p, name)
		if e != nil || (state != Absent && state != Running && state != Stopped) || (state != Absent && !present[name]) {
			return nil, ErrUnresolved
		}
		states[name] = state
	}
	if op == "status" {
		return states, nil
	}
	if op == "install" {
		// Refuse a partially installed set rather than silently reconstructing it.
		if len(present) != 0 && len(present) != len(files) {
			return states, ErrUnresolved
		}
		for _, name := range names {
			if !present[name] {
				fd, e := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0644)
				if e != nil {
					return states, ErrUnresolved
				}
				f := os.NewFile(uintptr(fd), name)
				e = unix.Fchmod(fd, 0644)
				if e == nil {
					_, e = f.Write(files[name])
				}
				if e == nil {
					e = f.Sync()
				}
				ce := f.Close()
				if e != nil || ce != nil {
					return states, ErrUnresolved
				}
			}
		}
		if m.Supervisor.Reload(ctx, p) != nil {
			return states, ErrUnresolved
		}
		// Register the prepare job first. Launchd may run it asynchronously;
		// daemon KeepAlive retries until the run directory exists.
		startNames := append([]string(nil), names...)
		if p.Platform == "darwin" {
			sort.SliceStable(startNames, func(i, j int) bool {
				return strings.Contains(startNames[i], ".prepare.") && !strings.Contains(startNames[j], ".prepare.")
			})
		}
		for _, name := range startNames {
			if strings.HasSuffix(name, ".sweep.service") {
				continue
			}
			if m.Supervisor.Start(ctx, p, name) != nil {
				return states, ErrUnresolved
			}
		}
		// Report the resulting state, not the pre-install snapshot. A successful
		// registration alone does not prove that the daemon is running.
		deadline := time.NewTimer(40 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			ready := true
			for _, name := range names {
				state, e := m.Supervisor.State(ctx, p, name)
				if e != nil || (state != Absent && state != Running && state != Stopped) {
					return states, ErrUnresolved
				}
				states[name] = state
				if name == Label+".plist" || name == Label+".service" || strings.HasSuffix(name, ".timer") {
					ready = ready && state == Running
				}
			}
			if ready {
				return states, nil
			}
			select {
			case <-ctx.Done():
				return states, ErrUnresolved
			case <-deadline.C:
				return states, ErrUnresolved
			case <-tick.C:
			}
		}
	}
	// Stop the scheduler before its worker. Every stop joins. Nothing is deleted
	// until all old jobs are known quiescent, including the on-demand sweep job.
	sort.SliceStable(names, func(i, j int) bool {
		return strings.HasSuffix(names[i], ".timer") && !strings.HasSuffix(names[j], ".timer")
	})
	for _, name := range names {
		if states[name] != Absent {
			if m.Supervisor.Stop(ctx, p, name) != nil {
				return states, ErrUnresolved
			}
		}
	}
	for _, name := range names {
		state, e := m.Supervisor.State(ctx, p, name)
		if e != nil || (state != Absent && state != Stopped) {
			return states, ErrUnresolved
		}
	}
	for _, name := range names {
		if present[name] {
			b, e := m.d.readAt(root, name, 0, ^uint32(0), 0644, 65536)
			if e != nil || !bytes.Equal(b, files[name]) || unix.Unlinkat(int(root.Fd()), name, 0) != nil {
				return states, ErrUnresolved
			}
		}
	}
	if m.Supervisor.Reload(ctx, p) != nil {
		return states, ErrUnresolved
	}
	return states, nil
}

type commandResult struct {
	out  []byte
	code int
}
type nativeSupervisor struct {
	run func(context.Context, string, ...string) commandResult
}

func NativeSupervisor() Supervisor {
	return nativeSupervisor{run: func(ctx context.Context, bin string, args ...string) commandResult {
		// Service commands have a bounded deadline and a clean public environment.
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
		b, e := cmd.CombinedOutput()
		if e != nil {
			if x, ok := e.(*exec.ExitError); ok {
				return commandResult{b, x.ExitCode()}
			}
			return commandResult{nil, -1}
		}
		return commandResult{b, 0}
	}}
}
func unitLabel(name string) string { return strings.TrimSuffix(name, ".plist") }
func (s nativeSupervisor) State(ctx context.Context, p Plan, name string) (State, error) {
	if p.Platform == "darwin" {
		r := s.run(ctx, "/bin/launchctl", "print", "system/"+unitLabel(name))
		if r.code != 0 {
			// Only launchctl's explicit not-found result establishes absence. Any
			// permission error, timeout, launch failure or unfamiliar output is unknown.
			if r.code == 113 && strings.Contains(string(r.out), "Could not find service") {
				return Absent, nil
			}
			return Unknown, ErrUnresolved
		}
		text := string(r.out)
		if !strings.Contains(text, "path = "+filepath.Join(p.UnitDir(), name)+"\n") {
			return Unknown, ErrUnresolved
		}
		user := p.User()
		if strings.Contains(name, ".prepare.") {
			user = "root"
		}
		if !strings.Contains(text, "program = "+p.Image+"\n") || !strings.Contains(text, "username = "+user+"\n") {
			return Unknown, ErrUnresolved
		}
		args := p.args("serve")
		if strings.Contains(name, ".sweep.") {
			args = p.args("sweep")
		}
		if strings.Contains(name, ".prepare.") {
			args = []string{p.Image, "daemon", "prepare-run-dir", "--image-sha256", p.ImageSHA256}
		}
		if !launchArguments(text, args) {
			return Unknown, ErrUnresolved
		}
		if strings.Contains(text, "state = running\n") {
			return Running, nil
		}
		if strings.Contains(text, "state = not running\n") || strings.Contains(text, "state = waiting\n") {
			return Stopped, nil
		}
		return Unknown, ErrUnresolved
	}
	r := s.run(ctx, "/bin/systemctl", "show", name, "--no-pager", "--property=LoadState,ActiveState,SubState,FragmentPath,ExecStart,NeedDaemonReload,User,Group")
	if r.code != 0 {
		return Unknown, ErrUnresolved
	}
	fields := map[string]string{}
	for _, l := range strings.Split(string(r.out), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			fields[k] = v
		}
	}
	if fields["LoadState"] == "not-found" && fields["ActiveState"] == "inactive" {
		return Absent, nil
	}
	if fields["LoadState"] != "loaded" || fields["FragmentPath"] != filepath.Join(p.UnitDir(), name) {
		return Unknown, ErrUnresolved
	}
	if fields["NeedDaemonReload"] != "no" {
		return Unknown, ErrUnresolved
	}
	if strings.HasSuffix(name, ".service") {
		if fields["User"] != "a2a" || fields["Group"] != "a2a" {
			return Unknown, ErrUnresolved
		}
		args := p.args("serve")
		if strings.Contains(name, ".sweep.") {
			args = p.args("sweep")
		}
		prefix := "{ path=" + p.Image + " ; argv[]=" + strings.Join(args, " ") + " ;"
		if !strings.HasPrefix(fields["ExecStart"], prefix) || strings.Count(fields["ExecStart"], "argv[]=") != 1 {
			return Unknown, ErrUnresolved
		}
	}
	switch fields["ActiveState"] {
	case "active":
		if (strings.HasSuffix(name, ".timer") && fields["SubState"] == "waiting") || (strings.HasSuffix(name, ".service") && fields["SubState"] == "running") {
			return Running, nil
		}
	case "inactive", "failed":
		if fields["SubState"] == "dead" || fields["SubState"] == "failed" {
			return Stopped, nil
		}
	}
	return Unknown, ErrUnresolved
}
func launchArguments(text string, args []string) bool {
	_, block, ok := strings.Cut(text, "arguments = {\n")
	if !ok {
		return false
	}
	block, _, ok = strings.Cut(block, "\n\t}")
	if !ok {
		return false
	}
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) != len(args) {
		return false
	}
	for i, l := range lines {
		if strings.TrimSpace(l) != args[i] {
			return false
		}
	}
	return true
}
func (s nativeSupervisor) Start(ctx context.Context, p Plan, name string) error {
	if p.Platform == "darwin" {
		state, e := s.State(ctx, p, name)
		if e != nil {
			return e
		}
		if state != Absent {
			if state == Stopped && s.run(ctx, "/bin/launchctl", "kickstart", "system/"+unitLabel(name)).code != 0 {
				return ErrUnresolved
			}
			return nil
		}
		if s.run(ctx, "/bin/launchctl", "bootstrap", "system", filepath.Join(p.UnitDir(), name)).code != 0 {
			return ErrUnresolved
		}
		return nil
	}
	if s.run(ctx, "/bin/systemctl", "enable", "--now", name).code != 0 {
		return ErrUnresolved
	}
	return nil
}
func (s nativeSupervisor) Stop(ctx context.Context, p Plan, name string) error {
	if p.Platform == "darwin" {
		// launchctl bootout joins removal, but retain the old PID too: fail closed
		// if it is still alive rather than unlinking underneath a stopping process.
		r := s.run(ctx, "/bin/launchctl", "print", "system/"+unitLabel(name))
		if r.code != 0 {
			return ErrUnresolved
		}
		pid := 0
		for _, l := range strings.Split(string(r.out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "pid = "); ok {
				pid, _ = strconv.Atoi(v)
				if pid <= 0 {
					return ErrUnresolved
				}
			}
		}
		if s.run(ctx, "/bin/launchctl", "bootout", "system/"+unitLabel(name)).code != 0 {
			return ErrUnresolved
		}
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for pid > 0 {
			if unix.Kill(pid, 0) == unix.ESRCH {
				break
			}
			select {
			case <-ctx.Done():
				return ErrUnresolved
			case <-deadline.C:
				return ErrUnresolved
			case <-tick.C:
			}
		}
		return nil
	}
	if s.run(ctx, "/bin/systemctl", "stop", name).code != 0 {
		return ErrUnresolved
	}
	if strings.HasSuffix(name, ".sweep.service") {
		return nil
	}
	if s.run(ctx, "/bin/systemctl", "disable", name).code != 0 {
		return ErrUnresolved
	}
	return nil
}
func (s nativeSupervisor) Reload(ctx context.Context, p Plan) error {
	if p.Platform == "linux" && s.run(ctx, "/bin/systemctl", "daemon-reload").code != 0 {
		return ErrUnresolved
	}
	return nil
}
