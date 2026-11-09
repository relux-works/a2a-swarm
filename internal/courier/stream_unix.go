//go:build linux || darwin

package courier

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"
)

const executionIdle = 60 * time.Second

type packet struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Batch    string `json:"batch,omitempty"`
	Identity string `json:"identity,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Code     int    `json:"code,omitempty"`
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
	id, identity, batch string
	ack                 chan struct{}
	cancel              context.CancelFunc
	ctx                 context.Context
	idle                time.Duration
}

func (s *stream) send(p packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return errRefused
	}
	s.c.SetWriteDeadline(time.Now().Add(s.idle))
	s.c.SetReadDeadline(time.Now().Add(s.idle))
	b, err := json.Marshal(p)
	if p.Type == "exit" {
		b, err = json.Marshal(struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Code int    `json:"code"`
		}{p.Type, p.ID, p.Code})
	}
	if err == nil {
		err = writeFrame(s.c, b)
	}
	if err != nil {
		s.cancel()
	}
	return err
}

type output struct {
	s    *stream
	kind string
}

func (w output) Write(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		k := min(len(b), 16384)
		if err := w.s.send(packet{Type: w.kind, ID: w.s.id, Data: b[:k]}); err != nil {
			return n, err
		}
		b = b[k:]
		n += k
	}
	return n, nil
}
func (s *stream) delivered() error {
	batch := randomID()
	if batch == "" {
		return errRefused
	}
	s.mu.Lock()
	s.batch = batch
	s.ack = make(chan struct{})
	ack := s.ack
	s.mu.Unlock()
	if err := s.send(packet{Type: "delivery", ID: s.id, Batch: batch, Identity: s.identity}); err != nil {
		return err
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
			return
		}
		f, err := object(b, map[string]bool{"type": true, "id": true, "batch": true, "identity": true})
		var p packet
		if err != nil || len(f) != 4 || json.Unmarshal(b, &p) != nil {
			return
		}
		s.mu.Lock()
		valid := p.Type == "ack" && p.ID == s.id && p.Identity == s.identity && s.batch != "" && p.Batch == s.batch
		if valid {
			s.batch = ""
			close(s.ack)
		}
		s.mu.Unlock()
		if !valid {
			return
		}
		s.c.SetReadDeadline(time.Now().Add(s.idle))
	}
}
func (s *server) execute(c *net.UnixConn, identity string) {
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
		s.refuse(c)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := s.executionIdle
	if idle == 0 {
		idle = executionIdle
	}
	st := &stream{c: c, id: r.ID, identity: identity, cancel: cancel, ctx: ctx, idle: idle}
	_ = c.SetDeadline(time.Now().Add(idle))
	done := make(chan struct{})
	go func() { defer close(done); st.receive() }()
	defer func() { c.Close(); <-done }()
	code := s.handler(ctx, Invocation{Identity: identity, Request: r, Stdout: output{st, "stdout"}, Stderr: output{st, "stderr"}, Delivered: st.delivered})
	_ = st.send(packet{Type: "exit", ID: r.ID, Code: code})
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

// Client authenticates using the unchanged nonce transcript, sends exactly one
// request, and ACKs delivery only after each local output write and flush works.
func Client(ctx context.Context, socket, station, identity string, key []byte, r Request, stdout, stderr io.Writer) (int, error) {
	if len(key) != StationKeyLen || !localName.MatchString(station) || ValidateIdentity(identity) != nil || ValidateSocketPath(socket) != nil {
		return 1, errRefused
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return 1, errRefused
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	b, err := readFrame(conn)
	if err != nil {
		return 1, errRefused
	}
	f, err := object(b, map[string]bool{"type": true, "nonce": true})
	var kind, nonce string
	if err != nil || len(f) != 2 || json.Unmarshal(f["type"], &kind) != nil || kind != "challenge" || json.Unmarshal(f["nonce"], &nonce) != nil {
		return 1, errRefused
	}
	raw, err := hex.DecodeString(nonce)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != nonce {
		return 1, errRefused
	}
	hello, _ := json.Marshal(map[string]string{"type": "hello", "station": station, "identity": identity, "mac": hex.EncodeToString(response(key, raw, station, identity))})
	if writeFrame(conn, hello) != nil {
		return 1, errRefused
	}
	b, err = readFrame(conn)
	if err != nil || string(b) != string(successFrame) {
		return 1, errRefused
	}
	r.Type = "request"
	r.ID = randomID()
	if r.ID == "" {
		return 1, errRefused
	}
	if r.Files == nil {
		r.Files = map[int]InputFile{}
	}
	b, err = json.Marshal(r)
	if err != nil || writeFrame(conn, b) != nil {
		return 1, errRefused
	}
	for {
		_ = conn.SetDeadline(time.Now().Add(executionIdle))
		b, err = readFrame(conn)
		if err != nil {
			return 1, errRefused
		}
		fields, e := object(b, map[string]bool{"type": true, "id": true, "data": true, "batch": true, "identity": true, "code": true})
		var p packet
		if e != nil || json.Unmarshal(b, &p) != nil || p.ID != r.ID {
			return 1, errRefused
		}
		switch p.Type {
		case "stdout", "stderr":
			if len(fields) != 3 || len(p.Data) == 0 {
				return 1, errRefused
			}
			w := stdout
			if p.Type == "stderr" {
				w = stderr
			}
			if err = localOutput(w, p.Data); err != nil {
				return 1, errRefused
			}
		case "delivery":
			if len(fields) != 4 || !validID(p.Batch) || p.Identity != identity {
				return 1, errRefused
			}
			if err = flush(stdout); err != nil {
				return 1, errRefused
			}
			ack, _ := json.Marshal(packet{Type: "ack", ID: r.ID, Batch: p.Batch, Identity: identity})
			if writeFrame(conn, ack) != nil {
				return 1, errRefused
			}
		case "exit":
			if len(fields) != 3 || fields["code"] == nil || string(fields["code"]) == "null" || p.Code < 0 || p.Code > 255 {
				return 1, errRefused
			}
			return p.Code, nil
		default:
			return 1, errRefused
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

// IdentityHome verifies an existing Courier-owned private config directory.
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
