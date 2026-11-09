//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/courier"
)

type cmListenerRun struct {
	address, capability string // test-generated only; never printed
	done                chan result
	cancel              context.CancelFunc
}

func cmStart(t *testing.T, home string, run func(context.Context, io.Writer) result) *cmListenerRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &cmListenerRun{done: make(chan result, 1), cancel: cancel}
	diag := &flushBuffer{}
	go func() { l.done <- run(ctx, diag) }()
	t.Cleanup(cancel)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(home, "state", noticeEndpoint))
		if err == nil {
			parts := strings.Fields(string(data))
			if len(parts) != 2 || !strings.HasPrefix(parts[0], "ws://127.0.0.1:") || len(parts[1]) != 64 || !strings.HasSuffix(string(data), "\n") {
				t.Fatal("invalid endpoint shape")
			}
			l.address, l.capability = parts[0], parts[1]
			break
		}
		select {
		case <-l.done:
			t.Fatal("listener exited before publishing")
		case <-timer.C:
			t.Fatal("endpoint publication timed out")
		case <-tick.C:
		}
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
			t.Error("listener failed to stop")
		}
		log, _ := diag.snapshot()
		if strings.Contains(log, l.capability) || strings.Contains(log, l.address) {
			t.Error("listener authority leaked into log")
		}
	})
	return l
}

func cmStartC5(t *testing.T, s *c5Station) *cmListenerRun {
	return cmStart(t, s.home, func(ctx context.Context, diag io.Writer) result {
		a := &App{Stdout: io.Discard, Stderr: diag, Getenv: func(k string) string {
			if k == "A2A_CONFIG_DIR" {
				return s.home
			}
			return ""
		}}
		return result{code: a.Run(ctx, []string{"watch", "--notices", "--listen"})}
	})
}

func cmDial(t *testing.T, l *cmListenerRun, capability string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, l.address, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + capability}}})
}

func cmConnect(t *testing.T, l *cmListenerRun) *websocket.Conn {
	t.Helper()
	c, _, err := cmDial(t, l, l.capability)
	if err != nil {
		t.Fatal("valid client refused")
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func cmRead(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type_, b, err := c.Read(ctx)
	if err != nil || type_ != websocket.MessageText {
		t.Fatal("valid notice not delivered")
	}
	return b
}

func cmRefusal(t *testing.T, l *cmListenerRun, cap, reason string, status int) {
	t.Helper()
	c, r, err := cmDial(t, l, cap)
	if c != nil {
		c.CloseNow()
	}
	if err == nil || r == nil || r.StatusCode != status {
		t.Fatal("refusal status differs")
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if string(b) != reason+"\n" {
		t.Fatal("refusal is not the exact closed reason")
	}
}

func TestCMListenerAdmission(t *testing.T) {
	emit := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		for {
			select {
			case <-ctx.Done():
				return 0
			case <-emit:
				_, _ = io.WriteString(inv.Stdout, `{"total":1,"types":{"to":1,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":0},"kinds":[],"names":[],"unknown":false,"more":0,"gaps":0}`+"\n")
			}
		}
	})
	l := cmStartC5(t, s)
	cmRefusal(t, l, strings.Repeat("0", 64), "not_allowed", http.StatusForbidden)
	c := cmConnect(t, l)
	cmRefusal(t, l, l.capability, "courier_busy", http.StatusConflict)
	select {
	case emit <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not start")
	}
	var n notice
	if json.Unmarshal(cmRead(t, c), &n) != nil || n.Total != 1 {
		t.Fatal("accepted client lost valid neighbour")
	}
	// Output-only: a fully authenticated client still cannot submit commands.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
		t.Fatal("could not exercise command refusal")
	}
	_, _, err := c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusPolicyViolation || closed.Reason != "not_allowed" {
		t.Fatal("command did not receive closed refusal")
	}
}

func TestCMListenerConcurrentClients(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { <-ctx.Done(); return 0 })
	l := cmStartC5(t, s)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, r, err := cmDial(t, l, l.capability)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
				t.Cleanup(func() { c.CloseNow() })
				return
			}
			if r == nil {
				t.Error("concurrent client lost refusal")
				return
			}
			defer r.Body.Close()
			b, _ := io.ReadAll(r.Body)
			if r.StatusCode != http.StatusConflict || string(b) != "courier_busy\n" {
				t.Error("concurrent refusal differs")
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted != 1 {
		t.Fatal("single-client invariant violated")
	}
}

