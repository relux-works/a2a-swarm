//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"
)

const executionIdle = StationReadDeadline

type packet struct {
	Type     string     `json:"type"`
	ID       string     `json:"id"`
	Batch    string     `json:"batch,omitempty"`
	Identity string     `json:"identity,omitempty"`
	Data     []byte     `json:"data,omitempty"`
	Code     int        `json:"code,omitempty"`
	Class    Diagnostic `json:"class,omitempty"`
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

type stream struct {
	c                   *net.UnixConn
	mu                  sync.Mutex
	outputMu            sync.Mutex
	outputBatch         string
	outputAck           chan struct{}
	id, identity, batch string
	ack                 chan struct{}
	cancel              context.CancelFunc
	ctx                 context.Context
	idle                time.Duration
	frameWrite          func(io.Writer, []byte) error
	hook                bool
	hookExit            bool
}

func (s *stream) send(p packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return errRefused
	}
	s.c.SetWriteDeadline(time.Now().Add(s.idle))
	b, err := json.Marshal(p)
	if p.Type == "exit" || p.Type == "complete" {
		b, err = json.Marshal(struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Code int    `json:"code"`
		}{p.Type, p.ID, p.Code})
	}
	if err == nil {
		if s.frameWrite != nil {
			err = s.frameWrite(s.c, b)
		} else {
			err = writeFrame(s.c, b)
		}
	}
	if err != nil {
		s.cancel()
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			return IdleTimeout
		}
	}
	return err
}

type output struct {
	s    *stream
	kind string
}

func (w output) Write(b []byte) (int, error) {
	w.s.outputMu.Lock()
	defer w.s.outputMu.Unlock()
	n := 0
	for len(b) > 0 {
		k := min(len(b), OutputChunkBytes)
		if err := w.s.chunk(w.kind, b[:k]); err != nil {
			return n, err
		}
		b = b[k:]
		n += k
	}
	return n, nil
}

// At most one bounded output chunk awaits local write/flush acknowledgement.
// This is flow control only; the separate delivery ACK remains the receipt gate.
func (s *stream) chunk(kind string, data []byte) error {
	batch := randomID()
	if batch == "" {
		return errRefused
	}
	s.mu.Lock()
	s.outputBatch = batch
	s.outputAck = make(chan struct{})
	ack := s.outputAck
	_ = s.c.SetReadDeadline(time.Now().Add(s.idle))
	s.mu.Unlock()
	if err := s.send(packet{Type: kind, ID: s.id, Data: data, Batch: batch, Identity: s.identity}); err != nil {
		return err
	}
	select {
	case <-ack:
		if s.ctx.Err() != nil {
			return errRefused
		}
		return nil
	case <-s.ctx.Done():
		// receive sends the idle diagnostic before canceling on a missed output ACK.
		return errRefused
	}
}

