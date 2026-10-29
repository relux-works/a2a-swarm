package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type namedTestStore struct {
	mu             sync.Mutex
	items          map[config.SecretReference]string
	reads, creates []config.SecretReference
	failToken      bool
}

func (s *namedTestStore) Read(_ context.Context, _ config.Paths, r config.SecretReference) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, r)
	v, ok := s.items[r]
	if !ok {
		return "", errKeychainMissing
	}
	return v, nil
}
func (s *namedTestStore) Create(_ context.Context, _ config.Paths, r config.SecretReference, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, r)
	if s.failToken && strings.HasSuffix(r.Service, ".token") {
		return errors.New("injected store refusal")
	}
	if _, ok := s.items[r]; ok {
		return errKeychainDuplicate
	}
	s.items[r] = v
	return nil
}

type namedFixture struct {
	t         *testing.T
	root      string
	r         config.ConnectionRegistry
	store     *namedTestStore
	env       map[string]string
	out, err  bytes.Buffer
	mu        sync.Mutex
	httpCalls atomic.Int32
	mode      string
}

func newNamedFixture(t *testing.T, metadata bool) *namedFixture {
	t.Helper()
	f := &namedFixture{t: t, root: t.TempDir(), env: map[string]string{}, store: &namedTestStore{items: map[config.SecretReference]string{}}}
	f.r = config.ConnectionRegistry{SchemaVersion: 1, Current: "alpha", Profiles: map[string]config.Connection{}}
	for _, label := range []string{"alpha", "beta"} {
		label := label
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.httpCalls.Add(1)
			mode := f.mode
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/register") {
				if mode == "ambiguous" {
					h := w.(http.Hijacker)
					conn, _, e := h.Hijack()
					if e == nil {
						conn.Close()
					}
					return
				}
				if mode == "rejected" {
					w.WriteHeader(403)
					io.WriteString(w, `{"errcode":"M_FORBIDDEN","error":"rejected"}`)
					return
				}
				fmt.Fprintf(w, `{"user_id":"@alice:%s.invalid","access_token":"isolated-new-token","device_id":"new-device"}`, label)
				return
			}
			if r.Header.Get("Authorization") != "Bearer isolated-"+label {
				w.WriteHeader(401)
				io.WriteString(w, `{"errcode":"M_UNKNOWN_TOKEN"}`)
				return
			}
			fmt.Fprintf(w, `{"user_id":"@alice:%s.invalid","device_id":"test"}`, label)
		}))
		t.Cleanup(srv.Close)
		c := config.Connection{ID: label + "-id", URL: srv.URL, ServerName: label + ".invalid", Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "test." + label + ".token", PasswordService: "test." + label + ".password"}, RegistrationTokenRef: &config.SecretReference{Service: "test." + label + ".bootstrap", Account: "admitted-agent"}}
		f.r.Profiles[label] = c
		f.store.items[*c.RegistrationTokenRef] = "isolated-bootstrap-" + label
		if metadata {
			f.publish(label, false)
			f.store.items[config.SecretReference{Service: c.Store.AccessTokenService, Account: "@alice:" + c.ServerName}] = "isolated-" + label
		}
	}
	f.save()
	return f
}
func (f *namedFixture) save() {
	f.t.Helper()
	b, e := json.Marshal(f.r)
	if e != nil {
		f.t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(f.root, "connections.json"), b, 0600); e != nil {
		f.t.Fatal(e)
	}
}
func (f *namedFixture) paths(label string, human bool) config.Paths {
	p, e := f.r.Profiles[label].Paths(config.Paths{Base: f.root}, "alice", human)
	if e != nil {
		f.t.Fatal(e)
	}
	return p
}
func (f *namedFixture) publish(label string, human bool) {
	f.t.Helper()
	c := f.r.Profiles[label]
	b, _ := c.Binding("alice", human)
	if e := config.PublishBoundCredentials(f.paths(label, human), "alice", human, config.Credentials{UserID: b.UserID, Homeserver: c.URL, ConnectionBinding: &b}); e != nil {
		f.t.Fatal(e)
	}
}
func (f *namedFixture) app() *App {
	a := NewApp()
	a.Stdout = &f.out
	a.Stderr = &f.err
	a.connectionSecretStore = f.store
	a.Getenv = func(k string) string {
		switch k {
		case config.EnvConfigDir:
			return f.root
		case config.EnvAgent:
			return "alice"
		}
		return f.env[k]
	}
	return a
}
func (f *namedFixture) effects() int {
	f.store.mu.Lock()
	n := len(f.store.reads) + len(f.store.creates)
	f.store.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	return n + int(f.httpCalls.Load())
}

