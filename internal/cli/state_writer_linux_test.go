//go:build linux

package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A minimal native ELF with Go's inline build-info format, never executed.
// This keeps the diagnostic test offline and independent of the installed Go
// toolchain, a Git checkout, any live process, or historical binary downloads.
func stateWriterELFFixture(t *testing.T, revision string) []byte {
	t.Helper()
	machine := map[string]uint16{"amd64": 62, "arm64": 183, "riscv64": 243, "ppc64le": 21}[runtime.GOARCH]
	if machine == 0 {
		t.Skip("ELF64 fixture is unavailable on this architecture")
	}
	blob := make([]byte, 32)
	copy(blob, "\xff Go buildinf:")
	blob[14], blob[15] = 8, 2
	blob = binary.AppendUvarint(blob, uint64(len("go1.25.12")))
	blob = append(blob, "go1.25.12"...)
	module := strings.Repeat("x", 16) + "path\tgithub.com/relux-works/a2a-swarm/cmd/a2a\nmod\tgithub.com/relux-works/a2a-swarm\t(devel)\t\nbuild\tvcs.revision=" + revision + "\nbuild\tvcs.modified=false\n" + strings.Repeat("x", 16)
	blob = binary.AppendUvarint(blob, uint64(len(module)))
	blob = append(blob, module...)
	const namesAt, infoAt = 320, 352
	names := "\x00.shstrtab\x00.go.buildinfo\x00"
	data := make([]byte, infoAt+len(blob))
	copy(data, "\x7fELF\x02\x01\x01")
	put16 := func(at int, value uint16) { binary.LittleEndian.PutUint16(data[at:], value) }
	put32 := func(at int, value uint32) { binary.LittleEndian.PutUint32(data[at:], value) }
	put64 := func(at int, value uint64) { binary.LittleEndian.PutUint64(data[at:], value) }
	put16(16, 2)
	put16(18, machine)
	put32(20, 1)
	put64(32, 64)
	put64(40, 120)
	put16(52, 64)
	put16(54, 56)
	put16(56, 1)
	put16(58, 64)
	put16(60, 3)
	put16(62, 1)
	put32(64, 1)
	put32(68, 5)
	put64(96, uint64(len(data)))
	put64(104, uint64(len(data)))
	// Section 1 is the name table; section 2 is Go's build-info section.
	put32(184, 1)
	put32(188, 3)
	put64(208, namesAt)
	put64(216, uint64(len(names)))
	put32(248, 11)
	put32(252, 1)
	put64(256, 2)
	put64(264, infoAt)
	put64(272, infoAt)
	put64(280, uint64(len(blob)))
	copy(data[namesAt:], names)
	copy(data[infoAt:], blob)
	return data
}

func TestStateWriterNativeImageAndStatusWarning(t *testing.T) {
	e, _ := world(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := hookExecutableParents(filepath.Join(home, "fixture")); err != nil {
		t.Skip(err)
	}
	dir, err := os.MkdirTemp(home, ".state-writer-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	image := filepath.Join(dir, "libexec", "a2a", "a2a-image")
	if err := os.MkdirAll(filepath.Dir(image), 0700); err != nil {
		t.Fatal(err)
	}
	old := "bb71277ac9c4d49649a8903424d8bafb0e0c779a"
	for _, tt := range []struct {
		name, revision string
		want           bool
	}{
		{"old", old, true},
		{"fixed", "5f2b8439a5e51070335cfd8ea07b27d7faa3af13", false},
		{"unknown", strings.Repeat("f", 40), false},
		{"malformed-revision", "unknown", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(image, stateWriterELFFixture(t, tt.revision), 0700); err != nil {
				t.Fatal(err)
			}
			stamp, err := observeTerminalExecutable(image)
			if err != nil {
				t.Fatal(err)
			}
			// The same file can be inspected as a selected hook image or a
			// consumer image only when its loaded-image identity matches.
			for _, loaded := range []string{"", stamp.Image} {
				if got := oldLibexecStateWriter(image, loaded); got != tt.want {
					t.Fatalf("native classification = %v, want %v", got, tt.want)
				}
			}
			for _, kind := range []string{"own", "stopped", "foreign-host", "foreign-identity", "foreign-root", "wrong-command", "birth-mismatch", "birth-changed", "image-mismatch", "image-changed", "unknown-birth"} {
				b := &config.TerminalBinding{Host: machineLabel(), DaemonPID: 42, DaemonBirth: "own-birth"}
				identity, root, command := "alice", dir, "bind"
				if kind == "stopped" {
					b.Stopped = true
				}
				if kind == "foreign-host" {
					b.Host += "-elsewhere"
				}
				if kind == "foreign-identity" {
					identity = "bob"
				}
				if kind == "foreign-root" {
					root = filepath.Dir(dir)
				}
				if kind == "wrong-command" {
					command = "inbox"
				}
				birthCalls, imageCalls := 0, 0
				inspect := stateWriterProcessInspection{
					birth: func(context.Context, int) (string, error) {
						birthCalls++
						if kind == "unknown-birth" {
							return "", os.ErrPermission
						}
						if kind == "birth-mismatch" || kind == "birth-changed" && birthCalls > 1 {
							return "another-birth", nil
						}
						return b.DaemonBirth, nil
					},
					arguments: func(int) ([]string, []string, error) {
						return []string{image, "--as", identity, command}, []string{"A2A_CONFIG_DIR=" + root}, nil
					},
					loaded: func(_ context.Context, _ int, imageID string) bool { return oldLibexecStateWriter(image, imageID) },
					image: func(context.Context, int) (string, error) {
						imageCalls++
						if kind == "image-mismatch" || kind == "image-changed" && imageCalls > 1 {
							return "another-image", nil
						}
						return stamp.Image, nil
					},
				}
				want := kind == "own" && tt.want
				if got := stateWriterConsumerWarning(context.Background(), "alice", dir, b, inspect); got != want {
					t.Fatalf("consumer %s: warning = %v, want %v", kind, got, want)
				}
			}
			if oldLibexecStateWriter(image, "different-loaded-image") {
				t.Fatal("argv path overrode loaded-image mismatch")
			}
			if err := os.MkdirAll(".codex", 0700); err != nil {
				t.Fatal(err)
			}
			settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []hookCommand{{Type: "command", Command: image + " --as alice hook stop --runtime codex"}}}}}})
			if err := os.WriteFile(DefaultCodexHooks, settings, 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"bind", "status"}, {"bind", "status", "--json"}} {
				r := e.as("alice", args...).ok(t)
				if got := strings.Contains(r.stdout, oldStateWriterWarning); got != tt.want {
					t.Fatalf("status warning = %v, output %s", got, r.stdout)
				}
			}
		})
	}
}

func stateWriterPrivateDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".writer-guards-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := hookExecutableParents(filepath.Join(dir, "fixture")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stateWriterWriteFixture(t *testing.T, path, revision string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stateWriterELFFixture(t, revision), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestStateWriterLayoutsAndCustody(t *testing.T) {
	dir := stateWriterPrivateDir(t)
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"libexec/a2a/legacy-image", true},
		{"libexec/a2a/image.Ab2xY9/a2a", true},
		{"libexec/a2a/image.AAAAAA/a2a", true},
		{"libexec/a2a/image.234567/a2a", true},
		{"libexec/a2a/image.Ab2xY9/wrong", false},
		{"libexec/a2a/image./a2a", false},
		{"libexec/a2a/image.ABCDE/a2a", false},
		{"libexec/a2a/image.ABCDEFG/a2a", false},
		{"libexec/a2a/image.Ab2-Y9/a2a", false},
		{"libexec/a2a/image.Ab2_Y9/a2a", false},
		{"libexec/a2a/image.Ab2.Y9/a2a", false},
		{"libexec/a2a/image.éABCDE/a2a", false},
		{"libexec/other/image.Ab2xY9/a2a", false},
		{"bin/a2a/image.Ab2xY9/a2a", false},
		{"libexec/a2a/extra/image.Ab2xY9/a2a", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			stateWriterWriteFixture(t, path, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
			if got := oldLibexecStateWriter(path, ""); got != tt.want {
				t.Fatalf("layout known-old = %v, want %v", got, tt.want)
			}
		})
	}
	for _, kind := range []string{"valid", "public-image", "no-execute", "setid", "public-parent", "checkout-parent"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(dir, kind, "libexec/a2a/image.Ab2xY9/a2a")
			stateWriterWriteFixture(t, path, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
			switch kind {
			case "public-image":
				if err := os.Chmod(path, 0720); err != nil {
					t.Fatal(err)
				}
			case "no-execute":
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "setid":
				if err := os.Chmod(path, 0700|os.ModeSetuid); err != nil {
					t.Fatal(err)
				}
			case "public-parent":
				if err := os.Chmod(filepath.Dir(path), 0720); err != nil {
					t.Fatal(err)
				}
			case "checkout-parent":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".git"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := oldLibexecStateWriter(path, ""); got != (kind == "valid") {
				t.Fatalf("custody %s known-old = %v", kind, got)
			}
		})
	}
}

