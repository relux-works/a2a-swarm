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
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// These public-entry regressions retain the CR1 reviewer's early-leader
// witnesses, extended to both runners and both pipe variants. No real Keychain
// or status executable is invoked. The child records membership BEFORE cleanup.
func TestKeychainEarlyLeaderExit(t *testing.T) {
	for _, stage := range []string{"security", "probe"} {
		for _, pipes := range []string{"inherited", "closed"} {
			t.Run(stage+"/"+pipes, func(t *testing.T) {
				testKeychainProcessLifetime(t, stage, pipes, false)
			})
		}
	}
}

func TestKeychainProcessCancellation(t *testing.T) {
	for _, stage := range []string{"security", "probe"} {
		t.Run(stage, func(t *testing.T) {
			testKeychainProcessLifetime(t, stage, "inherited", true)
		})
	}
}

func TestKeychainProcessStartFailure(t *testing.T) {
	for _, stage := range []string{"security", "probe"} {
		for _, mode := range []string{"missing", "nonexec"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				e := newEnv(t)
				uid := e.agent("alice")
				dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
				fake := probeFixture(t, mode, dir)
				e.configure = func(a *App) {
					a.keychain = func(p config.Paths) *keychainStore {
						k := &keychainStore{paths: p, command: securityCommand(dir), probeCommand: unlockedProbe}
						if stage == "security" {
							k.command = fake
						} else {
							k.probeCommand = fake
						}
						return k
					}
				}
				fragment := "operation failed"
				if stage == "probe" {
					fragment = "preflight failed"
				}
				e.as("alice", "whoami").failsWith(t, ExitError, fragment)
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
				if len(records(t, dir)) != 0 {
					t.Fatal("startup failure invoked security")
				}
			})
		}
	}
}

// Nearby success/missing/duplicate controls are also retained in
// TestKeychainCredentialSelection and TestKeychainRegistrationAndUpsert.

func testKeychainProcessLifetime(t *testing.T, stage, pipes string, cancelOnReady bool) {
	t.Helper()
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	path := filepath.Join(dir, "early-leader")
	witness := filepath.Join(dir, "owned-group")
	value := e.tokens["alice"] + "\n"
	if stage == "probe" {
		value = "unlocked\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	redirect := ""
	if pipes == "closed" {
		redirect = " </dev/null >/dev/null 2>&1"
	}
	terminal := "exit 0"
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
			k := &keychainStore{paths: p, command: securityCommand(dir), probeCommand: unlockedProbe}
			fake := func(ctx context.Context, args ...string) *exec.Cmd { return exec.CommandContext(ctx, path) }
			if stage == "security" {
				k.command = fake
			} else {
				k.probeCommand = fake
			}
			return k
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
	t.Logf("stage=%s pipes=%s cancellation=%t leader=%s child=%d PGID=%s App.Run=%d elapsed=%s", stage, pipes, cancelOnReady, fields[0], pid, fields[2], r.code, time.Since(start))
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
	refused := pipes == "inherited" || cancelOnReady
	if refused {
		fragment := "cooldown recorded"
		if stage == "probe" {
			fragment = "preflight failed"
		}
		r.failsWith(t, ExitError, fragment)
		tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
		tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
		k := &keychainStore{paths: e.paths()}
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
			t.Fatal("cooldown admitted another probe/security/Matrix call", err)
		}
	} else {
		r.ok(t) // Successful leader remains successful after child cleanup.
		tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
		if err := (&keychainStore{paths: e.paths()}).checkCooldown(uid); err != nil {
			t.Fatal("successful cleanup triggered cooldown", err)
		}
	}
}
