//go:build unix

package darwinrelease

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSearchList is an in-memory user search list with scripted failures.
// setFailAfter / setFailBefore name the 1-based Set call that fails after or
// without mutating; partial makes a failing Set leave only the first entry.
// getFail fails the Nth 1-based Get; external mutates the list on the Nth Get,
// emulating another writer.
type fakeSearchList struct {
	list                        []string
	sets, gets                  int
	setFailAfter, setFailBefore int
	partial                     bool
	getFail                     map[int]bool
	external                    map[int][]string
}

func (f *fakeSearchList) Get() ([]string, error) {
	f.gets++
	if l, ok := f.external[f.gets]; ok {
		f.list = append([]string(nil), l...)
	}
	if f.getFail[f.gets] {
		return nil, errors.New("synthetic get failure")
	}
	return append([]string(nil), f.list...), nil
}

func (f *fakeSearchList) Set(l []string) error {
	f.sets++
	if f.sets == f.setFailBefore {
		return errors.New("synthetic set failure, unchanged")
	}
	if f.sets == f.setFailAfter {
		if f.partial && len(l) > 0 {
			f.list = l[:1]
		} else {
			f.list = append([]string(nil), l...)
		}
		return errors.New("synthetic set failure after mutation")
	}
	f.list = append([]string(nil), l...)
	return nil
}

var (
	origList  = []string{"/Users/runner/Library/Keychains/login.keychain-db"}
	releaseKC = "/runner/temp/a2a-release-private/signing.keychain-db"
	okSign    = func() signStatus { return signStatus{State: "ran", Exit: 0, Class: "none"} }
)

// Proves the guard's phase-aware statuses and post-states on scripted list
// failures: setup refusals (unchanged, partial, unreadable or mismatched
// install) keep search-list-set and are never attributed to another writer;
// a change after a VERIFIED install is search-list-concurrent; a failed or
// unverifiable restore is search-list-restore and wins over everything; fn's
// own status survives a clean restore (G11: exact Exit and Class). Each case
// also asserts how often fn and Set ran: a setup refusal never invokes fn,
// and G1/G2 attempt no mutation at all. The list is checked afterwards, next
// to a clean positive.
func TestSigningSearchListGuard(t *testing.T) {
	refused := func() signStatus { return signStatus{State: "ran", Exit: 1, Class: "identity-not-found"} }
	installed := append([]string{releaseKC}, origList...)
	for _, c := range []struct {
		name       string
		f          fakeSearchList
		start      []string
		fn         func() signStatus
		wantList   string
		wantState  string
		wantOK     bool
		wantAfter  []string // nil means: the original list
		checkAfter bool
		calls      int // fn invocations; 0 on every setup refusal
		sets       int // Set calls; 0 means no mutation was attempted
	}{
		{name: "positive: install, sign, restore", fn: okSign, wantList: "ok", wantState: "ran", wantOK: true, checkAfter: true, calls: 1, sets: 2},
		{name: "G1 S0 read fails", f: fakeSearchList{getFail: map[int]bool{1: true}}, fn: okSign, wantList: "search-list-unavailable", wantState: "unrun", checkAfter: true, calls: 0, sets: 0},
		{name: "G2 original already lists the release keychain", start: installed, fn: okSign, wantList: "search-list-unexpected", wantState: "unrun", wantAfter: installed, checkAfter: true, calls: 0, sets: 0},
		{name: "G3 set fails, list unchanged", f: fakeSearchList{setFailBefore: 1}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true, calls: 0, sets: 2},
		{name: "G4 set fails after a partial mutation", f: fakeSearchList{setFailAfter: 1, partial: true}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true, calls: 0, sets: 2},
		{name: "G5 install readback fails", f: fakeSearchList{getFail: map[int]bool{2: true}}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true, calls: 0, sets: 2},
		{name: "G6 install readback differs", f: fakeSearchList{external: map[int][]string{2: {"/other"}}}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true, calls: 0, sets: 2},
		{name: "G7 another writer after a verified install", f: fakeSearchList{external: map[int][]string{3: {releaseKC}}}, fn: okSign, wantList: "search-list-concurrent", wantState: "ran", checkAfter: true, calls: 1, sets: 2},
		{name: "G8 restore set fails twice", f: fakeSearchList{setFailBefore: 2, setFailAfter: 3}, fn: okSign, wantList: "search-list-restore", wantState: "ran", calls: 1, sets: 3},
		{name: "G9 restore readback differs", f: fakeSearchList{external: map[int][]string{4: {"/x"}, 5: {"/x"}}}, fn: okSign, wantList: "search-list-restore", wantState: "ran", calls: 1, sets: 3},
		{name: "G10 setup refusal and restore failure", f: fakeSearchList{setFailBefore: 1, getFail: map[int]bool{2: true, 3: true}}, fn: okSign, wantList: "search-list-restore", wantState: "unrun", calls: 0, sets: 3},
		{name: "G11 fn refusal survives a clean restore", fn: refused, wantList: "ok", wantState: "ran", checkAfter: true, calls: 1, sets: 2},
		{name: "S4 unreadable before restore", f: fakeSearchList{getFail: map[int]bool{3: true}}, fn: okSign, wantList: "search-list-restore", wantState: "ran", checkAfter: true, calls: 1, sets: 2},
		{name: "G16 empty original list", start: []string{}, fn: okSign, wantList: "ok", wantState: "ran", wantOK: true, wantAfter: []string{}, checkAfter: true, calls: 1, sets: 2},
	} {
		f := c.f
		start := origList
		if c.start != nil {
			start = c.start
		}
		f.list = append([]string(nil), start...)
		calls := 0
		st := withSigningSearchList(&f, releaseKC, nil, func() signStatus { calls++; return c.fn() })
		if calls != c.calls || f.sets != c.sets {
			t.Fatalf("%s: fn calls %d, Set calls %d; want %d, %d", c.name, calls, f.sets, c.calls, c.sets)
		}
		if st.List != c.wantList || st.State != c.wantState || st.ok() != c.wantOK {
			t.Fatalf("%s: got %+v ok=%v, want list=%s state=%s ok=%v", c.name, st, st.ok(), c.wantList, c.wantState, c.wantOK)
		}
		if c.name == "G11 fn refusal survives a clean restore" && (st.Exit != 1 || st.Class != "identity-not-found") {
			t.Fatalf("G11: fn status not preserved exactly: %+v", st)
		}
		if c.checkAfter {
			want := origList
			if c.wantAfter != nil {
				want = c.wantAfter
			}
			if !sameList(f.list, want) {
				t.Fatalf("%s: list after = %q, want %q", c.name, f.list, want)
			}
		}
	}
}

