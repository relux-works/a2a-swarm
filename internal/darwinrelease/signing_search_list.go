package darwinrelease

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// releaseSignDeadline bounds one release codesign --sign call.
const releaseSignDeadline = 10 * time.Minute

// stderrCap bounds the stderr bytes kept in memory for classification.
const stderrCap = 64 << 10

// executeWaitDelay bounds how long Run waits for output after the command
// exits or is cancelled; descendants holding inherited pipes cannot extend it.
const executeWaitDelay = 2 * time.Second

// errSearchListUnsupported is returned by platforms that cannot sign at all.
var errSearchListUnsupported = errors.New("search list unsupported")

// searchListOps reads and replaces the user keychain search list as ordered
// keychain paths. The darwin implementation uses Security APIs only.
type searchListOps interface {
	Get() ([]string, error)
	Set([]string) error
}

// signStatus is the bounded outcome of one signing step. It never carries
// stderr, argv, a selector or a keychain path.
//
//	State: ran | timeout | cancelled | signaled | start-failed | wait-delay | unrun
//	Exit:  the process exit code, meaningful only when State is ran
//	Class: a closed codesignErrorClass token
//	List:  ok, or a search-list-* code from withSigningSearchList
type signStatus struct {
	State string
	Exit  int
	Class string
	List  string
}

// commandOK is true only for a process that ran, exited 0 and did not
// overflow its stderr bound. Release verification uses it directly.
func (s signStatus) commandOK() bool {
	return s.State == "ran" && s.Exit == 0 && s.Class != "output-overflow"
}

// ok is commandOK plus a search-list guard that installed and restored cleanly.
func (s signStatus) ok() bool { return s.List == "ok" && s.commandOK() }

// message renders the closed error shape for a step and a public binary name.
func (s signStatus) message(step, name string) string {
	exit := "-"
	if s.State == "ran" {
		exit = strconv.Itoa(s.Exit)
	}
	class, list := s.Class, s.List
	if class == "" {
		class = "none"
	}
	if list == "" {
		list = "-"
	}
	return step + ": name=" + name + " state=" + s.State + " exit=" + exit + " class=" + class + " list=" + list
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restoreSearchList sets original and reads it back, with one retry. It
// reports whether the readback equals original exactly.
func restoreSearchList(ops searchListOps, original []string) bool {
	for attempt := 0; attempt < 2; attempt++ {
		if ops.Set(original) != nil {
			continue
		}
		if got, err := ops.Get(); err == nil && sameList(got, original) {
			return true
		}
	}
	return false
}

// withSigningSearchList runs fn with keychain first on the user search list
// and restores the original list on every exit, including a panic in fn.
//
// Phases: S0 reads the complete original list (refused before any mutation if
// unreadable or already naming keychain); S1 sets [keychain]+original; S2 reads
// it back and requires exactly that; S3 calls observe (a test seam, nil in
// release code) and fn; S4, only after a verified S2, re-reads the list to
// detect a change by another writer; S5 restores and verifies the original.
//
// Precedence, highest first: search-list-restore (the original was not
// reproduced, or S4 could not read), search-list-concurrent (only after a
// verified install), a setup refusal (search-list-set; an unchanged or partial
// failing setter is never attributed to another writer), then fn's status.
func withSigningSearchList(ops searchListOps, keychain string, observe func([]string), fn func() signStatus) (st signStatus) {
	original, err := ops.Get()
	if err != nil {
		code := "search-list-unavailable"
		if errors.Is(err, errSearchListUnsupported) {
			code = "search-list-unsupported"
		}
		return signStatus{State: "unrun", List: code}
	}
	for _, p := range original {
		if p == keychain {
			return signStatus{State: "unrun", List: "search-list-unexpected"}
		}
	}
	want := append([]string{keychain}, original...)
	installed := false
	defer func() {
		r := recover()
		concurrent, unreadable := false, false
		if installed {
			if now, err := ops.Get(); err != nil {
				unreadable = true
			} else if !sameList(now, want) {
				concurrent = true
			}
		}
		restored := restoreSearchList(ops, original)
		switch {
		case !restored || unreadable:
			st.List = "search-list-restore"
		case concurrent:
			st.List = "search-list-concurrent"
		}
		if r != nil {
			panic(r)
		}
	}()
	if ops.Set(want) != nil {
		return signStatus{State: "unrun", List: "search-list-set"}
	}
	got, err := ops.Get()
	if err != nil || !sameList(got, want) {
		return signStatus{State: "unrun", List: "search-list-set"}
	}
	installed = true
	if observe != nil {
		observe(append([]string(nil), got...))
	}
	st = fn()
	st.List = "ok"
	return st
}

// boundedBuffer keeps at most max bytes and records whether more arrived.
type boundedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// executeClassified runs a command under its own deadline and returns only a
// bounded status. stderr is held in memory up to stderrCap, mapped to a closed
// class and discarded; stdout is discarded.
//
// The command runs in its own process group (unix). Cancellation and the
// deadline kill the whole group, and WaitDelay bounds the wait for output, so
// a descendant holding an inherited pipe cannot hold Run open. Descendants
// left in the group after the command exits are killed. A parent
// cancellation, the deadline, a signal, a failed start and an output wait cut
// by WaitDelay (wait-delay) are distinct states, and none is reported as an
// exit code.
func executeClassified(ctx context.Context, deadline time.Duration, dir, name string, args ...string) signStatus {
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	c := exec.CommandContext(cctx, name, args...)
	c.Dir = dir
	c.Env = cleanEnvironment()
	ownProcessGroup(c)
	c.WaitDelay = executeWaitDelay
	stderr := &boundedBuffer{max: stderrCap}
	c.Stdout, c.Stderr = io.Discard, stderr
	err := c.Run()
	reapOwnGroup(c)
	switch {
	case ctx.Err() != nil:
		return signStatus{State: "cancelled"}
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return signStatus{State: "timeout"}
	}
	st := signStatus{State: "ran"}
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		return signStatus{State: "wait-delay"}
	case errors.As(err, &exit):
		if !exit.Exited() {
			return signStatus{State: "signaled"}
		}
		st.Exit = exit.ExitCode()
	case err != nil:
		return signStatus{State: "start-failed"}
	}
	if stderr.over {
		st.Class = "output-overflow"
	} else {
		st.Class = codesignErrorClass(stderr.buf.String())
	}
	return st
}

