package darwinrelease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// own status survives a clean restore. Each case checks the list afterwards,
// next to a clean positive.
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
	}{
		{name: "positive: install, sign, restore", fn: okSign, wantList: "ok", wantState: "ran", wantOK: true, checkAfter: true},
		{name: "G1 S0 read fails", f: fakeSearchList{getFail: map[int]bool{1: true}}, fn: okSign, wantList: "search-list-unavailable", wantState: "unrun", checkAfter: true},
		{name: "G2 original already lists the release keychain", start: installed, fn: okSign, wantList: "search-list-unexpected", wantState: "unrun", wantAfter: installed, checkAfter: true},
		{name: "G3 set fails, list unchanged", f: fakeSearchList{setFailBefore: 1}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true},
		{name: "G4 set fails after a partial mutation", f: fakeSearchList{setFailAfter: 1, partial: true}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true},
		{name: "G5 install readback fails", f: fakeSearchList{getFail: map[int]bool{2: true}}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true},
		{name: "G6 install readback differs", f: fakeSearchList{external: map[int][]string{2: {"/other"}}}, fn: okSign, wantList: "search-list-set", wantState: "unrun", checkAfter: true},
		{name: "G7 another writer after a verified install", f: fakeSearchList{external: map[int][]string{3: {releaseKC}}}, fn: okSign, wantList: "search-list-concurrent", wantState: "ran", checkAfter: true},
		{name: "G8 restore set fails twice", f: fakeSearchList{setFailBefore: 2, setFailAfter: 3}, fn: okSign, wantList: "search-list-restore", wantState: "ran"},
		{name: "G9 restore readback differs", f: fakeSearchList{external: map[int][]string{4: {"/x"}, 5: {"/x"}}}, fn: okSign, wantList: "search-list-restore", wantState: "ran"},
		{name: "G10 setup refusal and restore failure", f: fakeSearchList{setFailBefore: 1, getFail: map[int]bool{2: true, 3: true}}, fn: okSign, wantList: "search-list-restore", wantState: "unrun"},
		{name: "G11 fn refusal survives a clean restore", fn: refused, wantList: "ok", wantState: "ran", checkAfter: true},
		{name: "S4 unreadable before restore", f: fakeSearchList{getFail: map[int]bool{3: true}}, fn: okSign, wantList: "search-list-restore", wantState: "ran", checkAfter: true},
		{name: "G16 empty original list", start: []string{}, fn: okSign, wantList: "ok", wantState: "ran", wantOK: true, wantAfter: []string{}, checkAfter: true},
	} {
		f := c.f
		start := origList
		if c.start != nil {
			start = c.start
		}
		f.list = append([]string(nil), start...)
		st := withSigningSearchList(&f, releaseKC, nil, c.fn)
		if st.List != c.wantList || st.State != c.wantState || st.ok() != c.wantOK {
			t.Fatalf("%s: got %+v ok=%v, want list=%s state=%s ok=%v", c.name, st, st.ok(), c.wantList, c.wantState, c.wantOK)
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
// the original list, and that fn runs only after it; it is never called for
// a setup refusal.
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
	observed := false
	withSigningSearchList(&g, releaseKC, func([]string) { observed = true }, okSign)
	if observed {
		t.Fatal("observe ran for a setup refusal")
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

// Proves a failure on the first binary reports every later binary as unrun,
// and a failure on the last binary reports none.
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
