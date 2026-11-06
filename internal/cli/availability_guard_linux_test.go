//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Observe actual kernel file events, including a write followed by restoration
// that a final byte snapshot would miss. Fixtures contain synthetic data only.
func availabilityIOGuard(t *testing.T, root string) func() []string {
	t.Helper()
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	watches := map[int]string{}
	for _, dir := range dirs {
		wd, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_OPEN|syscall.IN_ACCESS|syscall.IN_MODIFY|syscall.IN_ATTRIB|syscall.IN_CREATE|syscall.IN_DELETE|syscall.IN_MOVED_FROM|syscall.IN_MOVED_TO|syscall.IN_CLOSE_WRITE|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, dir)
		watches[wd] = rel
	}
	return func() []string {
		var violations []string
		buf := make([]byte, 65536)
		for {
			n, err := syscall.Read(fd, buf)
			if errors.Is(err, syscall.EAGAIN) {
				break
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			for at := 0; at+16 <= n; {
				wd := int(int32(binary.NativeEndian.Uint32(buf[at:])))
				mask := binary.NativeEndian.Uint32(buf[at+4:])
				length := int(binary.NativeEndian.Uint32(buf[at+12:]))
				if at+16+length > n {
					t.Fatal("truncated kernel event")
				}
				name := strings.TrimRight(string(buf[at+16:at+16+length]), "\x00")
				path := filepath.Join(watches[wd], name)
				if mask&syscall.IN_Q_OVERFLOW != 0 {
					t.Fatal("file guard overflow")
				}
				if mask&(syscall.IN_MODIFY|syscall.IN_ATTRIB|syscall.IN_CREATE|syscall.IN_DELETE|syscall.IN_MOVED_FROM|syscall.IN_MOVED_TO|syscall.IN_CLOSE_WRITE|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF) != 0 {
					violations = append(violations, "STATE_WRITE_GUARD:"+path)
				}
				if mask&(syscall.IN_OPEN|syscall.IN_ACCESS) != 0 && path != "matrix.env" && path != "connections.json" {
					violations = append(violations, "SECRET_FILE_GUARD:"+path)
				}
				at += 16 + length
			}
		}
		return violations
	}
}

func TestAvailabilityKernelGuards(t *testing.T) {
	for _, mode := range []string{"available", "unsupported", "invalid-input"} {
		t.Run(mode, func(t *testing.T) {
			f := newAvailabilityFixture(t, true)
			if mode == "unsupported" {
				f.status = 404
				f.body = `{"errcode":"M_NOT_FOUND"}`
			}
			input := "alice"
			if mode == "invalid-input" {
				input = "@alice:other.invalid"
			}
			var out, errout bytes.Buffer
			a := f.app(&out, &errout)
			collect := availabilityIOGuard(t, f.root)
			code := a.Run(context.Background(), []string{"availability", input, "--json"})
			if violations := collect(); len(violations) != 0 {
				t.Fatalf("anonymous command accessed private state: %v", violations)
			}
			if f.store.calls != 0 {
				t.Fatal("SECRET_STORE_GUARD")
			}
			if (code == 0) != (mode == "available") {
				t.Fatalf("code=%d", code)
			}
		})
	}
}

func TestAvailabilityKernelGuardCausalControls(t *testing.T) {
	for _, fault := range []string{"secret-read", "state-write-restore"} {
		t.Run(fault, func(t *testing.T) {
			f := newAvailabilityFixture(t, true)
			path := filepath.Join(f.root, "state", "caller.json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			collect := availabilityIOGuard(t, f.root)
			if fault == "secret-read" {
				_, err = os.ReadFile(filepath.Join(f.root, "agents", "caller.json"))
			} else {
				err = os.WriteFile(path, []byte("synthetic-planted-state"), 0600)
				if err == nil {
					err = os.WriteFile(path, original, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			violations := strings.Join(collect(), "\n")
			want := "SECRET_FILE_GUARD:agents/caller.json"
			if fault == "state-write-restore" {
				want = "STATE_WRITE_GUARD:state/caller.json"
			}
			if !strings.Contains(violations, want) {
				t.Fatalf("guard missed bounded violation: %s", fault)
			}
		})
	}
}