func (s *stream) delivered() error {
	batch := randomID()
	if batch == "" {
		return errRefused
	}
	s.mu.Lock()
	s.batch = batch
	s.ack = make(chan struct{})
	_ = s.c.SetReadDeadline(time.Now().Add(s.idle))
	ack := s.ack
	s.mu.Unlock()
	if err := s.send(packet{Type: "delivery", ID: s.id, Batch: batch, Identity: s.identity}); err != nil {
		return err
	}
	// A hook's output is complete at its delivery barrier. Send the validated
	// exit boundary before waiting for the receipt; the station buffers output
	// until this boundary, writes/flushes real stdout, and only then ACKs.
	if s.hook {
		if s.hookExit {
			return errRefused
		}
		s.hookExit = true
		if err := s.send(packet{Type: "exit", ID: s.id, Code: 0}); err != nil {
			return err
		}
	}

	select {
	case <-ack:
		if s.ctx.Err() != nil {
			return errRefused
		}
		return nil
	case <-s.ctx.Done():
		return errRefused
	}
}
func (s *stream) receive() {
	defer s.cancel()
	for {
		b, err := readFrame(s.c)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				_ = s.send(packet{Type: "error", ID: s.id, Class: IdleTimeout})
			}
			return
		}
		f, err := object(b, map[string]bool{"type": true, "id": true, "batch": true, "identity": true})
		var p packet
		if err != nil || len(f) != 4 || json.Unmarshal(b, &p) != nil {
			return
		}
		s.mu.Lock()
		bound := s.ctx.Err() == nil && p.ID == s.id && p.Identity == s.identity
		valid := bound && p.Type == "ack" && s.batch != "" && p.Batch == s.batch
		outputValid := bound && p.Type == "output_ack" && s.outputBatch != "" && p.Batch == s.outputBatch
		if valid {
			s.batch = ""
			s.c.SetReadDeadline(time.Time{})
			close(s.ack)
		}
		if outputValid {
			s.outputBatch = ""
			s.c.SetReadDeadline(time.Time{})
			close(s.outputAck)
		}
		valid = valid || outputValid
		s.mu.Unlock()
		if !valid {
			return
		}
	}
}
func (s *server) execute(c *net.UnixConn, station, identity string, sessionAdvertised bool) {
	_ = c.SetDeadline(time.Now().Add(s.idle))
	data, err := readFrame(c)
	if err == io.EOF {
		return
	}
	if err != nil {
		s.refuse(c)
		return
	}
	r, err := parseRequest(data)
	if err != nil {
		if err == NotAllowed && validID(r.ID) {
			s.diagnostic(c, NotAllowed, r.ID)
			return
		}
		s.refuse(c)
		return
	}
	if r.Session != "" && !sessionAdvertised {
		s.diagnostic(c, NotAllowed, r.ID)
		return
	}
	plan, authErr := Authorize(r.Argv, identity)
	if err := authErr; err != nil {
		class := NotAllowed
		if err == IdentityMismatch {
			class = IdentityMismatch
		}
		s.diagnostic(c, class, r.ID)
		return
	}
	if !s.acquire(station) {
		s.diagnostic(c, Busy, r.ID)
		return
	}
	defer s.release(station)
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	idle := s.executionIdle
	if idle == 0 {
		idle = executionIdle
	}
	st := &stream{c: c, id: r.ID, identity: identity, cancel: cancel, ctx: ctx, idle: idle, hook: plan.Command == "hook"}
	st.frameWrite = s.writePacketFrame
	_ = c.SetWriteDeadline(time.Now().Add(idle))
	_ = c.SetReadDeadline(time.Time{})
	done := make(chan struct{})
	go func() { defer close(done); st.receive() }()
	keepDone := make(chan struct{})
	interval := s.keepalive
	if interval == 0 {
		interval = KeepaliveInterval
	}
	go func() {
		defer close(keepDone)
		ticks := s.keepaliveTicks
		if ticks == nil {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			ticks = ticker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				if s.keepaliveAttempt != nil {
					s.keepaliveAttempt()
				}
				if st.send(packet{Type: "keepalive", ID: r.ID}) != nil {
					return
				}
			}
		}
	}()
	closeOnCancel := context.AfterFunc(ctx, func() { c.Close() })
	defer closeOnCancel()
	defer func() { cancel(); c.Close(); <-done; <-keepDone }()
	defer func() {
		if recover() != nil {
			_ = st.send(packet{Type: "error", ID: r.ID, Class: Unavailable})
		}
	}()
	if plan.Command == "daemon" {
		_, _ = fmt.Fprintf(output{st, "stdout"}, "log_dropped: %d\n", s.sink.dropped.Load())
		_ = st.send(packet{Type: "exit", ID: r.ID, Code: 0})
		return
	}
	var failure Diagnostic
	var failureMu sync.Mutex
	code := s.handler(ctx, Invocation{Identity: identity, Request: r, Stdout: output{st, "stdout"}, Stderr: output{st, "stderr"}, Delivered: st.delivered, Refuse: func(d Diagnostic) int {
		failureMu.Lock()
		failure = Class(d)
		failureMu.Unlock()
		return 1
	}})
	failureMu.Lock()
	closedFailure := failure
	failureMu.Unlock()
	if closedFailure != "" {
		_ = st.send(packet{Type: "error", ID: r.ID, Class: closedFailure})
		return
	}
	kind := "exit"
	if st.hookExit {
		kind = "complete"
	}
	_ = st.send(packet{Type: kind, ID: r.ID, Code: code})
}

// ServeWithHandler adds one invocation to the C1 authenticated endpoint. A nil
// handler preserves the authentication-only contract used by C1 callers.
func ServeWithHandler(ctx context.Context, home, socket string, log io.Writer, handler Handler) error {
	if log == nil {
		log = io.Discard
	}
	s := &server{home: home, log: log, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: handshakeTimeout, idle: idleTimeout, handler: handler}
	return s.serve(ctx, socket, nil)
}

// ServeService preserves the C5 transport and shutdown path. The root-prepared
// run directory must already exist; ready is called with the serve lock held.
func ServeService(ctx context.Context, home, socket string, log io.Writer, handler Handler, ready func() error) error {
	if log == nil {
		log = io.Discard
	}
	s := &server{home: home, log: log, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: handshakeTimeout, idle: idleTimeout, handler: handler, serviceReady: ready, requireRunDir: true}
	return s.serve(ctx, socket, nil)
}