func TestCMListenerDaemonDeath(t *testing.T) {
	started := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { close(started); <-ctx.Done(); return 0 })
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("valid watch did not start")
	}
	begin := time.Now()
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), courier.StationReadDeadline)
	defer cancel()
	_, _, err := c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "courier_unavailable" || time.Since(begin) >= courier.StationReadDeadline {
		t.Fatal("daemon death was late or unclassified")
	}
}

func TestCMListenerStopReleasesInvocation(t *testing.T) {
	started, released := make(chan struct{}), make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int {
		close(started)
		<-ctx.Done()
		close(released)
		return 0
	})
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("valid watch did not start")
	}
	l.cancel()
	select {
	case <-released:
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("listener death retained engine invocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("stopped listener retained client")
	}
}

func TestCMEndpointInventory(t *testing.T) {
	home := canonicalRoot(t)
	if err := os.Mkdir(filepath.Join(home, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	line := "ws://127.0.0.1:1/ " + strings.Repeat("a", 64) + "\n"
	remove, err := publishNoticeEndpoint(home, line)
	if err != nil {
		t.Fatal("valid endpoint publication failed")
	}
	info, err := os.Stat(filepath.Join(home, "state", noticeEndpoint))
	if err != nil || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Fatal("endpoint mode differs")
	}
	if _, err := publishNoticeEndpoint(home, "replacement\n"); err != courier.Busy {
		t.Fatal("existing generation was not refused with courier_busy")
	}
	b, err := os.ReadFile(filepath.Join(home, "state", noticeEndpoint))
	if err != nil || string(b) != line {
		t.Fatal("atomic generation changed")
	}
	entries, _ := os.ReadDir(filepath.Join(home, "state"))
	if len(entries) != 1 || entries[0].Name() != noticeEndpoint {
		t.Fatal("publication leaked temporary file")
	}
	remove()
	if _, err := os.Stat(filepath.Join(home, "state", noticeEndpoint)); !os.IsNotExist(err) {
		t.Fatal("endpoint survived stop")
	}
	remove, err = publishNoticeEndpoint(home, line)
	if err != nil {
		t.Fatal("valid restart refused")
	}
	remove()
	alias := filepath.Join(canonicalRoot(t), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := publishNoticeEndpoint(alias, line); err != courier.NotAllowed {
		t.Fatal("alias refusal differs")
	}
	remove, err = publishNoticeEndpoint(home, line)
	if err != nil {
		t.Fatal("alias guard rejected canonical neighbour")
	}
	remove()
}

func TestCMStationWriteInventory(t *testing.T) {
	started := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { close(started); <-ctx.Done(); return 0 })
	if err := updateHookFailures(s.home, "Stop", courier.Unavailable, io.Discard); err != nil {
		t.Fatal(err)
	}
	l := cmStartC5(t, s)
	// Publication becomes visible before its temporary link is unlinked. Wait
	// for the real post-publication boundary, not a scheduler-dependent walk.
	c := cmConnect(t, l)
	select {
	case <-started:
	case <-l.done:
		t.Fatal("listener ended before inventory boundary")
	case <-time.After(5 * time.Second):
		t.Fatal("engine admission timed out")
	}
	var actual []string
	if err := filepath.WalkDir(s.home, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.home, path)
		actual = append(actual, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{".", ".a2a", ".a2a/courier.sock", ".a2a/identity", ".a2a/station", ".a2a/station.key", "state", "state/" + hookFailureLog, "state/" + noticeEndpoint}
	sort.Strings(actual)
	sort.Strings(want)
	if strings.Join(actual, "\n") != strings.Join(want, "\n") {
		t.Fatal("station inventory differs from C5 plus one endpoint")
	}
	c.CloseNow()
	l.cancel()
	// Wait for endpoint removal, then reuse C5's unchanged inventory oracle.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		if stationFootprint(s.home) == nil {
			break
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatal("C5 inventory not restored after stop")
		}
	}
}
