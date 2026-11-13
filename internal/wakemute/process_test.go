//go:build linux || darwin

package wakemute_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// The only subprocesses are copies of this test binary using parent-created
// temporary roots. They exit on their own; no signals or process-control tool
// is used. Child exit before stdout capture is a real caller-death witness.
func TestScopeMuteProcessCrashAndSerialization(t *testing.T) {
	if role := os.Getenv("A2A_SCOPE_FIXTURE_ROLE"); role != "" {
		root := os.Getenv("A2A_SCOPE_FIXTURE_ROOT")
		if !filepath.IsAbs(root) || (!strings.Contains(root, "TestScopeMuteProcessCrashAndSerialization") || filepath.Base(root) != "root") {
			t.Fatal("invalid private fixture root")
		}
		scope := fixture.Scope(root)
		switch role {
		case "crash":
			op := fixture.Confirm(t, scope, "crash-before-output", "mute")
			if _, err := wakemute.Mute(fixture.Context(t), op); err != nil {
				t.Fatal(err)
			}
			os.Exit(73) // Exit only this synthetic caller after durable commit.
		case "holder":
			err := wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error {
				if e := os.WriteFile(filepath.Join(root, "fixture-entered"), []byte("entered"), 0600); e != nil {
					return e
				}
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if _, e := os.Stat(filepath.Join(root, "fixture-release")); e == nil {
						return nil
					}
					time.Sleep(10 * time.Millisecond)
				}
				return errors.New("fixture release budget exhausted")
			})
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("unknown fixture role")
		}
		return
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	child := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestScopeMuteProcessCrashAndSerialization$", "-test.timeout=5s")
		cmd.Env = append(os.Environ(), "A2A_SCOPE_FIXTURE_ROLE="+role, "A2A_SCOPE_FIXTURE_ROOT="+root)
		return cmd
	}
	crash := child("crash")
	data, err := crash.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 73 {
		t.Fatalf("caller death not witnessed: %v %s", err, data)
	}
	status, err := wakemute.Status(fixture.Context(t), scope)
	if err != nil || status.Status != "muted" || len(status.Roots[0].Operations) != 1 || status.Roots[0].Operations[0].Run != "crash-before-output" {
		t.Fatal("dead caller stranded invisible mute")
	}
	fixture.Unmute(t, status.Roots[0].Operations[0])
	approved := fixture.Confirm(t, scope, "multiprocess", "mute")
	holder := child("holder")
	var log bytes.Buffer
	holder.Stdout = &log
	holder.Stderr = &log
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture PID %d (test binary; parent-owned temporary root)", holder.Process.Pid)
	start := time.Now()
	for {
		if _, e := os.Stat(filepath.Join(root, "fixture-entered")); e == nil {
			break
		}
		if time.Since(start) > time.Second {
			t.Fatal("child did not enter admission")
		}
		time.Sleep(10 * time.Millisecond)
	}
	type reply struct {
		result wakemute.Result
		err    error
	}
	ack := make(chan reply, 1)
	go func() { out, e := wakemute.Mute(fixture.Context(t), approved); ack <- reply{out, e} }()
	select {
	case <-ack:
		t.Fatal("cross-process directory lock failed")
	case <-time.After(30 * time.Millisecond):
	}
	if err = os.WriteFile(filepath.Join(root, "fixture-release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = holder.Wait(); err != nil {
		t.Fatalf("fixture child: %v %s", err, log.String())
	}
	result := <-ack
	if result.err != nil {
		t.Fatal(result.err)
	}
	fixture.Unmute(t, *result.result.Operation)
	os.Remove(filepath.Join(root, "fixture-entered"))
	os.Remove(filepath.Join(root, "fixture-release"))
	t.Log("fixture exit receipts: crash=73, holder=0; both waited, no child retained")
}
