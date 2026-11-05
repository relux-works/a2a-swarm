package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type presenceTripwire struct{ calls int }

func (p *presenceTripwire) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	p.calls++
	return "", errors.New("forbidden secret read")
}
func (p *presenceTripwire) Create(context.Context, config.Paths, config.SecretReference, string) error {
	p.calls++
	return errors.New("forbidden secret create")
}
func (p *presenceTripwire) RoundTrip(*http.Request) (*http.Response, error) {
	p.calls++
	return nil, errors.New("forbidden HTTP")
}

type presenceCLI struct {
	t        *testing.T
	root     string
	env      map[string]string
	c        config.Connection
	out, err bytes.Buffer
	poison   presenceTripwire
}

func newPresenceCLI(t *testing.T, named bool) *presenceCLI {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	f := &presenceCLI{t: t, root: root, env: map[string]string{config.EnvAgent: "real-custodian"}, c: config.Connection{ID: "selected-id", URL: "https://api.example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "test.token", PasswordService: "test.password"}}}
	if named {
		f.write(filepath.Join(f.root, "connections.json"), config.ConnectionRegistry{SchemaVersion: 1, Current: "work", Profiles: map[string]config.Connection{"work": f.c}})
	}
	return f
}

func (f *presenceCLI) write(path string, value any) {
	f.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *presenceCLI) publish(stage string) config.Paths {
	f.t.Helper()
	p, err := f.c.Paths(config.Paths{Base: f.root}, "target", false)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := f.c.Binding("target", false)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = config.PublishBoundCredentials(p, "target", false, config.Credentials{ConnectionBinding: &b, UserID: b.UserID, Homeserver: f.c.URL}); err != nil {
		f.t.Fatal(err)
	}
	if stage != "" {
		f.write(filepath.Join(p.Base, "registration.pending.json"), registrationJournal{Binding: b, Stage: stage})
	}
	return p
}

func (f *presenceCLI) app() *App {
	a := NewApp()
	a.Stdout = &f.out
	a.Stderr = &f.err
	a.HTTP = &http.Client{Transport: &f.poison}
	a.connectionSecretStore = &f.poison
	a.Getenv = func(key string) string {
		if key == config.EnvConfigDir {
			return f.root
		}
		return f.env[key]
	}
	a.keychain = func(p config.Paths) *keychainStore {
		f.poison.calls++
		// If a regression reaches native process construction it fails closed in
		// the fixture before any real command can be started.
		stop := func(context.Context, ...string) *exec.Cmd {
			f.poison.calls++
			f.t.Fatal("forbidden native credential process")
			return nil
		}
		return &keychainStore{paths: p, command: stop, probeCommand: stop}
	}
	a.beforeNamedPublish = func() error { f.poison.calls++; return errors.New("forbidden publish") }
	return a
}

func presenceTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += "/" + link
		} else if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "/" + stringHashPresence(raw)
		}
		out[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func stringHashPresence(raw []byte) string { sum := sha256.Sum256(raw); return string(sum[:]) }

func (f *presenceCLI) run(args []string, exit int, state, evidence, reason string) {
	f.t.Helper()
	before := presenceTree(f.t, f.root)
	actor := f.env[config.EnvAgent]
	got := f.app().Run(context.Background(), args)
	if got != exit {
		f.t.Fatalf("exit=%d want=%d stderr=%q", got, exit, f.err.String())
	}
	if f.poison.calls != 0 {
		f.t.Fatal("forbidden backend/HTTP/publish/process path")
	}
	if !reflect.DeepEqual(before, presenceTree(f.t, f.root)) || actor != f.env[config.EnvAgent] {
		f.t.Fatal("query changed fixture or caller")
	}
	if state == "" {
		if f.out.Len() != 0 {
			f.t.Fatal("usage emitted result")
		}
		return
	}
	var result config.AgentPresence
	decoder := json.NewDecoder(&f.out)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		f.t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		f.t.Fatal("extra output")
	}
	if result != (config.AgentPresence{Schema: "a2a.local-identity-presence/1", State: state, Evidence: evidence, Reason: reason}) {
		f.t.Fatalf("unexpected result %#v", result)
	}
	if exit == 0 && f.err.Len() != 0 {
		f.t.Fatal("unexpected warning/generic preflight")
	}
	if strings.Contains(f.err.String(), f.root) || strings.Contains(f.err.String(), "sensitive-fixture") {
		f.t.Fatal("unsafe diagnostic")
	}
}

var presenceArgs = []string{"--profile", "work", "connection", "identity-presence", "--target-agent", "target", "--json"}

