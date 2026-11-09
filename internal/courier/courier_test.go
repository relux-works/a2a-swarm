//go:build linux || darwin

package courier

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func privateTemp(t *testing.T) string {
	t.Helper()
	p, err := os.MkdirTemp("", "cr-")
	if err != nil {
		t.Fatal(err)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(p) })
	return p
}

func registered(t *testing.T) (string, []byte) {
	t.Helper()
	home := filepath.Join(privateTemp(t), "home")
	secret, err := Register(home, "station", "identity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal("registration failed")
	}
	return home, secret
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}
func (l *lockedLog) TryCourierLog(p []byte) (int, error) { return l.Write(p) }
func (l *lockedLog) text() string                        { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type running struct {
	home, path string
	secret     []byte
	log        *lockedLog
	s          *server
	cancel     context.CancelFunc
	done       chan error
}

func start(t *testing.T, peer func(*net.UnixConn) (uint32, error), handshake time.Duration) *running {
	t.Helper()
	home, secret := registered(t)
	r := &running{home: home, path: filepath.Join(privateTemp(t), "socket-dir", "courier.sock"), secret: secret, log: &lockedLog{}, done: make(chan error, 1)}
	if peer == nil {
		peer = peerUID
	}
	if handshake == 0 {
		handshake = time.Second
	}
	r.s = &server{home: home, log: r.log, conns: map[*net.UnixConn]bool{}, peer: peer, handshake: handshake, idle: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	ready := make(chan struct{})
	go func() { r.done <- r.s.serve(ctx, r.path, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-r.done:
		t.Fatalf("serve setup failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("serve setup timeout")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Error("serve exit failed")
			}
		case <-time.After(3 * time.Second):
			t.Error("serve cleanup timeout")
		}
	})
	return r
}

func connect(t *testing.T, r *running) (*net.UnixConn, []byte) {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	b, err := readFrame(c)
	if err != nil {
		t.Fatal("challenge missing")
	}
	var ch struct {
		Type  string `json:"type"`
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(b, &ch) != nil || ch.Type != "challenge" {
		t.Fatal("invalid challenge")
	}
	n, err := hex.DecodeString(ch.Nonce)
	if err != nil || len(n) != 32 {
		t.Fatal("invalid nonce")
	}
	return c, n
}

// Independently construct the documented raw nonce/station/identity transcript.
func hello(secret, nonce []byte, station, identity string) []byte {
	m := hmac.New(sha256.New, secret)
	transcript := append(append(append([]byte{}, nonce...), []byte(station)...), []byte(identity)...)
	m.Write(transcript)
	b, _ := json.Marshal(map[string]string{"type": "hello", "station": station, "identity": identity, "mac": hex.EncodeToString(m.Sum(nil))})
	return b
}

func assertRefused(t *testing.T, c *net.UnixConn) {
	t.Helper()
	b, err := readFrame(c)
	if err != nil || !bytes.Equal(b, refusalFrame) {
		t.Fatal("missing fixed refusal")
	}
	var one [1]byte
	if n, err := c.Read(one[:]); n != 0 || err != io.EOF {
		t.Fatal("refusal did not close connection")
	}
}

func assertAccepted(t *testing.T, r *running, key []byte) {
	t.Helper()
	c, n := connect(t, r)
	if writeFrame(c, hello(key, n, "station", "identity")) != nil {
		t.Fatal("hello write failed")
	}
	b, err := readFrame(c)
	if err != nil || !bytes.Equal(b, successFrame) {
		t.Fatal("valid neighbor refused")
	}
	c.CloseWrite()
	c.Close()
}

func TestAuthenticationAndUniformRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func([]byte, []byte) []byte
		uid    bool
	}{
		{"unknown_station", func(k, n []byte) []byte { return hello(k, n, "unknown", "identity") }, false},
		{"bad_mac", func(k, n []byte) []byte {
			k = append([]byte{}, k...)
			k[0] ^= 1
			return hello(k, n, "station", "identity")
		}, false},
		{"uid_mismatch", func(k, n []byte) []byte { return hello(k, n, "station", "identity") }, true},
		{"identity_override", func(k, n []byte) []byte { return hello(k, n, "station", "other") }, false},
		{"malformed", func(k, n []byte) []byte { return []byte(`{"type":`) }, false},
		{"unknown_field", func(k, n []byte) []byte {
			return append(hello(k, n, "station", "identity")[:len(hello(k, n, "station", "identity"))-1], []byte(`,"extra":0}`)...)
		}, false},
		{"duplicate_field", func(k, n []byte) []byte {
			return append(hello(k, n, "station", "identity")[:len(hello(k, n, "station", "identity"))-1], []byte(`,"station":"station"}`)...)
		}, false},
		{"trailing_json", func(k, n []byte) []byte { return append(hello(k, n, "station", "identity"), []byte(`{}`)...) }, false},
		{"wrong_case", func(k, n []byte) []byte {
			return bytes.Replace(hello(k, n, "station", "identity"), []byte(`"type"`), []byte(`"Type"`), 1)
		}, false},
		{"null", func(k, n []byte) []byte { return []byte(`null`) }, false},
		{"array", func(k, n []byte) []byte { return []byte(`[]`) }, false},
		{"bad_name", func(k, n []byte) []byte { return hello(k, n, "station\n", "identity") }, false},
		{"bad_mac_encoding", func(k, n []byte) []byte {
			return []byte(`{"type":"hello","station":"station","identity":"identity","mac":"x"}`)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peer := peerUID
			if tc.uid {
				peer = func(c *net.UnixConn) (uint32, error) { u, e := peerUID(c); return u + 1, e }
			}
			r := start(t, peer, 0)
			// Pair the mismatch with successes under the same injected probe.
			if tc.uid {
				key, err := Register(r.home, "station", "identity", uint32(os.Geteuid())+1, true)
				if err != nil {
					t.Fatal("matching UID fixture registration failed")
				}
				assertAccepted(t, r, key)
				r.secret, err = Register(r.home, "station", "identity", uint32(os.Geteuid()), true)
				if err != nil {
					t.Fatal("mismatching UID fixture registration failed")
				}
			} else {
				assertAccepted(t, r, r.secret)
			}
			c, n := connect(t, r)
			if writeFrame(c, tc.change(r.secret, n)) != nil {
				t.Fatal("request write failed")
			}
			assertRefused(t, c)
			// Logs are asynchronous; wait for the sink without delaying the server.
			if !awaitLog(t, r, refusalLine) {
				t.Fatal("refusal log is not exactly one fixed line")
			}
			if tc.uid {
				key, err := Register(r.home, "station", "identity", uint32(os.Geteuid())+1, true)
				if err != nil {
					t.Fatal("matching UID fixture restore failed")
				}
				assertAccepted(t, r, key)
			} else {
				assertAccepted(t, r, r.secret)
			}
		})
	}
}

