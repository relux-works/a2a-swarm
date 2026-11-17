//go:build linux

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relux-works/a2a-swarm/runorigin"
	"golang.org/x/sys/unix"
)

const OriginBudget = 5 * time.Second

// Reserve response time INSIDE the five-second call budget, never after it.
const originResponseReserve = 100 * time.Millisecond

// OriginConfig is supplied only by protected service configuration. This API is
// deliberately not wired to daemon station flags, credentials or registries.
// ControlDir must already exist under operator-qualified protected ancestors.
type OriginConfig struct {
	ControlDir  string
	Launcher    runorigin.Principal
	Generations runorigin.GenerationAuthority
	Producer    runorigin.Producer // nil in stock upstream; no name/generic fallback
}

// OriginService has one immutable process incarnation and no durable records.
// Create it once at service start; Serve may be invoked only once.
type OriginService struct {
	cfg         OriginConfig
	incarnation string
	started     atomic.Bool
	workers     chan struct{}
	budget      time.Duration
}

func NewOriginService(cfg OriginConfig) (*OriginService, error) {
	return newOriginService(cfg, uint32(os.Geteuid()))
}

// The private constructor accepts the observed service UID for isolated unit
// fixtures; the public entry always supplies the kernel's effective UID.
func newOriginService(cfg OriginConfig, serviceUID uint32) (*OriginService, error) {
	if cfg.Generations == nil || !runorigin.ValidGeneration(cfg.Launcher.Generation) || cfg.Launcher.UID == 0 || cfg.Launcher.UID == serviceUID {
		return nil, runorigin.ErrRefused
	}
	incarnation, err := originRandomID()
	if err != nil {
		return nil, runorigin.ErrUnavailable
	}
	return &OriginService{cfg: cfg, incarnation: incarnation, workers: make(chan struct{}, MaxConnections), budget: OriginBudget}, nil
}

func originRandomID() (string, error) {
	for {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if b != [16]byte{} {
			return hex.EncodeToString(b[:]), nil
		}
	}
}

// originDirectory never creates, chmods, follows links or repairs existing
// paths. The held descriptor pins the final A-owned 0711 directory.
type originPathStamp struct {
	device uint64
	inode  uint64
}

func originDirectory(path string) (*os.File, error) {
	dir, _, err := originDirectoryChain(path)
	return dir, err
}

// Every component is opened relative to its already pinned parent, never
// following links. Stamps let live checks reject replacement of any ancestor.
func originDirectoryChain(path string) (*os.File, []originPathStamp, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, nil, runorigin.ErrRefused
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, runorigin.ErrRefused
	}
	var chain []originPathStamp
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		unix.Close(fd)
		return nil, nil, runorigin.ErrRefused
	}
	chain = append(chain, originPathStamp{uint64(st.Dev), st.Ino})
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, nil, runorigin.ErrRefused
		}
		fd = next
		if unix.Fstat(fd, &st) != nil {
			unix.Close(fd)
			return nil, nil, runorigin.ErrRefused
		}
		chain = append(chain, originPathStamp{uint64(st.Dev), st.Ino})
	}
	if !originDirectoryMetadata(&st, uint32(os.Geteuid())) {
		unix.Close(fd)
		return nil, nil, runorigin.ErrRefused
	}
	return os.NewFile(uintptr(fd), path), chain, nil
}

type originEndpoint struct {
	dir                   *os.File
	chain                 []originPathStamp
	path                  string
	dirStamp, socketStamp unix.Stat_t
}

func (e *originEndpoint) valid() bool {
	dir, chain, err := originDirectoryChain(filepath.Dir(e.path))
	if err != nil {
		return false
	}
	defer dir.Close()
	if len(chain) != len(e.chain) {
		return false
	} // live ancestor gate
	for i, stamp := range chain {
		if stamp != e.chain[i] {
			return false
		}
	}
	var d, s unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &d) != nil || unix.Fstatat(int(dir.Fd()), "origin.sock", &s, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return false
	}
	return d.Dev == e.dirStamp.Dev && d.Ino == e.dirStamp.Ino && originDirectoryMetadata(&d, uint32(os.Geteuid())) &&
		s.Dev == e.socketStamp.Dev && s.Ino == e.socketStamp.Ino && originSocketMetadata(&s, uint32(os.Geteuid()))
}

// Serve binds only the separate origin.sock. Existing sockets (even stale),
// wrong metadata and links refuse without repair. Only its own newly created
// socket is chmodded; cleanup is pinned to the created inode and directory FD.
func (s *OriginService) Serve(ctx context.Context) error {
	return s.serve(ctx, nil)
}

