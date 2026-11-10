//go:build linux || darwin

package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const MaxConnections = 64
const handshakeTimeout = 5 * time.Second
const idleTimeout = 5 * time.Second
const writeTimeout = time.Second
const refusalLine = "daemon: refused\n"

var refusalFrame = []byte(`{"type":"refused"}`)
var successFrame = []byte(`{"type":"authenticated"}`)

func validSocket(path string) bool {
	return ValidateSocketPath(path) == nil && filepath.Clean(path) == path
}

type server struct {
	home             string
	log              io.Writer
	mu               sync.Mutex
	conns            map[*net.UnixConn]bool
	peer             func(*net.UnixConn) (uint32, error)
	handshake, idle  time.Duration
	handler          Handler
	executionIdle    time.Duration
	keepalive        time.Duration
	ctx              context.Context
	active           map[string]int
	invocations      int
	sink             *logSink
	permissions      func(*os.File, string, os.FileInfo) error
	socketInfo       func(string) (os.FileInfo, error)
	accept           func(*net.UnixListener) (*net.UnixConn, error)
	legacyHello      bool                          // fixture seam for an endpoint predating session evidence
	frameWrite       func(io.Writer, []byte) error // private transport-boundary fixture
	keepaliveTicks   <-chan time.Time
	keepaliveAttempt func()
	serviceReady     func() error                         // service heartbeat starts only after binding
	requireRunDir    bool                                 // a service never recreates the root-prepared dir
	runDirectory     func(string, bool) (*os.File, error) // root-path fixture seam
}

func (s *server) refuse(c *net.UnixConn) {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = writeFrame(c, refusalFrame)
	s.sink.write(refusalLine)
}

func parseHello(data []byte) (station, identity string, mac []byte, err error) {
	fields, err := object(data, map[string]bool{"type": true, "station": true, "identity": true, "mac": true, "capabilities": true})
	if err != nil || (len(fields) != 4 && len(fields) != 5) {
		return "", "", nil, errRefused
	}
	if fields["capabilities"] != nil && !sessionCapabilities(fields["capabilities"]) {
		return "", "", nil, errRefused
	}
	var kind, encoded string
	if json.Unmarshal(fields["type"], &kind) != nil || kind != "hello" {
		return "", "", nil, errRefused
	}
	if json.Unmarshal(fields["station"], &station) != nil || !localName.MatchString(station) {
		return "", "", nil, errRefused
	}
	if json.Unmarshal(fields["identity"], &identity) != nil || ValidateIdentity(identity) != nil {
		return "", "", nil, errRefused
	}
	if json.Unmarshal(fields["mac"], &encoded) != nil {
		return "", "", nil, errRefused
	}
	mac, err = hex.DecodeString(encoded)
	if err != nil || len(mac) != sha256.Size || hex.EncodeToString(mac) != encoded {
		return "", "", nil, errRefused
	}
	return station, identity, mac, nil
}

func response(secret, nonce []byte, station, identity string) []byte {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write(nonce)
	_, _ = m.Write([]byte(station))
	_, _ = m.Write([]byte(identity))
	return m.Sum(nil)
}

func (s *server) handle(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(s.handshake))
	uid, peerErr := s.peer(c)
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		s.refuse(c)
		return
	}
	challenge, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Nonce string `json:"nonce"`
	}{"challenge", hex.EncodeToString(nonce[:])})
	if writeFrame(c, challenge) != nil {
		s.refuse(c)
		return
	}
	data, err := readFrame(c)
	if err != nil {
		s.refuse(c)
		return
	}
	station, identity, mac, err := parseHello(data)
	if err != nil {
		s.refuse(c)
		return
	}
	fields, _ := object(data, nil)
	offered := fields["capabilities"] != nil
	if offered && s.legacyHello {
		s.refuse(c)
		return
	}
	e, found := lookup(s.home, station)
	// Unknown stations still perform the same MAC operation with a dummy key.
	key := e.Secret
	if !found {
		key = make([]byte, StationKeyLen)
	}
	matches := hmac.Equal(response(key, nonce[:], station, identity), mac)
	if !matches || !found || peerErr != nil || uid != e.UID || identity != e.Identity {
		s.refuse(c)
		return
	}
	// The registry identity is the sole execution binding.
	advertised := offered && s.handler != nil
	serverHello := successFrame
	if advertised {
		serverHello = []byte(`{"type":"authenticated","capabilities":["session-evidence/1"]}`)
	}
	if writeFrame(c, serverHello) != nil {
		s.refuse(c)
		return
	}
	if s.handler != nil {
		s.execute(c, station, e.Identity, advertised)
		return
	}
	_ = c.SetDeadline(time.Now().Add(s.idle))
	// Nothing is allowed after authentication. An orderly disconnect is clean;
	// any second frame/partial frame is uniformly refused. Idle sessions expire.
	_, err = readFrame(c)
	if err == io.EOF {
		return
	}
	if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
		return
	}
	s.refuse(c)
}