func TestKernelPeerUIDAndFreshNonceReplay(t *testing.T) {
	r := start(t, nil, 0)
	a, na := connect(t, r)
	b, nb := connect(t, r)
	if bytes.Equal(na, nb) {
		t.Fatal("nonce reused")
	}
	request := hello(r.secret, na, "station", "identity")
	writeFrame(a, request)
	got, err := readFrame(a)
	if err != nil || !bytes.Equal(got, successFrame) {
		t.Fatal("kernel UID neighbor failed")
	}
	writeFrame(b, request)
	assertRefused(t, b)
	// A second hello on an already authenticated connection is never executed.
	writeFrame(a, request)
	assertRefused(t, a)
	if !awaitLog(t, r, refusalLine+refusalLine) {
		t.Fatal("replay/second hello logs differ")
	}
	assertAccepted(t, r, r.secret)
}

func TestRotationUnregisterAndRegistryBinding(t *testing.T) {
	r := start(t, nil, 0)
	assertAccepted(t, r, r.secret)
	next, err := Register(r.home, "station", "identity", uint32(os.Geteuid()), true)
	if err != nil {
		t.Fatal("rotation failed")
	}
	if bytes.Equal(next, r.secret) {
		t.Fatal("rotation reused secret")
	}
	c, n := connect(t, r)
	writeFrame(c, hello(r.secret, n, "station", "identity"))
	assertRefused(t, c)
	assertAccepted(t, r, next)
	// A distinct station gets a distinct key, and cannot select another binding.
	other, err := Register(r.home, "stat", "ionidentity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal("neighbor registration failed")
	}
	// station||identity == stat||ionidentity, but lookup pins the original key.
	c, n = connect(t, r)
	writeFrame(c, hello(other, n, "station", "identity"))
	assertRefused(t, c)
	if err := Unregister(r.home, "station"); err != nil {
		t.Fatal("unregister failed")
	}
	c, n = connect(t, r)
	writeFrame(c, hello(next, n, "station", "identity"))
	assertRefused(t, c)
	e, ok := lookup(r.home, "stat")
	if !ok || e.Identity != "ionidentity" {
		t.Fatal("unregister damaged neighbor")
	}
	if _, ok := lookup(r.home, "station"); ok {
		t.Fatal("removed binding retained")
	}
}

