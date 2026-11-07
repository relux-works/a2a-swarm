package hostnotification

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type Client struct {
	stream  *net.UnixConn
	binding config.HostNotificationBinding
	timeout time.Duration
	mu      sync.Mutex
	closed  atomic.Bool
}

// New takes ownership only on success. The actual admitted manager must supply
// this endpoint and binding; a STREAM type check does not authenticate custody.
// No path dial, endpoint environment, backend fallback or native API is used.
func New(stream *net.UnixConn, binding config.HostNotificationBinding, callTimeoutMS int) (*Client, error) {
	if stream == nil || binding.Validate() != nil || callTimeoutMS < 1 || callTimeoutMS > 60000 {
		return nil, ErrProtocol
	}
	raw, err := stream.SyscallConn()
	if err != nil {
		return nil, ErrProtocol
	}
	kind := 0
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		kind, optionErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TYPE)
	})
	if err != nil || optionErr != nil || kind != syscall.SOCK_STREAM {
		return nil, ErrProtocol
	}
	return &Client{stream: stream, binding: binding, timeout: time.Duration(callTimeoutMS) * time.Millisecond}, nil
}

// NewRequestID is separate from the source notice token. Persist the ID with
// UNKNOWN before passing it to the one trigger exchange; never reuse it.
func NewRequestID() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", ErrProtocol
	}
	return "request-" + hex.EncodeToString(nonce[:]), nil
}
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.stream.Close()
}

// Call performs one bounded exchange with no pipeline or retry. The owning
// source adapter must durably Begin before trigger and retain UNKNOWN on every
// error. A successful refusal still says nothing about an earlier attempt.
func (c *Client) Call(ctx context.Context, requestID, method, notice string) (Response, error) {
	var empty Response
	if ctx == nil {
		return empty, ErrProtocol
	}
	request := Request{Contract: Contract, RequestID: requestID, Method: method, Binding: c.binding, NoticeID: notice}
	frame, err := EncodeRequest(request)
	if err != nil {
		return empty, err
	}
	if !c.mu.TryLock() {
		return empty, ErrBusy
	}
	defer c.mu.Unlock()
	if c.closed.Load() {
		return empty, ErrUnresolved
	}
	deadline := time.Now().Add(c.timeout)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	if ctx.Err() != nil || c.stream.SetDeadline(deadline) != nil {
		_ = c.Close()
		return empty, ErrUnresolved
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	fail := func() (Response, error) { _ = c.Close(); return empty, ErrUnresolved }
	for len(frame) > 0 {
		n, err := c.stream.Write(frame)
		if err != nil || n <= 0 {
			return fail()
		}
		frame = frame[n:]
	}
	// Read one LF frame without buffered lookahead into the next exchange. A
	// malformed/oversized frame closes the endpoint; never scan for resync.
	response := make([]byte, 0, MaxFrame)
	var one [1]byte
	for len(response) < MaxFrame {
		if _, err := io.ReadFull(c.stream, one[:]); err != nil {
			return fail()
		}
		response = append(response, one[0])
		if one[0] != '\n' {
			continue
		}
		r, err := DecodeResponse(response, request)
		if err != nil {
			return fail()
		}
		return r, nil
	}
	return fail()
}