func (s *OriginService) serve(ctx context.Context, ready func()) error {
	if !s.started.CompareAndSwap(false, true) {
		return runorigin.ErrRefused
	}
	dir, chain, err := originDirectoryChain(s.cfg.ControlDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	e := &originEndpoint{dir: dir, chain: chain, path: filepath.Join(s.cfg.ControlDir, "origin.sock")}
	if unix.Fstat(int(dir.Fd()), &e.dirStamp) != nil {
		return runorigin.ErrRefused
	}
	var st unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), "origin.sock", &st, unix.AT_SYMLINK_NOFOLLOW) != unix.ENOENT {
		return runorigin.ErrRefused
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: e.path, Net: "unix"})
	if err != nil {
		return runorigin.ErrRefused
	}
	l.SetUnlinkOnClose(false)
	defer l.Close()
	if unix.Fstatat(int(dir.Fd()), "origin.sock", &e.socketStamp, unix.AT_SYMLINK_NOFOLLOW) != nil || e.socketStamp.Uid != uint32(os.Geteuid()) || e.socketStamp.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return runorigin.ErrRefused
	}
	defer func() {
		var now unix.Stat_t
		if unix.Fstatat(int(dir.Fd()), "origin.sock", &now, unix.AT_SYMLINK_NOFOLLOW) == nil && now.Dev == e.socketStamp.Dev && now.Ino == e.socketStamp.Ino {
			_ = unix.Unlinkat(int(dir.Fd()), "origin.sock", 0)
		}
	}()
	if unix.Fchmodat(int(dir.Fd()), "origin.sock", 0666, 0) != nil || !e.valid() {
		return runorigin.ErrRefused
	}
	var mu sync.Mutex
	conns := map[*net.UnixConn]bool{}
	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for c := range conns {
			c.Close()
		}
	})
	defer stop()
	defer wg.Wait()
	if ready != nil {
		ready()
	}
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Also close all existing sessions on a terminal accept failure.
			mu.Lock()
			for c := range conns {
				c.Close()
			}
			mu.Unlock()
			return runorigin.ErrUnavailable
		}
		mu.Lock()
		if ctx.Err() != nil || len(conns) >= MaxConnections {
			mu.Unlock()
			s.originError(c, runorigin.ErrUnavailable)
			c.Close()
			continue
		}
		conns[c] = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { mu.Lock(); delete(conns, c); mu.Unlock() }()
			s.handleOrigin(ctx, c, e)
		}()
	}
}