// Proves early selection/environment/metadata failures cause zero secret or
// HTTP effects. Every case has the same nearby functional selected control.
func TestNamedSelectionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		change func(*namedFixture)
	}{
		{"empty", []string{"--profile=", "whoami"}, nil},
		{"conflicting", []string{"--profile", "alpha", "--profile", "beta", "whoami"}, nil},
		{"unknown", []string{"--profile", "missing", "whoami"}, nil},
		{"no-registry", []string{"--profile", "alpha", "whoami"}, func(f *namedFixture) { os.Remove(filepath.Join(f.root, "connections.json")) }},
		{"malformed", []string{"whoami"}, func(f *namedFixture) {
			os.WriteFile(filepath.Join(f.root, "connections.json"), []byte(`{"schema_version":1,"current":"alpha","current":"beta"}`), 0600)
		}},
		{"current", []string{"whoami"}, func(f *namedFixture) { f.r.Current = "missing"; f.save() }},
		{"url", []string{"whoami"}, func(f *namedFixture) { f.env[config.EnvMatrixURL] = "https://wrong.invalid" }},
		{"server", []string{"whoami"}, func(f *namedFixture) { f.env[config.EnvServerName] = "wrong.invalid" }},
		{"store", []string{"whoami"}, func(f *namedFixture) { f.env[envSecretStore] = "file" }},
		{"unknown-store", []string{"whoami"}, func(f *namedFixture) { f.env[envSecretStore] = "unrecognized" }},
		{"missing-metadata", []string{"whoami"}, func(f *namedFixture) { os.Remove(f.paths("alpha", false).AgentFile("alice")) }},
		{"identity-guard", []string{"--as", "bob", "whoami"}, nil},
		{"human-guard", []string{"--human", "--as", "alice", "whoami"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			if tc.change != nil {
				tc.change(f)
			}
			if code := f.app().Run(context.Background(), tc.args); code == 0 {
				t.Fatal("invalid invocation succeeded")
			}
			if f.effects() != 0 {
				t.Fatalf("early refusal produced %d effects", f.effects())
			}
		})
	}
	f := newNamedFixture(t, true)
	f.env[config.EnvMatrixURL] = f.r.Profiles["alpha"].URL
	f.env[config.EnvServerName] = "alpha.invalid"
	f.env[envSecretStore] = "keychain"
	f.env[config.EnvRegistrationToken] = "never-used-inherited-token"
	if code := f.app().Run(context.Background(), []string{"whoami"}); code != 0 {
		t.Fatalf("valid named selection: %s", f.err.String())
	}
	if f.httpCalls.Load() != 1 || len(f.store.reads) != 1 {
		t.Fatal("functional control did not use intended store/transport")
	}
	for _, r := range f.store.reads {
		if strings.Contains(r.Service, "bootstrap") {
			t.Fatal("ordinary command read bootstrap")
		}
	}
}

