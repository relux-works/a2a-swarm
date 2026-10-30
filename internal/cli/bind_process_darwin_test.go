package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The darwin lane runs only Darwin-only test files, so these witnesses keep the
// kern.procargs2 and lsof backend of the binding guard under test on macOS.

// Proves the guard refuses a separately loaded old image of the same identity
// through the Darwin backend, with a foreign identity and a foreign config root
// as positive controls.
func TestBindingProcessDarwinImageWitness(t *testing.T) {
	bindingImageWitness(t)
}

// startBindingChild starts exe as the witness child with exactly env and the
// given extra arguments, and waits until it is running.
func startBindingChild(t *testing.T, exe string, env []string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(exe, append([]string{"-test.run=^TestBindingProcessWitnessChild$"}, extra...)...)
	cmd.Env = env
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := make([]byte, 1)
	if _, err = io.ReadFull(out, ready); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// withoutA2A drops every A2A_* entry, so the child carries only the selectors
// a test sets.
func withoutA2A(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "A2A_") {
			out = append(out, kv)
		}
	}
	return out
}

// reapedPID returns the PID of a process that has exited and been waited for.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// Proves bindingArguments returns a live process's exact argv, including an
// empty argument and one with spaces, and keeps only the A2A_AGENT and
// A2A_CONFIG_DIR entries of its environment, never another variable. A
// process that has exited is an error, not an empty result.
func TestBindingArgumentsDarwin(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := append(withoutA2A(os.Environ()), "A2A_BIND_WITNESS=1", "SYNTHETIC_SECRET=must-not-survive", "A2A_AGENT=synthetic-args", "A2A_CONFIG_DIR="+dir)
	child := startBindingChild(t, self, env, "synthetic arg with spaces", "")
	args, selectors, err := bindingArguments(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, child.Args) {
		t.Fatalf("argv = %q, want %q", args, child.Args)
	}
	if want := []string{"A2A_AGENT=synthetic-args", "A2A_CONFIG_DIR=" + dir}; !reflect.DeepEqual(selectors, want) {
		t.Fatalf("environment selectors = %q, want %q", selectors, want)
	}
	if _, _, err := bindingArguments(reapedPID(t)); err == nil {
		t.Fatal("arguments of an exited process were read")
	}
}

// Proves bindingImage identifies the running executable image: a child of the
// same binary has this process's image, a byte-identical copy at another path
// does not, and a process that has exited is an error.
func TestBindingImageDarwin(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "a2a-copy-fixture")
	if err = os.WriteFile(copied, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(withoutA2A(os.Environ()), "A2A_BIND_WITNESS=1")
	same := startBindingChild(t, self, env)
	other := startBindingChild(t, copied, env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	own, err := bindingImage(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := bindingImage(ctx, same.Process.Pid); err != nil || got != own {
		t.Fatalf("same binary: image %q, %v; want %q", got, err, own)
	}
	if got, err := bindingImage(ctx, other.Process.Pid); err != nil || got == own {
		t.Fatalf("copied binary: image %q, %v; want an image other than %q", got, err, own)
	}
	if _, err := bindingImage(ctx, reapedPID(t)); err == nil {
		t.Fatal("image of an exited process was read")
	}
}

// Proves parseBindingImage takes the device and inode of the first txt entry
// only: fields of other descriptors before or after it never count, and an
// incomplete first txt entry or no txt entry at all is an error.
func TestParseBindingImage(t *testing.T) {
	for name, tc := range map[string]struct {
		raw, want string
	}{
		"single txt":                  {"p42\nftxt\nD0x1000010\ni12345\nn/opt/a2a\n", "0x1000010:12345"},
		"no trailing newline":         {"p42\nftxt\nD0x5\ni6", "0x5:6"},
		"cwd before txt is ignored":   {"p42\nfcwd\nD0x2\ni2\nn/\nftxt\nD0x1000010\ni777\nn/opt/a2a\n", "0x1000010:777"},
		"first of several txt":        {"p42\nftxt\nD0x1000010\ni777\nn/opt/a2a\nftxt\nD0x3\ni3\nn/usr/lib/dyld\n", "0x1000010:777"},
		"txt then another descriptor": {"p42\nftxt\nD0x5\ni6\nfmem\nD0x7\ni8\n", "0x5:6"},
		"first txt without device":    {"p42\nftxt\ni777\nn/opt/a2a\nftxt\nD0x3\ni3\n", ""},
		"txt without inode":           {"p42\nftxt\nD0x1\nn/opt/a2a\n", ""},
		"only another descriptor":     {"p42\nfcwd\nD0x2\ni2\n", ""},
		"fields only before txt":      {"p42\nfcwd\nD0x9\ni9\nftxt\nn/opt/a2a\n", ""},
		"empty output":                {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseBindingImage(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
