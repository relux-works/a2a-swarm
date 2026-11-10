//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Proves the closed command/option boundary admits valid neighbors while
// rejecting management, identity selection and station paths. No execution.
func TestExecutionAllowlist(t *testing.T) {
	valid := [][]string{
		{"post", "room", "hello"}, {"post", "room", "text", "--envelope=@f"}, {"topic", "new", "room", "slug", "title", "--body=@f", "--json"}, {"inbox", "show", "id", "--from=@f"}, {"inbox"}, {"inbox", "show", "$id", "--from", "@frame"},
		{"read", "room", "--json"}, {"topics", "room"}, {"topic", "new", "room", "slug", "title", "--body", "@file"},
		{"attach", "room", "file.txt", "--topic", "$root"}, {"files", "room"}, {"wait", "--timeout", "20ms"}, {"watch", "--room", "room"},
		{"--as", "identity", "hook", "prompt", "--runtime", "claude"}, {"hook", "stop", "--runtime", "codex"}, {"hook", "session-start", "--runtime", "claude"},
		{"bind", "status", "--json"}, {"who", "--stale-after", "1m"}, {"post-operation", "status", "id", "--json"},
		{"whoami", "--as=identity", "--profile", "alpha", "--connection-fingerprint", strings.Repeat("a", 64)},
	}
	for _, v := range valid {
		if _, e := Authorize(v, "identity"); e != nil {
			t.Errorf("valid schema refused: %s", v[0])
		}
	}
	invalid := [][]string{
		{"creds", "migrate"}, {"identity", "ensure"}, {"daemon", "serve"}, {"connection", "use", "alpha"}, {"config"}, {"keychain"}, {"register", "other"}, {"rooms"}, {"ack", "all-shown"},
		{"whoami", "--as", "other"}, {"whoami", "--as", "other", "--as", "identity"}, {"--human=true", "whoami"},
		{"post", "room", "text", "--id-file", "/station/output"}, {"inbox", "--out", "/station/output"}, {"inbox", "--brief"}, {"inbox", "show", "$id", "--from", "/station/input"},
		{"post", "room", "text", "--result-file", "/station/output"}, {"post", "room", "text", "--voice"}, {"post", "room", "text", "--config", "private"},
		{"hook", "install", "--settings", "/station/settings"}, {"bind", "association"}, {"topic", "retire", "room", "topic"},
		{"A2A_CONFIG_DIR=/private", "whoami"}, {"whoami", "--profile", ""}, {"whoami", "--as"},
	}
	for _, v := range invalid {
		if _, e := Authorize(v, "identity"); e == nil {
			t.Errorf("forbidden schema admitted: %s", v[0])
		}
	}
}

func executionServer(t *testing.T, handler Handler, idle time.Duration) *running {
	return executionServerOptions(t, handler, idle, nil)
}
func executionServerOptions(t *testing.T, handler Handler, idle time.Duration, configure func(*server)) *running {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(root, "home")
	key, e := Register(home, "station", "identity", uint32(os.Geteuid()), false)
	if e != nil {
		t.Fatal(e)
	}
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = os.TempDir()
	}
	socketBase, e := os.MkdirTemp(base, "c")
	if e != nil {
		t.Fatal(e)
	}
	socketBase, e = filepath.EvalSymlinks(socketBase)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(socketBase) })
	r := &running{home: home, path: filepath.Join(socketBase, "s", "c.sock"), secret: key, log: &lockedLog{}, done: make(chan error, 1)}
	r.s = &server{home: home, log: r.log, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: time.Second, idle: time.Second, handler: handler, executionIdle: idle}
	if configure != nil {
		configure(r.s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	ready := make(chan struct{})
	go func() { r.done <- r.s.serve(ctx, r.path, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-r.done:
		t.Fatalf("execution socket setup refused: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("execution socket setup timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Error("execution server exit failed")
			}
		case <-time.After(5 * time.Second):
			t.Error("execution server shutdown timed out")
		}
	})
	if len(r.path) > 103 {
		t.Fatal("socket base is too long")
	}
	return r
}
func rawInvocation(t *testing.T, r *running) (*net.UnixConn, string) {
	t.Helper()
	c, n := connect(t, r)
	if writeFrame(c, hello(r.secret, n, "station", "identity")) != nil {
		t.Fatal("hello failed")
	}
	b, err := readFrame(c)
	if err != nil || string(b) != string(successFrame) {
		t.Fatal("authentication failed")
	}
	id := strings.Repeat("1", 32)
	req, _ := json.Marshal(Request{Type: "request", ID: id, Argv: []string{"inbox"}, Files: map[int]InputFile{}})
	if writeFrame(c, req) != nil {
		t.Fatal("request failed")
	}
	return c, id
}