// Proves real ACTOR differs from TARGET and public named/complete/legacy/absence
// snapshots avoid HTTP/backend/native/publish seams and preserve fixture bytes.
// Legacy positive says nothing about whether its unopened token bytes are valid.
func TestIdentityPresenceCLIStates(t *testing.T) {
	for _, stage := range []string{"", "complete", "bootstrap-unavailable", "remote-outcome-ambiguous"} {
		t.Run("named-"+stage, func(t *testing.T) {
			f := newPresenceCLI(t, true)
			f.publish(stage)
			// An ordinary warning would inspect unsafe caller owner state. The
			// query must neither follow it nor print a warning/target-independent data.
			if err := os.MkdirAll(filepath.Join(f.root, "owner-dm"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/nonexistent-private-fixture", filepath.Join(f.root, "owner-dm", "real-custodian.json")); err != nil {
				t.Fatal(err)
			}
			if stage == "" || stage == "complete" {
				f.run(presenceArgs, 0, "present", "named_bound_metadata", "validated")
			} else {
				f.run(presenceArgs, 0, "pending", "registration_journal", "registration_incomplete")
			}
		})
	}
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-absent", true: "named-absent"}[named], func(t *testing.T) {
			f := newPresenceCLI(t, named)
			args := presenceArgs
			if !named {
				args = args[2:]
			}
			f.run(args, 0, "absent", "none", "local_absence")
		})
	}
	f := newPresenceCLI(t, false)
	if err := os.Mkdir(filepath.Join(f.root, "agents"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "agents", "target.json"), []byte("sensitive-fixture token-bearing invalid JSON\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	f.run(presenceArgs[2:], 0, "present", "legacy_regular_file_only", "regular_file_only")
}

// Proves every --as form, human mode, absent/unsafe actor, unsafe target and
// ambiguous flags refuse with usage2 before inspecting selected target files.
func TestIdentityPresenceCallerAndUsage(t *testing.T) {
	for _, extra := range [][]string{{"--as", "real-custodian"}, {"--as", "target"}, {"--as="}, {"-as=real-custodian"}, {"--human"}, {"--target-agent", "target"}, {"--json"}, {"--target-agent", "../sensitive-fixture"}, {"--unknown=sensitive-fixture"}} {
		t.Run(strings.Join(extra, "/"), func(t *testing.T) {
			f := newPresenceCLI(t, true)
			f.publish("complete")
			f.run(append(append([]string{}, presenceArgs...), extra...), 2, "", "", "")
		})
	}
	for _, actor := range []string{"", "../sensitive-fixture"} {
		f := newPresenceCLI(t, true)
		f.env[config.EnvAgent] = actor
		f.run(presenceArgs, 2, "", "", "")
	}
	f := newPresenceCLI(t, true)
	f.publish("complete")
	f.run(presenceArgs[:len(presenceArgs)-1], 2, "", "", "")
}

// Proves routing/fingerprint/identity/registry/journal errors emit only bounded
// inconclusive codes; neither legacy nor another valid profile can mask them.
func TestIdentityPresenceCLIRefusals(t *testing.T) {
	for _, variant := range []string{"no-profile", "wrong-profile", "no-registry", "url", "server", "store", "fingerprint", "wrong-target", "secret", "bad-journal", "human", "routing-file", "routing-file-duplicate", "matching-routing", "matching-routing-file"} {
		t.Run(variant, func(t *testing.T) {
			f := newPresenceCLI(t, true)
			p := f.publish("complete")
			args := append([]string{}, presenceArgs...)
			reason := "routing_conflict"
			switch variant {
			case "no-profile":
				args = args[2:]
				reason = "invalid_selection"
			case "wrong-profile":
				args[1] = "unknown"
				reason = "invalid_selection"
			case "no-registry":
				if err := os.Remove(filepath.Join(f.root, "connections.json")); err != nil {
					t.Fatal(err)
				}
				reason = "invalid_selection"
			case "url":
				f.env[config.EnvMatrixURL] = "https://wrong.invalid"
			case "server":
				f.env[config.EnvServerName] = "wrong.invalid"
			case "store":
				f.env[envSecretStore] = "file"
			case "fingerprint":
				args = append(args, "--connection-fingerprint", strings.Repeat("a", 64))
				reason = "binding_mismatch"
			case "wrong-target":
				b, _ := f.c.Binding("other", false)
				f.write(p.AgentFile("target"), config.Credentials{ConnectionBinding: &b, UserID: b.UserID, Homeserver: f.c.URL})
				reason = "invalid_metadata"
			case "secret":
				raw, _ := os.ReadFile(p.AgentFile("target"))
				raw = append([]byte(`{"access_token":"sensitive-fixture",`), raw[1:]...)
				if err := os.WriteFile(p.AgentFile("target"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				reason = "invalid_metadata"
			case "bad-journal":
				f.write(filepath.Join(p.Base, "registration.pending.json"), map[string]string{"stage": "sensitive-fixture"})
				reason = "invalid_journal"
			case "human":
				f.write(p.HumanFile("target"), map[string]string{"ignored": "never opened"})
				reason = "identity_class"
			case "routing-file":
				if err := os.WriteFile(filepath.Join(f.root, "matrix.env"), []byte("A2A_MATRIX_URL=https://wrong.invalid\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "routing-file-duplicate":
				if err := os.WriteFile(filepath.Join(f.root, "matrix.env"), []byte("A2A_MATRIX_SERVER_NAME=example.invalid\nA2A_MATRIX_SERVER_NAME=example.invalid\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "matching-routing":
				f.env[config.EnvMatrixURL] = f.c.URL
				f.env[config.EnvServerName] = f.c.ServerName
				f.env[envSecretStore] = "keychain"
				f.env[config.EnvRegistrationToken] = "sensitive-fixture"
			case "matching-routing-file":
				if err := os.WriteFile(filepath.Join(f.root, "matrix.env"), []byte("A2A_MATRIX_SERVER_NAME=example.invalid\nA2A_MATRIX_REGISTRATION_TOKEN=sensitive-fixture\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(variant, "matching") {
				f.run(args, 0, "present", "named_bound_metadata", "validated")
			} else {
				f.run(args, 1, "inconclusive", "none", reason)
			}
		})
	}
}

// Proves the existing successful first-registration fixture's actual retained
// journal remains accepted by the query under a different synthetic actor.
// Setup uses only the existing fake HTTP/store, never a live account/backend.
func TestIdentityPresenceRegistrationCompatibility(t *testing.T) {
	f := newNamedFixture(t, false)
	if err := os.Chmod(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if code := f.app().Run(context.Background(), []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}); code != 0 {
		t.Fatal(f.err.String())
	}
	before := presenceTree(t, f.root)
	effects := f.effects()
	a := f.app()
	oldenv := a.Getenv
	a.Getenv = func(k string) string {
		if k == config.EnvAgent {
			return "real-custodian"
		}
		return oldenv(k)
	}
	var output, diagnostics bytes.Buffer
	a.Stdout = &output
	a.Stderr = &diagnostics
	if code := a.Run(context.Background(), []string{"--profile", "alpha", "connection", "identity-presence", "--target-agent", "alice", "--json"}); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	var result config.AgentPresence
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "present" || result.Evidence != "named_bound_metadata" || f.effects() != effects || !reflect.DeepEqual(before, presenceTree(t, f.root)) {
		t.Fatal("actual complete journal compatibility/effects")
	}
}

// The counter detects deliberate secret-read/create, HTTP and mutation plants;
// no plant invokes native credentials or a live server. Nearby query positives
// above establish zero counts rather than accepting a blanket refusal.
func TestIdentityPresenceEffectInstrumentControls(t *testing.T) {
	p := &presenceTripwire{}
	p.Read(context.Background(), config.Paths{}, config.SecretReference{})
	p.Create(context.Background(), config.Paths{}, config.SecretReference{}, "fixture")
	p.RoundTrip(&http.Request{})
	if p.calls != 3 {
		t.Fatal("forbidden-call counter missed intended plants")
	}
	root := t.TempDir()
	before := presenceTree(t, root)
	if err := os.WriteFile(filepath.Join(root, "planted-mutation"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, presenceTree(t, root)) {
		t.Fatal("fixture-tree instrument missed intended mutation")
	}
}

// This bounded static instrument guards named credential decoders/writers in
// the new inspector/handler files, not arbitrary future transitive call graphs.
func presenceForbiddenAPIs(source []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "presence.go", source, 0)
	if err != nil {
		return nil, err
	}
	forbidden := map[string]bool{"Credentials": true, "LoadCredentialMetadata": true, "loadCredentials": true, "LoadMatrix": true, "loadMatrix": true, "selectedConnection": true, "namedMetadata": true, "warnOwnerDM": true, "readConnectionSecret": true, "createConnectionSecret": true, "keychainStore": true, "Write": true, "WriteFile": true, "Mkdir": true, "MkdirAll": true, "Remove": true, "RemoveAll": true, "Rename": true, "Create": true, "lockFile": true, "SaveState": true, "UpdateState": true, "PublishBoundCredentials": true, "LoadState": true}
	var hits []string
	ast.Inspect(file, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && forbidden[id.Name] {
			hits = append(hits, id.Name)
		}
		return true
	})
	return hits, nil
}

// Positive real files pair with intentional credential/writer violation snippets;
// parse/setup errors are not credited as intended rejection controls.
func TestIdentityPresenceSourceInstrumentControls(t *testing.T) {
	for _, path := range []string{"identity_presence.go", "../config/presence.go", "../config/presence_unix.go", "../config/presence_other.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := presenceForbiddenAPIs(raw)
		if err != nil || len(hits) != 0 {
			t.Fatalf("public query forbidden APIs %s: %v %v", path, hits, err)
		}
	}
	for _, symbol := range []string{"LoadCredentialMetadata", "Credentials", "WriteFile", "UpdateState", "readConnectionSecret"} {
		source := []byte("package fixture\nfunc plant(){ _ = " + symbol + " }\n")
		hits, err := presenceForbiddenAPIs(source)
		if err != nil || len(hits) != 1 || hits[0] != symbol {
			t.Fatalf("intended %s plant not detected: %v %v", symbol, hits, err)
		}
	}
}
