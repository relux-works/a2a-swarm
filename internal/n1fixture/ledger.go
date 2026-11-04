//go:build n1_fixture && darwin

// Package n1fixture is compiled only into the reviewed private N1 source overlay.
// It is not a runtime switch in an ordinary a2a binary.
package n1fixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Row struct {
	PID, PPID, UID                              int
	Name, Birth, Image, Nonce, Generation, Role string
	Sealed, Reaped                              bool
	Exit                                        int
	Signal                                      string
}

func Dir() string { return os.Getenv("N1_DIR") }
func Nonce() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func Write(name string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(Dir(), name)
	f, err := os.CreateTemp(Dir(), ".receipt-")
	if err != nil {
		return err
	}
	n := f.Name()
	defer os.Remove(n)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(n, p)
}
func Read(name string, v any) error {
	b, e := os.ReadFile(filepath.Join(Dir(), name))
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func Until(ctx context.Context, predicate func() bool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func Latch(ctx context.Context, name string) error {
	if err := Write(name+".arrived", map[string]any{"pid": os.Getpid(), "at": time.Now().UTC()}); err != nil {
		return err
	}
	return Until(ctx, func() bool { _, e := os.Stat(filepath.Join(Dir(), name+".release")); return e == nil })
}
func Release(name string) error { return Write(name+".release", true) }
func ledger(fn func(*[]Row) error) error {
	f, e := os.OpenFile(filepath.Join(Dir(), "ledger.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	until := time.Now().Add(2 * time.Second)
	for {
		e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			break
		}
		if time.Now().After(until) {
			return errors.New("fixture ledger lock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	var rows []Row
	if e = Read("ledger.json", &rows); e != nil && !os.IsNotExist(e) {
		return e
	}
	if e = fn(&rows); e != nil {
		return e
	}
	return Write("ledger.json", rows)
}
func Rows() ([]Row, error) {
	var result []Row
	e := ledger(func(rows *[]Row) error { result = append(result, (*rows)...); return nil })
	return result, e
}
func known(pid int) (Row, error) {
	rows, e := Rows()
	if e != nil {
		return Row{}, e
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.PID == pid && r.Sealed {
			return r, nil
		}
	}
	return Row{}, errors.New("unregistered fixture PID")
}

// Raw identity probes are restricted to a parent's actual Start PID or bootstrap
// self. They are never fed a state-file PID or a system enumeration result.
func identity(pid int) (birth string, ppid int, uid int, err error) {
	k, e := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if e != nil {
		return "", 0, 0, e
	}
	if int(k.Proc.P_pid) != pid {
		return "", 0, 0, errors.New("PID absent")
	}
	return fmt.Sprintf("%d:%d", k.Proc.P_starttime.Sec, k.Proc.P_starttime.Usec), int(k.Eproc.Ppid), int(k.Eproc.Ucred.Uid), nil
}
func image(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "finD").Output()
	if e != nil {
		return "", e
	}
	text := false
	dev, ino := "", ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "f") {
			if text && dev != "" && ino != "" {
				break
			}
			text = line == "ftxt"
		}
		if text && strings.HasPrefix(line, "D") {
			dev = line[1:]
		}
		if text && strings.HasPrefix(line, "i") {
			ino = line[1:]
		}
	}
	if dev == "" || ino == "" {
		return "", errors.New("fixture image unavailable")
	}
	return dev + ":" + ino, nil
}
func seal(pid, parent int, name, nonce, generation, role string) (Row, error) {
	birth, ppid, uid, e := identity(pid)
	if e != nil {
		return Row{}, e
	}
	if ppid != parent || uid != os.Getuid() {
		return Row{}, errors.New("fixture parent/UID mismatch")
	}
	img, e := image(pid)
	if e != nil {
		return Row{}, e
	}
	r := Row{PID: pid, PPID: parent, UID: uid, Name: name, Birth: birth, Image: img, Nonce: nonce, Generation: generation, Role: role, Sealed: true}
	e = ledger(func(rows *[]Row) error {
		for _, old := range *rows {
			if old.PID == pid && !old.Reaped {
				return errors.New("live PID already registered")
			}
		}
		*rows = append(*rows, r)
		return nil
	})
	return r, e
}
func Require(pid int) error {
	r, e := known(pid)
	if e != nil {
		return e
	}
	if r.Reaped {
		return errors.New("fixture PID already reaped")
	}
	b, p, u, e := identity(pid)
	if e != nil {
		return e
	}
	if b != r.Birth || p != r.PPID || u != r.UID {
		return errors.New("fixture identity changed")
	}
	img, e := image(pid)
	if e != nil {
		return e
	}
	if img != r.Image {
		return errors.New("fixture image changed")
	}
	return nil
}
func Enumerate() ([]Row, error) {
	self, e := known(os.Getpid())
	if e != nil || self.Reaped {
		return nil, errors.New("unregistered enumerator caller")
	}
	rows, e := Rows()
	if e != nil {
		return nil, e
	}
	var live []Row
	for _, r := range rows {
		if r.Reaped {
			continue
		}
		if e = Require(r.PID); e != nil {
			return nil, e
		}
		live = append(live, r)
	}
	if len(live) == 0 {
		return nil, errors.New("empty fixture ledger")
	}
	return live, nil
}

// BootstrapSelf records the driver or the pane supervisor. The driver must corroborate the pane's
// nonce, self PID and parent with the private socket's exact pane_pid before
// releasing it. It has no parent Wait receipt; see the explicit exit protocol.
func BootstrapSelf(role string) error {
	_, e := seal(os.Getpid(), os.Getppid(), os.Args[0], os.Getenv("N1_NONCE"), "", role)
	if e != nil {
		return e
	}
	name := "driver-bootstrap.json"
	if role == "pane-supervisor" {
		name = "pane-bootstrap.json"
	}
	return Write(name, map[string]any{"pid": os.Getpid(), "parent": os.Getppid(), "nonce": os.Getenv("N1_NONCE")})
}
func Enter(ctx context.Context) error {
	return Until(ctx, func() bool {
		r, e := known(os.Getpid())
		return e == nil && !r.Reaped && r.Nonce == os.Getenv("N1_NONCE") && Require(os.Getpid()) == nil
	})
}

var pending sync.WaitGroup

// Start installs its Wait owner and cleanup obligation immediately after Start,
// before identity sealing can fail. No caller is allowed to discard this Handle.
type Child struct {
	Cmd     *exec.Cmd
	Row     Row
	Done    chan struct{}
	Err     error
	sealErr error
}

func Start(cmd *exec.Cmd, nonce, generation, role string) (*Child, error) {
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	c := &Child{Cmd: cmd, Done: make(chan struct{})}
	sealed := make(chan struct{})
	pending.Add(1)
	go func() {
		defer pending.Done()
		c.Err = cmd.Wait()
		<-sealed
		recordWait(cmd.Process.Pid, c.Err)
		close(c.Done)
	}()
	// Registration and reaping are serialized: the child waits in Enter until sealed.
	c.Row, c.sealErr = seal(cmd.Process.Pid, os.Getpid(), cmd.Path, nonce, generation, role)
	close(sealed)
	if c.sealErr != nil { // Ownership exists, but identity could not be sealed: never signal.
		_ = Write(fmt.Sprintf("STOP-%d.json", cmd.Process.Pid), map[string]any{"reason": "Start succeeded, sealing failed; no signal, directory retained", "pid": cmd.Process.Pid})
		return c, c.sealErr
	}
	return c, nil
}
func recordWait(pid int, err error) {
	exit := 0
	signal := ""
	if err != nil {
		exit = -1
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signal = ws.Signal().String()
			}
		}
	}
	var sealed Row
	ledgerErr := ledger(func(rows *[]Row) error {
		for i := len(*rows) - 1; i >= 0; i-- {
			if (*rows)[i].PID == pid {
				(*rows)[i].Reaped = true
				(*rows)[i].Exit = exit
				(*rows)[i].Signal = signal
				sealed = (*rows)[i]
				return nil
			}
		}
		return errors.New("unregistered Wait PID")
	})
	if ledgerErr != nil {
		_ = Write(fmt.Sprintf("STOP-wait-%d.json", pid), map[string]any{"pid": pid, "reason": "actual Wait returned but ledger update failed; retain attempt"})
		return
	}
	_ = Write(fmt.Sprintf("wait-%d.json", pid), map[string]any{"pid": pid, "parent": os.Getpid(), "birth": sealed.Birth, "nonce": sealed.Nonce, "generation": sealed.Generation, "exit": exit, "signal": signal, "kind": "exec.Cmd.Wait returned", "at": time.Now().UTC()})
}
func WaitAll(ctx context.Context) error {
	done := make(chan struct{})
	go func() { pending.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop never derives authority from state or FindProcess. Its PID must already
// have a sealed launch record. TERM and any bounded KILL each revalidate identity.
func Stop(ctx context.Context, pid int) error {
	r, e := known(pid)
	if e != nil {
		return e
	}
	if r.Reaped {
		if waitMatches(r) {
			return nil
		}
		return errors.New("owned reap receipt missing")
	}
	if e = Require(pid); e != nil {
		// Require may race a genuine parent Wait; accepting that receipt probes no
		// reused PID. Every other identity failure remains STOP without a signal.
		current, readErr := known(pid)
		if readErr == nil && current.Reaped && current.Birth == r.Birth && current.Nonce == r.Nonce && waitMatches(r) {
			return nil
		}
		return e
	}
	if e = unix.Kill(pid, unix.SIGTERM); e != nil {
		return e
	}
	wait := func() bool { return waitMatches(r) }
	grace, cancel := context.WithTimeout(ctx, 3*time.Second)
	e = Until(grace, wait)
	cancel()
	if e == nil {
		return nil
	}
	if e = Require(pid); e != nil {
		return e
	}
	if e = unix.Kill(pid, unix.SIGKILL); e != nil {
		return e
	}
	kill, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return Until(kill, wait)
}
func CloseChild(ctx context.Context, c *Child) error {
	if c == nil {
		return nil
	}
	select {
	case <-c.Done:
		return c.sealErr
	default:
	}
	if c.sealErr != nil {
		return c.sealErr
	}
	if e := Stop(ctx, c.Cmd.Process.Pid); e != nil {
		return e
	}
	select {
	case <-c.Done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// OwnServer has no entry gate; its actual Start PID is sealed before any client
// can create a pane. An early exit is retained as a failure, not treated as ready.
func OwnServer(cmd *exec.Cmd, nonce string) (*Child, error) {
	return Start(cmd, nonce, "", "foreground-tmux")
}
func MarkPaneExited(pid int) error {
	// The server owns this non-child's wait. No invented driver Wait receipt.
	return ledger(func(rows *[]Row) error {
		for i := len(*rows) - 1; i >= 0; i-- {
			if (*rows)[i].PID == pid {
				(*rows)[i].Reaped = true
				(*rows)[i].Signal = "NOT RECORDED: tmux parent"
				return nil
			}
		}
		return errors.New("unknown pane")
	})
}

// Gone replaces only unsafe post-reap PID probing: a parent's completed Wait
// is authoritative for its child, without touching a potentially reused PID.
// Live registered PIDs still take the ordinary production signal-zero path.
func Gone(pid int) (bool, error) {
	r, e := known(pid)
	if e != nil {
		return false, e
	}
	if r.Reaped {
		return true, nil
	}
	if e = Require(pid); e != nil {
		return false, e
	}
	return false, nil
}

// Client is a synchronous helper, never a candidate for enumeration or signals.
// A short client may exit before birth/image inspection is possible. Its actual
// parent Start/Wait receipt is retained with those fields explicitly unavailable;
// it supplies no server/consumer ownership evidence. On timeout it is NOT killed.
func Client(ctx context.Context, cmd *exec.Cmd) error {
	if e := cmd.Start(); e != nil {
		return e
	}
	pid := cmd.Process.Pid
	pending.Add(1)
	done := make(chan error, 1)
	go func() {
		defer pending.Done()
		e := cmd.Wait()
		exit := 0
		signal := ""
		if e != nil {
			exit = -1
			if ee, ok := e.(*exec.ExitError); ok {
				exit = ee.ExitCode()
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					signal = ws.Signal().String()
				}
			}
		}
		_ = Write(fmt.Sprintf("client-wait-%d.json", pid), map[string]any{"pid": pid, "parent": os.Getpid(), "exit": exit, "signal": signal, "kind": "exec.Cmd.Wait returned", "birth_image": "not sealed; never enumerated or signalled"})
		done <- e
	}()
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		_ = Write(fmt.Sprintf("STOP-client-%d.json", pid), map[string]any{"pid": pid, "reason": "client Wait pending; no signal; retain directory"})
		return ctx.Err()
	}
}
func StopConsumer(ctx context.Context, pid int, generation string) error {
	r, e := known(pid)
	if e != nil {
		return e
	}
	if r.Role != "consumer" || r.Generation != generation || r.Nonce == "" {
		return errors.New("consumer launch generation/role mismatch")
	}
	return Stop(ctx, pid)
}

func Canceled() bool { _, e := os.Stat(filepath.Join(Dir(), "cancel.json")); return e == nil }
func Cancel() error {
	return Write("cancel.json", map[string]any{"at": time.Now().UTC(), "reason": "fixture cleanup; no new launches"})
}

// WatchCancellation reaches the same context cancellation path used for SIGTERM;
// no production lock is dropped. It is fixture-only fail-path cancellation.
func WatchCancellation(ctx context.Context, cancel context.CancelFunc) {
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if Canceled() {
					cancel()
					return
				}
			}
		}
	}()
}

func waitMatches(r Row) bool {
	var receipt map[string]any
	if Read(fmt.Sprintf("wait-%d.json", r.PID), &receipt) != nil {
		return false
	}
	return receipt["pid"] == float64(r.PID) && receipt["parent"] == float64(r.PPID) && receipt["nonce"] == r.Nonce && receipt["birth"] == r.Birth && receipt["generation"] == r.Generation && receipt["kind"] == "exec.Cmd.Wait returned"
}
