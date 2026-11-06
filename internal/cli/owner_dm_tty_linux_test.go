//go:build linux

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"golang.org/x/sys/unix"
)

// Test 9: real Linux controlling-terminal children exercise /dev/tty and both
// isatty checks without the confirmation seam. Wrong type-back refuses without
// config changes; correct ensure/set/unset flows succeed against the fake HS.
// The fixture reports cursor position; unsupported OSC color queries may fall back.
func TestOwnerDMTTY(t *testing.T) {
	if action := os.Getenv("A2A_OWNER_DM_TTY_CHILD"); action != "" {
		a := NewApp()
		args := []string{"owner-dm", action}
		if action == "set" {
			args = append(args, "--room", os.Getenv("A2A_OWNER_DM_TTY_ROOM"))
		}
		if code := a.Run(context.Background(), args); code != 0 {
			os.Exit(code)
		}
		return
	}
	e, _, f := ownerFixture(t)
	run := func(action, typed, room string) string {
		t.Helper()
		fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		master := os.NewFile(uintptr(fd), "owner-dm-test-pty")
		defer master.Close()
		if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			t.Fatal(err)
		}
		n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
		if err != nil {
			t.Fatal(err)
		}
		slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer slave.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnerDMTTY$")
		cmd.Env = append(os.Environ(), "A2A_OWNER_DM_TTY_CHILD="+action, "A2A_OWNER_DM_TTY_ROOM="+room, config.EnvConfigDir+"="+e.dir, config.EnvAgent+"=bob", "A2A_SECRET_STORE=file")
		cmd.Stdin = slave
		cmd.Stdout = slave
		cmd.Stderr = slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		t.Logf("own terminal child PID %d", pid)
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		output := make(chan string, 1)
		go func() {
			var b strings.Builder
			buf := make([]byte, 1024)
			wrote := false
			queries := 0
			for {
				n, e := master.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
					// A PTY alone cannot answer terminal queries. Test-only Bubble Tea
					// initialization queries OSC 11 then DSR before this child runs.
					// DSR alone is a valid unsupported-OSC fallback in termenv.
					// Count the accumulated stream so split/repeated queries work.
					for queries < strings.Count(b.String(), "\x1b[6n") {
						queries++
						if _, err := io.WriteString(master, "\x1b[1;1R"); err != nil {
							t.Errorf("terminal DSR reply: %v", err)
						}
					}
					if !wrote && strings.Contains(b.String(), "to confirm:") {
						wrote = true
						_, _ = io.WriteString(master, typed+"\n")
					}
				}
				if e != nil {
					t.Logf("terminal fixture: cursor reports=%d, type-back sent=%t", queries, wrote)
					output <- b.String()
					return
				}
			}
		}()
		err = <-wait
		slave.Close()
		master.Close()
		text := <-output
		if ctx.Err() != nil {
			t.Fatal("terminal child timed out", text)
		}
		if typed == "wrong" {
			if err == nil || !strings.Contains(text, "type-back mismatch") {
				t.Fatal("wrong type-back accepted", err, text)
			}
		} else if err != nil {
			t.Fatal("terminal flow refused", err, text)
		}
		return text
	}
	before := ownerSnapshot(t, f)
	run("ensure", "wrong", "")
	ownerUnchanged(t, f, before)
	run("ensure", "@alice:"+testServer, "")
	c := ownerLoad(t, f)
	ownerJoin(e, c, c.Owner)
	run("set", "@alice:"+testServer, c.Room)
	if !ownerLoad(t, f).Active {
		t.Fatal("set not active")
	}
	run("unset", "unset", "")
	if ownerLoad(t, f).Active || ownerLoad(t, f).Pending {
		t.Fatal("unset not inactive")
	}
}
