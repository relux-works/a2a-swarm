//go:build unix

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// These public-entry regressions retain the CR1 reviewer's early-leader
// witnesses for the status probe, the only helper process left after phase 2
// (#138), in both pipe variants. No real Keychain or status executable is
// invoked. The child records membership BEFORE cleanup.
func TestKeychainEarlyLeaderExit(t *testing.T) {
	for _, pipes := range []string{"inherited", "closed"} {
		t.Run(pipes, func(t *testing.T) {
			testKeychainProcessLifetime(t, pipes, false, 0)
		})
	}
}

func TestKeychainProcessCancellation(t *testing.T) {
	testKeychainProcessLifetime(t, "inherited", true, 0)
}

// A probe that cannot start refuses before any backend operation or Matrix call.
func TestKeychainProcessStartFailure(t *testing.T) {
	for _, mode := range []string{"missing", "nonexec"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
			fake := probeFixture(t, mode, dir)
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: fake})
				}
			}
			e.as("alice", "whoami").failsWith(t, ExitError, "preflight failed")
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
			if len(records(t, dir)) != 0 {
				t.Fatal("startup failure reached the keychain backend")
			}
		})
	}
}

// TestKeychainPipeCompletion retains the CR2 nonzero-leader reviewer witness
// for the probe: any nonzero exit refuses, and a successful leader stays
// successful after its group is cleaned up.
func TestKeychainPipeCompletion(t *testing.T) {
	for _, pipes := range []string{"inherited", "closed"} {
		for _, code := range []int{0, 1, 36, 44, 45} {
			t.Run(fmt.Sprintf("%s/exit%d", pipes, code), func(t *testing.T) {
				testKeychainProcessLifetime(t, pipes, false, code)
			})
		}
	}
}

// Nearby success/missing/duplicate controls
// are also retained in
// TestKeychainCredentialSelection and TestKeychainRegistrationAndUpsert.

