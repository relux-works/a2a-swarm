//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

type courierFixture struct {
	store        *namedTestStore
	t            *testing.T
	home, socket string
	stations     map[string]string
	paths        map[string]config.Paths
	bindings     map[string]config.ConnectionBinding
	envs         map[string]*env
	rooms        map[string]string
	daemon       *App
	done         chan error
	log          *flushBuffer
}

func canonicalRoot(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func shortCourierBase(t *testing.T) string {
	t.Helper()
	// macOS's per-user temp directory is short and accepted by landed C1. Keep
	// TMPDIR unchanged. Linux's canonical /tmp provides a short base regardless
	// of TMPDIR length. The owned 0700 base encloses the separate 0711 socket dir.
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = os.TempDir()
	}
	p, e := os.MkdirTemp(base, "c")
	if e != nil {
		t.Fatal(e)
	}
	p, e = filepath.EvalSymlinks(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(p) })
	return p
}
func newCourierFixture(t *testing.T) *courierFixture {
	t.Helper()
	f := &courierFixture{store: &namedTestStore{items: map[config.SecretReference]string{}}, t: t, home: filepath.Join(canonicalRoot(t), "courier"), socket: filepath.Join(shortCourierBase(t), "s", "c.sock"), stations: map[string]string{}, paths: map[string]config.Paths{}, bindings: map[string]config.ConnectionBinding{}, envs: map[string]*env{}, rooms: map[string]string{}, done: make(chan error, 1), log: &flushBuffer{}}
	for _, name := range []string{"alice", "bob"} {
		e, room := world(t)
		canonical, err := filepath.EvalSymlinks(e.dir)
		if err != nil {
			t.Fatal(err)
		}
		e.dir = canonical
		f.envs[name] = e
		f.rooms[name] = room
		root := filepath.Join(f.home, "identities", name)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		c := config.Connection{ID: name + "-connection", URL: e.hs.srv.URL, ServerName: testServer, Store: config.CredentialStore{Backend: "file"}}
		if name == "bob" {
			c.Store = config.CredentialStore{Backend: "keychain", AccessTokenService: "courier.bob.token", PasswordService: "courier.bob.password"}
		}
		reg := config.ConnectionRegistry{SchemaVersion: 1, Current: name + "-profile", Profiles: map[string]config.Connection{name + "-profile": c}}
		data, _ := json.Marshal(reg)
		if err := os.WriteFile(filepath.Join(root, "connections.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		p, err := c.Paths(config.Paths{Base: root}, name, false)
		if err != nil {
			t.Fatal(err)
		}
		f.paths[name] = p
		b, err := c.Binding(name, false)
		if err != nil {
			t.Fatal(err)
		}
		f.bindings[name] = b
		token := e.tokens[name]
		if name == "bob" {
			f.store.items[config.SecretReference{Service: c.Store.AccessTokenService, Account: b.UserID}] = token
			token = ""
		}
		if err := config.PublishBoundCredentials(p, name, false, config.Credentials{UserID: b.UserID, Homeserver: c.URL, AccessToken: token, DeviceID: "D", ConnectionBinding: &b}); err != nil {
			t.Fatal(err)
		}
		if err := config.SaveState(p, name, config.State{NextBatch: "s0"}); err != nil {
			t.Fatal(err)
		}
		key, err := courier.Register(f.home, "station-"+name, name, uint32(os.Geteuid()), false)
		if err != nil {
			t.Fatal(err)
		}
		station := canonicalRoot(t)
		f.stations[name] = station
		payload, _ := json.Marshal(map[string]string{"socket": f.socket, "station": "station-" + name, "identity": name, "secret_b64": base64.StdEncoding.EncodeToString(key)})
		if err := courierenroll.Install(filepath.Join(station, ".a2a"), bytes.NewReader(append(payload, '\n')), false); err != nil {
			t.Fatal(err)
		}
		clear(key)
		clear(payload)
	}
	f.daemon = &App{connectionSecretStore: f.store, HTTP: f.envs["alice"].hs.srv.Client(), Now: time.Now, Getenv: func(string) string { panic("daemon read ambient selection") }, as: "ambient-identity", profile: "ambient-profile", connectionResolved: true, connection: &config.Connection{ID: "poison"}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		f.done <- courier.ServeWithHandler(ctx, f.home, f.socket, f.log, f.daemon.courierHandler(f.home))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-f.done:
			if err != nil {
				t.Error("daemon failed")
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon shutdown timeout")
		}
	})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		// bind publishes a socket inode before listen is ready. Probe the actual
		// listener rather than treating pathname existence as a readiness signal.
		if conn, err := net.DialTimeout("unix", f.socket, 100*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-f.done:
			t.Fatalf("serve setup failed: %v", err)
		case <-timer.C:
			t.Fatal("serve setup timeout")
		case <-tick.C:
		}
	}
	if len(f.socket) > 103 {
		t.Fatal("short socket exceeds v1 bound")
	}
	return f
}
func (f *courierFixture) app(name, input string, out, diag io.Writer) *App {
	return &App{Stdout: out, Stderr: diag, Stdin: strings.NewReader(input), HTTP: &http.Client{Transport: courierHTTPTrap{}}, Getenv: func(k string) string {
		switch k {
		case config.EnvConfigDir:
			return f.stations[name]
		case config.EnvAgent:
			return "ambient-other"
		case config.EnvMatrixURL:
			return "http://unavailable.invalid"
		case envSecretStore:
			return "poison"
		case EnvSessionID:
			return "ambient-session"
		}
		return ""
	}, connectionSecretStore: courierSecretTrap{}}
}

