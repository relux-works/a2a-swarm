//go:build linux || darwin

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/daemonenroll"
)

// Child mode is reachable only inside the Go test binary, with explicit
// synthetic paths. It never resolves the installed a2a or ambient config.
func TestCMProcessFixture(t *testing.T) {
	mode := os.Getenv("A2A_CM_TEST_CHILD")
	if mode == "" {
		t.Skip("subprocess fixture only")
	}
	home := os.Getenv("A2A_CM_TEST_HOME")
	if mode == "daemon" {
		err := daemon.ServeWithHandler(context.Background(), home, os.Getenv("A2A_CM_TEST_SOCKET"), io.Discard, func(ctx context.Context, _ daemon.Invocation) int {
			_, _ = fmt.Fprintln(os.Stdout, "started")
			<-ctx.Done()
			return 0
		})
		if err != nil {
			t.Fatal("fake daemon refused")
		}
		return
	}
	if mode != "listener" {
		t.Fatal("unknown child mode")
	}
	a := &App{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return home
		}
		if k == EnvSessionID {
			return "kill-owner"
		}
		return ""
	}}
	a.Run(context.Background(), []string{"--as", "alice", "watch", "--notices", "--listen"})
}

func cmChild(t *testing.T, mode, home, socket string) (*exec.Cmd, <-chan struct{}, <-chan string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCMProcessFixture$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), "A2A_CM_TEST_CHILD="+mode, "A2A_CM_TEST_HOME="+home, "A2A_CM_TEST_SOCKET="+socket)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default:
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned fixture did not stop")
		}
	})
	return cmd, done, lines
}

func TestCMDaemonKill(t *testing.T) {
	home := filepath.Join(canonicalRoot(t), "daemon")
	station := canonicalRoot(t)
	socket := filepath.Join(shortDaemonBase(t), "s", "c.sock")
	key, err := daemon.Register(home, "station", "identity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	payload, _ := json.Marshal(map[string]string{"socket": socket, "station": "station", "identity": "identity", "secret_b64": base64.StdEncoding.EncodeToString(key)})
	if err := daemonenroll.Install(filepath.Join(station, ".a2a"), bytes.NewReader(append(payload, '\n')), false); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	cmd, done, started := cmChild(t, "daemon", home, socket)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		c, err := net.DialTimeout("unix", socket, 10*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-tick.C:
		case <-done:
			t.Fatal("fake daemon stopped at startup")
		case <-timer.C:
			t.Fatal("fake daemon readiness timed out")
		}
	}
	s := &c5Station{home: station, socket: socket}
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	select {
	case <-started:
	case <-done:
		t.Fatal("healthy watch died")
	case <-time.After(5 * time.Second):
		t.Fatal("watch not admitted")
	}
	begin := time.Now()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "daemon_unavailable" || time.Since(begin) >= daemon.StationReadDeadline {
		t.Fatal("daemon kill not visible with bounded closed reason")
	}
}

func TestCMListenerKillReleasesLock(t *testing.T) {
	f := newDaemonFixture(t)
	cmSeedOwner(t, f, "alice", "kill-owner", "@bob:"+testServer)
	cmd, done, _ := cmChild(t, "listener", f.stations["alice"], "")
	// cmStart observes publication by the child; its callback is only a waiter.
	l := cmStart(t, f.stations["alice"], func(ctx context.Context, _ io.Writer) result {
		select {
		case <-done:
		case <-ctx.Done():
		}
		return result{}
	})
	c := cmConnect(t, l)
	cmAwait(t, l.done, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor != nil
	})
	begin := time.Now()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemon.LockReleaseBound)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("killed listener retained client")
	}
	cmAwait(t, nil, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor.Phase == "stopped"
	})
	release, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", begin.Add(daemon.LockReleaseBound))
	if err != nil || time.Since(begin) >= daemon.LockReleaseBound {
		t.Fatal("listener kill retained consumer lock")
	}
	release()
	// SIGKILL cannot run defers; remove only this fixture's stale endpoint.
	if err := os.Remove(filepath.Join(f.stations["alice"], "state", noticeEndpoint)); err != nil {
		t.Fatal(err)
	}
}
