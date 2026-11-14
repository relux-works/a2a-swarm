package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	Reaper  bindFixtureProcess // Live observer reserves its killed-owner controls.
	Owner   bindFixtureProcess
	Server  bindFixtureProcess
	Fixture bindFixtureProcess
	// Daemon is the consumer the test owner last identified (PID, birth and
	// start ticks). It survives an in-place re-exec, which keeps all three, and
	// lets cleanup stop a consumer that the state can no longer identify.
	Daemon bindFixtureProcess `json:",omitempty"`
	// Launch is the command the fixture pane runs and Consumers the argv[0]
	// values its detached consumer may carry; both default to dir/a2a.
	Launch    string   `json:",omitempty"`
	Consumers []string `json:",omitempty"`
	// Custody is a private launch-alias root under the real home (custody
	// policy refuses shared temporary ancestry), removed with the record.
	Custody string `json:",omitempty"`
}

func (r bindFixtureRecord) launch(dir string) string {
	if r.Launch != "" {
		return r.Launch
	}
	return filepath.Join(dir, "a2a")
}

func (r bindFixtureRecord) consumers(dir string) []string {
	if len(r.Consumers) > 0 {
		return r.Consumers
	}
	return []string{filepath.Join(dir, "a2a")}
}

// bindFixtureHome is the real home, read before any test isolation moves HOME.
var bindFixtureHome, _ = os.UserHomeDir()

const launchAliasCustodyPrefix = ".a2a-launch-alias-fixture-"

