//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		fmt.Println("A2A-MUSE-CTL-INNER-RAN")
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
	t.Run("claude env only", func(t *testing.T) {
		// A Claude-only env (CLAUDE_CODE_SESSION_ID, no Muse/A2A
		// selector) is refused at the selector gate beside the valid
		// controls in this function, even when the named owner is a
		// real muse-bin-* process. Production probes run here: a gate
		// that exempted a Claude env would fall through to ancestry
		// and refuse differently (or reach the manual gate).
		fake := museBindCopyBinary(t, sleepBin, "muse-bin-claude")
		owner := museBindSleeper(t, fake)
		err := museBindAttemptReal(t, map[string]string{"CLAUDE_CODE_SESSION_ID": "claude-1"}, owner)
		if err == nil || !strings.Contains(err.Error(), "requires this Muse session") {
			t.Fatalf("claude env only: err = %v, want selector refusal", err)
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
		cmd := exec.CommandContext(ctx, fake, "-c", `"$0" -test.v -test.run='^TestMuseBindAuthorityProductionProbes$' -test.count=1; exit $?`, self)
		cmd.Env = append(os.Environ(), "A2A_MUSE_BIND_INNER=1")
		cmd.WaitDelay = 10 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("inner control under muse-bin parent failed: %v\n%s", err, out)
		}
		// Exit status alone would also pass if the -test.run filter
		// matched nothing; require the inner marker and PASS line.
		if !strings.Contains(string(out), "A2A-MUSE-CTL-INNER-RAN") || !strings.Contains(string(out), "--- PASS: TestMuseBindAuthorityProductionProbes") {
			t.Fatalf("inner control under muse-bin parent did not execute:\n%s", out)
		}
	})
}

func museBindProcessRow(t *testing.T, pid int) bindingProcess {
	t.Helper()
	rows, err := bindingProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.PID == pid {
			return p
		}
	}
	t.Fatalf("process %d not in process table", pid)
	return bindingProcess{}
}

// The real Muse tool-shell shape: muse-bin-* -> intermediate shell ->
// this binary, with the owner being the muse-bin grandparent. This is
// the passing multi-hop row the zero-hop controls cannot pin: production
// descendant() must walk one hop (parent shell -> grandparent owner)
// and the production image lookup must read the owner's pid, not the
// invocation's parent. The same inner run refuses the intermediate
// shell itself as owner (non-muse image) and a sibling muse-bin-*
// process as owner (not an ancestor).
func TestMuseBindAuthorityToolShellShape(t *testing.T) {
	own := map[string]string{"MUSE_SESSION_ID": "muse-session-1"}
	if os.Getenv("A2A_MUSE_BIND_TOOLSHELL_INNER") == "1" {
		owner, err := strconv.Atoi(os.Getenv("A2A_MUSE_BIND_OWNER"))
		if err != nil || owner <= 1 {
			t.Fatalf("tool-shell premise: bad owner %q: %v", os.Getenv("A2A_MUSE_BIND_OWNER"), err)
		}
		sibling, err := strconv.Atoi(os.Getenv("A2A_MUSE_BIND_SIBLING"))
		if err != nil || sibling <= 1 {
			t.Fatalf("tool-shell premise: bad sibling %q: %v", os.Getenv("A2A_MUSE_BIND_SIBLING"), err)
		}
		if owner == os.Getppid() {
			t.Fatal("tool-shell premise: grandparent owner is the direct parent")
		}
		if parent := museBindProcessRow(t, os.Getppid()); parent.PPID != owner {
			t.Fatalf("tool-shell premise: parent ppid = %d, want grandparent owner %d", parent.PPID, owner)
		}
		if row := museBindProcessRow(t, owner); !isMuseOwnerName(row.Name) {
			t.Fatalf("tool-shell premise: grandparent owner image = %q, want muse-bin-*", row.Name)
		}
		if row := museBindProcessRow(t, sibling); row.PPID != os.Getppid() || !isMuseOwnerName(row.Name) {
			t.Fatalf("tool-shell premise: sibling row = %+v, want a muse-bin-* child of the intermediate shell (ppid %d)", row, os.Getppid())
		}
		// (a) Grandparent muse-bin owner: multi-hop ancestry and the
		// owner-pid image lookup must pass, stopping at the manual gate.
		if err := museBindAttemptReal(t, own, owner); err == nil || !strings.Contains(err.Error(), "manual gate") {
			t.Fatalf("grandparent muse-bin owner: err = %v, want manual gate", err)
		}
		// Intermediate shell as owner: ancestry passes (it is the
		// parent) but its non-muse image must refuse.
		if err := museBindAttemptReal(t, own, os.Getppid()); err == nil || !strings.Contains(err.Error(), "not a muse process") {
			t.Fatalf("intermediate shell owner: err = %v, want image refusal", err)
		}
		// (b) Sibling muse-bin owner: a muse-bin-* process sharing
		// the intermediate shell as parent is not an ancestor, so
		// ancestry must refuse even though its image is muse-bin-*.
		if err := museBindAttemptReal(t, own, sibling); err == nil || !strings.Contains(err.Error(), "not this invocation's muse runtime") {
			t.Fatalf("sibling muse-bin owner: err = %v, want ancestry refusal", err)
		}
		fmt.Println("A2A-MUSE-TOOLSHELL-INNER-RAN")
		return
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	museSh := museBindCopyBinary(t, "/bin/sh", "muse-bin-hop")
	toolSh := museBindCopyBinary(t, "/bin/sh", "toolsh")
	// The sibling stand-in is a dash copy, not a sleep copy: on hosts
	// where sleep is a multicall binary (uutils coreutils), a copy
	// invoked under a muse-bin-* name dies on argv0 dispatch. It runs a
	// loop, not a lone "sleep N": shells that exec the last simple
	// command of -c in place (bash, some dash builds) would turn the
	// sibling's image into "sleep" before the inner run reads it. Its
	// stdio is /dev/null so that a "sleep 1" child left behind by
	// "kill $SIB" cannot hold the CombinedOutput pipe open. The loop
	// ends by itself after 90 s (longer than the 1-minute context), so
	// a tool shell that dies before "kill $SIB" leaves no endless orphan.
	siblingBin := museBindCopyBinary(t, "/bin/sh", "muse-bin-sib")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	outer := `A2A_MUSE_BIND_OWNER=$$ "$1" -c '"$1" -c "i=0; while [ \$i -lt 90 ]; do sleep 1; i=\$((i+1)); done" </dev/null >/dev/null 2>&1 & SIB=$!; A2A_MUSE_BIND_SIBLING=$SIB "$0" -test.v -test.run="^TestMuseBindAuthorityToolShellShape$" -test.count=1; st=$?; kill $SIB 2>/dev/null; wait $SIB 2>/dev/null; exit $st' "$0" "$2"; exit $?`
	cmd := exec.CommandContext(ctx, museSh, "-c", outer, self, toolSh, siblingBin)
	cmd.Env = append(os.Environ(), "A2A_MUSE_BIND_TOOLSHELL_INNER=1")
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tool-shell shape failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "A2A-MUSE-TOOLSHELL-INNER-RAN") || !strings.Contains(string(out), "--- PASS: TestMuseBindAuthorityToolShellShape") {
		t.Fatalf("tool-shell inner run did not execute:\n%s", out)
	}
}