func (f *courierFixture) run(name, input string, args ...string) result {
	var out, diag bytes.Buffer
	code := f.app(name, input, &out, &diag).Run(context.Background(), args)
	r := result{code: code, stdout: out.String(), stderr: diag.String()}
	f.envs[name].assertNoLeak(r)
	return r
}
func (f *courierFixture) rawRun(name string, args ...string) result {
	f.t.Helper()
	e, _, err := courierenroll.Read(filepath.Join(f.stations[name], ".a2a"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer clear(e.Key)
	var out, diag bytes.Buffer
	code, err := courier.Client(context.Background(), e.Socket, e.Station, e.Identity, e.Key, courier.Request{Argv: args}, &out, &diag)
	if err != nil {
		return result{code: ExitError, stderr: "courier: " + string(courier.Class(err)) + "\n"}
	}
	return result{code: code, stdout: out.String(), stderr: diag.String()}
}

func (f *courierFixture) received(name, id string) bool {
	f.t.Helper()
	mb, err := config.OpenMailbox(f.paths[name], name)
	if err != nil {
		f.t.Fatal(err)
	}
	return mb.Received(id)
}

// Proves fetch help remains local and effect-free while executable fetch forms
// (including a help spelling used as a path value) still hit the closed gate.
// Existing TestFetchHelpWithoutSession separately traps every environment read.
func TestCourierExecutionFetchHelpBoundary(t *testing.T) {
	f := newCourierFixture(t)
	before := len(f.envs["alice"].hs.urls())
	assertFetchHelp(t, f.run("alice", "", "fetch", "--help"))
	for _, args := range [][]string{
		{"fetch", "proj", "$file"},
		{"fetch", "proj", "$file", "-o", "--help"},
		{"fetch", "--", "proj", "$file", "--help"},
	} {
		r := f.run("alice", "", args...)
		if r.code != ExitError || r.stdout != "" || (!strings.Contains(r.stderr, "not_allowed") && !strings.Contains(r.stderr, "identity_mismatch")) {
			t.Fatal("fetch execution bypassed Courier refusal")
		}
	}
	if len(f.envs["alice"].hs.urls()) != before {
		t.Fatal("help or refused fetch contacted Matrix")
	}
	f.run("alice", "", "whoami").ok(t)
}

// Proves public thin-client post/read/inbox/wait round trips execute the real
// command code against fake Matrix, with ACK receipts and unchanged hook lines.
// Network calls stay on the in-process fake; this does not qualify live Matrix.
func TestCourierExecutionRoundTrips(t *testing.T) {
	f := newCourierFixture(t)
	e := f.envs["alice"]
	room := f.rooms["alice"]
	f.run("alice", "", "--as", "alice", "whoami").ok(t)
	f.run("alice", "", "topic", "new", room, "alpha", "Alpha").ok(t)
	f.run("alice", "", "post", room, "--topic", "alpha", "hello through Courier").ok(t)
	if r := f.run("alice", "", "read", room); !strings.Contains(r.ok(t).stdout, "hello through Courier") {
		t.Fatal("read lost the post")
	}
	id := e.hs.inject(room, "@bob:"+testServer, ping(1))
	r := f.run("alice", "", "inbox").ok(t)
	if !strings.Contains(r.stdout, id) || !f.received("alice", id) {
		t.Fatal("inbox did not print then receipt")
	}
	if strings.Contains(f.run("alice", "", "inbox").ok(t).stdout, id) {
		t.Fatal("received entry repeated")
	}
	id = e.hs.inject(room, "@bob:"+testServer, ping(2))
	r = f.run("alice", "", "wait", "--timeout", "3s").ok(t)
	if !strings.Contains(r.stdout, id) || f.received("alice", id) {
		t.Fatalf("wait lost event or receipted: output=%q received=%v", r.stdout, f.received("alice", id))
	}
	// A hook line keeps its installed --as NAME hook EVENT --runtime R shape.
	// Seed no explicit session id, so the daemon's request-local lease is reused.
	f.run("alice", `{}`, "--as", "alice", "hook", "prompt", "--runtime", "claude").ok(t)
	if f.received("alice", id) {
		t.Fatal("doorbell receipted")
	}
	log, _ := f.log.snapshot()
	if strings.Contains(log, "hello through Courier") {
		t.Fatal("message body entered daemon diagnostics")
	}
}

// Proves validation refuses unauthorized command/identity/selection/path/config
// requests beside a successful matching whoami, with zero Matrix calls on refusal.
func TestCourierExecutionRefusals(t *testing.T) {
	f := newCourierFixture(t)
	b := f.bindings["alice"]
	f.run("alice", "", "whoami", "--as", "alice", "--profile", "alice-profile", "--connection-fingerprint", b.Fingerprint()).ok(t)
	for _, args := range [][]string{
		{"identity", "ensure"}, {"creds", "migrate"}, {"whoami", "--as", "bob"}, {"whoami", "--profile", "bob-profile"}, {"whoami", "--connection-fingerprint", strings.Repeat("0", 64)},
		{"inbox", "--out", "private-station-path"}, {"whoami", "--config", "private"}, {"A2A_CONFIG_DIR=private", "whoami"}, {"bind", "--runtime", "codex"},
	} {
		before := len(f.envs["alice"].hs.Calls())
		r := f.run("alice", "", args...)
		if r.code != ExitError || (!strings.Contains(r.stderr, "not_allowed") && !strings.Contains(r.stderr, "identity_mismatch")) {
			t.Fatal("request not refused with bounded diagnostic")
		}
		if raw := f.rawRun("alice", args...); raw.code != ExitError || (raw.stderr != "courier: not_allowed\n" && raw.stderr != "courier: identity_mismatch\n") {
			t.Fatal("daemon did not independently refuse request")
		}
		if len(f.envs["alice"].hs.Calls()) != before {
			t.Fatal("refusal called Matrix")
		}
	}
	root := filepath.Join(f.home, "identities", "alice")
	if err := os.Rename(root, root+"-held"); err != nil {
		t.Fatal(err)
	}
	before := len(f.envs["alice"].hs.Calls())
	r := f.run("alice", "", "whoami")
	if r.code != ExitError || r.stderr != "courier: courier_unavailable\n" || len(f.envs["alice"].hs.Calls()) != before {
		t.Fatal("missing identity did not refuse without Matrix")
	}
	if err := os.Rename(root+"-held", root); err != nil {
		t.Fatal(err)
	}
	f.run("alice", "", "whoami").ok(t)
}

// Proves station enrollment failures cannot fall back to local credentials and
// an absent enrollment still uses the legacy dispatcher. No daemon state reset.
func TestCourierExecutionEnrollmentBoundary(t *testing.T) {
	f := newCourierFixture(t)
	f.run("alice", "", "whoami").ok(t)
	dir := filepath.Join(f.stations["alice"], ".a2a")
	station, err := os.ReadFile(filepath.Join(dir, "station"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "station")); err != nil {
		t.Fatal(err)
	}
	r := f.run("alice", "", "whoami")
	if r.code != ExitError || !strings.Contains(r.stderr, "re-run enroll") {
		t.Fatal("partial enrollment fell back")
	}
	if err = os.WriteFile(filepath.Join(dir, "station"), station, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(canonicalRoot(t), "alias")
	if err = os.Symlink(f.stations["alice"], alias); err != nil {
		t.Fatal(err)
	}
	original := f.stations["alice"]
	f.stations["alice"] = alias
	if f.run("alice", "", "whoami").code != ExitError {
		t.Fatal("noncanonical enrollment accepted")
	}
	f.stations["alice"] = original
	e := f.envs["alice"]
	e.as("alice", "whoami").ok(t)
}

// Proves overlapping stations with distinct identities, connections, stores and
// mailboxes cannot cross selection; each waits while the other sends traffic.
func TestCourierExecutionIsolation(t *testing.T) {
	f := newCourierFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		name string
		r    result
	}
	done := make(chan outcome, 2)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for _, name := range []string{"alice", "bob"} {
		name := name
		go func() {
			var out, diag bytes.Buffer
			a := f.app(name, "", &out, &diag)
			code := a.Run(ctx, []string{"--as", name, "--profile", name + "-profile", "--connection-fingerprint", f.bindings[name].Fingerprint(), "wait", "--timeout", "10s"})
			done <- outcome{name, result{code: code, stdout: out.String(), stderr: diag.String()}}
		}()
		// C1 refuses a busy registry lock rather than waiting. Authenticate the
		// second station after the first has entered execution; both requests then
		// remain live together during the interleaved Matrix and mailbox effects.
		for f.envs[name].hs.SyncCount("@"+name+":"+testServer) == 0 {
			select {
			case got := <-done:
				t.Fatalf("station %s exited before interleave, code %d, diagnostics %q", got.name, got.r.code, got.r.stderr)
			case err := <-f.done:
				t.Fatalf("daemon stopped: %v", err)
			case <-timer.C:
				t.Fatal("stations never overlapped sync")
			case <-tick.C:
			}
		}
	}
	ids := map[string]string{}
	ids["alice"] = f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, msg("alice-only @alice:"+testServer, nil))
	ids["bob"] = f.envs["bob"].hs.inject(f.rooms["bob"], "@alice:"+testServer, msg("bob-only @bob:"+testServer, nil))
	for range 2 {
		select {
		case got := <-done:
			got.r.ok(t)
			other := "alice"
			if got.name == "alice" {
				other = "bob"
			}
			if !strings.Contains(got.r.stdout, got.name+"-only") || strings.Contains(got.r.stdout, other+"-only") {
				t.Fatal("identity or connection crossed")
			}
		case err := <-f.done:
			t.Fatalf("daemon stopped: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("interleaved station did not finish")
		}
	}
	for _, name := range []string{"alice", "bob"} {
		r := f.run(name, "", "whoami").ok(t)
		if !strings.Contains(r.stdout, "agent: "+name+"\n") {
			t.Fatal("identity cache crossed requests")
		}
		r = f.run(name, "", "inbox").ok(t)
		if !strings.Contains(r.stdout, name+"-only") || !f.received(name, ids[name]) {
			t.Fatal("mailbox or receipt crossed")
		}
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if len(f.store.reads) == 0 {
		t.Fatal("keychain station never used its store")
	}
	for _, ref := range f.store.reads {
		if ref.Service != "courier.bob.token" || ref.Account != "@bob:"+testServer {
			t.Fatal("store binding crossed stations")
		}
	}

}

// Proves a failed station stdout leaves the real mailbox pending and a retry
// prints and receipts it. Complements raw wrong/missing ACK transport tests.
func TestCourierExecutionFailedOutputKeepsMailbox(t *testing.T) {
	f := newCourierFixture(t)
	id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(99))
	var diag bytes.Buffer
	if code := f.app("alice", "", shortCourierOutput{}, &diag).Run(context.Background(), []string{"inbox"}); code != ExitError {
		t.Fatal("short output accepted")
	}
	// Retry serializes behind any still-unwinding delivery lock; it also proves
	// the failed output left the entry eligible instead of receiving it.
	r := f.run("alice", "", "inbox").ok(t)
	if !strings.Contains(r.stdout, id) || !f.received("alice", id) {
		t.Fatal("short output lost unreceived mail")
	}
}

type shortCourierOutput struct{}

func (shortCourierOutput) Write(p []byte) (int, error) { return len(p) - 1, nil }

// Proves input files cross as bytes (body, envelope, attachment and saved inbox)
// while the daemon only opens invocation-owned memory, never station paths.
func TestCourierExecutionStreamedInputs(t *testing.T) {
	f := newCourierFixture(t)
	root := canonicalRoot(t)
	body := filepath.Join(root, "body.txt")
	if err := os.WriteFile(body, []byte("streamed body"), 0600); err != nil {
		t.Fatal(err)
	}
	f.run("alice", "", "topic", "new", f.rooms["alice"], "alpha", "Alpha").ok(t)
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "alpha", "@"+body).ok(t)
	if !strings.Contains(f.run("alice", "", "read", f.rooms["alice"]).ok(t).stdout, "streamed body") {
		t.Fatal("file body changed")
	}
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "alpha", "--envelope", "@"+body, "envelope text").ok(t)
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "alpha", "--envelope=@"+body, "inline envelope").ok(t)
	f.run("alice", "", "topic", "new", f.rooms["alice"], "inline", "Inline", "--body=@"+body, "--json").ok(t)
	f.run("alice", "", "attach", f.rooms["alice"], "--topic", "alpha", body).ok(t)
	record, _ := json.Marshal(EventRecord{EventID: "$saved", Body: "saved input body", Sender: "@bob:" + testServer})
	saved := filepath.Join(root, "saved.txt")
	if err := os.WriteFile(saved, []byte(inboxBegin+"\n"+inboxIndent+string(record)+"\n"+inboxEnd+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := f.run("alice", "", "inbox", "show", "$saved", "--from", "@"+saved).ok(t); !strings.Contains(r.stdout, "saved input body") {
		t.Fatal("streamed saved frame lost body")
	}
	if r := f.run("alice", "", "inbox", "show", "$saved", "--from=@"+saved).ok(t); !strings.Contains(r.stdout, "saved input body") {
		t.Fatal("equals input lost body")
	}
	// A forged file-free engine request must not open even a valid daemon path.
	var out, diag bytes.Buffer
	code := f.daemon.courierHandler(f.home)(context.Background(), courier.Invocation{Identity: "alice", Request: courier.Request{Argv: []string{"post", f.rooms["alice"], "@" + body}}, Stdout: &out, Stderr: &diag, Delivered: func() error { return nil }})
	if code != ExitError || out.Len() != 0 || diag.String() != "courier: not_allowed\n" {
		t.Fatal("unstreamed path reached execution")
	}
}

// Proves a short canonical per-user socket base starts the unchanged C1 server
// on this host with inherited TMPDIR; the path and host identity are not logged.
func TestCourierExecutionShortSocketBase(t *testing.T) {
	f := newCourierFixture(t)
	f.run("alice", "", "whoami").ok(t)
	t.Logf("GOOS=%s socket_bytes=%d C1 setup and client handshake accepted", runtime.GOOS, len(f.socket))
}

// Proves streamed watch output remains unreceived after client cancellation,
// and the daemon releases the consumer so inbox can subsequently receipt it.
func TestCourierExecutionWatchDisconnect(t *testing.T) {
	f := newCourierFixture(t)
	id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(101))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &flushBuffer{}
	var diag bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- f.app("alice", "", out, &diag).Run(ctx, []string{"watch"}) }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s, _ := out.snapshot()
		if strings.Contains(s, id) {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("watch stopped before output: %d", code)
		case err := <-f.done:
			t.Fatalf("daemon stopped: %v", err)
		case <-timer.C:
			t.Fatal("watch did not stream")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case <-done:
	case err := <-f.done:
		t.Fatalf("daemon stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel client")
	}
	if f.received("alice", id) {
		t.Fatal("watch receipted")
	}
	// The consumer lock is released asynchronously when EOF reaches the daemon.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		release, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now().Add(10*time.Millisecond))
		if err == nil {
			release()
			break
		}
		select {
		case err := <-f.done:
			t.Fatalf("daemon stopped: %v", err)
		case <-deadline.C:
			t.Fatal("watch consumer did not stop")
		case <-tick.C:
		}
	}
	if r := f.run("alice", "", "inbox").ok(t); !strings.Contains(r.stdout, id) || !f.received("alice", id) {
		t.Fatal("watch lost pending mail")
	}
}

