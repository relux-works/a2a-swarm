//go:build linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// museBindAttemptReal runs one manual muse bind with NO authority seams:
// museBindAncestor and museOwnerName stay nil, so the production
// descendant() walk and the production process-table image lookup run
// against real processes.
func museBindAttemptReal(t *testing.T, env map[string]string, owner int) error {
	t.Helper()
	e, a := museBindSetup(t, env)
	return museBindRun(t, e, a, owner)
}

func museBindSleeper(t *testing.T, path string) int {
	t.Helper()
	cmd := exec.Command(path, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd.Process.Pid
}

func museBindCopyBinary(t *testing.T, src, name string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, b, 0o700); err != nil {
		t.Fatal(err)
	}
	return dst
}

// Production-probe companion to TestMuseBindAuthority: the seam rows there
// cannot catch a weakened production guard, so these rows leave both
// seams nil and use real processes. An unrelated muse-bin-* owner must
// fail ancestry, a real non-muse ancestor must fail the image check, and
// the same bind under a real muse-bin-* parent must pass authority.
func TestMuseBindAuthorityProductionProbes(t *testing.T) {
	own := map[string]string{"MUSE_SESSION_ID": "muse-session-1"}
	// Inner control run: this test binary was re-run by the outer control
	// subtest under a parent shell whose image is muse-bin-*. The owner is
	// that real parent, so production ancestry and image checks must pass
	// and the bind must stop at the manual gate.
	if os.Getenv("A2A_MUSE_BIND_INNER") == "1" {
		err := museBindAttemptReal(t, own, os.Getppid())
		if err == nil || !strings.Contains(err.Error(), "manual gate") {
			t.Fatalf("bind under a real muse-bin parent did not pass authority: err = %v", err)
		}
		return
	}
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep unavailable")
	}
	t.Run("unrelated muse-bin owner", func(t *testing.T) {
		// A real own-UID child whose image is muse-bin-* (a copy of sleep)
		// but which is not an ancestor of this invocation: another lane's
		// runtime must not be bindable from here.
		fake := museBindCopyBinary(t, sleepBin, "muse-bin-other")
		owner := museBindSleeper(t, fake)
		err := museBindAttemptReal(t, own, owner)
		if err == nil || !strings.Contains(err.Error(), "not this invocation's muse runtime") {
			t.Fatalf("unrelated muse-bin owner: err = %v, want ancestry refusal", err)
		}
	})
	t.Run("non-muse ancestor", func(t *testing.T) {
		// The real parent is an ancestor by construction, so ancestry
		// passes and the production image lookup decides. If this test is
		// itself run directly under a muse process the premise is void.
		rows, err := bindingProcesses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			if p.PID == os.Getppid() && isMuseOwnerName(p.Name) {
				t.Skip("test parent is itself a muse process")
			}
		}
		err = museBindAttemptReal(t, own, os.Getppid())
		if err == nil || !strings.Contains(err.Error(), "not a muse process") {
			t.Fatalf("non-muse ancestor: err = %v, want image refusal", err)
		}
	})
	t.Run("control under muse-bin parent", func(t *testing.T) {
		// Re-run this test binary under a parent shell whose image is
		// muse-bin-* (a copy of /bin/sh); the inner run above performs the
		// bind with that shell as owner and must reach the manual gate.
		fake := museBindCopyBinary(t, "/bin/sh", "muse-bin-ctl")
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, fake, "-c", `"$0" -test.run='^TestMuseBindAuthorityProductionProbes$' -test.count=1; exit $?`, self)
		cmd.Env = append(os.Environ(), "A2A_MUSE_BIND_INNER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inner control under muse-bin parent failed: %v\n%s", err, out)
		}
	})
}