// With a muse-bin parent, a non-muse grandparent as owner must refuse
// on the image check. Ancestry passes through the muse-bin parent (one
// hop), so only an image lookup that reads the owner's pid (not the
// invocation's parent) refuses; a parent-reading lookup would see the
// muse-bin parent and admit the non-muse owner to the manual gate.
func TestMuseBindAuthorityNonMuseGrandparentOwner(t *testing.T) {
	own := map[string]string{"MUSE_SESSION_ID": "muse-session-1"}
	if os.Getenv("A2A_MUSE_BIND_GP_INNER") == "1" {
		owner, err := strconv.Atoi(os.Getenv("A2A_MUSE_BIND_OUTER"))
		if err != nil || owner <= 1 {
			t.Fatalf("grandparent premise: bad owner %q: %v", os.Getenv("A2A_MUSE_BIND_OUTER"), err)
		}
		if owner == os.Getppid() {
			t.Fatal("grandparent premise: owner is the direct parent")
		}
		if parent := museBindProcessRow(t, os.Getppid()); parent.PPID != owner || !isMuseOwnerName(parent.Name) {
			t.Fatalf("grandparent premise: parent row = %+v, want muse-bin-* child of owner %d", parent, owner)
		}
		if row := museBindProcessRow(t, owner); isMuseOwnerName(row.Name) {
			t.Fatalf("grandparent premise: owner image = %q, want non-muse", row.Name)
		}
		if err := museBindAttemptReal(t, own, owner); err == nil || !strings.Contains(err.Error(), "not a muse process") {
			t.Fatalf("non-muse grandparent owner: err = %v, want image refusal", err)
		}
		fmt.Println("A2A-MUSE-GP-INNER-RAN")
		return
	}
	museSh := museBindCopyBinary(t, "/bin/sh", "muse-bin-gp")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, museSh, "-c", `"$0" -test.v -test.run='^TestMuseBindAuthorityNonMuseGrandparentOwner$' -test.count=1; exit $?`, self)
	cmd.Env = append(os.Environ(), "A2A_MUSE_BIND_GP_INNER=1", "A2A_MUSE_BIND_OUTER="+strconv.Itoa(os.Getpid()))
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("non-muse grandparent inner failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "A2A-MUSE-GP-INNER-RAN") || !strings.Contains(string(out), "--- PASS: TestMuseBindAuthorityNonMuseGrandparentOwner") {
		t.Fatalf("non-muse grandparent inner run did not execute:\n%s", out)
	}
}
