package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// env is one isolated test world: a fake homeserver plus a temporary config dir.
type env struct {
	t      *testing.T
	hs     *fakeHS
	dir    string
	stdin  string
	tokens map[string]string // agent name -> access token (for leak checks)
	extra  map[string]string // extra environment variables
	// configure, when set, adjusts each App before it runs (replace Stdout,
	// install the afterPrint crash hook).
	configure func(*App)
	// now, when set, is every App's clock (hook rate-limit tests).
	now func() time.Time
	// leaseTTL, when set, replaces the lease TTL (sequentialSessions).
	leaseTTL time.Duration
	// projDir is the project directory e.ensure passes as --dir (set by e.swarm).
	projDir string
}

type result struct {
	code           int
	stdout, stderr string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	hs := newFakeHS(t)
	dir := t.TempDir()
	envFile := "A2A_MATRIX_URL=" + hs.srv.URL + "\nA2A_MATRIX_SERVER_NAME=" + testServer + "\nA2A_MATRIX_REGISTRATION_TOKEN=" + hs.regToken + "\n"
	if err := os.WriteFile(filepath.Join(dir, "matrix.env"), []byte(envFile), 0o600); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, hs: hs, dir: dir, tokens: map[string]string{}, extra: map[string]string{}}
}

// agent creates a homeserver user and its 0600 credential file.
func (e *env) agent(name string) string {
	e.t.Helper()
	uid, tok := e.hs.addUser(name)
	e.writeCred(name, uid, tok, 0o600)
	e.tokens[name] = tok
	return uid
}

func (e *env) writeCred(name, uid, tok string, mode os.FileMode) {
	e.t.Helper()
	p := config.Paths{Base: e.dir}
	os.MkdirAll(filepath.Dir(p.AgentFile(name)), 0o700)
	data, _ := json.Marshal(config.Credentials{UserID: uid, AccessToken: tok, DeviceID: "D", Password: "pw-" + name + "-secret", Homeserver: e.hs.srv.URL})
	path := p.AgentFile(name)
	os.Remove(path)
	if err := os.WriteFile(path, data, mode); err != nil {
		e.t.Fatal(err)
	}
	os.Chmod(path, mode)
}

// sequentialSessions makes every lease lapse at once: the sessions of a test
// that exercises per-session doorbell state take turns instead of being
// refused as a second live session (the lease itself: lease_test.go).
func (e *env) sequentialSessions() { e.leaseTTL = -time.Nanosecond }

func (e *env) paths() config.Paths { return config.Paths{Base: e.dir} }

// run executes one a2a invocation in-process.
func (e *env) run(args ...string) result {
	return e.runCtx(context.Background(), args...)
}

func (e *env) runCtx(ctx context.Context, args ...string) result {
	e.t.Helper()
	var out, errb bytes.Buffer
	a := &App{
		Stdout: &out,
		Stderr: &errb,
		Stdin:  strings.NewReader(e.stdin),
		Getenv: func(k string) string {
			if k == config.EnvConfigDir {
				return e.dir
			}
			return e.extra[k]
		},
		HTTP: e.hs.srv.Client(),
		Now:  time.Now,
	}
	if e.now != nil {
		a.Now = e.now
	}
	a.leaseTTL = e.leaseTTL
	if e.configure != nil {
		e.configure(a)
	}
	code := a.Run(ctx, args)
	r := result{code: code, stdout: out.String(), stderr: errb.String()}
	e.assertNoLeak(r)
	return r
}

// assertNoLeak fails if any access token, password, or the registration token
// shows up in command output. It runs after every command in every test.
func (e *env) assertNoLeak(r result) {
	e.t.Helper()
	secrets := []string{e.hs.regToken}
	for _, tok := range e.tokens {
		secrets = append(secrets, tok)
	}
	e.hs.mu.Lock()
	for tok := range e.hs.tokens {
		secrets = append(secrets, tok)
	}
	e.hs.mu.Unlock()
	for _, s := range secrets {
		tuitestkit.NotContainsStr(e.t, r.stdout, s)
		tuitestkit.NotContainsStr(e.t, r.stderr, s)
	}
	tuitestkit.NotContainsStr(e.t, r.stdout, "-secret")
	tuitestkit.NotContainsStr(e.t, r.stderr, "-secret")
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	return r
}

func (r result) failsWith(t *testing.T, code int, fragment string) result {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", r.code, code, r.stdout, r.stderr)
	}
	tuitestkit.ContainsStr(t, r.stderr, fragment)
	return r
}

// world sets up alice and bob joined to #proj, with carol registered but outside.
func world(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	alice := e.agent("alice")
	bob := e.agent("bob")
	e.agent("carol")
	room := e.hs.addRoom("proj", alice, bob)
	e.hs.addRoom("lobby", alice, bob)
	return e, room
}

func (e *env) as(name string, args ...string) result {
	e.t.Helper()
	return e.run(append([]string{"--as", name}, args...)...)
}
