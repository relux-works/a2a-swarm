//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestOperationOwnerStartNeedsNoPSStartTime proves the owner of a cursor or
// status reservation is identified without the `ps` start time, which exists
// on macOS only: with that reader declared unsupported, as it is on Linux, and
// with no helper on PATH, this process still gets a start token, and it is the
// native one.
//
// Regression witness: the owner used to be named by the `ps` start time, so on
// Linux no reservation could be made and every bind of a marked identity
// failed. Control: under the same conditions the `ps` reader itself refuses.
func TestOperationOwnerStartNeedsNoPSStartTime(t *testing.T) {
	want, err := processStart(os.Getpid())
	if err != nil {
		t.Fatalf("native start token of this process: %v", err)
	}
	a := &App{leaseGOOS: "linux"}
	t.Setenv("PATH", t.TempDir())
	if _, err := a.pidBirth(context.Background(), os.Getpid()); !errors.Is(err, errPIDBirthUnsupported) {
		t.Fatalf("control: ps start time = %v, want the unsupported-platform refusal", err)
	}
	got, err := a.operationOwnerStart(context.Background(), os.Getpid())
	if err != nil || got == "" || got != want {
		t.Fatalf("operation owner start = %q, %v; want the native token %q", got, err, want)
	}
}

// TestOperationOwnerDeadByNativeStart proves when a recorded reservation owner
// counts as dead: never for another host, not while its PID runs with the
// recorded start token, yes when that PID runs with another start token (a
// reused PID), and yes once the process is gone.
func TestOperationOwnerDeadByNativeStart(t *testing.T) {
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			child.Process.Kill()
			child.Wait()
		}
	})
	start, err := processStart(child.Process.Pid)
	if err != nil {
		t.Fatalf("native start token of the child: %v", err)
	}
	a := &App{leaseGOOS: "linux"}
	dead := a.bindOperationOwnerDead(context.Background())
	owner := config.BindOperationOwner{Holder: "holder", Host: machineLabel(), PID: child.Process.Pid, Birth: start}
	if dead(owner) {
		t.Fatal("a running owner with its recorded start token was called dead")
	}
	foreign := owner
	foreign.Host, foreign.Birth = "another-host", "other"
	if dead(foreign) {
		t.Fatal("an owner on another host was called dead from this host")
	}
	reused := owner
	reused.Birth = start + "0"
	if !dead(reused) {
		t.Fatal("a PID running with another start token was not recognised as reused")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	exited = true
	if !dead(owner) {
		t.Fatal("an owner whose process is gone was not called dead")
	}
}