// Client authenticates using the unchanged nonce transcript, sends exactly one
// request, and ACKs delivery only after each local output write and flush works.
func Client(ctx context.Context, socket, station, identity string, key []byte, r Request, stdout, stderr io.Writer) (int, error) {
	return client(ctx, socket, station, identity, key, r, stdout, stderr, StationReadDeadline)
}
func client(ctx context.Context, socket, station, identity string, key []byte, r Request, stdout, stderr io.Writer, readDeadline time.Duration) (int, error) {
	if r.Session != "" && !ValidSession(r.Session) {
		return 1, NotAllowed
	}
	code, err := clientAttempt(ctx, socket, station, identity, key, r, stdout, stderr, readDeadline, r.Session != "")
	if err == errLegacyHello && ctx.Err() == nil {
		// Old daemons strictly reject an extended station hello before executing
		// anything. Retry once with the original hello and omit session evidence.
		return clientAttempt(ctx, socket, station, identity, key, r, stdout, stderr, readDeadline, false)
	}
	return code, err
}

var errLegacyHello = errors.New("daemon: legacy hello")

func clientAttempt(ctx context.Context, socket, station, identity string, key []byte, r Request, stdout, stderr io.Writer, readDeadline time.Duration, offerSession bool) (int, error) {
	if len(key) != StationKeyLen || !localName.MatchString(station) || ValidateIdentity(identity) != nil || ValidateSocketPath(socket) != nil {
		return 1, EnrollmentInvalid
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return 1, Unavailable
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	b, err := readFrame(conn)
	if err != nil {
		return 1, Unavailable
	}
	if d := diagnosticFrame(b, ""); d != nil {
		return 1, d
	}
	f, err := object(b, map[string]bool{"type": true, "nonce": true})
	var kind, nonce string
	if err != nil || len(f) != 2 || json.Unmarshal(f["type"], &kind) != nil || kind != "challenge" || json.Unmarshal(f["nonce"], &nonce) != nil {
		return 1, Unavailable
	}
	raw, err := hex.DecodeString(nonce)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != nonce {
		return 1, Unavailable
	}
	helloFields := map[string]any{"type": "hello", "station": station, "identity": identity, "mac": hex.EncodeToString(response(key, raw, station, identity))}
	if offerSession {
		helloFields["capabilities"] = []string{SessionEvidenceCapability}
	}
	hello, _ := json.Marshal(helloFields)
	if writeFrame(conn, hello) != nil {
		return 1, Unavailable
	}
	b, err = readFrame(conn)
	if err != nil {
		return 1, Unavailable
	}
	if offerSession && bytes.Equal(b, refusalFrame) {
		return 1, errLegacyHello
	}
	auth, authErr := object(b, map[string]bool{"type": true, "capabilities": true})
	var authenticated string
	if authErr != nil || json.Unmarshal(auth["type"], &authenticated) != nil || authenticated != "authenticated" || (len(auth) != 1 && len(auth) != 2) {
		return 1, EnrollmentInvalid
	}
	advertised := auth["capabilities"] != nil
	if advertised && (!offerSession || !sessionCapabilities(auth["capabilities"])) {
		return 1, EnrollmentInvalid
	}
	if !advertised {
		r.Session = ""
	}
	r.Type = "request"
	r.ID = randomID()
	if r.ID == "" {
		return 1, Unavailable
	}
	if r.Files == nil {
		r.Files = map[int]InputFile{}
	}
	b, err = json.Marshal(r)
	if err != nil || writeFrame(conn, b) != nil {
		return 1, Unavailable
	}
	plan, _ := Authorize(r.Argv, identity)
	hook := plan.Command == "hook"
	var hookOut, hookErr bytes.Buffer
	var delivery *packet
	committed := false
	for {
		_ = conn.SetDeadline(time.Now().Add(readDeadline))
		b, err = readFrame(conn)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				return 1, IdleTimeout
			}
			return 1, Unavailable
		}
		if d := diagnosticFrame(b, r.ID); d != nil {
			return 1, d
		}
		fields, e := object(b, map[string]bool{"type": true, "id": true, "data": true, "batch": true, "identity": true, "code": true})
		var p packet
		if e != nil || json.Unmarshal(b, &p) != nil || p.ID != r.ID {
			return 1, Unavailable
		}
		switch p.Type {
		case "keepalive":
			if len(fields) != 2 {
				return 1, Unavailable
			}
		case "stdout", "stderr":
			if len(fields) != 5 || len(p.Data) == 0 || len(p.Data) > OutputChunkBytes || !validID(p.Batch) || p.Identity != identity {
				return 1, Unavailable
			}
			w := stdout
			if p.Type == "stderr" {
				w = stderr
			}
			if hook {
				if committed {
					return 1, Unavailable
				}
				buffer := &hookOut
				if p.Type == "stderr" {
					buffer = &hookErr
				}
				if buffer.Len()+len(p.Data) > HookOutputBytes {
					return 1, Unavailable
				}
				w = buffer
			}
			if err = localOutput(w, p.Data); err != nil {
				return 1, Unavailable
			}
			ack, _ := json.Marshal(packet{Type: "output_ack", ID: r.ID, Batch: p.Batch, Identity: identity})
			if writeFrame(conn, ack) != nil {
				return 1, Unavailable
			}
		case "delivery":
			if len(fields) != 4 || !validID(p.Batch) || p.Identity != identity {
				return 1, Unavailable
			}
			if hook {
				if delivery != nil || committed {
					return 1, Unavailable
				}
				delivery = &p
				continue
			}
			if err = flush(stdout); err != nil {
				return 1, Unavailable
			}
			ack, _ := json.Marshal(packet{Type: "ack", ID: r.ID, Batch: p.Batch, Identity: identity})
			if writeFrame(conn, ack) != nil {
				return 1, Unavailable
			}
		case "exit", "complete":
			if len(fields) != 3 || fields["code"] == nil || string(fields["code"]) == "null" || p.Code < 0 || p.Code > 255 {
				return 1, Unavailable
			}
			if !hook {
				if p.Type != "exit" {
					return 1, Unavailable
				}
				return p.Code, nil
			}
			if committed {
				if p.Type != "complete" {
					return 1, Unavailable
				}
				return p.Code, nil
			}
			if p.Type != "exit" {
				return 1, Unavailable
			}
			if p.Code != 0 {
				return p.Code, nil
			}
			if ctx.Err() != nil {
				return 1, IdleTimeout
			}
			if hookOut.Len() > 0 {
				if err := localOutput(stdout, hookOut.Bytes()); err != nil {
					return 1, Unavailable
				}
			} else if delivery != nil {
				if err := flush(stdout); err != nil {
					return 1, Unavailable
				}
			}
			if delivery != nil {
				ack, _ := json.Marshal(packet{Type: "ack", ID: r.ID, Batch: delivery.Batch, Identity: identity})
				if ctx.Err() != nil || writeFrame(conn, ack) != nil {
					return 1, Unavailable
				}
				committed = true
			}
			if hookErr.Len() > 0 {
				if err := localOutput(stderr, hookErr.Bytes()); err != nil {
					return 1, Unavailable
				}
			}
			if delivery == nil {
				return p.Code, nil
			}
		default:
			return 1, Unavailable
		}
	}
}
func flush(w io.Writer) error {
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}
func localOutput(w io.Writer, b []byte) error {
	if w == nil {
		return errRefused
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = flush(w)
	}
	return err
}