func TestFrameBoundsFragmentationAndShortWrites(t *testing.T) {
	for _, n := range []uint32{0, MaxFrameBytes + 1, ^uint32(0)} {
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], n)
		if _, err := readFrame(bytes.NewReader(h[:])); err == nil {
			t.Fatal("invalid frame length accepted")
		}
	}
	body := bytes.Repeat([]byte{' '}, MaxFrameBytes)
	var out bytes.Buffer
	if writeFrame(&out, body) != nil {
		t.Fatal("inclusive frame cap refused")
	}
	got, err := readFrame(&byteReader{r: bytes.NewReader(out.Bytes())})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("fragmented inclusive frame failed")
	}
	if writeFrame(io.Discard, append(body, ' ')) == nil {
		t.Fatal("oversize output accepted")
	}
	for _, cut := range []int{1, 3, 4, 5, 20} {
		if _, err := readFrame(bytes.NewReader(out.Bytes()[:cut])); err == nil {
			t.Fatal("partial frame accepted")
		}
	}
	var short bytes.Buffer
	if writeFrame(&byteWriter{w: &short}, []byte(`{}`)) != nil {
		t.Fatal("short writes not completed")
	}
	got, err = readFrame(&short)
	if err != nil || string(got) != "{}" {
		t.Fatal("short write corrupted frame")
	}
	if writeFrame(zeroWriter{}, []byte(`{}`)) == nil {
		t.Fatal("zero-progress write accepted")
	}
}

type byteReader struct{ r io.Reader }

func (r *byteReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.r.Read(b)
}

type byteWriter struct{ w io.Writer }

func (w *byteWriter) Write(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return w.w.Write(b)
}

type zeroWriter struct{}

func (zeroWriter) Write(b []byte) (int, error) { return 0, nil }

func TestWireOversizeFragmentedAndTimeout(t *testing.T) {
	r := start(t, nil, 100*time.Millisecond)
	for _, kind := range []string{"oversize", "zero", "truncated", "timeout", "partial_timeout"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := connect(t, r)
			switch kind {
			case "oversize":
				var h [4]byte
				binary.BigEndian.PutUint32(h[:], MaxFrameBytes+1)
				c.Write(h[:])
			case "zero":
				c.Write(make([]byte, 4))
			case "truncated":
				c.Write([]byte{0, 0, 0, 3, '{'})
				c.CloseWrite()
			case "partial_timeout":
				c.Write([]byte{0})
			}
			assertRefused(t, c)
		})
	}
	// Fragment every byte of a valid request over a real kernel Unix socket.
	c, n := connect(t, r)
	if writeFrame(&byteWriter{w: c}, hello(r.secret, n, "station", "identity")) != nil {
		t.Fatal("fragmented hello write")
	}
	b, err := readFrame(c)
	if err != nil || !bytes.Equal(b, successFrame) {
		t.Fatal("fragmented hello refused")
	}
	c.Close()
	if !awaitLog(t, r, strings.Repeat(refusalLine, 5)) {
		t.Fatal("wire refusals not uniform")
	}
}

