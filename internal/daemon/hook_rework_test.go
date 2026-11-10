//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fabricated endpoint loses/corrupts the exit after delivery, then retries
// the same unconsumed doorbell. Output ACKs never count as receipt ACKs.
func TestDaemonHookExitBeforeACK(t *testing.T) {
	for _, mode := range []string{"lost", "malformed", "nonzero", "valid"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(privateTemp(t), "c.sock")
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan error, 1)
			var receipts atomic.Int32
			serveAttempt := func(mode string) {
				c, err := l.AcceptUnix()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(LockReleaseBound))
				fail := func(e error) { done <- e }
				send := func(p packet) error {
					b, _ := json.Marshal(p)
					if p.Type == "exit" || p.Type == "complete" {
						b, _ = json.Marshal(map[string]any{"type": p.Type, "id": p.ID, "code": p.Code})
					}
					return writeFrame(c, b)
				}
				if err := writeFrame(c, []byte(`{"type":"challenge","nonce":"`+hex.EncodeToString(make([]byte, 32))+`"}`)); err != nil {
					fail(err)
					return
				}
				if _, err := readFrame(c); err != nil {
					fail(err)
					return
				}
				if err := writeFrame(c, successFrame); err != nil {
					fail(err)
					return
				}
				b, err := readFrame(c)
				if err != nil {
					fail(err)
					return
				}
				r, err := parseRequest(b)
				if err != nil {
					fail(err)
					return
				}
				batch := strings.Repeat("a", 32)
				if err := send(packet{Type: "stdout", ID: r.ID, Identity: "identity", Batch: batch, Data: []byte("doorbell\n")}); err != nil {
					fail(err)
					return
				}
				b, err = readFrame(c)
				var ack packet
				if err != nil || json.Unmarshal(b, &ack) != nil || ack.Type != "output_ack" {
					fail(errors.New("missing flow ACK"))
					return
				}
				if err := send(packet{Type: "delivery", ID: r.ID, Identity: "identity", Batch: batch}); err != nil {
					fail(err)
					return
				}
				switch mode {
				case "lost":
					c.Close()
					done <- nil
					return
				case "malformed":
					writeFrame(c, []byte(`{"type":"exit","id":"`+r.ID+`","code":null}`))
				case "nonzero":
					send(packet{Type: "exit", ID: r.ID, Code: 1})
				case "valid":
					send(packet{Type: "exit", ID: r.ID, Code: 0})
				}
				b, err = readFrame(c)
				if mode == "valid" {
					if err != nil || json.Unmarshal(b, &ack) != nil || ack.Type != "ack" || ack.ID != r.ID || ack.Batch != batch || ack.Identity != "identity" {
						fail(errors.New("missing receipt ACK"))
						return
					}
					receipts.Add(1)
					send(packet{Type: "complete", ID: r.ID, Code: 0})
				} else if err == nil {
					fail(errors.New("ACK before valid exit"))
					return
				}
				done <- nil
			}
			go func() {
				serveAttempt(mode)
				if mode != "valid" {
					serveAttempt("valid")
				}
			}()
			var out, diag bytes.Buffer
			code, err := Client(context.Background(), path, "station", "identity", make([]byte, 32), Request{Argv: []string{"hook", "prompt"}}, &out, &diag)
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(LockReleaseBound):
				t.Fatal("endpoint stuck")
			}
			if mode == "valid" {
				if err != nil || code != 0 || out.String() != "doorbell\n" || receipts.Load() != 1 {
					t.Fatal("valid neighbor", code, err, receipts.Load())
				}
			} else {
				wantErr := error(Unavailable)
				if mode == "nonzero" {
					wantErr = nil
				}
				if err != wantErr || code != 1 || out.Len() != 0 || receipts.Load() != 0 {
					t.Fatal("failed exit consumed output", code, err, receipts.Load())
				}
				// The same endpoint presents the still-unconsumed doorbell again.
				out.Reset()
				code, err = Client(context.Background(), path, "station", "identity", make([]byte, 32), Request{Argv: []string{"hook", "prompt"}}, &out, &diag)
				select {
				case e := <-done:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(LockReleaseBound):
					t.Fatal("re-ring endpoint stuck")
				}
				if err != nil || code != 0 || out.String() != "doorbell\n" || receipts.Load() != 1 {
					t.Fatal("lost exit prevented re-ring", code, err, receipts.Load())
				}
			}
		})
	}
}