// Proves the observe seam sees exactly the verified install, [keychain] plus
// the original list, and that fn runs only after it; neither observe nor fn
// is called for a setup refusal.
func TestSigningSearchListObserveAfterVerifiedInstall(t *testing.T) {
	f := fakeSearchList{list: append([]string(nil), origList...)}
	var seen []string
	calledBefore := false
	st := withSigningSearchList(&f, releaseKC, func(l []string) { seen = l }, func() signStatus {
		calledBefore = seen != nil
		return okSign()
	})
	if !st.ok() || !calledBefore || !sameList(seen, append([]string{releaseKC}, origList...)) {
		t.Fatalf("observe %q before fn=%v, status %+v", seen, calledBefore, st)
	}
	g := fakeSearchList{list: append([]string(nil), origList...), setFailBefore: 1}
	observed, called := false, false
	withSigningSearchList(&g, releaseKC, func([]string) { observed = true }, func() signStatus { called = true; return okSign() })
	if observed || called {
		t.Fatalf("setup refusal ran observe=%v fn=%v", observed, called)
	}
}

// Proves a panic in fn still restores the original list and then propagates.
func TestSigningSearchListRestoresOnPanic(t *testing.T) {
	f := fakeSearchList{list: append([]string(nil), origList...)}
	defer func() {
		if recover() == nil {
			t.Fatal("panic was swallowed")
		}
		if !sameList(f.list, origList) {
			t.Fatalf("list after panic = %q", f.list)
		}
	}()
	withSigningSearchList(&f, releaseKC, nil, func() signStatus { panic("synthetic") })
}

// Proves the unsupported platform stub refuses before any mutation.
func TestSigningSearchListUnsupported(t *testing.T) {
	st := withSigningSearchList(unsupportedOps{}, releaseKC, nil, okSign)
	if st.List != "search-list-unsupported" || st.ok() {
		t.Fatalf("unsupported = %+v", st)
	}
}

type unsupportedOps struct{}

func (unsupportedOps) Get() ([]string, error) { return nil, errSearchListUnsupported }
func (unsupportedOps) Set([]string) error     { return errSearchListUnsupported }