// Proves Muse diagnostics use the request's connection/identity store, without
// selecting the daemon's cwd or environment, and never log message body bytes.
func TestCourierExecutionMuseJournalIsolation(t *testing.T) {
	f := newCourierFixture(t)
	f.run("alice", `{"session_id":"a","hook_event_name":"UserPromptSubmit"}`, "--as", "alice", "hook", "prompt", "--runtime", "muse").ok(t)
	for _, name := range []string{"alice", "bob"} {
		a := &App{courierEngine: true, as: name, Getenv: func(k string) string {
			if k == config.EnvConfigDir {
				return filepath.Join(f.home, "identities", name)
			}
			return ""
		}}
		j, err := a.museHookJournal()
		if err != nil {
			t.Fatal(err)
		}
		if err = j.failure(time.Now(), hookEvents[0], "test_error"); err != nil {
			t.Fatal(err)
		}
		j.root.Close()
		path := filepath.Join(f.paths[name].Base, ".temp", "a2a-muse-hooks", name+".jsonl")
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, []byte("test_error")) {
			t.Fatal("journal did not stay in identity store")
		}
	}
}

// Proves a literal hyphen in a wait predicate does not consume station stdin,
// while post's actual '-' body operand still streams its input contents.
func TestCourierExecutionUnusedStdin(t *testing.T) {
	f := newCourierFixture(t)
	var out, diag bytes.Buffer
	spy := &unusedCourierStdin{}
	a := f.app("alice", "", &out, &diag)
	a.Stdin = spy
	code := a.Run(context.Background(), []string{"wait", "--match-text", "-", "--timeout", "1ms"})
	if code != ExitOK || spy.reads != 0 {
		t.Fatal("wait predicate consumed unused station stdin")
	}
	f.run("alice", "", "topic", "new", f.rooms["alice"], "stdin-body", "Stdin").ok(t)
	f.run("alice", "streamed stdin body", "post", f.rooms["alice"], "--topic", "stdin-body", "-").ok(t)
	if !strings.Contains(f.run("alice", "", "read", f.rooms["alice"]).ok(t).stdout, "streamed stdin body") {
		t.Fatal("body operand did not stream stdin")
	}
}

type unusedCourierStdin struct{ reads int }

func (r *unusedCourierStdin) Read([]byte) (int, error) { r.reads++; return 0, io.ErrClosedPipe }