func TestRegistryPermissionsSymlinksAndLockCleanup(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "directory", "home_mode", "home_symlink", "lock_symlink", "lock_mode"} {
		t.Run(kind, func(t *testing.T) {
			home, _ := registered(t)
			path := filepath.Join(home, "stations.json")

			switch kind {
			case "mode":
				os.Chmod(path, 0644)
			case "symlink":
				os.Rename(path, path+".old")
				os.Symlink(path+".old", path)
			case "hardlink":
				os.Link(path, path+".link")
			case "directory":
				os.Remove(path)
				os.Mkdir(path, 0600)
			case "home_mode":
				os.Chmod(home, 0755)
			case "home_symlink":
				os.Rename(home, home+".old")
				os.Symlink(home+".old", home)
			case "lock_symlink":
				os.Remove(filepath.Join(home, ".stations.lock"))
				os.Symlink(path, filepath.Join(home, ".stations.lock"))
			case "lock_mode":
				os.Chmod(filepath.Join(home, ".stations.lock"), 0644)
			}
			if _, err := Register(home, "station", "identity", uint32(os.Geteuid()), true); err == nil {
				t.Fatal("unsafe registry accepted")
			}
			if _, ok := lookup(home, "station"); ok {
				t.Fatal("unsafe registry authenticated")
			}
		})
	}
	home, _ := registered(t)
	dir, err := openDirectory(home, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	lock, err := privateFile(dir, ".stations.lock", unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		t.Fatal("fixture lock")
	}
	if _, err := Register(home, "station", "identity", uint32(os.Geteuid()), true); err == nil {
		t.Fatal("held registry lock ignored")
	}
	lock.Close()
	if _, err := Register(home, "station", "identity", uint32(os.Geteuid()), true); err != nil {
		t.Fatal("registry lock not released")
	}
	files, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatal("registry temporary files leaked")
	}
}

func TestRegistryOwnerValidationAndStrictSchema(t *testing.T) {
	// Rootless tests cannot chown a private fixture to an unrelated host user.
	// Validate the exact fstat policy with independent synthetic owner metadata.
	st := unix.Stat_t{}
	st.Uid = uint32(os.Geteuid())
	st.Mode = unix.S_IFREG | 0600
	st.Nlink = 1
	if !validMetadata(&st, false, st.Uid) {
		t.Fatal("valid owner neighbor refused")
	}
	if validMetadata(&st, false, st.Uid+1) {
		t.Fatal("wrong owner accepted")
	}
	home, _ := registered(t)
	path := filepath.Join(home, "stations.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`{} {}`), []byte(`{"x":{},"x":{}}`),
		[]byte(`{"bad/name":{}}`), bytes.Replace(good, []byte(`"uid":`), []byte(`"UID":`), 1),
		bytes.Replace(good, []byte(`"uid":`), []byte(`"extra":0,"uid":`), 1),
		bytes.Replace(good, []byte(`"uid":`), []byte(`"uid":0,"uid":`), 1),
		bytes.Replace(good, []byte(`"secret":`), []byte(`"secret":null,"secret":`), 1),
		bytes.Repeat([]byte{' '}, maxRegistryBytes+1),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("fixture write")
		}
		if _, ok := lookup(home, "station"); ok {
			t.Fatal("invalid registry accepted")
		}
		if os.WriteFile(path, good, 0600) != nil {
			t.Fatal("restore fixture")
		}
		if _, ok := lookup(home, "station"); !ok {
			t.Fatal("valid registry neighbor refused")
		}
	}
	if _, err := Register(home, "bad/name", "identity", 0, false); err == nil {
		t.Fatal("bad station accepted")
	}
	if _, err := Register(home, "station", "Uppercase", 0, false); err == nil {
		t.Fatal("bad identity accepted")
	}
}