// A recorded custody root grants removal only for this harness's own private
// directory directly under the real home.
func bindFixtureCustody(path string) bool {
	return bindFixtureHome != "" && filepath.Dir(path) == bindFixtureHome &&
		strings.HasPrefix(filepath.Base(path), launchAliasCustodyPrefix) && privateBindFixturePath(path, true)
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

func bindFixtureReaper() (bindFixtureProcess, error) {
	var p bindFixtureProcess
	if raw := os.Getenv("A2A_RUN_BIND_FIXTURE_REAPER"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.PID <= 1 || p.Birth == "" {
			return p, errors.New("invalid bind fixture reaper")
		}
	}
	return p, nil
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
		if err == nil && json.Unmarshal(b, &screen) == nil && bindFixtureCommand(screen.PID) == "python3 "+filepath.Join(dir, "fixture.py")+" "+dir+" "+r.launch(dir) {
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
	if !slices.ContainsFunc(r.consumers(dir), func(argv0 string) bool {
		return command == argv0+" --as alice bind --run-generation "+b.Generation
	}) {
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

// bindFixtureObservation is one observation of a cleanup obligation.
type bindFixtureObservation int

const (
	bindFixtureExited  bindFixtureObservation = iota // no such process, or its PID now names another process
	bindFixtureLive                                  // the same process is still running
	bindFixtureUnknown                               // it could not be observed
)

// observe classifies p. Only ESRCH from signal 0, or a readable identity that
// differs, is exit evidence. A failed identity read is unknown, never exited.
// The zero process is no obligation.
func (p bindFixtureProcess) observe() bindFixtureObservation {
	if p.PID == 0 || processGone(p.PID) {
		return bindFixtureExited
	}
	current, err := bindFixtureIdentity(p.PID)
	switch {
	case err != nil:
		return bindFixtureUnknown
	case current == p:
		return bindFixtureLive
	}
	return bindFixtureExited
}

// bindFixtureNamedConsumer observes the consumer the fixture state names, by
// its PID and persisted birth only. It grants no signal authority: cleanup
// waits for it but never signals it on that basis. Absent state, or state
// that names no consumer, is no obligation; unreadable state is unknown.
func bindFixtureNamedConsumer(dir string) (int, bindFixtureObservation) {
	st, err := config.LoadState(config.Paths{Base: filepath.Join(dir, "config")}, "alice")
	if err != nil {
		return 0, bindFixtureUnknown
	}
	if st.Terminal == nil || st.Terminal.DaemonPID <= 1 || st.Terminal.DaemonBirth == "" {
		return 0, bindFixtureExited
	}
	pid := st.Terminal.DaemonPID
	if processGone(pid) {
		return pid, bindFixtureExited
	}
	current, err := bindFixtureIdentity(pid)
	switch {
	case err != nil:
		return pid, bindFixtureUnknown
	case current.Birth == st.Terminal.DaemonBirth:
		return pid, bindFixtureLive
	}
	return pid, bindFixtureExited
}

// bindFixturePending names every obligation that is not yet observed exited.
func bindFixturePending(dir string, r bindFixtureRecord, consumers []bindFixtureProcess) []string {
	var pending []string
	note := func(role string, pid int, obs bindFixtureObservation) {
		switch obs {
		case bindFixtureLive:
			pending = append(pending, fmt.Sprintf("%s pid %d still live", role, pid))
		case bindFixtureUnknown:
			pending = append(pending, fmt.Sprintf("%s pid %d unobservable", role, pid))
		}
	}
	note("fixture", r.Fixture.PID, r.Fixture.observe())
	note("server", r.Server.PID, r.Server.observe())
	for _, p := range consumers {
		note("consumer", p.PID, p.observe())
	}
	pid, obs := bindFixtureNamedConsumer(dir)
	switch {
	case pid == 0 && obs == bindFixtureUnknown:
		pending = append(pending, "fixture state unreadable")
	case slices.ContainsFunc(consumers, func(p bindFixtureProcess) bool { return p.PID == pid }):
		// Already an identified obligation, observed with its full identity.
	default:
		note("unidentified state consumer", pid, obs)
	}
	return pending
}

// Normal t.Cleanup and the stale sweep share the same narrowly owned cleanup.
// Stop the request source first, then reread state to include a bind which was
// still completing when its test owner died. Each signal rechecks PID/birth.
//
// Every consumer known to this fixture is a separate obligation: the one the
// test owner recorded and each one the state later identifies. None replaces
// another. Removal starts only after every obligation, and any live consumer
// the state still names, is observed exited; an unobservable one is never
// treated as exited. Past the bound, cleanup keeps the fixture and returns a
// refusal that names what is pending.
func reclaimBindFixture(dir string, r bindFixtureRecord) error {
	recoverBindFixtureProcesses(dir, &r)
	var consumers []bindFixtureProcess
	owe := func(p bindFixtureProcess) {
		if p.PID != 0 && !slices.Contains(consumers, p) {
			consumers = append(consumers, p)
		}
	}
	owe(r.Daemon)
	owe(bindFixtureDaemon(dir, r))
	stopBindFixtureProcess(r.Fixture, syscall.SIGTERM)
	stopBindFixtureProcess(r.Server, syscall.SIGTERM)
	until := time.Now().Add(3 * time.Second)
	for {
		owe(bindFixtureDaemon(dir, r))
		for _, p := range consumers {
			stopBindFixtureProcess(p, syscall.SIGTERM)
		}
		pending := bindFixturePending(dir, r, consumers)
		if len(pending) == 0 {
			break
		}
		if time.Now().After(until) {
			for _, p := range append(slices.Clone(consumers), r.Fixture, r.Server) {
				stopBindFixtureProcess(p, syscall.SIGKILL)
			}
			return fmt.Errorf("bind fixture did not terminate within cleanup bound: %s", strings.Join(pending, "; "))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return removeBindFixtureDir(dir)
}

// removeBindFixtureDir removes a reclaimed fixture once. It never retries: a
// failure keeps the residue and reports the entry names left under dir and
// the same-user processes whose cwd or open files point into it.
func removeBindFixtureDir(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w; leftover=[%s] holders=[%s]", err, strings.Join(bindFixtureResidue(dir), " "), bindFixtureHolders(dir))
}

const bindFixtureResidueLimit = 32

// bindFixtureResidue lists at most bindFixtureResidueLimit entry names under
// dir, relative to it. It reads names only, never file contents.
func bindFixtureResidue(dir string) []string {
	var names []string
	_ = filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		if len(names) == bindFixtureResidueLimit {
			names = append(names, "...")
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(dir, path)
		names = append(names, rel)
		return nil
	})
	return names
}

func bindFixtureInside(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// bindFixtureHolders names the same-user processes whose cwd or an open
// file points into dir, as "pid=N comm=C via=cwd,fd" ("self" marks this test
// process). It reads /proc and is "unavailable" without it.
func bindFixtureHolders(dir string) string {
	if _, err := os.Stat("/proc/self"); err != nil {
		return "unavailable"
	}
	return bindFixtureHoldersIn(os.DirFS("/proc"), dir)
}

// bindFixtureHoldersIn reads only each process directory's owner, its cwd
// and fd links, and comm. It never opens environ, cmdline or any other file
// that may carry another process's secrets.
func bindFixtureHoldersIn(proc fs.FS, dir string) string {
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return "unavailable"
	}
	var holders []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		info, err := fs.Stat(proc, entry.Name())
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) {
			continue
		}
		var via []string
		if cwd, err := fs.ReadLink(proc, entry.Name()+"/cwd"); err == nil && bindFixtureInside(dir, cwd) {
			via = append(via, "cwd")
		}
		if fds, err := fs.ReadDir(proc, entry.Name()+"/fd"); err == nil {
			for _, fd := range fds {
				if target, err := fs.ReadLink(proc, entry.Name()+"/fd/"+fd.Name()); err == nil && bindFixtureInside(dir, target) {
					via = append(via, "fd")
					break
				}
			}
		}
		if len(via) == 0 {
			continue
		}
		comm, _ := fs.ReadFile(proc, entry.Name()+"/comm")
		h := fmt.Sprintf("pid=%d comm=%s via=%s", pid, strings.TrimSpace(string(comm)), strings.Join(via, ","))
		if pid == os.Getpid() {
			h += " self"
		}
		holders = append(holders, h)
	}
	if len(holders) == 0 {
		return "none"
	}
	return strings.Join(holders, "; ")
}

func sweepBindFixtures(root string) error {
	if !privateBindFixturePath(root, true) {
		return errors.New("unsafe bind fixture registry")
	}
	reaper, err := bindFixtureReaper()
	if err != nil {
		return err
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
		if !entry.IsDir() || (!strings.HasPrefix(entry.Name(), "fixture-") && !strings.HasPrefix(entry.Name(), "control-")) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		r, err := readBindFixtureRecord(dir)
		if err != nil {
			continue
		} // An incomplete/untrusted record grants no signal authority.
		// A different live observer owns the next-run assertion. If it dies,
		// ordinary future runs may still recover the abandoned controls.
		if r.Reaper.PID != 0 && r.Reaper != reaper && !r.Reaper.gone() {
			continue
		}
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
		// An interrupted owner never ran its own custody cleanup.
		if r.Custody != "" && bindFixtureCustody(r.Custody) {
			if err := os.RemoveAll(r.Custody); err != nil {
				return err
			}
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
	reaper, err := bindFixtureReaper()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "fixture-"
	if reaper.PID != 0 {
		prefix = "control-" // Older fixture sweepers only recognize fixture-.
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := bindFixtureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r := bindFixtureRecord{Owner: owner, Reaper: reaper}
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