// shell runs a /bin/sh script through executeClassified with a short deadline.
func shell(t *testing.T, ctx context.Context, deadline time.Duration, script string) signStatus {
	t.Helper()
	return executeClassified(ctx, deadline, t.TempDir(), "/bin/sh", "-c", script)
}

// Proves executeClassified's distinct states on real subprocesses: a clean
// exit, a nonzero exit with its class, the deadline, a parent cancellation, a
// signal and a failed start. None of the non-ran states reports an exit code,
// and none is ok.
func TestExecuteClassifiedStates(t *testing.T) {
	if st := shell(t, context.Background(), 10*time.Second, "exit 0"); st.State != "ran" || st.Exit != 0 || st.Class != "none" {
		t.Fatalf("clean = %+v", st)
	}
	if st := shell(t, context.Background(), 10*time.Second, "echo 'x: No such file or directory' >&2; exit 1"); st.State != "ran" || st.Exit != 1 || st.Class != "target-missing" {
		t.Fatalf("refusal = %+v", st)
	}
	if st := shell(t, context.Background(), 200*time.Millisecond, "sleep 5"); st.State != "timeout" || st.message("binary-sign", "a2a") != "binary-sign: name=a2a state=timeout exit=- class=none list=-" {
		t.Fatalf("timeout = %+v %q", st, st.message("binary-sign", "a2a"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if st := shell(t, ctx, 10*time.Second, "sleep 5"); st.State != "cancelled" {
		t.Fatalf("cancelled = %+v", st)
	}
	if st := shell(t, context.Background(), 10*time.Second, "kill -KILL $$"); st.State != "signaled" {
		t.Fatalf("signaled = %+v", st)
	}
	if st := executeClassified(context.Background(), 10*time.Second, t.TempDir(), filepath.Join(t.TempDir(), "absent")); st.State != "start-failed" {
		t.Fatalf("start-failed = %+v", st)
	}
	for _, st := range []signStatus{{State: "timeout", List: "ok"}, {State: "cancelled", List: "ok"}, {State: "signaled", List: "ok"}, {State: "start-failed", List: "ok"}, {State: "unrun", List: "ok"}} {
		if st.ok() {
			t.Fatalf("%s reads as ok", st.State)
		}
	}
}

// Proves stderr over the 64 KiB bound is refused as output-overflow even on
// exit 0, beside a small stderr that is classified normally.
func TestExecuteClassifiedOverflow(t *testing.T) {
	st := shell(t, context.Background(), 30*time.Second, "head -c 70000 /dev/zero | tr '\\0' x >&2; exit 0")
	if st.Class != "output-overflow" || (signStatus{State: st.State, Exit: st.Exit, Class: st.Class, List: "ok"}).ok() {
		t.Fatalf("overflow = %+v", st)
	}
	if st := shell(t, context.Background(), 10*time.Second, "echo small >&2; exit 0"); st.Class != "other" {
		t.Fatalf("small = %+v", st)
	}
}

// Proves no stderr, argv, selector or keychain path reaches the bounded
// error: synthetic sentinels in each place are absent from the message,
// while the public binary name and closed tokens are present.
func TestSigningErrorCarriesNoValues(t *testing.T) {
	const stderrSentinel, argSentinel = "SENTINEL-7f3a", "SENTINEL-ARG-91c2"
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codesign")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '"+stderrSentinel+" errSecInternalComponent' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	st := executeClassified(context.Background(), 10*time.Second, dir, script, "--sign", argSentinel, "--keychain", "/"+argSentinel+"/kc")
	st.List = "ok"
	msg := st.message("binary-sign", "a2a")
	for _, s := range []string{stderrSentinel, argSentinel} {
		if strings.Contains(msg, s) {
			t.Fatalf("sentinel %q leaked into %q", s, msg)
		}
	}
	if msg != "binary-sign: name=a2a state=ran exit=1 class=internal-component list=ok" {
		t.Fatalf("message = %q", msg)
	}
	err := signFailure([]string{"a2a", "a2a-keychain-status"}, 0, st)
	if strings.Contains(err.Error(), stderrSentinel) || strings.Contains(err.Error(), argSentinel) {
		t.Fatalf("sentinel leaked into %q", err)
	}
}

// Proves the error FORMAT for a partial run: a failure on the first binary
// reports every later binary as unrun, and a failure on the last reports
// none. Limit: formatting only; it does not execute Prepare or prove that a
// later binary's signing step is not invoked (Prepare returns on the first
// failure by construction).
func TestSignFailureMarksLaterBinariesUnrun(t *testing.T) {
	st := signStatus{State: "ran", Exit: 1, Class: "identity-not-found", List: "ok"}
	if got := signFailure(Binaries, 0, st).Error(); got != "binary-sign: name=a2a state=ran exit=1 class=identity-not-found list=ok; a2a-keychain-status state=unrun" {
		t.Fatalf("first = %q", got)
	}
	if got := signFailure(Binaries, 1, st).Error(); strings.Contains(got, "unrun") {
		t.Fatalf("last = %q", got)
	}
}

// Proves the closed codesign class table, including the new target-missing.
func TestCodesignErrorClass(t *testing.T) {
	for in, want := range map[string]string{
		"/x/a2a: No such file or directory":                             "target-missing",
		"error: The specified item could not be found in the keychain.": "identity-not-found",
		"errSecInternalComponent":                                       "internal-component",
		"The timestamp service is not available.":                       "timestamp",
		"":                   "none",
		"unrecognised words": "other",
	} {
		if got := codesignErrorClass(in); got != want {
			t.Fatalf("codesignErrorClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// waitFile polls until path exists or the bound passes.
func waitFile(t *testing.T, path string, bound time.Duration) {
	t.Helper()
	for end := time.Now().Add(bound); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear within %s", filepath.Base(path), bound)
}

// childState observes one process the test owns, with a bounded
// observation: "gone" (no such process), "not-running" (a zombie awaiting its
// reaper), "running", or "unknown" when the observation itself fails. ps is
// the observer binary, so a control can break it.
func childState(pid int, ps string, bound time.Duration) string {
	switch err := syscall.Kill(pid, 0); {
	case errors.Is(err, syscall.ESRCH):
		return "gone"
	case err != nil:
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	out, err := exec.CommandContext(ctx, ps, "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "unknown"
	}
	switch st := strings.TrimSpace(string(out)); {
	case st == "":
		return "unknown"
	case strings.HasPrefix(st, "Z"):
		return "not-running"
	default:
		return "running"
	}
}

// stopOwnChild stops a fixture child this test started, by its exact PID,
// and requires it to be observed gone or not running within the bound. An
// unknown observation fails. Production code makes no claim to reap such
// descendants; the test owns and cleans up its own.
func stopOwnChild(t *testing.T, pid int) {
	t.Helper()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	state := ""
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if state = childState(pid, "/bin/ps", time.Second); state == "gone" || state == "not-running" {
			return
		}
	}
	t.Fatalf("own fixture child %d not stopped: last observation %q", pid, state)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("bad fixture pid %q", b)
	}
	return pid
}

// Proves the guard restores the original list when its signing call ends by
// the per-call deadline (G14) or by a parent cancellation issued only AFTER
// the command has started (G13), each with a fresh context and a bounded
// completion, beside a call that exits cleanly. The fixture's own child is
// then stopped by the test itself (production does not claim to reap it).
func TestSigningSearchListRestoresAfterTimeoutAndCancel(t *testing.T) {
	for _, c := range []struct {
		name      string
		deadline  time.Duration
		cancel    bool
		wantState string
		wantOK    bool
	}{
		{"positive: clean exit", 10 * time.Second, false, "ran", true},
		{"G14 deadline", 300 * time.Millisecond, false, "timeout", false},
		{"G13 parent cancellation after start", 10 * time.Second, true, "cancelled", false},
	} {
		dir := t.TempDir()
		marker, pidFile := filepath.Join(dir, "entered"), filepath.Join(dir, "child.pid")
		script := "exit 0"
		if c.wantState != "ran" {
			script = "sleep 30 & echo $! > " + pidFile + "; echo in > " + marker + "; wait"
		}
		ctx, cancel := context.WithCancel(context.Background())
		if c.cancel {
			// Cancel only after the command reports that it started; never call
			// t.Fatal from this goroutine.
			go func() {
				for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
					if _, err := os.Stat(marker); err == nil {
						break
					}
				}
				cancel()
			}()
		}
		f := fakeSearchList{list: append([]string(nil), origList...)}
		start := time.Now()
		st := withSigningSearchList(&f, releaseKC, nil, func() signStatus {
			return executeClassified(ctx, c.deadline, dir, "/bin/sh", "-c", script)
		})
		elapsed := time.Since(start)
		cancel()
		if st.State != c.wantState || st.ok() != c.wantOK || st.List != "ok" || !sameList(f.list, origList) {
			t.Fatalf("%s: %+v ok=%v list=%q", c.name, st, st.ok(), f.list)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("%s: took %s, not bounded", c.name, elapsed)
		}
		if c.wantState != "ran" {
			stopOwnChild(t, readPID(t, pidFile))
		}
	}
}

// Proves a descendant holding the command's inherited output pipes cannot
// hold executeClassified open past its bound: on the deadline the state is
// timeout, and when the command exits 0 while a descendant keeps the pipe,
// WaitDelay ends the wait with state wait-delay, not ok and not an exit code.
// Positive neighbour: a quick clean exit. The descendant may outlive the call
// (stated residual); the test stops its own fixture child by exact PID.
func TestExecuteClassifiedBoundsInheritedPipes(t *testing.T) {
	if st := shell(t, context.Background(), 10*time.Second, "exit 0"); !st.commandOK() {
		t.Fatalf("positive = %+v", st)
	}
	for _, c := range []struct {
		name, script string
		deadline     time.Duration
		wantState    string
	}{
		{"deadline with a pipe-holding child", "sleep 30 & echo $! > PID; wait", 300 * time.Millisecond, "timeout"},
		{"exit 0 while a child holds the pipe", "sleep 30 & echo $! > PID; exit 0", 10 * time.Second, "wait-delay"},
	} {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "child.pid")
		start := time.Now()
		st := executeClassified(context.Background(), c.deadline, dir, "/bin/sh", "-c", strings.ReplaceAll(c.script, "PID", pidFile))
		if elapsed := time.Since(start); elapsed > c.deadline+executeWaitDelay+3*time.Second {
			t.Fatalf("%s: took %s, not bounded", c.name, elapsed)
		}
		if st.State != c.wantState || st.commandOK() {
			t.Fatalf("%s: %+v", c.name, st)
		}
		stopOwnChild(t, readPID(t, pidFile))
	}
}

// Control for the test above: a plain CommandContext with no WaitDelay and no
// own process group stays blocked well past its deadline while a child holds
// the pipe, so the bounded-executor test does detect a missing bound. The
// control then stops only its own child, by its exact PID.
func TestUnboundedCommandControl(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	c.Stdout, c.Stderr = io.Discard, io.Discard
	done := make(chan struct{})
	go func() { _ = c.Run(); close(done) }()
	select {
	case <-done:
		t.Fatal("control returned within its bound: it cannot detect a missing bound")
	case <-time.After(2500 * time.Millisecond):
	}
	waitFile(t, pidFile, 2*time.Second)
	stopOwnChild(t, readPID(t, pidFile))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("control did not return after its own child stopped")
	}
}

// Proves commandOK, the release verification predicate, refuses a real
// over-cap stderr with exit 0, beside a normal exit 0 that passes; and that
// ok additionally requires the guard's List=ok.
func TestCommandOKRefusesOverflow(t *testing.T) {
	over := shell(t, context.Background(), 30*time.Second, "head -c 70000 /dev/zero | tr '\\0' x >&2; exit 0")
	if over.State != "ran" || over.Exit != 0 || over.commandOK() {
		t.Fatalf("over-cap exit 0 = %+v commandOK=%v", over, over.commandOK())
	}
	normal := shell(t, context.Background(), 10*time.Second, "exit 0")
	if !normal.commandOK() || normal.ok() {
		t.Fatalf("normal = %+v commandOK=%v ok=%v (ok needs List=ok)", normal, normal.commandOK(), normal.ok())
	}
	normal.List = "ok"
	if !normal.ok() {
		t.Fatal("ok refused a clean, restored command")
	}
}

// Proves childState keeps observation failure distinct from absence: a
// broken observer yields "unknown" for a live owned child, never "gone",
// beside genuine controls where the same child is observed "running" and,
// after the test stops and reaps it, "gone".
func TestChildStateUnknownIsNotAbsence(t *testing.T) {
	c := exec.Command("/bin/sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if got := childState(pid, "/bin/ps", 2*time.Second); got != "running" {
		t.Fatalf("live child = %q, want running", got)
	}
	if got := childState(pid, filepath.Join(t.TempDir(), "no-such-ps"), 2*time.Second); got != "unknown" {
		t.Fatalf("broken observer = %q, want unknown", got)
	}
	_ = c.Process.Kill()
	_ = c.Wait()
	if got := childState(pid, "/bin/ps", 2*time.Second); got != "gone" {
		t.Fatalf("stopped and reaped child = %q, want gone", got)
	}
}