func TestConnectionLimitCancellationAndSocketCustody(t *testing.T) {
	r := start(t, nil, 0)
	var clients []*net.UnixConn
	for i := 0; i < MaxConnections; i++ {
		c, _ := connect(t, r)
		clients = append(clients, c)
	}
	extra, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	extra.SetDeadline(time.Now().Add(3 * time.Second))
	assertRefused(t, extra)
	extra.Close()
	if !awaitLog(t, r, refusalLine) {
		t.Fatal("capacity refusal log differs")
	}
	st, err := os.Lstat(r.path)
	if err != nil || st.Mode().Perm() != 0666 {
		t.Fatal("socket mode unsafe")
	}
	// A concurrent serve must leave the live endpoint intact.
	if err := Serve(context.Background(), r.home, r.path, io.Discard); err == nil {
		t.Fatal("second serve accepted")
	}
	current, err := os.Lstat(r.path)
	if err != nil || !os.SameFile(st, current) {
		t.Fatal("second serve changed socket")
	}
	r.cancel()
	// Cleanup runs after this body; cancellation must close every active client.
	for _, c := range clients {
		c.SetDeadline(time.Now().Add(2 * time.Second))
		var b [128]byte
		_, err := c.Read(b[:])
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("active connection leaked")
			}
		}
		c.Close()
	}
}

func TestUniformRegistryAndPeerProbeRefusals(t *testing.T) {
	for _, kind := range []string{"registry_mode", "registry_missing", "peer_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			probe := peerUID
			if kind == "peer_unavailable" {
				probe = func(*net.UnixConn) (uint32, error) { return 0, errRefused }
			}
			r := start(t, probe, 0)
			path := filepath.Join(r.home, "stations.json")
			if kind != "peer_unavailable" {
				assertAccepted(t, r, r.secret)
			}
			if kind == "registry_mode" {
				if os.Chmod(path, 0644) != nil {
					t.Fatal("fixture chmod")
				}
			}
			if kind == "registry_missing" {
				if os.Rename(path, path+".saved") != nil {
					t.Fatal("fixture rename")
				}
			}
			c, n := connect(t, r)
			writeFrame(c, hello(r.secret, n, "station", "identity"))
			assertRefused(t, c)
			if !awaitLog(t, r, refusalLine) {
				t.Fatal("registry/probe error leaked into log")
			}
			if kind == "registry_mode" {
				os.Chmod(path, 0600)
			}
			if kind == "registry_missing" {
				os.Rename(path+".saved", path)
			}
			if kind != "peer_unavailable" {
				assertAccepted(t, r, r.secret)
			}
		})
	}
}

func TestSocketRefusesUnsafePathsAndReleasesResources(t *testing.T) {
	home, _ := registered(t)
	for _, kind := range []string{"directory_mode", "directory_symlink", "existing_file", "existing_symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateTemp(t)
			if os.Chmod(dir, 0711) != nil {
				t.Fatal("fixture directory mode")
			}
			path := filepath.Join(dir, "socket")
			switch kind {
			case "directory_mode":
				os.Chmod(dir, 0755)
			case "directory_symlink":
				old := dir
				dir = dir + "-link"
				os.Symlink(old, dir)
				t.Cleanup(func() { os.Remove(dir) })
				path = filepath.Join(dir, "socket")
			case "existing_file":
				os.WriteFile(path, []byte("inert"), 0600)
			case "existing_symlink":
				os.Symlink("absent", path)
			}
			if err := Serve(context.Background(), home, path, io.Discard); err == nil {
				t.Fatal("unsafe socket path accepted")
			}
			if kind == "existing_file" {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != "inert" {
					t.Fatal("existing path was modified")
				}
			}
		})
	}
	// Start/stop twice on the identical paths; this checks lock and inode cleanup.
	path := filepath.Join(privateTemp(t), "socket-dir", "socket")
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		s := &server{home: home, log: io.Discard, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: time.Second, idle: 20 * time.Millisecond}
		go func() { done <- s.serve(ctx, path, func() { close(ready) }) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("restart failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("restart timeout")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("shutdown failed")
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown timeout")
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("owned socket not removed")
		}
	}
}

