//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/runorigin"
)

// TestRev365LiveAncestorSymlink changes only a protected test fixture's ancestor after a valid operation; the production socket path must refuse a newly symlinked component.
func TestRev365LiveAncestorSymlink(t *testing.T) {
	base := shortOriginDir(t)
	route := filepath.Join(base, "route")
	control := filepath.Join(route, "control")
	if err := os.Mkdir(route, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(control, 0711); err != nil {
		t.Fatal(err)
	}
	p := fakeOriginProducer()
	f := startOrigin(t, p, func(c *OriginConfig) { c.ControlDir = control })
	f.dir = control
	c := f.client(t)
	prepared := prepareOn(t, c)
	sendOrigin(t, c, originValidate, validateParams(prepared))
	_ = originRead(t, c)
	c.Close()
	p.awaitClose(t)
	saved := filepath.Join(base, "saved")
	if err := os.Rename(route, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, route); err != nil {
		t.Fatal(err)
	}
	c = f.dial(t)
	b := originRead(t, c)
	var h map[string]json.RawMessage
	_ = json.Unmarshal(b, &h)
	if h["code"] != nil {
		var code string
		_ = json.Unmarshal(h["code"], &code)
		if code != runorigin.Refused {
			t.Fatalf("wrong path refusal: %s", b)
		}
		return
	}
	sendOrigin(t, c, originPrepare, prepareParams())
	b = originRead(t, c)
	var got runorigin.PreparedOrigin
	if json.Unmarshal(b, &got) == nil && got.Origin.Valid() {
		t.Fatalf("symlinked live ancestor admitted protected prepare: opens=%d, path=%s", p.opens.Load(), control)
	}
	t.Fatalf("symlinked ancestor admitted hello and then returned %s", b)
}

type rev365PausingAuthority struct {
	calls   atomic.Int32
	pauseAt int32
	entered chan struct{}
	release chan struct{}
}

func (a *rev365PausingAuthority) CurrentGeneration(ctx context.Context, uid uint32) (string, error) {
	at := a.pauseAt
	if at == 0 {
		at = 2
	}
	if a.calls.Add(1) == at {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return launcherGeneration, nil
}

// TestRev365PathChangeDuringAuthentication pauses the production authority call after the per-operation path check, changes socket mode, and requires refusal before producer construction.
func TestRev365PathChangeDuringAuthentication(t *testing.T) {
	t.Run("unchanged-positive", validOriginControl)
	t.Run("mode-changed-during-authority", func(t *testing.T) {
		a := &rev365PausingAuthority{entered: make(chan struct{}), release: make(chan struct{})}
		p := fakeOriginProducer()
		f := startOrigin(t, p, func(c *OriginConfig) { c.Generations = a })
		c := f.client(t)
		sendOrigin(t, c, originPrepare, prepareParams())
		select {
		case <-a.entered:
		case <-time.After(time.Second):
			t.Fatal("authority barrier not entered")
		}
		if err := os.Chmod(filepath.Join(f.dir, "origin.sock"), 0600); err != nil {
			t.Fatal(err)
		}
		close(a.release)
		b := originRead(t, c)
		var errFrame map[string]string
		_ = json.Unmarshal(b, &errFrame)
		if errFrame["code"] != runorigin.Refused || p.opens.Load() != 0 {
			t.Fatalf("path changed after last endpoint check but protected work ran: opens=%d response=%s", p.opens.Load(), b)
		}
	})
	t.Run("validate-mode-changed-during-authority", func(t *testing.T) {
		a := &rev365PausingAuthority{pauseAt: 3, entered: make(chan struct{}), release: make(chan struct{})}
		p := fakeOriginProducer()
		f := startOrigin(t, p, func(c *OriginConfig) { c.Generations = a })
		c := f.client(t)
		prepared := prepareOn(t, c)
		sendOrigin(t, c, originValidate, validateParams(prepared))
		select {
		case <-a.entered:
		case <-time.After(time.Second):
			t.Fatal("validate authority barrier not entered")
		}
		if err := os.Chmod(filepath.Join(f.dir, "origin.sock"), 0600); err != nil {
			t.Fatal(err)
		}
		close(a.release)
		wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
		p.awaitClose(t)
		if p.validates.Load() != 0 {
			t.Fatal("path changed during authority reached Session.Validate")
		}
	})

}
