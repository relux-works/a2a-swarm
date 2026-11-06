package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// This registry belongs only to this synthetic test harness. There is no /tmp
// prefix scan and no production daemon lifetime policy. Killed runs are reclaimed
// on the next lifecycle invocation, not immediately when the owner dies.
type bindFixtureProcess struct {
	PID   int
	Birth string
	Start string // Linux start ticks distinguish PID reuse within one wall-clock second.
}

type bindFixtureRecord struct {
	Owner   bindFixtureProcess
	Server  bindFixtureProcess
	Fixture bindFixtureProcess
}

func bindFixtureIdentity(pid int) (bindFixtureProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	birth, err := tmuxwake.Birth(ctx, pid)
	if err != nil {
		return bindFixtureProcess{}, err
	}
	p := bindFixtureProcess{PID: pid, Birth: birth}
	if b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); e == nil {
		fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(fields) < 20 {
			return p, errors.New("invalid process stat")
		}
		p.Start = fields[19]
	}
	return p, nil
}

func (p bindFixtureProcess) live() bool {
	if p.PID <= 1 || p.Birth == "" {
		return false
	}
	current, err := bindFixtureIdentity(p.PID)
	return err == nil && current == p
}

func (p bindFixtureProcess) gone() bool {
	if p.PID == 0 || processGone(p.PID) {
		return true
	}
	current, err := bindFixtureIdentity(p.PID)
	return err == nil && current != p
}

func privateBindFixturePath(path string, dir bool) bool {
	s, err := os.Lstat(path)
	if err != nil || s.IsDir() != dir || (!dir && !s.Mode().IsRegular()) {
		return false
	}
	owner, ok := s.Sys().(*syscall.Stat_t)
	return ok && owner.Uid == uint32(os.Getuid()) && s.Mode().Perm() == map[bool]os.FileMode{true: 0700, false: 0600}[dir]
}

func bindFixtureRoot() (string, error) {
	root := filepath.Join(os.TempDir(), "a2a-bind-fixtures-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	if !privateBindFixturePath(root, true) {
		return "", errors.New("unsafe bind fixture registry")
	}
	return root, nil
}

func writeBindFixtureRecord(dir string, r bindFixtureRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "owned.next"), b, 0600); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "owned.next"), filepath.Join(dir, "owned.json"))
}

func readBindFixtureRecord(dir string) (bindFixtureRecord, error) {
	var r bindFixtureRecord
	if !privateBindFixturePath(dir, true) || !privateBindFixturePath(filepath.Join(dir, "owned.json"), false) {
		return r, errors.New("unsafe bind fixture record")
	}
	b, err := os.ReadFile(filepath.Join(dir, "owned.json"))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err == nil && (r.Owner.PID <= 1 || r.Owner.Birth == "") {
		err = errors.New("missing fixture owner")
	}
	return r, err
}

func bindFixtureCommand(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// A private socket and exact command also cover a kill between launch and the
// parent's PID acknowledgement. No directory outside this record is consulted.
func recoverBindFixtureProcesses(dir string, r *bindFixtureRecord) {
	if r.Server.PID == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b, err := exec.CommandContext(ctx, "tmux", "-S", filepath.Join(dir, "socket"), "display-message", "-p", "#{pid}").Output()
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			command := bindFixtureCommand(pid)
			if strings.Contains(command, filepath.Join(dir, "socket")) {
				r.Server, _ = bindFixtureIdentity(pid)
			}
		}
	}
	if r.Fixture.PID == 0 {
		var screen struct{ PID int }
		b, err := os.ReadFile(filepath.Join(dir, "screen.json"))
		if err == nil && json.Unmarshal(b, &screen) == nil && bindFixtureCommand(screen.PID) == "python3 "+filepath.Join(dir, "fixture.py")+" "+dir+" "+filepath.Join(dir, "a2a") {
			r.Fixture, _ = bindFixtureIdentity(screen.PID)
		}
	}
}