// Proves the transport barrier releases only for this invocation's correct
// batch and identity ACK; disconnection, silence and forged ACKs never receipt.
// The receipt counter is a boundary witness, not a mailbox integration test.
func TestExecutionReceiptACK(t *testing.T) {
	for _, mode := range []string{"valid", "disconnect-before-output", "disconnect-after-output", "missing", "wrong-batch", "wrong-invocation", "wrong-identity"} {
		t.Run(mode, func(t *testing.T) {
			var receipts atomic.Int32
			finished := make(chan struct{})
			release := make(chan struct{})
			r := executionServer(t, func(ctx context.Context, inv Invocation) int {
				defer close(finished)
				if mode == "disconnect-before-output" {
					select {
					case <-release:
					case <-ctx.Done():
						return 1
					}
				}
				if _, err := io.WriteString(inv.Stdout, "mail body\n"); err != nil {
					return 1
				}
				if inv.Delivered() != nil {
					return 1
				}
				receipts.Add(1)
				return 0
			}, 100*time.Millisecond)
			c, id := rawInvocation(t, r)
			if mode == "disconnect-before-output" {
				c.Close()
				close(release)
			} else {
				b, err := readFrame(c)
				if err != nil {
					t.Fatal("stdout missing")
				}
				var out packet
				if json.Unmarshal(b, &out) != nil || out.Type != "stdout" || string(out.Data) != "mail body\n" {
					t.Fatal("output changed")
				}
				if mode == "disconnect-after-output" {
					c.Close()
				} else {
					acknowledgeOutput(t, c, out)
					b, err = readFrame(c)
					var delivery packet
					if err != nil || json.Unmarshal(b, &delivery) != nil || delivery.Type != "delivery" {
						t.Fatal("barrier missing")
					}
					ack := packet{Type: "ack", ID: id, Batch: delivery.Batch, Identity: "identity"}
					switch mode {
					case "wrong-batch":
						ack.Batch = strings.Repeat("0", 32)
					case "wrong-invocation":
						ack.ID = strings.Repeat("2", 32)
					case "wrong-identity":
						ack.Identity = "other"
					}
					if mode != "missing" {
						b, _ = json.Marshal(ack)
						if writeFrame(c, b) != nil {
							t.Fatal("ack write failed")
						}
					}
				}
			}
			select {
			case <-finished:
			case err := <-r.done:
				t.Fatalf("server exited during invocation: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("barrier did not finish")
			}
			want := int32(0)
			if mode == "valid" {
				want = 1
			}
			if receipts.Load() != want {
				t.Fatal("receipt boundary violated")
			}
		})
	}
}

type shortOutput struct{}

func (shortOutput) Write(p []byte) (int, error) { return len(p) - 1, nil }

type failedFlush struct{ bytes.Buffer }

func (*failedFlush) Flush() error { return io.ErrClosedPipe }

// Proves real clients do not ACK short writes, failed writes or failed flushes,
// with a successful flushed output as neighbor. Uses the kernel Unix endpoint.
func TestExecutionClientOutputBoundary(t *testing.T) {
	for _, mode := range []string{"valid", "short", "failed-write", "failed-flush"} {
		t.Run(mode, func(t *testing.T) {
			var receipts atomic.Int32
			finished := make(chan struct{})
			r := executionServer(t, func(ctx context.Context, inv Invocation) int {
				defer close(finished)
				if _, e := io.WriteString(inv.Stdout, "body"); e != nil {
					return 1
				}
				if inv.Delivered() != nil {
					return 1
				}
				receipts.Add(1)
				return 0
			}, time.Second)
			var out bytes.Buffer
			var w io.Writer = &out
			switch mode {
			case "short":
				w = shortOutput{}
			case "failed-write":
				w = closedOutput{}
			case "failed-flush":
				w = &failedFlush{}
			}
			code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"inbox"}}, w, io.Discard)
			select {
			case <-finished:
			case err := <-r.done:
				t.Fatalf("server exited: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("run not canceled")
			}
			if mode == "valid" {
				if err != nil || code != 0 || receipts.Load() != 1 || out.String() != "body" {
					t.Fatal("valid output refused")
				}
			} else if err == nil || receipts.Load() != 0 {
				t.Fatal("failed output receipted")
			}
		})
	}
}