func TestStateWriterBinaryReadBound(t *testing.T) {
	dir := stateWriterPrivateDir(t)
	path := filepath.Join(dir, "libexec/a2a/image.Ab2xY9/a2a")
	stateWriterWriteFixture(t, path, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
	for _, size := range []int64{stateWriterImageMax, stateWriterImageMax + 1} {
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
		if got := oldLibexecStateWriter(path, ""); got != (size == stateWriterImageMax) {
			t.Fatalf("size %d known-old = %v", size, got)
		}
	}
}

func TestStateWriterOpenedPathRecheck(t *testing.T) {
	dir := stateWriterPrivateDir(t)
	path := filepath.Join(dir, "libexec/a2a/image.Ab2xY9/a2a")
	for _, kind := range []string{"valid", "replaced-before-parse", "replaced-after-parse"} {
		t.Run(kind, func(t *testing.T) {
			stateWriterWriteFixture(t, path, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			replace := func() {
				replacement := path + ".new"
				stateWriterWriteFixture(t, replacement, "5f2b8439a5e51070335cfd8ea07b27d7faa3af13")
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "replaced-before-parse" {
				replace()
			}
			calls := 0
			got := oldStateWriterImage(f, "", func(stamp terminalExecutable) bool {
				calls++
				if kind == "replaced-after-parse" && calls == 2 {
					replace()
				}
				return stateWriterImageAtPath(path, stamp, false)
			})
			if got != (kind == "valid") {
				t.Fatalf("path recheck %s = %v", kind, got)
			}
		})
	}
}

// A kernel fd link supplies the same loaded-inode semantics as /proc/PID/exe,
// without executing an examined binary. Replacing/unlinking its name must not
// make the new image at argv[0] evidence about this retained old inode.
func TestStateWriterLoadedImageReplacement(t *testing.T) {
	dir := stateWriterPrivateDir(t)
	oldPath := filepath.Join(dir, "libexec/a2a/image.Ab2xY9/a2a")
	newPath := filepath.Join(dir, "libexec/a2a/image.Bb3yZ8/a2a")
	stateWriterWriteFixture(t, oldPath, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
	stateWriterWriteFixture(t, newPath, "5f2b8439a5e51070335cfd8ea07b27d7faa3af13")
	f, err := os.Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	image := executableMetadata(st).Image
	procPath := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	link := filepath.Join(dir, "a2a")
	if err := os.Symlink(newPath, link); err != nil {
		t.Fatal(err)
	}
	inspect := stateWriterProcessInspection{
		birth: func(context.Context, int) (string, error) { return "own-birth", nil },
		arguments: func(int) ([]string, []string, error) {
			return []string{link, "--as", "alice", "bind"}, []string{"A2A_CONFIG_DIR=" + dir}, nil
		},
		image:  func(context.Context, int) (string, error) { return image, nil },
		loaded: func(_ context.Context, _ int, image string) bool { return oldProcLibexecStateWriter(procPath, image) },
	}
	b := &config.TerminalBinding{Host: machineLabel(), DaemonPID: 42, DaemonBirth: "own-birth"}
	for _, kind := range []string{"retargeted-command", "unlinked-loaded-image", "replaced-loaded-name"} {
		if kind == "unlinked-loaded-image" {
			if err := os.Remove(oldPath); err != nil {
				t.Fatal(err)
			}
		}
		if kind == "replaced-loaded-name" {
			stateWriterWriteFixture(t, oldPath, "5f2b8439a5e51070335cfd8ea07b27d7faa3af13")
		}
		if oldLibexecStateWriter(link, "") {
			t.Fatal("replacement hook should be fixed")
		}
		if !stateWriterConsumerWarning(context.Background(), "alice", dir, b, inspect) {
			t.Fatalf("old loaded image lost after %s", kind)
		}
	}
	if oldProcLibexecStateWriter(procPath, "different-vnode") {
		t.Fatal("foreign loaded inode accepted")
	}
	if oldProcLibexecStateWriter(procPath+"-missing", image) {
		t.Fatal("unreadable loaded image accepted")
	}
	fixed, err := observeTerminalExecutable(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if oldProcLibexecStateWriter(procPath, fixed.Image) {
		t.Fatal("new image was proof about old loaded inode")
	}
	inspect.loaded = func(context.Context, int, string) bool { return false }
	if stateWriterConsumerWarning(context.Background(), "alice", dir, b, inspect) {
		t.Fatal("unknown loaded image warned")
	}
}

func TestStateWriterLoadedLiteralDeletedName(t *testing.T) {
	dir := stateWriterPrivateDir(t)
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"libexec/a2a/image.Ab2xY9/a2a (deleted)", false},
		{"libexec/a2a/legacy (deleted)", true},
	} {
		path := filepath.Join(dir, tt.name)
		stateWriterWriteFixture(t, path, "bb71277ac9c4d49649a8903424d8bafb0e0c779a")
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatal(err)
		}
		procPath := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
		got := oldProcLibexecStateWriter(procPath, executableMetadata(st).Image)
		f.Close()
		if got != tt.want {
			t.Fatalf("literal deleted-marker name %s = %v, want %v", tt.name, got, tt.want)
		}
	}
}
