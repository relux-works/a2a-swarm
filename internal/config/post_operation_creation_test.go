//go:build darwin || linux

package config

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// Claim: Initial exclusive creation and one EEXIST fallback use exact flags; other errors refuse without creation; injected syscall outcomes only.
func TestPostJournalExclusiveCreationFlags(t *testing.T) {
	for _, tc := range []struct {
		name             string
		create, existing bool
		injected         []error
		want             []int
		refused          bool
	}{
		{name: "new", create: true, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL}},
		{name: "existing", create: true, existing: true, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL, os.O_RDWR}},
		{name: "later", existing: true, want: []int{os.O_RDWR}},
		{name: "missing_later", want: []int{os.O_RDWR}, refused: true},
		{name: "initial_enoent_no_retry", create: true, injected: []error{os.ErrNotExist}, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL}, refused: true},
		{name: "eexist_then_deleted_no_retry", create: true, injected: []error{os.ErrExist, os.ErrNotExist}, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL, os.O_RDWR}, refused: true},
		{name: "permission_no_retry", create: true, injected: []error{os.ErrPermission}, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL}, refused: true},
		{name: "eexist_then_io_no_retry", create: true, injected: []error{os.ErrExist, unix.EIO}, want: []int{os.O_RDWR | os.O_CREATE | os.O_EXCL, os.O_RDWR}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, j := operationFixture(t)
			if !tc.existing {
				if err := j.root.Remove(".lock"); err != nil {
					t.Fatal(err)
				}
			}
			var flags []int
			opener := func(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
				if name != ".lock" {
					t.Fatalf("name=%q", name)
				}
				flags = append(flags, flag)
				if flag&os.O_CREATE != 0 && perm != 0600 {
					t.Fatalf("creation mode=%o", perm)
				}
				if len(flags) <= len(tc.injected) {
					return nil, tc.injected[len(flags)-1]
				}
				return openPostOperationFile(root, name, flag, perm)
			}
			f, err := openPostOperationLock(j.root, tc.create, opener)
			if (err != nil) != tc.refused {
				t.Fatalf("refused=%v err=%v", tc.refused, err)
			}
			if f != nil {
				defer f.Close()
				fdflags, e := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
				if e != nil || fdflags&unix.FD_CLOEXEC == 0 {
					t.Fatal("CLOEXEC missing", e)
				}
				// os.File.Fd switches descriptors back to blocking mode. Observe the
				// open syscall flags separately through SyscallConn, without calling Fd.
			}
			if !reflect.DeepEqual(flags, tc.want) {
				t.Fatalf("flags=%v want=%v", flags, tc.want)
			}
			if tc.refused {
				if _, e := j.root.Lstat(".lock"); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("refusal created lock", e)
				}
			}
		})
	}
}

// Linux simulation of the narrowly reported Darwin failure: nonexclusive
// O_CREAT on an existing name can return ENOENT. No claim of native Darwin
// qualification follows from this injected syscall outcome.
func TestPostJournalDarwinENOENTSimulation(t *testing.T) {
	_, j := operationFixture(t)
	calls := 0
	f, err := openPostOperationLock(j.root, true, func(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
		calls++
		if flags&os.O_CREATE != 0 && flags&os.O_EXCL == 0 {
			return nil, os.ErrNotExist
		}
		return openPostOperationFile(root, name, flags, perm)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if calls != 2 {
		t.Fatalf("calls=%d want exclusive EEXIST then existing open", calls)
	}
}

// Claim: Journal opens retain NONBLOCK/CLOEXEC in the kernel descriptor; local Unix inspection, not exec-inheritance testing.
func TestPostJournalOpenFileDescriptorFlags(t *testing.T) {
	_, j := operationFixture(t)
	f, err := openPostOperationFile(j.root, ".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Control(func(fd uintptr) {
		flags, e := unix.FcntlInt(fd, unix.F_GETFL, 0)
		if e != nil || flags&unix.O_NONBLOCK == 0 {
			t.Error("NONBLOCK missing", e)
		}
		flags, e = unix.FcntlInt(fd, unix.F_GETFD, 0)
		if e != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Error("CLOEXEC missing", e)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// Claim: Status locks use O_RDWR and never recreate a missing lock; local descriptor/absence checks only.
func TestPostJournalStatusLockReadWriteNoncreating(t *testing.T) {
	_, j := operationFixture(t)
	calls := 0
	j.lock = func(f *os.File) (func(), error) {
		calls++
		conn, err := f.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.Control(func(fd uintptr) {
			flags, e := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if e != nil || flags&unix.O_ACCMODE != unix.O_RDWR {
				t.Error("status lock must use O_RDWR", e)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return lockPostOperation(f)
	}
	if _, ok, err := j.Read("absent"); err != nil || ok || calls != 1 {
		t.Fatalf("status: ok=%v calls=%d err=%v", ok, calls, err)
	}
	if err := j.root.Remove(".lock"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Read("absent"); err == nil {
		t.Fatal("missing status lock accepted")
	}
	if calls != 1 {
		t.Fatal("missing status lock reached flock")
	}
	if _, err := j.root.Lstat(".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status recreated missing lock", err)
	}
}