func testKeychainProcessLifetime(t *testing.T, pipes string, cancelOnReady bool, exitCode int) {
	t.Helper()
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	path := filepath.Join(dir, "early-leader")
	witness := filepath.Join(dir, "owned-group")
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte("unlocked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	redirect := ""
	if pipes == "closed" {
		redirect = " </dev/null >/dev/null 2>&1"
	}
	terminal := fmt.Sprintf("exit %d", exitCode)
	if cancelOnReady {
		terminal = "wait"
	}
	body := fmt.Sprintf("#!/bin/sh\ncd '%s' || exit 1\nprintf 'call\\n' >> calls\nsleep 30%s &\nchild=$!\ngroup=$(/bin/ps -o pgid= -p \"$child\")\nprintf '%%s %%s %%s\\n' \"$$\" \"$child\" \"$group\" > owned-group\n/bin/cat value\n%s\n", dir, redirect, terminal)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	// Always clean ONLY the witnessed own group, including on mutant failures.
	t.Cleanup(func() {
		if data, err := os.ReadFile(witness); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 3 && fields[0] == fields[2] {
				group, gerr := strconv.Atoi(fields[0])
				child, cerr := strconv.Atoi(fields[1])
				if actual, err := syscall.Getpgid(child); err == nil && gerr == nil && cerr == nil && group > 1 && actual == group {
					err := syscall.Kill(-group, syscall.SIGKILL)
					t.Logf("fixture cleanup own PGID=%d signal=%v", group, err)
				}
			}
		}
	})
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			fake := func(ctx context.Context, args ...string) *exec.Cmd { return exec.CommandContext(ctx, "/bin/sh", path) }
			return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: fake})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelOnReady {
		stopped := make(chan struct{})
		defer func() { <-stopped }()
		go func() {
			defer close(stopped)
			deadline := time.NewTimer(4 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				// Creation precedes the write: cancel only after the complete
				// ownership record, not while the shell still has an empty file.
				if data, err := os.ReadFile(witness); err == nil && len(strings.Fields(string(data))) == 3 {
					cancel()
					return
				}
				select {
				case <-deadline.C:
					cancel()
					return
				case <-tick.C:
				}
			}
		}()
	}
	start := time.Now()
	r := e.runCtx(ctx, "--as", "alice", "whoami")
	if elapsed := time.Since(start); elapsed > securityDeadline+time.Second {
		t.Fatalf("process deadline exceeded: %s", elapsed)
	}
	data, err := os.ReadFile(witness)
	if err != nil {
		t.Fatal("missing owned group witness", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 || fields[0] != fields[2] {
		t.Fatalf("descendant escaped invocation PGID: %s", data)
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pipes=%s cancellation=%t leader=%s child=%d PGID=%s App.Run=%d elapsed=%s", pipes, cancelOnReady, fields[0], pid, fields[2], r.code, time.Since(start))
	// Allow asynchronous orphan reaping. A zombie has stopped executing; a live
	// sleeper must disappear within the invocation deadline even with no pipes.
	for until := time.Now().Add(150 * time.Millisecond); ; {
		out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(out))
		if err == nil && (state == "" || strings.HasPrefix(state, "Z")) {
			break
		}
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && state == "" {
			break // ps: PID absent
		}
		if time.Now().After(until) {
			t.Fatalf("early-leader descendant survived own-group cleanup: pid=%d state=%q ps=%v", pid, state, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pipes == "inherited" || cancelOnReady || exitCode != 0 {
		r.failsWith(t, ExitError, "preflight failed")
		tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
		tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
		k := isolatedKeychainStore(keychainStore{paths: e.paths()})
		if err := k.checkCooldown(uid); err == nil {
			t.Fatal("pipe-wait/cancellation failure omitted identity cooldown")
		}
		before, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		e.as("alice", "whoami").failsWith(t, ExitError, "cooldown active")
		after, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil || string(before) != string(after) || len(records(t, dir)) != 0 {
			t.Fatal("cooldown admitted another probe/backend/Matrix call", err)
		}
	} else {
		r.ok(t) // Successful leader remains successful after child cleanup.
		tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
		if err := (isolatedKeychainStore(keychainStore{paths: e.paths()})).checkCooldown(uid); err != nil {
			t.Fatal("successful cleanup triggered cooldown", err)
		}
	}
}

// migrationStages numbers the backend operations of one migration, including
// duplicate-create recovery; every operation is preceded by its own probe.
var migrationStages = []struct {
	name, mode string
	stop       int
	existing   bool
}{
	{"lookup", "", 1, false}, {"create", "", 2, false},
	{"update", "", 2, true}, {"readback", "", 3, true},
	{"relookup", "duplicate", 3, false}, {"duplicate-update", "duplicate", 4, false},
	{"duplicate-readback", "duplicate", 5, false},
}

// A failed probe pipe before any migration operation must not become
// absence/duplicate, reach that operation, or scrub a recoverable source file.
func TestKeychainPipeCompletionMigrationStages(t *testing.T) {
	for _, stage := range migrationStages {
		for _, code := range []int{1, 44, 45} {
			t.Run(fmt.Sprintf("%s/exit%d", stage.name, code), func(t *testing.T) {
				e := newEnv(t)
				uid := e.agent("alice")
				items := map[string]string{}
				if stage.existing {
					items[uid] = e.tokens["alice"]
				}
				dir := fakeKeychain(e, items, stage.mode)
				original, err := os.ReadFile(e.paths().AgentFile("alice"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "value"), []byte("unlocked\n"), 0600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "nonzero-pipe")
				witness := filepath.Join(dir, "ownership")
				body := fmt.Sprintf("#!/bin/sh\ncd '%s' || exit 99\n/bin/cat >/dev/null\n/bin/sleep 30 &\nchild=$!\ngroup=$(/bin/ps -o pgid= -p \"$child\")\nprintf '%%s %%s %%s\\n' \"$$\" \"$child\" \"$group\" > ownership\n/bin/cat value\nexit %d\n", dir, code)
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					data, err := os.ReadFile(witness)
					if err != nil {
						return
					}
					f := strings.Fields(string(data))
					if len(f) != 3 || f[0] != f[2] {
						return
					}
					group, _ := strconv.Atoi(f[0])
					child, _ := strconv.Atoi(f[1])
					if actual, err := syscall.Getpgid(child); err == nil && group > 1 && actual == group {
						_ = syscall.Kill(-group, syscall.SIGKILL)
					}
				})
				probes := 0
				e.configure = func(a *App) {
					a.keychain = func(p config.Paths) *keychainStore {
						return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd {
							probes++
							if probes == stage.stop {
								return exec.CommandContext(ctx, "/bin/sh", path)
							}
							return unlockedProbe(ctx, args...)
						}})
					}
				}
				start := time.Now()
				e.as("alice", "creds", "migrate").failsWith(t, ExitError, "preflight failed")
				if time.Since(start) > securityDeadline+time.Second {
					t.Fatal("pipe failure exceeded deadline")
				}
				data, err := os.ReadFile(witness)
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Fields(string(data))
				if len(fields) != 3 || fields[0] != fields[2] {
					t.Fatalf("invalid invocation group witness %q", data)
				}
				child, err := strconv.Atoi(fields[1])
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("stage=%s exit=%d leader=%s child=%d PGID=%s", stage.name, code, fields[0], child, fields[2])
				for until := time.Now().Add(300 * time.Millisecond); ; {
					out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(child)).Output()
					state := strings.TrimSpace(string(out))
					if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && state == "" {
						break
					}
					if err == nil && (state == "" || strings.HasPrefix(state, "Z")) {
						break
					}
					if time.Now().After(until) {
						t.Fatalf("owned child survived: pid=%d state=%q err=%v", child, state, err)
					}
					time.Sleep(5 * time.Millisecond)
				}
				ops := len(records(t, dir))
				if ops != stage.stop-1 || probes != stage.stop {
					t.Fatalf("pipe failure entered another stage: ops=%d probe=%d", ops, probes)
				}
				after, err := os.ReadFile(e.paths().AgentFile("alice"))
				if err != nil || string(after) != string(original) {
					t.Fatal("pipe failure lost recoverable credentials", err)
				}
				k := isolatedKeychainStore(keychainStore{paths: e.paths()})
				if err := k.checkCooldown(uid); err == nil {
					t.Fatal("nonzero pipe failure omitted identity cooldown")
				}
				host, err := k.hostPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := k.checkMarker(host+".cooldown", uid); err == nil {
					t.Fatal("probe pipe failure omitted host cooldown")
				}
				e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
				if len(records(t, dir)) != ops || probes != stage.stop {
					t.Fatal("cooldown launched another probe or backend operation")
				}
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
			})
		}
	}
}