// Proves selected metadata cannot redirect credentials or leak file secrets,
// and a missing selected item never falls back to another profile or legacy file.
func TestNamedMetadataNoFallback(t *testing.T) {
	for _, field := range []string{"connection", "class", "mxid", "url", "service", "secret", "duplicate", "missing-item"} {
		t.Run(field, func(t *testing.T) {
			f := newNamedFixture(t, true)
			p := f.paths("alpha", false)
			path := p.AgentFile("alice")
			raw, _ := os.ReadFile(path)
			var c config.Credentials
			json.Unmarshal(raw, &c)
			switch field {
			case "connection":
				c.ConnectionBinding.Connection.ID = "other"
			case "class":
				c.ConnectionBinding.Class = "humans"
			case "mxid":
				c.UserID = "@alice:beta.invalid"
			case "url":
				c.Homeserver = "https://other.invalid"
			case "service":
				c.ConnectionBinding.Connection.Store.AccessTokenService = "test.beta.token"
			case "secret":
				c.AccessToken = "must-not-appear"
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"user_id":`), []byte(`"user_id":"@alice:beta.invalid","user_id":`), 1)
			case "missing-item":
				delete(f.store.items, config.SecretReference{Service: f.r.Profiles["alpha"].Store.AccessTokenService, Account: c.UserID})
			}
			if field != "duplicate" {
				raw, _ = json.Marshal(c)
			}
			os.WriteFile(path, raw, 0600)
			if f.app().Run(context.Background(), []string{"whoami"}) == 0 {
				t.Fatal("bad binding succeeded")
			}
			if f.httpCalls.Load() != 0 {
				t.Fatal("invalid binding contacted server")
			}
			want := 0
			if field == "missing-item" {
				want = 1
			}
			if len(f.store.reads) != want {
				t.Fatal("unexpected secret lookup")
			}
			if strings.Contains(f.err.String(), "must-not-appear") {
				t.Fatal("metadata secret leaked")
			}
		})
	}
}

// Proves active and newly executed pinned consumers retain functional traffic
// after current/unrelated edits. Selected edits refuse before effects and leave
// the existing state/mailbox/lease witness bytes unchanged.
func TestNamedConsumerPinAndState(t *testing.T) {
	for _, change := range []string{"service", "endpoint", "identity-domain", "connection-id"} {
		t.Run(change, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.profile = "alpha"
			s, e := a.session()
			if e != nil {
				t.Fatal(e)
			}
			pin, e := a.pinnedArgs("alice")
			if e != nil {
				t.Fatal(e)
			}
			p := s.paths
			os.MkdirAll(filepath.Dir(p.StateFile("alice")), 0700)
			state := []byte(`{"next_batch":"old-cursor","lease":{"holder":"original"}}`)
			os.WriteFile(p.StateFile("alice"), state, 0600)
			mailbox := []byte("unreceipted-event-and-original-receipt-witness")
			os.WriteFile(p.MailboxFile("alice"), mailbox, 0600)
			f.r.Current = "beta"
			c := f.r.Profiles["beta"]
			c.Store.AccessTokenService = "test.unrelated.token"
			f.r.Profiles["beta"] = c
			f.save()
			if _, e = s.client.Whoami(context.Background()); e != nil {
				t.Fatal(e)
			}
			if code := f.app().Run(context.Background(), append(pin, "whoami")); code != 0 {
				t.Fatalf("pinned child failed after current change: %s", f.err.String())
			}
			if f.httpCalls.Load() != 2 {
				t.Fatal("pinned traffic was not functional")
			}
			if len(f.store.reads) != 2 {
				t.Fatal("unexpected selected lookups")
			}
			for _, r := range f.store.reads {
				if r.Service != "test.alpha.token" {
					t.Fatal("consumer retargeted store")
				}
			}
			before := f.effects()
			c = f.r.Profiles["alpha"]
			switch change {
			case "service":
				c.Store.AccessTokenService = "test.selected-change.token"
			case "endpoint":
				c.URL = f.r.Profiles["beta"].URL
			case "identity-domain":
				c.ServerName = "changed.invalid"
			case "connection-id":
				c.ID = "changed-id"
			}
			f.r.Profiles["alpha"] = c
			f.save()
			if _, e = s.client.Whoami(context.Background()); e == nil {
				t.Fatal("active stale binding admitted")
			}
			if f.app().Run(context.Background(), append(pin, "whoami")) == 0 {
				t.Fatal("stale child admitted")
			}
			if f.effects() != before {
				t.Fatal("changed selection caused effects")
			}
			got, _ := os.ReadFile(p.StateFile("alice"))
			if !bytes.Equal(got, state) {
				t.Fatal("refusal changed state/lease")
			}
			gotMailbox, _ := os.ReadFile(p.MailboxFile("alice"))
			if !bytes.Equal(gotMailbox, mailbox) {
				t.Fatal("refusal changed mailbox/receipts")
			}
			if f.paths("alpha", false).Base == f.paths("beta", false).Base {
				t.Fatal("same localpart shares state")
			}
		})
	}

}

// Proves public management is offline, cannot overwrite an entry, and use/add
// preserve explicit current semantics without credential lookup.
func TestNamedConnectionManagement(t *testing.T) {
	f := newNamedFixture(t, true)
	for _, args := range [][]string{{"connection", "list"}, {"connection", "show", "beta"}, {"connection", "use", "beta"}} {
		if f.app().Run(context.Background(), args) != 0 {
			t.Fatal(f.err.String())
		}
	}
	c := f.r.Profiles["alpha"]
	c.ID = "third-id"
	raw, _ := json.Marshal(c)
	file := filepath.Join(f.root, "public-entry.json")
	os.WriteFile(file, raw, 0600)
	if f.app().Run(context.Background(), []string{"connection", "add", "third", "--file", file}) != 0 {
		t.Fatal(f.err.String())
	}
	before, _ := os.ReadFile(filepath.Join(f.root, "connections.json"))
	if f.app().Run(context.Background(), []string{"connection", "add", "third", "--file", file}) == 0 {
		t.Fatal("replacement admitted")
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "connections.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused add mutated registry")
	}
	var r config.ConnectionRegistry
	json.Unmarshal(after, &r)
	if r.Current != "beta" {
		t.Fatal("add retargeted current")
	}
	if f.effects() != 0 {
		t.Fatal("management consulted secret/network")
	}
}

// Proves authorized first creation uses only selected bootstrap and exclusive
// selected items. Negative outcomes retain precise private recovery evidence,
// publish no credentials and never repeat a remote attempt on re-invocation.
func TestNamedFirstRegistration(t *testing.T) {
	for _, mode := range []string{"success", "unauthorized", "human", "bound-other", "no-bootstrap-ref", "missing-bootstrap", "existing-metadata", "existing-item", "token-store-failure", "publication-interruption", "ambiguous", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, false)
			a := f.app()
			args := []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}
			switch mode {
			case "unauthorized":
				args = args[:len(args)-1]
			case "human":
				args = append(args, "--human", "--as", "alice")
				a.stdinTTY = func() bool { return true }
			case "bound-other":
				args[3] = "bob" // command is invalid without any secret effects
			case "no-bootstrap-ref":
				c := f.r.Profiles["alpha"]
				c.RegistrationTokenRef = nil
				f.r.Profiles["alpha"] = c
				f.save()
			case "missing-bootstrap":
				delete(f.store.items, *f.r.Profiles["alpha"].RegistrationTokenRef)
			case "existing-metadata":
				f.publish("alpha", false)
			case "existing-item":
				f.store.items[config.SecretReference{Service: "test.alpha.token", Account: "@alice:alpha.invalid"}] = "pre-existing"
			case "token-store-failure":
				f.store.failToken = true
			case "publication-interruption":
				a.beforeNamedPublish = func() error { return errors.New("injected interruption") }
			case "ambiguous", "rejected":
				f.mode = mode
			}
			code := a.Run(context.Background(), args)
			p := f.paths("alpha", false)
			if mode == "success" {
				if code != 0 {
					t.Fatal(f.err.String())
				}
				creds, e := config.LoadCredentialMetadata(p, "alice", false)
				if e != nil {
					t.Fatal(e)
				}
				b, _ := f.r.Profiles["alpha"].Binding("alice", false)
				if e = config.ValidateBoundCredentials(creds, b); e != nil {
					t.Fatal(e)
				}
				if f.store.items[config.SecretReference{Service: "test.alpha.token", Account: b.UserID}] != "isolated-new-token" {
					t.Fatal("selected token was not stored")
				}
				if f.httpCalls.Load() != 1 {
					t.Fatal("unexpected registration count")
				}
			} else {
				if code == 0 {
					t.Fatal("negative registration succeeded")
				}
				if mode != "existing-metadata" {
					if _, e := os.Lstat(p.AgentFile("alice")); !errors.Is(e, os.ErrNotExist) {
						t.Fatal("failed registration published metadata")
					}
				}
				stages := map[string]string{"token-store-failure": "remote-created-token-store-failed", "publication-interruption": "remote-created-metadata-publication-pending", "ambiguous": "remote-outcome-ambiguous", "rejected": "remote-rejected"}
				if want, ok := stages[mode]; ok {
					raw, e := os.ReadFile(filepath.Join(p.Base, "registration.pending.json"))
					if e != nil {
						t.Fatal(e)
					}
					var j registrationJournal
					if e = json.Unmarshal(raw, &j); e != nil || j.Stage != want {
						t.Fatalf("recovery stage %s want %s", j.Stage, want)
					}
					if f.httpCalls.Load() != 1 {
						t.Fatalf("attempts %d", f.httpCalls.Load())
					}
					if f.app().Run(context.Background(), args) == 0 || f.httpCalls.Load() != 1 {
						t.Fatal("pending registration repeated remote creation")
					}
				} else if f.httpCalls.Load() != 0 {
					t.Fatal("early refusal contacted server")
				}
			}
			for _, r := range append(f.store.reads, f.store.creates...) {
				if !strings.HasPrefix(r.Service, "test.alpha.") {
					t.Fatal("cross-profile credential access")
				}
			}
			if strings.Contains(f.out.String()+f.err.String(), "isolated-new-token") || strings.Contains(f.out.String()+f.err.String(), "isolated-bootstrap") {
				t.Fatal("secret leaked in output")
			}
		})
	}
}

// Bounded instrumentation control: intentional forbidden lookup and HTTP call
// must each trip the same effect counter used by zero-effect refusal tests.
func TestNamedEffectInstrumentControl(t *testing.T) {
	f := newNamedFixture(t, true)
	if f.effects() != 0 {
		t.Fatal("dirty initial instrument")
	}
	f.store.Read(context.Background(), config.Paths{}, config.SecretReference{Service: "planted", Account: "violation"})
	if f.effects() != 1 {
		t.Fatal("secret instrument missed planted lookup")
	}
	resp, e := http.Get(f.r.Profiles["beta"].URL + "/_matrix/client/v3/account/whoami")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if f.effects() != 2 {
		t.Fatal("HTTP instrument missed planted request")
	}
}

// Proves named hook commands carry root, explicit selection and operational pin,
// remain recognizable for idempotent replacement, and keep legacy syntax valid.
func TestNamedHookPinGrammar(t *testing.T) {
	f := newNamedFixture(t, true)
	renamed := f.root + " space'quote"
	if e := os.Rename(f.root, renamed); e != nil {
		t.Fatal(e)
	}
	f.root = renamed
	t.Cleanup(func() { os.RemoveAll(renamed) })
	a := f.app()
	a.profile = "alpha"
	a.as = "alice"
	a.exe = func() (string, error) { return "/synthetic/a2a", nil }
	settings := filepath.Join(f.root, "hooks.json")
	args := []string{"--agent", "alice", "--runtime", "codex", "--settings", settings}
	if e := cmdHookInstall(a, args, true); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(settings)
	if !strings.Contains(string(raw), "--profile alpha --connection-fingerprint") || !strings.Contains(string(raw), "A2A_CONFIG_DIR=") {
		t.Fatal("hook missing selection/root pin")
	}
	if e := cmdHookInstall(a, args, true); e != nil {
		t.Fatal(e)
	}
	again, _ := os.ReadFile(settings)
	if !bytes.Equal(raw, again) {
		t.Fatal("pinned hooks not idempotent")
	}
}

// Owner decision 74: creating a named Keychain secret refuses for a human
// identity before the store is touched; the same call for an agent creates it.
func TestNamedHumanSecretCreateRefused(t *testing.T) {
	f := newNamedFixture(t, true)
	ref := config.SecretReference{Service: "test.alpha.token", Account: "@carol:alpha.invalid"}
	a := f.app()
	a.human = true
	if e := a.createConnectionSecret(context.Background(), f.paths("alpha", true), ref, "isolated-new"); !errors.Is(e, errHumanKeychain) || f.effects() != 0 {
		t.Fatalf("human create admitted: %v effects=%d", e, f.effects())
	}
	if e := f.app().createConnectionSecret(context.Background(), f.paths("alpha", false), ref, "isolated-new"); e != nil || f.effects() != 1 {
		t.Fatalf("agent control failed: %v effects=%d", e, f.effects())
	}
}

// Proves a human name cannot be reinterpreted through a parallel agents file,
// and that the explicitly interactive human path never reads a named profile's
// Keychain secret (owner decision 74) while keeping its own class namespace.
func TestNamedHumanClassBoundary(t *testing.T) {
	f := newNamedFixture(t, true)
	f.publish("alpha", true)
	if f.app().Run(context.Background(), []string{"whoami"}) == 0 || f.effects() != 0 {
		t.Fatal("human name reinterpreted as agent")
	}
	a := f.app()
	a.stdinTTY = func() bool { return true }
	if a.Run(context.Background(), []string{"--human", "--as", "alice", "whoami"}) == 0 || f.effects() != 0 {
		t.Fatal("human path read a named Keychain secret")
	}
	if !strings.Contains(f.err.String(), "owner decision 74") {
		t.Fatal("human refusal not stated", f.err.String())
	}
	if f.paths("alpha", true).Base == f.paths("alpha", false).Base {
		t.Fatal("human state shared with agent")
	}
}

// Proves the shared session fence covers local consumer work that need not
// issue HTTP. Valid current/unrelated changes still permit lease/delivery work;
// a selected change returns the binding error before callbacks or state writes.
func TestNamedLocalConsumerEffectBoundaries(t *testing.T) {
	for _, op := range []string{"lease", "mailbox", "terminal-dispatch", "terminal-update", "runtime-outcome", "liveness"} {
		t.Run(op, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.profile = "alpha"
			s, e := a.session()
			if e != nil {
				t.Fatal(e)
			}
			if e = config.SaveState(s.paths, s.name, config.State{NextBatch: "original"}); e != nil {
				t.Fatal(e)
			}
			f.r.Current = "beta"
			f.save()
			if e = s.checkConnection(); e != nil {
				t.Fatal(e)
			}
			if e = a.acquireLease(s, "test-session", time.Now().Add(time.Second)); e != nil {
				t.Fatal(e)
			}
			callback := false
			if _, e = s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(*config.State, *config.Mailbox) ([]string, error) { callback = true; return nil, nil }); e != nil || !callback {
				t.Fatalf("valid local delivery control: %v", e)
			}
			before, _ := os.ReadFile(s.paths.StateFile(s.name))
			beforeMail, _ := os.ReadFile(s.paths.MailboxFile(s.name))
			c := f.r.Profiles["alpha"]
			c.Store.AccessTokenService = "test.changed.token"
			f.r.Profiles["alpha"] = c
			f.save()
			callback = false
			effects := f.effects()
			switch op {
			case "lease":
				e = a.acquireLease(s, "forbidden", time.Now().Add(time.Second))
			case "mailbox":
				_, e = s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(*config.State, *config.Mailbox) ([]string, error) { callback = true; return nil, nil })
			case "terminal-dispatch":
				e = a.dispatchTerminal(context.Background(), s, "not-a-real-terminal")
			case "terminal-update":
				e = a.terminalUpdate(s, "not-a-real-terminal", func(*config.TerminalBinding) { callback = true })
			case "runtime-outcome":
				e = a.runtimeOutcome(s, "not-a-real-runtime", "forbidden")
			case "liveness":
				e = s.writeLivenessSource(context.Background(), config.AgentStatus{}, "runtime")
			}
			if e == nil || !strings.Contains(e.Error(), "selected connection changed") {
				t.Fatalf("expected binding refusal, got %v", e)
			}
			after, _ := os.ReadFile(s.paths.StateFile(s.name))
			afterMail, _ := os.ReadFile(s.paths.MailboxFile(s.name))
			if callback || !bytes.Equal(before, after) || !bytes.Equal(beforeMail, afterMail) || f.effects() != effects {
				t.Fatal("stale local consumer produced effects")
			}
		})
	}
}