func bindFixtureDaemon(dir string, r bindFixtureRecord) bindFixtureProcess {
	st, err := config.LoadState(config.Paths{Base: filepath.Join(dir, "config")}, "alice")
	if err != nil || st.Terminal == nil {
		return bindFixtureProcess{}
	}
	b := st.Terminal
	if b.Generation == "" || b.SessionID != "bind-fixture" || b.Host != machineLabel() || b.OwnerPID != r.Fixture.PID || b.OwnerBirth != r.Fixture.Birth || b.Target.OwnerPID != r.Fixture.PID || b.Target.OwnerBirth != r.Fixture.Birth || b.Target.Socket != filepath.Join(dir, "socket") {
		return bindFixtureProcess{}
	}
	// Match the executable and generation before using the persisted birth.
	command := bindFixtureCommand(b.DaemonPID)
	if command != filepath.Join(dir, "a2a")+" --as alice bind --run-generation "+b.Generation {
		return bindFixtureProcess{}
	}
	p, err := bindFixtureIdentity(b.DaemonPID)
	if err != nil || p.Birth != b.DaemonBirth {
		return bindFixtureProcess{}
	}
	return p
}

func stopBindFixtureProcess(p bindFixtureProcess, signal syscall.Signal) {
	if p.live() {
		proc, _ := os.FindProcess(p.PID)
		_ = proc.Signal(signal)
	}
}

// Normal t.Cleanup and the stale sweep share the same narrowly owned cleanup.
// Stop the request source first, then reread state to include a bind which was
// still completing when its test owner died. Each signal rechecks PID/birth.
func reclaimBindFixture(dir string, r bindFixtureRecord) error {
	recoverBindFixtureProcesses(dir, &r)
	daemon := bindFixtureDaemon(dir, r)
	stopBindFixtureProcess(r.Fixture, syscall.SIGTERM)
	stopBindFixtureProcess(r.Server, syscall.SIGTERM)
	until := time.Now().Add(3 * time.Second)
	for {
		if next := bindFixtureDaemon(dir, r); next.PID != 0 {
			daemon = next
		}
		stopBindFixtureProcess(daemon, syscall.SIGTERM)
		if r.Fixture.gone() && r.Server.gone() && daemon.gone() {
			break
		}
		if time.Now().After(until) {
			for _, p := range []bindFixtureProcess{daemon, r.Fixture, r.Server} {
				stopBindFixtureProcess(p, syscall.SIGKILL)
			}
			return errors.New("bind fixture did not terminate within cleanup bound")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return os.RemoveAll(dir)
}

func sweepBindFixtures(root string) error {
	if !privateBindFixturePath(root, true) {
		return errors.New("unsafe bind fixture registry")
	}
	lock, err := os.OpenFile(filepath.Join(root, "sweep.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if !privateBindFixturePath(lock.Name(), false) {
		return errors.New("unsafe bind fixture sweep lock")
	}
	until := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK || time.Now().After(until) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "fixture-") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		r, err := readBindFixtureRecord(dir)
		if err != nil {
			continue
		} // An incomplete/untrusted record grants no signal authority.
		if r.Owner.live() {
			continue
		}
		// An unavailable birth alone does not prove death (ps can fail).
		if !processGone(r.Owner.PID) {
			current, err := bindFixtureIdentity(r.Owner.PID)
			if err != nil || current == r.Owner {
				continue
			}
		}
		if err := reclaimBindFixture(dir, r); err != nil {
			return err
		}
	}
	return nil
}

func newBindFixtureDir(t *testing.T) (string, bindFixtureRecord) {
	t.Helper()
	root, err := bindFixtureRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err = sweepBindFixtures(root); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "fixture-")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := bindFixtureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := bindFixtureRecord{Owner: owner}
	if err = writeBindFixtureRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		current, err := readBindFixtureRecord(dir)
		if err == nil {
			err = reclaimBindFixture(dir, current)
		}
		if err != nil {
			t.Errorf("private bind fixture cleanup: %v", err)
		}
	})
	return dir, r
}