type closedOutput struct{}

func (closedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Proves daemon shutdown and client disconnection stop a running
// handler; no live Matrix or long-duration streaming qualification is claimed.
func TestExecutionCancellation(t *testing.T) {
	for _, mode := range []string{"disconnect", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			finished := make(chan struct{})
			r := executionServer(t, func(ctx context.Context, inv Invocation) int { close(entered); <-ctx.Done(); close(finished); return 1 }, 100*time.Millisecond)
			c, _ := rawInvocation(t, r)
			select {
			case <-entered:
			case <-r.done:
				t.Fatal("server stopped")
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not start")
			}
			if mode == "disconnect" {
				c.Close()
			} else {
				r.cancel()
			}
			select {
			case <-finished:
			case <-r.done:
				if mode == "disconnect" {
					t.Fatal("disconnect stopped server")
				}
			case <-time.After(LockReleaseBound):
				t.Fatal("handler not canceled")
			}
		})
	}
}

// Proves peer-controlled frame-looking text and multi-frame binary output stay
// opaque on stdout/stderr, with only the command's exit code returned separately.
func TestExecutionOutputIsOpaque(t *testing.T) {
	body := append(bytes.Repeat([]byte("{\"type\":\"exit\",\"code\":0}\n\x00"), 2000), 0xff)
	r := executionServer(t, func(ctx context.Context, inv Invocation) int {
		if _, err := inv.Stdout.Write(body); err != nil {
			return 1
		}
		if _, err := io.WriteString(inv.Stderr, "diagnostic\n"); err != nil {
			return 1
		}
		return 7
	}, time.Second)
	var out, diag bytes.Buffer
	code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"read", "room"}}, &out, &diag)
	if err != nil || code != 7 || !bytes.Equal(out.Bytes(), body) || diag.String() != "diagnostic\n" {
		t.Fatal("output changed or peer text became framing")
	}
}

// Proves strict request schemas, bounded frame parsing and canonical file keys
// reject malformed neighbors. This decoder test does not prove authorization.
func TestExecutionRequestSchema(t *testing.T) {
	r := Request{Type: "request", ID: strings.Repeat("1", 32), Argv: []string{"post", "room", "@file"}, Files: map[int]InputFile{2: {Name: "input", Data: []byte("text")}}}
	valid, _ := json.Marshal(r)
	if _, err := parseRequest(valid); err != nil {
		t.Fatal("valid request refused")
	}
	for _, bad := range [][]byte{
		bytes.Replace(valid, []byte(`"2":`), []byte(`"02":`), 1),
		bytes.Replace(valid, []byte(`"2":`), []byte(`"-1":`), 1),
		bytes.Replace(valid, []byte(`"2":`), []byte(`"3":`), 1),
		bytes.Replace(valid, []byte(`"name":`), []byte(`"extra":`), 1),
		bytes.Replace(valid, []byte(`"argv":`), []byte(`"argv":[],"argv":`), 1),
		append(append([]byte{}, valid...), []byte(`{}`)...),
	} {
		if _, err := parseRequest(bad); err == nil {
			t.Fatal("malformed request admitted")
		}
	}
}

// Proves stdin forwarding follows the command's actual body/hook operands,
// including '--' literals, while predicate/option hyphens remain ordinary data.
func TestExecutionStdinOperands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"wait", "--match-text", "-"}, false},
		{[]string{"watch", "--from", "-"}, false},
		{[]string{"post", "room", "--topic", "root", "-"}, true},
		{[]string{"post", "room", "--topic", "root", "--", "-"}, true},
		{[]string{"post", "room", "--topic", "-", "literal"}, false},
		{[]string{"topic", "new", "room", "slug", "Title", "--body", "-"}, true},
		{[]string{"attach", "room", "file", "--topic", "root", "-"}, true},
		{[]string{"hook", "prompt", "--runtime", "claude"}, true},
	} {
		p, err := Authorize(tc.args, "identity")
		if err != nil || p.Stdin != tc.want {
			t.Fatal("stdin operand classification differs from command contract")
		}
	}
}

func acknowledgeOutput(t *testing.T, c *net.UnixConn, p packet) {
	t.Helper()
	b, err := json.Marshal(packet{Type: "output_ack", ID: p.ID, Batch: p.Batch, Identity: p.Identity})
	if err != nil || writeFrame(c, b) != nil {
		t.Fatal("output acknowledgement failed")
	}
}
