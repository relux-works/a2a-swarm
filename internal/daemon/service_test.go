//go:build darwin || linux

package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// W-RESTART/C-ORPHAN: only an owned 0666 orphaned socket is reclaimed with the
// serve lock held. Live sockets, files and symlinks are retained. Every path is
// inside t.TempDir; no real service manager or privileged run path is involved.
func TestServiceOrphanSocketRecovery(t *testing.T) {
	for _, kind := range []string{"orphan", "live", "file", "symlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			dir := filepath.Join(root, "run")
			if os.Mkdir(dir, 0711) != nil || os.Chmod(dir, 0711) != nil {
				t.Fatal("mkdir")
			}
			t.Chdir(dir)
			path := "s"
			switch kind {
			case "file":
				os.WriteFile(path, []byte("control"), 0600)
			case "symlink":
				os.Symlink(root, path)
			default:
				l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if e != nil {
					t.Fatal(e)
				}
				l.SetUnlinkOnClose(false)
				defer l.Close()
				if os.Chmod(path, 0666) != nil {
					t.Fatal("chmod")
				}
				if kind != "live" {
					l.Close()
				}
				if kind == "mode" {
					os.Chmod(path, 0600)
				}
			}
			before, _ := os.Lstat(path)
			held, e := openDirectoryMode(dir, false, 0711)
			if e != nil {
				t.Fatal(e)
			}
			defer held.Close()
			lock, e := privateFile(held, ".daemon.lock", unix.O_RDWR|unix.O_CREAT)
			if e != nil {
				t.Fatal(e)
			}
			defer lock.Close()
			if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
				t.Fatal("lock")
			}
			var st unix.Stat_t
			if unix.Fstatat(int(held.Fd()), "s", &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
				t.Fatal("stat")
			}
			e = reclaimServiceSocket(held, path, &st)
			if kind == "orphan" {
				if e != nil {
					t.Fatal(e)
				}
				if _, e = os.Lstat(path); !os.IsNotExist(e) {
					t.Fatal("orphan retained")
				}
			} else {
				if e == nil {
					t.Fatal("unsafe reclaim")
				}
				after, _ := os.Lstat(path)
				if !os.SameFile(before, after) {
					t.Fatal("refused object changed")
				}
			}
		})
	}
}

// W-RUNDIR-START/C-NO-ROOT: the service seam refuses an absent boot directory
// before heartbeat readiness and does not create it as the daemon user.
func TestServiceRequiresPreparedDirectory(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(root, "home")
	if _, e = Register(home, "station", "identity", uint32(os.Geteuid()), false); e != nil {
		t.Fatal(e)
	}
	run := filepath.Join(root, "missing")
	ready := false
	s := &server{home: home, log: io.Discard, requireRunDir: true, serviceReady: func() error { ready = true; return nil }, runDirectory: func(path string, create bool) (*os.File, error) {
		if create || path != filepath.Dir(DefaultSocket()) {
			t.Fatal("wrong run-dir contract")
		}
		return openDirectoryMode(run, false, 0711)
	}}
	e = s.serve(context.Background(), DefaultSocket(), nil)
	if e == nil || ready {
		t.Fatal("unprepared service started")
	}
	if _, e = os.Lstat(run); !os.IsNotExist(e) {
		t.Fatal("daemon recreated root-owned boot directory")
	}
}
