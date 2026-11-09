//go:build linux || darwin

package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

const noticeEndpoint = "courier-notice-listener.endpoint"

// Listener authority is private to this invocation. No request bytes, URL,
// address, capability or backend error is ever used as a diagnostic.
type noticeListener struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	logMu   sync.Mutex
	client  *websocket.Conn
	claimed bool
	ready   chan struct{}
	log     io.Writer
	partial []byte
}

func (a *App) stationNoticeListener(ctx context.Context, home string, e courierenroll.Enrolled, args []string) int {
	plan, err := courier.Authorize(args, e.Identity)
	if err != nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: not_allowed\n")
		return ExitError
	}
	o, err := parseWatchOptions(plan.Args)
	if err != nil || !o.notices || !o.listen {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: not_allowed\n")
		return ExitError
	}
	session, err := stationSession(plan, nil, a.Getenv)
	if err != nil {
		_, _ = io.WriteString(a.Stderr, "a2a: courier: not_allowed\n")
		return ExitError
	}
	args = o.noticeListenerArgs(plan.Selection)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l := &noticeListener{ctx: ctx, cancel: cancel, ready: make(chan struct{}), log: a.Stderr}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return a.noticeUnavailable()
	}
	capability := hex.EncodeToString(secret[:])
	clear(secret[:])
	socket, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		l.closedLog("a2a: courier: ", courier.Class(err))
		return ExitError
	}
	defer socket.Close()
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 4096, DisableGeneralOptionsHandler: true}
	// Suppress net/http's free-form error logging too (including panic reports).
	server.ErrorLog = log.New(io.Discard, "", 0)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { l.accept(w, r, capability) })
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = server.Serve(socket) }()
	defer func() { _ = server.Close(); <-serveDone }()
	remove, err := publishNoticeEndpoint(home, "ws://"+socket.Addr().String()+"/ "+capability+"\n")
	if err != nil {
		l.closedLog("a2a: courier: ", courier.Class(err))
		return ExitError
	}
	defer remove()
	select {
	case <-ctx.Done():
		return 130
	case <-l.ready:
	}
	code, err := courier.Client(ctx, e.Socket, e.Station, e.Identity, e.Key, courier.Request{Argv: args, Session: session}, l, io.Discard)
	reason := courier.Unavailable
	if err != nil {
		reason = courier.Class(err)
	}
	if reason != courier.IdleTimeout {
		reason = courier.Unavailable
	}
	l.mu.Lock()
	client := l.client
	l.mu.Unlock()
	canceled := ctx.Err() != nil
	if client != nil {
		_ = client.Close(websocket.StatusGoingAway, string(reason))
	}
	if canceled {
		return 130
	}
	l.closedLog("a2a: courier: ", reason)
	if err != nil || code == 0 {
		return ExitError
	}
	return code
}

func (a *App) noticeUnavailable() int {
	_, _ = io.WriteString(a.Stderr, "a2a: courier: courier_unavailable\n")
	return ExitError
}

// Diagnostic backpressure must never hold the client admission/liveness lock.
// In particular, daemon death closes the socket before writing its diagnostic.
func (l *noticeListener) closedLog(prefix string, reason courier.Diagnostic) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	_, _ = fmt.Fprintf(l.log, "%s%s\n", prefix, reason)
}

func (l *noticeListener) accept(w http.ResponseWriter, r *http.Request, capability string) {
	refuse := func(reason courier.Diagnostic, status int) {
		l.closedLog("courier notice listener: ", reason)
		http.Error(w, string(reason), status)
	}
	if r.Method != "GET" || r.URL.Path != "/" || r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+capability)) != 1 {
		refuse(courier.NotAllowed, http.StatusForbidden)
		return
	}
	l.mu.Lock()
	if l.claimed {
		l.mu.Unlock()
		refuse(courier.Busy, http.StatusConflict)
		return
	}
	l.claimed = true
	l.mu.Unlock()
	client, err := websocket.Accept(&closedUpgrade{ResponseWriter: w}, r, nil)
	if err != nil {
		l.mu.Lock()
		l.claimed = false
		l.mu.Unlock()
		l.closedLog("courier notice listener: ", courier.NotAllowed)
		return
	}
	l.mu.Lock()
	l.client = client
	l.mu.Unlock()
	close(l.ready)
	defer client.CloseNow()
	defer l.cancel()
	// Read is only a disconnect/policy detector. Data is never interpreted as
	// a command. Control ping/pong frames remain valid WebSocket housekeeping.
	client.SetReadLimit(-1)
	_, _, err = client.Reader(l.ctx)
	if err == nil {
		l.closedLog("courier notice listener: ", courier.NotAllowed)
		_ = client.Close(websocket.StatusPolicyViolation, "not_allowed")
	}
}