// Serve authenticates only, with a fixed connection/allocation/deadline budget.
// It does not load a Matrix identity, read credentials, or execute commands.
func Serve(ctx context.Context, home, socket string, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	s := &server{home: home, log: log, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: handshakeTimeout, idle: idleTimeout}
	return s.serve(ctx, socket, nil)
}

func (s *server) serve(ctx context.Context, socket string, ready func()) error {
	s.ctx = ctx
	s.sink = newLogSink(s.log)
	defer s.sink.close()
	if !validSocket(socket) {
		return errRefused
	}
	if err := withRegistry(s.home, false, func(_ *os.File, _ registry) error { return nil }); err != nil {
		return err
	}
	openRunDir := s.runDirectory
	if openRunDir == nil {
		openRunDir = func(path string, create bool) (*os.File, error) { return openDirectoryMode(path, create, 0711) }
	}
	dir, err := openRunDir(filepath.Dir(socket), !s.requireRunDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	lock, err := privateFile(dir, ".daemon.lock", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return err
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return errRefused
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	// The library endpoint never adopts an existing path. A supervised restart
	// may reclaim only its own dead socket after acquiring the exclusive lock.
	var existing unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), filepath.Base(socket), &existing, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && s.requireRunDir {
		if reclaimServiceSocket(dir, socket, &existing) != nil {
			return errRefused
		}
		err = unix.Fstatat(int(dir.Fd()), filepath.Base(socket), &existing, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != unix.ENOENT {
		return errRefused
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return errRefused
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	// Pin cleanup through the held directory before any pathname lookup can
	// refuse. A failed Lstat (for example after an ancestor rename) must not
	// leave our just-bound socket behind.
	var created unix.Stat_t
	name := filepath.Base(socket)
	if unix.Fstatat(int(dir.Fd()), name, &created, unix.AT_SYMLINK_NOFOLLOW) != nil || created.Mode&unix.S_IFMT != unix.S_IFSOCK || created.Uid != uint32(os.Geteuid()) {
		return errRefused
	}
	defer func() {
		if sameSocketInodeAt(dir, name, uint64(created.Dev), created.Ino) {
			_ = unix.Unlinkat(int(dir.Fd()), name, 0)
		}
	}()
	socketInfo := s.socketInfo
	if socketInfo == nil {
		socketInfo = os.Lstat
	}
	info, err := socketInfo(socket)
	if err != nil {
		return errRefused
	}
	stamp, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stamp.Dev) != uint64(created.Dev) || stamp.Ino != created.Ino {
		return errRefused
	}
	permissions := s.permissions
	if permissions == nil {
		permissions = setSocketPermissions
	}
	if permissions(dir, socket, info) != nil {
		return errRefused
	}

	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		listener.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	defer stop()
	if s.serviceReady != nil {
		if err := s.serviceReady(); err != nil {
			return err
		}
	}
	if ready != nil {
		ready()
	}
	defer wg.Wait()
	for {
		accept := s.accept
		if accept == nil {
			accept = func(l *net.UnixListener) (*net.UnixConn, error) { return l.AcceptUnix() }
		}
		c, err := accept(listener)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if transientAccept(err) {
				timer := time.NewTimer(AcceptBackoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
				continue
			}
			return errRefused
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			c.Close()
			return nil
		}
		full := len(s.conns) >= MaxConnections
		if !full {
			s.conns[c] = true
		}
		s.mu.Unlock()
		if full {
			if s.handler != nil {
				s.diagnostic(c, Busy, "")
			} else {
				s.refuse(c)
			}
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }()
			s.handle(c)
		}()
	}
}

func reclaimServiceSocket(dir *os.File, path string, st *unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) || st.Mode&07777 != 0666 || st.Nlink != 1 {
		return errRefused
	}
	probe, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if probe != nil {
		probe.Close()
	}
	if !errors.Is(err, unix.ECONNREFUSED) || !sameSocketInodeAt(dir, filepath.Base(path), uint64(st.Dev), st.Ino) {
		return errRefused
	}
	if unix.Unlinkat(int(dir.Fd()), filepath.Base(path), 0) != nil {
		return errRefused
	}
	return nil
}

// sameSocketAt pins pathname operations to the held directory and the socket
// inode just created. A renamed/replaced ancestor cannot redirect a chmod or
// cleanup into a different directory or a private file.
func sameSocketAt(dir *os.File, name string, info os.FileInfo) bool {
	original, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	return sameSocketInodeAt(dir, name, uint64(original.Dev), original.Ino)
}
func sameSocketInodeAt(dir *os.File, name string, device, inode uint64) bool {
	var current unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return false
	}
	return current.Mode&unix.S_IFMT == unix.S_IFSOCK && current.Uid == uint32(os.Geteuid()) && uint64(current.Dev) == device && current.Ino == inode
}

func setSocketPermissions(dir *os.File, path string, info os.FileInfo) error {
	name := filepath.Base(path)
	if !sameSocketAt(dir, name, info) {
		return errRefused
	}
	// The held 0711 directory is writable only by the trusted Daemon UID.
	if unix.Fchmodat(int(dir.Fd()), name, 0666, 0) != nil {
		return errRefused
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errRefused
	}
	return nil
}

func transientAccept(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EINTR)
}
