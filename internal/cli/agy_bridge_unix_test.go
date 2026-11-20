//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real inherited blocking pipe stays open at its upstream end. A refusal
// must still join the reader promptly and restore the descriptor flags.
func TestAgyBridgeBlockingPipeRefusal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(r.Fd())
	if err = unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	in, restore, err := agyBridgePipe(r)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- NewApp().runAgyBridge(context.Background(), in, agyTestID, agyQualifiedModel, time.Hour)
	}()
	if _, err = w.WriteString("{\"event\":\"init\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "invalid agy frame") {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		w.Close()
		<-finished
		t.Fatal("refusal hung behind the upstream writer")
	}
	restore()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		t.Fatal("inherited flags not restored")
	}
}

// A stalled native receiver cannot hold the notification transaction indefinitely.
func TestAgyBridgeBlockingPipeOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(w.Fd())
	if err = unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{make([]byte, 4096), {1}} {
		for {
			_, err = unix.Write(fd, chunk)
			if err == unix.EAGAIN {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	output, restore, err := agyBridgePipe(w)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := wakemute.WriteOutput(ctx, output, []byte("notice")); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("full pipe accepted a notice")
		}
	case <-time.After(2 * time.Second):
		r.Close()
		<-finished
		t.Fatal("native output ignored its deadline")
	}
}

type agyTestTransport func(*http.Request) (*http.Response, error)

func (f agyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Establish custody using an actual native hook, then inject a pending event.
// All bridge calls below use the public CLI and real OS pipes.
func agyPollFixture(t *testing.T) (*env, config.Paths) {
	t.Helper()
	e, room := world(t)
	e.extra["A2A_AGENT"] = "alice"
	e.extra[EnvSessionID] = agyTestID
	e.stdin = `{"conversationId":"` + agyTestID + `","hookEventName":"SessionStart"}`
	e.as("alice", "hook", "session-start", "--runtime", "agy").ok(t)
	e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "private-poll-witness"))
	return e, config.Paths{Base: e.dir}
}

func agyTestPipes(t *testing.T) (*os.File, *os.File, *os.File, *os.File) {
	t.Helper()
	in, feed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); feed.Close(); read.Close(); out.Close() })
	return in, feed, read, out
}
func agyTestReady(t *testing.T, feed *os.File) {
	t.Helper()
	_, err := fmt.Fprintf(feed, "{\"event\":\"init\",\"conversation_id\":%q,\"init\":{\"model\":%q}}\n{\"event\":\"result\",\"result\":{\"conversation_id\":%q,\"status\":\"SUCCESS\"}}\n", agyTestID, agyQualifiedModel, agyTestID)
	if err != nil {
		t.Fatal(err)
	}
}
func agyTestRun(e *env, ctx context.Context) result {
	return e.runCtx(ctx, "--as", "alice", "agy", "bridge", "--session", agyTestID, "--model", agyQualifiedModel)
}

// While a real poll is stalled on /sync, a second consumer cannot enter.
// An observed provider fault cancels that poll without ringing or receipting.
func TestAgyBridgePollInvalidation(t *testing.T) {
	e, p := agyPollFixture(t)
	in, feed, read, out := agyTestPipes(t)
	started := make(chan struct{})
	var once sync.Once
	e.configure = func(a *App) {
		a.Stdin = in
		a.Stdout = out
		c := *a.HTTP
		upstream := c.Transport
		c.Transport = agyTestTransport(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/sync") {
				once.Do(func() { close(started) })
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return upstream.RoundTrip(req)
		})
		a.HTTP = &c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	finished := make(chan result, 1)
	go func() { finished <- agyTestRun(e, ctx) }()
	agyTestReady(t, feed)
	select {
	case <-started:
	case got := <-finished:
		t.Fatalf("early bridge exit %v", got)
	case <-ctx.Done():
		t.Fatal("poll never reached network")
	}
	second := *e
	i, _, _, o := agyTestPipes(t)
	second.configure = func(a *App) { a.Stdin = i; a.Stdout = o }
	got := agyTestRun(&second, ctx)
	if got.code == 0 || !strings.Contains(got.stderr, config.ErrLockTimeout.Error()) {
		t.Fatal("second bridge did not refuse custody", got)
	}
	io.WriteString(feed, "{\"event\":\"error\"}\n")
	select {
	case got := <-finished:
		if got.code == 0 || !strings.Contains(got.stderr, "provider reported an error") {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not cancel pending poll")
	}
	out.Close()
	buf, err := io.ReadAll(read)
	if err != nil || len(buf) != 0 {
		t.Fatal("wrote after invalidation", err)
	}
	st, err := config.LoadState(p, "alice")
	if err != nil || st.Notification.Reservation != nil {
		t.Fatal("unexpected reservation", err)
	}
	e.configure = nil
	if !strings.Contains(e.as("alice", "inbox").ok(t).stdout, "private-poll-witness") {
		t.Fatal("lost pending message")
	}
}

// A full inherited provider input exercises reserve/write/cancel as one unit.
// A proven zero-byte refusal restores the previous cooldown and retains mail.
func TestAgyBridgeFullPipeRollback(t *testing.T) {
	e, p := agyPollFixture(t)
	in, feed, _, out := agyTestPipes(t)
	fd := int(out.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{make([]byte, 4096), {1}} {
		for {
			_, err := unix.Write(fd, b)
			if err == unix.EAGAIN {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	before, err := config.LoadState(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.Stdin = in; a.Stdout = out }
	agyTestReady(t, feed)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	got := agyTestRun(e, ctx)
	if got.code == 0 || !strings.Contains(got.stderr, "write output") {
		t.Fatal(got)
	}
	after, err := config.LoadState(p, "alice")
	if err != nil || after.Notification.Reservation != nil || !after.Notification.NotBefore.Equal(before.Notification.NotBefore) {
		t.Fatalf("reservation or cooldown retained: %+v %v", after.Notification, err)
	}
	e.configure = nil
	if !strings.Contains(e.as("alice", "inbox").ok(t).stdout, "private-poll-witness") {
		t.Fatal("lost pending message")
	}
}

// Startup validates the current lease before waiting for a single input byte.
func TestAgyBridgeColdStartRefusal(t *testing.T) {
	e, _ := world(t)
	e.extra["A2A_AGENT"] = "alice"
	e.extra[EnvSessionID] = agyTestID
	in, _, _, out := agyTestPipes(t)
	e.configure = func(a *App) { a.Stdin = in; a.Stdout = out }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := agyTestRun(e, ctx)
	if got.code == 0 || !strings.Contains(got.stderr, "startup custody: agy notice requires an existing current conversation lease") {
		t.Fatal(got)
	}
}