func TestStrictJSONAndRegistryLimits(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"type":"hello","type":"hello"}`), []byte(`{"type":"hello",}`),
		[]byte(`{"x":[{"z":0}]}`), []byte("{\"x\":\"\xff\"}"), []byte(`{"type":"hello"} true`),
	} {
		if _, _, _, err := parseHello(data); err == nil {
			t.Fatal("non-schema hello accepted")
		}
	}
	home, _ := registered(t)
	path := filepath.Join(home, "stations.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, badUID := range []string{"null", " null ", "-1", "4294967296", "1.0", "\"1\""} {
		var entry map[string]map[string]json.RawMessage
		if json.Unmarshal(original, &entry) != nil {
			t.Fatal("fixture decode")
		}
		entry["station"]["uid"] = json.RawMessage(badUID)
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal("fixture encoding")
		}
		if _, err := decodeRegistry(data); err == nil {
			t.Fatal("invalid UID accepted")
		}
	}
	e, ok := lookup(home, "station")
	if !ok {
		t.Fatal("valid registry neighbor missing")
	}
	r := registry{}
	for i := 0; i < maxStations; i++ {
		r[fmt.Sprintf("s%d", i)] = e
	}
	if err := withRegistry(home, false, func(d *os.File, _ registry) error { return saveRegistry(d, r) }); err != nil {
		t.Fatal("registry cap neighbor failed")
	}
	if _, err := Register(home, "additional", "identity", uint32(os.Geteuid()), false); err == nil {
		t.Fatal("station limit ignored")
	}
	if _, err := Register(home, "s0", "identity", uint32(os.Geteuid()), true); err != nil {
		t.Fatal("rotation refused at station limit")
	}
	r["additional"] = e
	data, _ := json.Marshal(r)
	if _, err := decodeRegistry(data); err == nil {
		t.Fatal("oversize station count accepted")
	}
}

func TestPublicSocketAccessAndForeignUIDAuth(t *testing.T) {
	// This rootless fixture uses a real kernel connection with an injected foreign
	// UID after probing it. It does not claim a second OS-user qualification.
	r := start(t, func(c *net.UnixConn) (uint32, error) { u, err := peerUID(c); return u + 1, err }, 0)
	directory, err := os.Lstat(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0711 {
		t.Fatal("public socket directory policy differs")
	}
	st, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		t.Fatal("socket directory owner differs")
	}
	socket, err := os.Lstat(r.path)
	if err != nil || socket.Mode().Perm() != 0666 {
		t.Fatal("public socket policy differs")
	}
	socketStat, ok := socket.Sys().(*syscall.Stat_t)
	if !ok || socketStat.Uid != uint32(os.Geteuid()) {
		t.Fatal("socket owner differs")
	}
	// Other-user traversal and connect bits are present, but directory read/list
	// permission is absent. No station group membership is part of admission.
	if directory.Mode().Perm()&0001 == 0 || directory.Mode().Perm()&0004 != 0 || socket.Mode().Perm()&0002 == 0 {
		t.Fatal("other-user access bits differ")
	}
	c, n := connect(t, r)
	writeFrame(c, hello(r.secret, n, "unregistered", "identity"))
	assertRefused(t, c)
	// The correct key cannot defeat a mismatched registered UID.
	c, n = connect(t, r)
	writeFrame(c, hello(r.secret, n, "station", "identity"))
	assertRefused(t, c)
	key, err := Register(r.home, "station", "identity", uint32(os.Geteuid())+1, true)
	if err != nil {
		t.Fatal("foreign UID registration failed")
	}
	assertAccepted(t, r, key)
	if !awaitLog(t, r, refusalLine+refusalLine) {
		t.Fatal("foreign UID refusals are not uniform")
	}
	home, err := os.Lstat(r.home)
	if err != nil || home.Mode().Perm() != 0700 {
		t.Fatal("private home policy changed")
	}
	registry, err := os.Lstat(filepath.Join(r.home, "stations.json"))
	if err != nil || registry.Mode().Perm() != 0600 {
		t.Fatal("private registry policy changed")
	}
}

func TestV1PathBytesAndExplicitRotation(t *testing.T) {
	if StationKeyLen != 32 || ProtocolVersion != 1 {
		t.Fatal("v1 constants differ")
	}
	for _, path := range []string{"/", "/" + strings.Repeat("a", 102), "/tmp/é.sock"} {
		if ValidateSocketPath(path) != nil {
			t.Fatal("valid UTF-8 byte-bound path refused")
		}
	}
	for _, path := range []string{"", "relative", "/" + strings.Repeat("a", 103), "/" + strings.Repeat("é", 52), "/a\rb", "/a\nb", "/a\x00b", "/\uFEFFa", "\uFEFF/a", "/\xff"} {
		if ValidateSocketPath(path) == nil {
			t.Fatal("invalid v1 socket path accepted")
		}
	}
	home, original := registered(t)
	if _, err := Register(home, "station", "identity", uint32(os.Geteuid()), false); err == nil {
		t.Fatal("implicit rotation accepted")
	}
	e, ok := lookup(home, "station")
	if !ok || !bytes.Equal(original, e.Secret) {
		t.Fatal("refused implicit rotation changed registry")
	}
	replacement, err := Register(home, "station", "identity", uint32(os.Geteuid()), true)
	if err != nil || bytes.Equal(original, replacement) {
		t.Fatal("explicit rotation did not replace key")
	}
}

func TestSocketPermissionsStayWithDirectoryInode(t *testing.T) {
	parent := privateTemp(t)
	path := filepath.Join(parent, "public")
	if os.Mkdir(path, 0711) != nil {
		t.Fatal("socket directory setup")
	}
	dir, err := openDirectoryMode(path, false, 0711)
	if err != nil {
		t.Fatal("socket directory open")
	}
	defer dir.Close()
	socket := filepath.Join(path, "courier.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal("socket setup")
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	privateFile := filepath.Join(parent, "private-file")
	if os.WriteFile(privateFile, []byte("inert"), 0600) != nil {
		t.Fatal("private file setup")
	}
	if os.Rename(path, path+"-original") != nil || os.Mkdir(path, 0711) != nil || os.Symlink(privateFile, socket) != nil {
		t.Fatal("retarget setup")
	}
	if setSocketPermissions(dir, socket, info) == nil {
		t.Fatal("retargeted socket path accepted")
	}
	untouched, err := os.Lstat(privateFile)
	if err != nil || untouched.Mode().Perm() != 0600 {
		t.Fatal("permission update escaped the held directory")
	}
	if !sameSocketAt(dir, "courier.sock", info) {
		t.Fatal("original socket inode not retained")
	}
	if unix.Unlinkat(int(dir.Fd()), "courier.sock", 0) != nil {
		t.Fatal("original socket cleanup failed")
	}
	if st, err := os.Lstat(socket); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("cleanup removed a replacement path")
	}
}

func awaitLog(t *testing.T, r *running, want string) bool {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if r.log.text() == want {
			return true
		}
		select {
		case <-r.done:
			return false
		case <-timer.C:
			return false
		case <-tick.C:
		}
	}
}