// Keep library handshake diagnostics closed, including attacker-controlled
// Origin/Upgrade values. Successful upgrades still use the original socket.
type closedUpgrade struct {
	http.ResponseWriter
	refused bool
}

func (w *closedUpgrade) WriteHeader(status int) {
	if status != http.StatusSwitchingProtocols {
		w.refused = true
		http.Error(w.ResponseWriter, "not_allowed", status)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *closedUpgrade) Write(p []byte) (int, error) {
	if w.refused {
		return len(p), nil
	}
	w.WriteHeader(http.StatusBadRequest)
	return len(p), nil
}
func (w *closedUpgrade) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (l *noticeListener) Write(p []byte) (int, error) {
	// C5 can split a line at any chunk boundary. Hold at most one L01 record.
	if len(l.partial)+len(p) > noticeJSONMax+1 {
		return 0, courier.NotAllowed
	}
	l.partial = append(l.partial, p...)
	for {
		line, rest, found := bytes.Cut(l.partial, []byte{'\n'})
		if !found {
			return len(p), nil
		}
		var n notice
		d := json.NewDecoder(bytes.NewReader(line))
		d.DisallowUnknownFields()
		if d.Decode(&n) != nil || n.validate() != nil || n.Total == 0 && n.Gaps == 0 {
			return 0, courier.NotAllowed
		}
		canonical, err := n.encode(true)
		if err != nil || !bytes.Equal(canonical, line) {
			return 0, courier.NotAllowed
		}
		l.mu.Lock()
		client := l.client
		l.mu.Unlock()
		if client == nil {
			return 0, courier.Unavailable
		}
		ctx, cancel := context.WithTimeout(l.ctx, courier.LockReleaseBound)
		err = client.Write(ctx, websocket.MessageText, canonical)
		cancel()
		if err != nil {
			return 0, courier.Unavailable
		}
		l.partial = append(l.partial[:0], rest...)
	}
}

// Publish a complete 0600 inode with a create-only atomic hard link. The open
// root pins the station namespace; symlink aliases and existing generations
// refuse. No stale generation is removed or adopted automatically.
func publishNoticeEndpoint(home, line string) (func(), error) {
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil || canonical != home {
		return nil, courier.NotAllowed
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, courier.Unavailable
	}
	defer root.Close()
	if err := root.Mkdir("state", 0700); err != nil && !os.IsExist(err) {
		return nil, courier.Unavailable
	}
	info, err := root.Lstat("state")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, courier.NotAllowed
	}
	state, err := root.OpenRoot("state")
	if err != nil {
		return nil, courier.NotAllowed
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		state.Close()
		return nil, courier.Unavailable
	}
	temp := ".notice-" + hex.EncodeToString(random[:])
	f, err := state.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		state.Close()
		return nil, courier.Unavailable
	}
	defer func() { _ = state.Remove(temp) }()
	err = f.Chmod(0600)
	if err == nil {
		_, err = io.Copy(f, strings.NewReader(line))
	}
	if err == nil {
		err = f.Sync()
	}
	published, statErr := f.Stat()
	if err == nil {
		err = statErr
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = state.Link(temp, noticeEndpoint)
	}
	if err != nil {
		_ = state.Remove(temp)
		state.Close()
		return nil, courier.Busy
	}
	return func() {
		current, err := state.Lstat(noticeEndpoint)
		if err == nil && os.SameFile(published, current) {
			_ = state.Remove(noticeEndpoint)
		}
		_ = state.Close()
	}, nil
}