func originCode(err error) string {
	switch {
	case errors.Is(err, runorigin.ErrNotSupported):
		return runorigin.NotSupported
	case errors.Is(err, runorigin.ErrUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return runorigin.Unavailable
	default:
		return runorigin.Refused
	}
}

func (s *OriginService) originError(c *net.UnixConn, err error) {
	s.originErrorUntil(c, err, time.Now().Add(time.Second))
}

func (s *OriginService) originErrorUntil(c *net.UnixConn, err error, deadline time.Time) {
	// Only named codes and incarnation, never producer text or caller fields.
	if cap := time.Now().Add(time.Second); cap.Before(deadline) {
		deadline = cap
	}
	_ = c.SetWriteDeadline(deadline)
	_ = originWrite(c, struct {
		Code              string `json:"code"`
		SourceIncarnation string `json:"source_incarnation"`
	}{originCode(err), s.incarnation})
}

func originWrite(c *net.UnixConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(c, b)
}

func (s *OriginService) authenticateOrigin(ctx context.Context, c *net.UnixConn) error {
	uid, err := peerUID(c)
	if err != nil || uid != s.cfg.Launcher.UID {
		return runorigin.ErrRefused
	} // peer gate
	generation, err := s.cfg.Generations.CurrentGeneration(ctx, uid)
	if err != nil {
		return err
	}
	if !runorigin.ValidGeneration(generation) || generation != s.cfg.Launcher.Generation {
		return runorigin.ErrRefused
	}
	return nil
}

type originRecord struct {
	prepared runorigin.PreparedOrigin
	session  runorigin.Session
}

func (r *originRecord) close() {
	if r != nil {
		_ = r.session.Close()
	}
}

type originWork struct {
	record *originRecord
	result runorigin.PreparedOrigin
	err    error
}

// boundedOrigin discards late results. The worker retains its slot until actual
// completion, bounding even a broken downstream that ignores cancellation.
func (s *OriginService) boundedOrigin(ctx context.Context, owned *originRecord, work func() originWork) originWork {
	select {
	case s.workers <- struct{}{}:
	default:
		owned.close()
		return originWork{err: runorigin.ErrUnavailable}
	}
	out := make(chan originWork)
	go func() {
		defer func() { <-s.workers }()
		result := func() originWork { defer owned.close(); return work() }()
		select {
		case out <- result:
		case <-ctx.Done():
			result.record.close()
		}
	}()
	select {
	case result := <-out:
		if ctx.Err() != nil {
			result.record.close()
			return originWork{err: runorigin.ErrUnavailable}
		}
		return result
	case <-ctx.Done():
		return originWork{err: runorigin.ErrUnavailable}
	}
}

func (s *OriginService) handleOrigin(ctx context.Context, c *net.UnixConn, endpoint *originEndpoint) {
	defer c.Close()
	deadline := time.Now().Add(s.budget)
	call, cancel := context.WithDeadline(ctx, deadline.Add(-originResponseReserve))
	auth := s.boundedOrigin(call, nil, func() originWork { return originWork{err: s.authenticateOrigin(call, c)} })
	cancel()
	if auth.err != nil || !endpoint.valid() {
		if auth.err == nil {
			auth.err = runorigin.ErrRefused
		}
		s.originErrorUntil(c, auth.err, deadline)
		return
	}
	_ = c.SetWriteDeadline(deadline)
	capabilities := []string{}
	if s.cfg.Producer != nil {
		capabilities = []string{originPrepare, originValidate}
	}
	if originWrite(c, struct {
		Capabilities      []string `json:"capabilities"`
		SourceIncarnation string   `json:"source_incarnation"`
	}{capabilities, s.incarnation}) != nil {
		return
	}
	var record *originRecord
	defer func() { record.close() }()
	for {
		// The read and all protected work share this operation's independent budget.
		deadline = time.Now().Add(s.budget)
		call, cancel = context.WithDeadline(ctx, deadline.Add(-originResponseReserve))
		_ = c.SetWriteDeadline(deadline)
		_ = c.SetReadDeadline(deadline.Add(-originResponseReserve))
		b, err := readFrame(c)
		if err != nil {
			cancel()
			if err != io.EOF {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					s.originErrorUntil(c, runorigin.ErrUnavailable, deadline)
				} else {
					s.originErrorUntil(c, runorigin.ErrRefused, deadline)
				}
			}
			return
		}
		r, err := decodeOriginRequest(b)
		if err != nil {
			cancel()
			s.originErrorUntil(c, err, deadline)
			return
		}
		old := record
		record = nil // Any validation attempt consumes authority, including a refusal.
		work := s.boundedOrigin(call, old, func() originWork {
			if !endpoint.valid() {
				return originWork{err: runorigin.ErrRefused}
			}
			if err := s.authenticateOrigin(call, c); err != nil {
				return originWork{err: err}
			}
			if !endpoint.valid() {
				return originWork{err: runorigin.ErrRefused}
			} // post-authority endpoint gate
			if r.op == originPrepare {
				return s.prepareOrigin(call, r)
			}
			if old == nil || r.incarnation != s.incarnation || // incarnation gate
				r.incarnation != old.prepared.SourceIncarnation || r.prepared != old.prepared.PreparedID ||
				r.id != old.prepared.RequestID || r.nonce != old.prepared.LaunchNonce {
				return originWork{err: runorigin.ErrRefused}
			}
			result, err := old.session.Validate(call)
			if err != nil {
				return originWork{err: err}
			}
			if !result.Origin.Valid() || result.Origin != old.prepared.Origin || result.IdentityRegistrationNumber != old.prepared.IdentityRegistrationNumber {
				return originWork{err: runorigin.ErrRefused}
			}
			return originWork{result: old.prepared}
		})
		cancel()
		if work.err != nil {
			s.originErrorUntil(c, work.err, deadline)
			return
		}
		record = work.record
		if originWrite(c, work.result) != nil {
			return
		}
	}
}

func (s *OriginService) prepareOrigin(ctx context.Context, r originRequest) originWork {
	if s.cfg.Producer == nil {
		return originWork{err: runorigin.ErrNotSupported}
	} // capability gate
	session, err := s.cfg.Producer.Open(ctx, runorigin.Selection{Launcher: s.cfg.Launcher, Event: r.event, Input: r.input})
	if err != nil || session == nil {
		if session != nil {
			_ = session.Close()
		}
		if err == nil {
			err = runorigin.ErrRefused
		}
		return originWork{err: err}
	}
	retained := false
	defer func() {
		if !retained {
			_ = session.Close()
		}
	}()
	result, err := session.Prepare(ctx)
	if err != nil {
		return originWork{err: err}
	}
	if !result.Origin.Valid() || len(result.IdentityRegistrationNumber) == 0 || len(result.IdentityRegistrationNumber) > 4096 || strings.ContainsAny(result.IdentityRegistrationNumber, "\x00\r\n") {
		return originWork{err: runorigin.ErrRefused}
	}
	id, err := originRandomID()
	if err != nil {
		return originWork{err: runorigin.ErrUnavailable}
	}
	prepared := runorigin.PreparedOrigin{RequestID: r.id, LaunchNonce: r.nonce, InputPrincipal: r.input, IdentityRegistrationNumber: result.IdentityRegistrationNumber, SourceIncarnation: s.incarnation, PreparedID: id, Origin: result.Origin}
	retained = true
	return originWork{record: &originRecord{prepared: prepared, session: session}, result: prepared}
}

func originDirectoryMetadata(st *unix.Stat_t, uid uint32) bool {
	return st.Uid == uid && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0711
}
func originSocketMetadata(st *unix.Stat_t, uid uint32) bool {
	return st.Uid == uid && st.Mode&unix.S_IFMT == unix.S_IFSOCK && st.Mode&07777 == 0666 && st.Nlink == 1
}