// IdentityHome verifies an existing Daemon-owned private config directory.
func IdentityHome(home, identity string) (string, error) {
	if ValidateIdentity(identity) != nil || identity == "." || identity == ".." {
		return "", errRefused
	}
	root := filepath.Join(home, "identities", identity)
	f, err := openDirectory(root, false)
	if err != nil {
		return "", errRefused
	}
	f.Close()
	return root, nil
}

// diagnosticFrame admits only closed classes and the current invocation binding.
func diagnosticFrame(b []byte, id string) error {
	fields, err := object(b, map[string]bool{"type": true, "class": true, "id": true})
	var p packet
	if err != nil || json.Unmarshal(b, &p) != nil || p.Type != "error" || !validDiagnostic(p.Class) {
		return nil
	}
	if id == "" {
		if len(fields) != 2 {
			return nil
		}
	} else if len(fields) != 3 || p.ID != id {
		return nil
	}
	return p.Class
}
func (s *server) diagnostic(c *net.UnixConn, class Diagnostic, id string) {
	p := map[string]string{"type": "error", "class": string(class)}
	if id != "" {
		p["id"] = id
	}
	b, _ := json.Marshal(p)
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = s.writePacketFrame(c, b)
}

func (s *server) writePacketFrame(w io.Writer, b []byte) error {
	if s.frameWrite != nil {
		return s.frameWrite(w, b)
	}
	return writeFrame(w, b)
}
func (s *server) acquire(station string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = map[string]int{}
	}
	if s.invocations >= MaxInvocations || s.active[station] >= MaxStationInvocations {
		return false
	}
	s.invocations++
	s.active[station]++
	return true
}
func (s *server) release(station string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invocations--
	s.active[station]--
	if s.active[station] == 0 {
		delete(s.active, station)
	}
}
