//go:build darwin || linux

package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// W-SERVE-READY/M-READY/M-REQUIRE: drive serve itself with requireRunDir on a
// prepared 0711 fixture. Readiness verifies a bound/connectable 0666 socket and
// cancels the server. W-SERVE-ORPHAN drives reclaim through the same start path.
// C-SERVE-REFUSE narrows this to owned directories/dead sockets and a successful
// ready callback. No station authentication, host boot or real unit is claimed.
func TestServicePreparedDirectoryStarts(t *testing.T) {
	// Keep the Unix path below Darwin's 103-byte limit even if the caller's
	// isolated TMPDIR is long. The nested fixtures still come from t.TempDir.
	outer, err := os.MkdirTemp("/tmp", "c6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	// BSD may inherit /tmp's group. Align only this owned scratch inode to
	// our own primary group, never to the real service account or a host path.
	if err := os.Chown(outer, -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", outer)
	for _, kind := range []string{"prepared", "orphan", "live", "file", "directory_mode", "ready_failure"} {
		t.Run(kind, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "h")
			if _, err = Register(home, "station", "identity", uint32(os.Geteuid()), false); err != nil {
				t.Fatal(err)
			}
			run := filepath.Join(root, "r")
			if os.Mkdir(run, 0711) != nil || os.Chmod(run, 0711) != nil {
				t.Fatal("run fixture")
			}
			socket := filepath.Join(run, "s")
			if kind == "orphan" || kind == "live" {
				l, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
				if e != nil {
					t.Fatal(e)
				}
				l.SetUnlinkOnClose(false)
				defer l.Close()
				if os.Chmod(socket, 0666) != nil {
					t.Fatal("socket mode")
				}
				if kind == "orphan" {
					if e = l.Close(); e != nil {
						t.Fatal(e)
					}
				}
			}
			if kind == "file" {
				if os.WriteFile(socket, []byte("keep"), 0600) != nil {
					t.Fatal("file plant")
				}
			}
			if kind == "directory_mode" {
				if os.Chmod(run, 0755) != nil {
					t.Fatal("mode plant")
				}
			}
			before, _ := os.Lstat(socket)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			calls := 0
			s := &server{home: home, log: io.Discard, requireRunDir: true, conns: map[*net.UnixConn]bool{}}
			s.serviceReady = func() error {
				calls++
				info, e := os.Lstat(socket)
				if e != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0666 {
					return errors.New("ready before socket bind/permissions")
				}
				c, e := net.DialTimeout("unix", socket, time.Second)
				if e != nil {
					return e
				}
				c.Close()
				if kind == "ready_failure" {
					return errRefused
				}
				cancel()
				return nil
			}
			err = s.serve(ctx, socket, nil)
			if kind == "prepared" || kind == "orphan" {
				if err != nil || calls != 1 || ctx.Err() != context.Canceled {
					t.Fatalf("ready/cancel: err=%v calls=%d context=%v", err, calls, ctx.Err())
				}
			} else if kind == "ready_failure" {
				if !errors.Is(err, errRefused) || calls != 1 {
					t.Fatal("ready error not propagated", err, calls)
				}
			} else {
				if !errors.Is(err, errRefused) || calls != 0 {
					t.Fatal("unsafe start", err, calls)
				}
				if before != nil {
					after, _ := os.Lstat(socket)
					if !os.SameFile(before, after) {
						t.Fatal("refusal replaced path")
					}
				}
				return
			}
			if _, err = os.Lstat(socket); !os.IsNotExist(err) {
				t.Fatal("socket cleanup not joined", err)
			}
			lock, err := os.OpenFile(filepath.Join(run, ".daemon.lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
				t.Fatal("serve lock not released")
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