// codesignErrorClass maps codesign stderr to a closed vocabulary. Raw stderr
// never leaves the process: only the class does.
func codesignErrorClass(stderr string) string {
	s := strings.ToLower(stderr)
	for _, c := range []struct{ needle, class string }{
		{"errsecinternalcomponent", "internal-component"},
		{"user interaction is not allowed", "interaction-not-allowed"},
		{"unable to build chain", "chain-build"},
		{"not trusted", "not-trusted"},
		{"no such file or directory", "target-missing"},
		{"timestamp", "timestamp"},
		{"no identity found", "identity-not-found"},
		{"could not be found", "identity-not-found"},
		{"cannot be used for signing", "identity-unusable"},
		{"test-requirement", "requirement-unsatisfied"},
		{"does not satisfy", "requirement-unsatisfied"},
	} {
		if strings.Contains(s, c.needle) {
			return c.class
		}
	}
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return "other"
}

// signOne signs one release binary with the private keychain first on the
// user search list for exactly that codesign call.
func signOne(ctx context.Context, ops searchListOps, selector, keychain, name, expr, path, dir string, deadline time.Duration, observe func([]string)) signStatus {
	return withSigningSearchList(ops, keychain, observe, func() signStatus {
		return executeClassified(ctx, deadline, dir, "/usr/bin/codesign", signArgs(selector, keychain, name, expr, path)...)
	})
}

// signFailure renders a failed signing step and marks every later binary in
// names as unrun, so a partial run is visible in the bounded error.
func signFailure(names []string, i int, st signStatus) error {
	msg := st.message("binary-sign", names[i])
	for _, rest := range names[i+1:] {
		msg += "; " + rest + " state=unrun"
	}
	return errors.New(msg)
}
