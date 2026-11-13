//go:build linux || darwin

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// The real websocket writer rechecks admission after assembling a split daemon
// frame. A muted buffered notice is not forwarded; a fresh post-thaw frame is.
func TestScopeMuteListenerBufferedForwarding(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	sockets := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		sockets <- c
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	peer := <-sockets
	defer peer.CloseNow()
	listener := &noticeListener{scopeRoot: root, ctx: ctx, client: peer}
	payload, err := (notice{Total: 1, Types: noticeTypes{To: 1}, Kinds: []string{}, Names: []string{}}).encode(true)
	if err != nil {
		t.Fatal(err)
	}
	split := len(payload) / 2
	if _, err = listener.Write(payload[:split]); err != nil {
		t.Fatal(err)
	}
	op := fixture.Mute(t, scope, "buffered")
	if _, err = listener.Write(append(payload[split:], '\n')); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		_, data, e := client.Read(ctx)
		if e != nil {
			readErr <- e
		} else {
			received <- data
		}
	}()
	select {
	case <-received:
		t.Fatal("buffered notice forwarded after mute ACK")
	case e := <-readErr:
		t.Fatal(e)
	case <-time.After(25 * time.Millisecond):
	}
	fixture.Unmute(t, op)
	if _, err = listener.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if string(data) != string(payload) {
			t.Fatal("wrong positive frame")
		}
	case e := <-readErr:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
