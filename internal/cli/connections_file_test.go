package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type panicNamedKeychain struct{}

func (panicNamedKeychain) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	panic("file connection reached Keychain Read")
}
func (panicNamedKeychain) Create(context.Context, config.Paths, config.SecretReference, string) error {
	panic("file connection reached Keychain Create")
}
func forbidFileKeychain(a *App) {
	a.connectionSecretStore = panicNamedKeychain{}
	a.keychain = func(config.Paths) *keychainStore { panic("file connection constructed Keychain adapter") }
}
func newNamedFileFixture(t *testing.T) *namedFixture {
	t.Helper()
	f := newNamedFixture(t, false)
	c := f.r.Profiles["alpha"]
	c.Store = config.CredentialStore{Backend: "file"}
	c.RegistrationTokenRef = nil
	f.r.Profiles["alpha"] = c
	f.save()
	b, err := c.Binding("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PublishBoundCredentials(f.paths("alpha", false), "alice", false, config.Credentials{UserID: b.UserID, Homeserver: c.URL, AccessToken: "isolated-alpha", ConnectionBinding: &b}); err != nil {
		t.Fatal(err)
	}
	return f
}

// Unset/equal env values select each backend; exact mismatches in either
// direction (including unknown/case/space variants) refuse before HTTP or secrets.
func TestNamedBackendEnvironment(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		for _, env := range []string{"", "file", "keychain", "FILE", "file ", "unknown"} {
			t.Run(backend+"/"+env, func(t *testing.T) {
				var f *namedFixture
				if backend == "file" {
					f = newNamedFileFixture(t)
				} else {
					f = newNamedFixture(t, true)
				}
				f.env[envSecretStore] = env
				a := f.app()
				if backend == "file" {
					forbidFileKeychain(a)
				}
				got, err := a.selectedStore()
				if env != "" && env != backend {
					if err == nil || err.Error() != "A2A_SECRET_STORE conflicts with selected profile" {
						t.Fatalf("expected store conflict, got %v", err)
					}
					if a.Run(context.Background(), []string{"whoami"}) == 0 || f.effects() != 0 {
						t.Fatal("conflict performed effects")
					}
					return
				}
				if err != nil || got != backend {
					t.Fatalf("selection %q: %v", got, err)
				}
				if a.Run(context.Background(), []string{"whoami"}) != 0 || f.httpCalls.Load() != 1 {
					t.Fatal("valid backend did not reach its credentials/server")
				}
				if backend == "file" && len(f.store.reads)+len(f.store.creates) != 0 {
					t.Fatal("file mode consulted Keychain")
				}
			})
		}
	}
}

// With no injected store, file is selected on every host; Keychain keeps the
// exact non-macOS error. Selection itself never accesses a real Keychain.
func TestNamedBackendPlatform(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		t.Run(backend, func(t *testing.T) {
			var f *namedFixture
			if backend == "file" {
				f = newNamedFileFixture(t)
			} else {
				f = newNamedFixture(t, true)
			}
			a := f.app()
			a.connectionSecretStore = nil
			a.keychain = nil
			got, err := a.selectedStore()
			if backend == "keychain" && runtime.GOOS != "darwin" {
				if err == nil || err.Error() != "keychain secret store requires macOS" {
					t.Fatal("non-macOS Keychain selection changed")
				}
			} else if err != nil || got != backend {
				t.Fatalf("backend selection %q: %v", got, err)
			}
			if f.effects() != 0 {
				t.Fatal("selection accessed secrets or HTTP")
			}
		})
	}
}