// stagedBackend fails exactly the stop-th backend operation and passes every
// other one to the file-backed fake. The counter is shared by all copies.
type stagedBackend struct {
	fakeBackend
	stop  int32
	fail  string
	calls *atomic.Int32
}

func (s stagedBackend) step(ctx context.Context) (bool, error) {
	if s.calls.Add(1) != s.stop {
		return false, nil
	}
	switch s.fail {
	case "interaction":
		return true, errKeychainInteraction
	case "timeout":
		<-ctx.Done()
		return true, ctx.Err()
	}
	return true, fmt.Errorf("planted backend failure")
}

func (s stagedBackend) find(ctx context.Context, keychain, service, account string) ([]byte, error) {
	if hit, err := s.step(ctx); hit {
		return nil, err
	}
	return s.fakeBackend.find(ctx, keychain, service, account)
}

func (s stagedBackend) add(ctx context.Context, keychain, service, account string, secret []byte) error {
	if hit, err := s.step(ctx); hit {
		return err
	}
	return s.fakeBackend.add(ctx, keychain, service, account, secret)
}

func (s stagedBackend) update(ctx context.Context, keychain, service, account string, secret []byte) error {
	if hit, err := s.step(ctx); hit {
		return err
	}
	return s.fakeBackend.update(ctx, keychain, service, account, secret)
}

// A failure at any backend operation of a migration, including duplicate
// recovery, ends the migration there: it never turns into absence or a
// duplicate, never reaches the next operation and never scrubs the recoverable
// credential file. Interaction refusals and timeouts record the identity
// cooldown, which then suppresses every backend call; any other error fails
// without one.
func TestKeychainBackendFailureMigrationStages(t *testing.T) {
	for _, stage := range migrationStages {
		for _, fail := range []string{"interaction", "timeout", "error"} {
			t.Run(stage.name+"/"+fail, func(t *testing.T) {
				e := newEnv(t)
				uid := e.agent("alice")
				items := map[string]string{}
				if stage.existing {
					items[uid] = e.tokens["alice"]
				}
				dir := fakeKeychain(e, items, stage.mode)
				original, err := os.ReadFile(e.paths().AgentFile("alice"))
				if err != nil {
					t.Fatal(err)
				}
				calls := new(atomic.Int32)
				e.configure = func(a *App) {
					a.keychain = func(p config.Paths) *keychainStore {
						return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, backend: stagedBackend{fakeBackend{dir}, int32(stage.stop), fail, calls}, timeout: 300 * time.Millisecond})
					}
				}
				fragment := "cooldown recorded"
				if fail == "error" {
					fragment = "keychain operation failed"
				}
				e.as("alice", "creds", "migrate").failsWith(t, ExitError, fragment)
				if got := calls.Load(); got != int32(stage.stop) {
					t.Fatalf("failure entered another operation: %d", got)
				}
				after, err := os.ReadFile(e.paths().AgentFile("alice"))
				if err != nil || string(after) != string(original) {
					t.Fatal("backend failure lost recoverable credentials", err)
				}
				k := isolatedKeychainStore(keychainStore{paths: e.paths()})
				if fail == "error" {
					if err := k.checkCooldown(uid); err != nil {
						t.Fatal("ordinary failure manufactured cooldown", err)
					}
				} else {
					if err := k.checkCooldown(uid); err == nil {
						t.Fatal("interaction or timeout omitted identity cooldown")
					}
					e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
					if calls.Load() != int32(stage.stop) {
						t.Fatal("cooldown reached the keychain backend")
					}
				}
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
			})
		}
	}
}