// A named file identity uses the existing 0600 credentials with no Keychain
// access, and preserves file, identity and strict binding refusals before HTTP.
func TestNamedFileCredentials(t *testing.T) {
	for _, mode := range []string{"valid", "public", "symlink", "missing-token", "password", "mxid", "class", "backend", "duplicate", "human"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFileFixture(t)
			p := f.paths("alpha", false)
			path := p.AgentFile("alice")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var c config.Credentials
			if err = json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "public":
				if err = os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(f.root, "other-credentials.json")
				if err = os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "missing-token":
				c.AccessToken = ""
			case "password":
				c.Password = "forbidden-password"
			case "mxid":
				c.UserID = "@alice:beta.invalid"
			case "class":
				c.ConnectionBinding.Class = "humans"
			case "backend":
				c.ConnectionBinding.Connection.Store = f.r.Profiles["beta"].Store
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"user_id":`), []byte(`"user_id":"@other:alpha.invalid","user_id":`), 1)
			case "human":
				if err = os.MkdirAll(filepath.Dir(p.HumanFile("alice")), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(p.HumanFile("alice"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "public" && mode != "symlink" {
				if mode != "duplicate" {
					raw, err = json.Marshal(c)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := f.app()
			forbidFileKeychain(a)
			exit := a.Run(context.Background(), []string{"whoami"})
			if mode == "valid" {
				if exit != 0 || f.httpCalls.Load() != 1 {
					t.Fatal("valid file credentials refused")
				}
			} else {
				if exit == 0 || f.httpCalls.Load() != 0 {
					t.Fatal("invalid credentials reached HTTP")
				}
				diagnostic := map[string]string{"public": "want 0600", "symlink": "not a regular file", "missing-token": "missing selected access token", "password": "named metadata must not contain secrets", "mxid": "binding mismatch", "class": "binding mismatch", "backend": "binding mismatch", "duplicate": "invalid named credential metadata", "human": "human identity"}[mode]
				if !strings.Contains(f.err.String(), diagnostic) {
					t.Fatalf("missing refusal %q", diagnostic)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("refusal mutated credential bytes")
				}
			}
		})
	}
}

// Even direct secret-reference helpers refuse file connections before the
// panicking stand-in, including on macOS and with empty service names.
func TestNamedFileReferenceRefusal(t *testing.T) {
	f := newNamedFileFixture(t)
	a := f.app()
	forbidFileKeychain(a)
	p := f.paths("alpha", false)
	if _, err := a.readConnectionSecret(context.Background(), p, config.SecretReference{}); err == nil || !strings.Contains(err.Error(), "file connections cannot read") {
		t.Fatal("file reference read admitted")
	}
	if err := a.createConnectionSecret(context.Background(), p, config.SecretReference{}, "forbidden"); err == nil || !strings.Contains(err.Error(), "file connections cannot create") {
		t.Fatal("file reference create admitted")
	}
}

// File consumers retain their selected profile across a current change; a
// backend change invalidates active/child pins before HTTP or state mutation.
func TestNamedFileBindingFence(t *testing.T) {
	f := newNamedFileFixture(t)
	a := f.app()
	a.profile = "alpha"
	forbidFileKeychain(a)
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := a.pinnedArgs("alice")
	if err != nil {
		t.Fatal(err)
	}
	f.r.Current = "beta"
	f.save()
	if _, err = s.client.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := f.app()
	forbidFileKeychain(child)
	if child.Run(context.Background(), append(pin, "whoami")) != 0 {
		t.Fatal("pinned file child retargeted current")
	}
	if err = config.SaveState(s.paths, s.name, config.State{NextBatch: "preserved"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	c := f.r.Profiles["alpha"]
	c.Store = f.r.Profiles["beta"].Store
	f.r.Profiles["alpha"] = c
	f.save()
	count := f.httpCalls.Load()
	if _, err = s.client.Whoami(context.Background()); err == nil || !strings.Contains(err.Error(), "selected connection changed") {
		t.Fatal("active backend change admitted")
	}
	child = f.app()
	forbidFileKeychain(child)
	if child.Run(context.Background(), append(pin, "whoami")) == 0 {
		t.Fatal("stale child admitted")
	}
	after, err := os.ReadFile(s.paths.StateFile(s.name))
	if err != nil || !bytes.Equal(before, after) || f.httpCalls.Load() != count {
		t.Fatal("stale binding produced effects")
	}
}

// Named file profiles explicitly refuse legacy migration/human mutation and
// bootstrap registration, leaving credentials intact and performing no HTTP.
func TestNamedFileMutationRefusals(t *testing.T) {
	for _, args := range [][]string{{"creds", "migrate"}, {"creds", "move-human", "alice"}, {"register", "newbie", "--authorize-registration"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			f := newNamedFileFixture(t)
			a := f.app()
			forbidFileKeychain(a)
			before, err := os.ReadFile(f.paths("alpha", false).AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if a.Run(context.Background(), args) == 0 || f.effects() != 0 {
				t.Fatal("unsupported mutation admitted")
			}
			if args[0] == "creds" && !strings.Contains(f.err.String(), "unavailable for named profiles") {
				t.Fatal("missing named mutation refusal")
			}
			after, err := os.ReadFile(f.paths("alpha", false).AgentFile("alice"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed file credentials")
			}
		})
	}
	// The direct named entry point must also refuse file bootstrap registration by name.
	f := newNamedFileFixture(t)
	a := f.app()
	forbidFileKeychain(a)
	a.authorizeRegistration = true
	_, _, err := a.registerNamedAgent(context.Background(), "alice")
	if err == nil || !strings.Contains(err.Error(), "file connections require connection register --via-admin-room") {
		t.Fatal("file bootstrap route did not refuse by name")
	}
	if _, err = os.Stat(filepath.Join(f.paths("alpha", false).Base, "registration.pending.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported bootstrap route left a journal")
	}
}

// Named file room retirement reaches alias deletion for an empty room; a
// message-bearing room retains its alias and neither path changes credentials.
func TestNamedFileRoomRetire(t *testing.T) {
	e := newEnv(t)
	uid, token := e.hs.addUser("alice")
	e.tokens["alice"] = token
	c := config.Connection{ID: "file-room-test", URL: e.hs.srv.URL, ServerName: testServer, Store: config.CredentialStore{Backend: "file"}}
	b, err := c.Binding("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Paths(e.paths(), "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.PublishBoundCredentials(p, "alice", false, config.Credentials{UserID: uid, AccessToken: token, Homeserver: c.URL, ConnectionBinding: &b}); err != nil {
		t.Fatal(err)
	}
	registry, err := json.Marshal(config.ConnectionRegistry{SchemaVersion: 1, Current: "file", Profiles: map[string]config.Connection{"file": c}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.paths().ConnectionsFile(), registry, 0600); err != nil {
		t.Fatal(err)
	}
	e.configure = forbidFileKeychain
	before, err := os.ReadFile(p.AgentFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "room", "new", "empty").ok(t)
	e.as("alice", "room", "retire", "empty").ok(t)
	if _, ok := e.hs.aliases["#empty:"+testServer]; ok {
		t.Fatal("empty room alias was not deleted")
	}
	e.as("alice", "room", "new", "busy").ok(t)
	e.as("alice", "topic", "new", "busy", "t1", "Topic").ok(t)
	e.as("alice", "room", "retire", "busy").failsWith(t, ExitError, "has messages")
	if _, ok := e.hs.aliases["#busy:"+testServer]; !ok {
		t.Fatal("nonempty room lost its alias")
	}
	after, err := os.ReadFile(p.AgentFile("alice"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("room deletion changed credentials")
	}
}
