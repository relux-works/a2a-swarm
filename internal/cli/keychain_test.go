//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

type securityModel struct {
	Items  map[string]string
	Mode   string
	Reads  int
	Writes int
}

// opRecord is one backend operation as the fake saw it. The secret is never
// recorded, so a record can never leak it.
type opRecord struct {
	Op, Keychain, Service, Account string
}

func modelFile(dir string) string { return filepath.Join(dir, "model.json") }
func saveModel(t *testing.T, dir string, m securityModel) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := os.WriteFile(modelFile(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func readModel(t *testing.T, dir string) securityModel {
	t.Helper()
	data, err := os.ReadFile(modelFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m securityModel
	if err = json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func records(t *testing.T, dir string) []opRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "ops.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []opRecord
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r opRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

// fakeBackend models the Keychain over a JSON file, so independent CLI test
// processes share it. It NEVER touches a real keychain. Its modes plant the
// failures the store must handle: interaction refusals, timeouts, duplicates,
// readback mismatches and error text carrying the secret.
type fakeBackend struct{ dir string }

func (f fakeBackend) load() (securityModel, error) {
	var m securityModel
	data, err := os.ReadFile(modelFile(f.dir))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if m.Items == nil {
		m.Items = map[string]string{}
	}
	return m, err
}

func (f fakeBackend) save(m securityModel) error {
	data, _ := json.Marshal(m)
	return os.WriteFile(modelFile(f.dir), data, 0o600)
}

func (f fakeBackend) record(op, keychain, service, account string) error {
	file, err := os.OpenFile(filepath.Join(f.dir, "ops.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(opRecord{op, keychain, service, account})
}

func (f fakeBackend) find(ctx context.Context, keychain, service, account string) ([]byte, error) {
	if err := f.record("find", keychain, service, account); err != nil {
		return nil, err
	}
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	switch {
	case m.Mode == "denied" || m.Mode == "locked":
		return nil, errKeychainInteraction
	case m.Mode == "echo-error":
		return nil, fmt.Errorf("planted backend text %s", m.Items[account])
	case m.Mode == "timeout" || m.Mode == "timeout-readback" && m.Writes > 0:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.Reads++
	if err := f.save(m); err != nil {
		return nil, err
	}
	if m.Mode == "empty" {
		return []byte{}, nil
	}
	token, ok := m.Items[account]
	if !ok {
		return nil, errKeychainMissing
	}
	if m.Mode == "mismatch" && m.Writes > 0 {
		token = "mismatching-sentinel"
	}
	if m.Mode == "interrupt" && m.Writes > 0 {
		return nil, fmt.Errorf("planted interrupted read")
	}
	return []byte(token), nil
}

func (f fakeBackend) add(ctx context.Context, keychain, service, account string, secret []byte) error {
	return f.put(ctx, "add", keychain, service, account, secret)
}

func (f fakeBackend) update(ctx context.Context, keychain, service, account string, secret []byte) error {
	return f.put(ctx, "update", keychain, service, account, secret)
}

func (f fakeBackend) put(ctx context.Context, op, keychain, service, account string, secret []byte) error {
	if err := f.record(op, keychain, service, account); err != nil {
		return err
	}
	m, err := f.load()
	if err != nil {
		return err
	}
	switch m.Mode {
	case "denied", "locked":
		return errKeychainInteraction
	case "echo-error", "write-echo-error":
		return fmt.Errorf("planted backend text %s", secret)
	case "timeout", "timeout-write":
		<-ctx.Done()
		return ctx.Err()
	case "fail-write":
		return fmt.Errorf("planted write failure")
	}
	if (m.Mode == "duplicate" && m.Writes == 0) || (m.Mode == "duplicate-twice" && m.Writes < 2) {
		m.Writes++
		m.Items[account] = "external-token"
		if err := f.save(m); err != nil {
			return err
		}
		return errKeychainDuplicate
	}
	if _, exists := m.Items[account]; exists && op == "add" {
		return errKeychainDuplicate
	}
	// update is an upsert: a deletion between lookup and update recreates it.
	if m.Mode == "delete-before-update" {
		delete(m.Items, account)
	}
	m.Items[account] = string(secret)
	m.Writes++
	return f.save(m)
}

// isolatedKeychainStore keeps every synthetic gate in the test config namespace
// without consulting the kernel host identity or the user's login Keychain path.
// Explicit target/host controls remain available to the corresponding tests.
func isolatedKeychainStore(k keychainStore) *keychainStore {
	if k.hostID == "" {
		k.hostID = "synthetic-host"
	}
	if k.fixture == "" {
		k.fixture = filepath.Join(k.paths.Base, "synthetic.keychain-db")
	}
	return &k
}

func fakeKeychain(e *env, items map[string]string, mode string) string {
	e.t.Helper()
	dir := e.t.TempDir()
	saveModel(e.t, dir, securityModel{Items: items, Mode: mode})
	e.extra[envSecretStore] = "keychain"
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, backend: fakeBackend{dir}, timeout: time.Second})
		}
	}
	return dir
}

func countOps(recs []opRecord, ops ...string) int {
	n := 0
	for _, r := range recs {
		for _, op := range ops {
			if r.Op == op {
				n++
			}
		}
	}
	return n
}

func TestKeychainCredentialSelection(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "denied", "locked", "empty", "unknown", "scrubbed-missing"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			token := e.tokens["alice"]
			items := map[string]string{uid: token, "@bob:test.local": "other-identity-secret"}
			if mode == "missing" || mode == "scrubbed-missing" {
				delete(items, uid)
			}
			fakeMode := mode
			if mode == "valid" || mode == "missing" || mode == "scrubbed-missing" || mode == "unknown" {
				fakeMode = ""
			}
			dir := fakeKeychain(e, items, fakeMode)
			// The on-disk token is deliberately invalid, so only the full-ID Keychain
			// item can make the positive control pass.
			fileToken := "ignored-file-secret"
			if mode == "scrubbed-missing" {
				fileToken = ""
			}
			e.writeCred("alice", uid, fileToken, 0o600)
			if mode == "unknown" {
				e.extra[envSecretStore] = "typo"
			}
			r := e.as("alice", "whoami")
			if mode == "valid" {
				r.ok(t)
				tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
			} else {
				if r.code == 0 {
					t.Fatal("credential failure admitted")
				}
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
			}
			if mode == "unknown" && len(records(t, dir)) != 0 {
				t.Fatal("unknown store reached the keychain backend")
			}
			for _, r := range records(t, dir) {
				if r.Account != uid || r.Service != keychainService {
					t.Fatal("lookup not bound to the service and full Matrix ID")
				}
			}
		})
	}
}

func TestKeychainAuthorizationBoundaries(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	e.extra[config.EnvAgent] = "bob"
	e.as("alice", "whoami").failsWith(t, ExitUsage, "refused")
	e.extra[config.EnvAgent] = ""
	if _, err := config.MoveToHumans(e.paths(), "alice"); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "whoami").failsWith(t, ExitError, "human's identity")
	e.run("--human", "--as", "alice", "whoami").failsWith(t, ExitUsage, "interactive terminal")
	e.run("register", "alice").failsWith(t, ExitError, "human's identity")
	if len(records(t, dir)) != 0 {
		t.Fatal("authorization refusal reached the keychain backend")
	}
	// Human credential boundary: even the interactive human path never reads a2a's
	// Keychain, and migration refuses before touching the human file. An
	// agent beside it still reads its own item (positive control).
	prior := e.configure
	e.configure = func(a *App) { prior(a); a.stdinTTY = func() bool { return true } }
	before, err := os.ReadFile(e.paths().HumanFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	e.run("--human", "--as", "alice", "whoami").failsWith(t, ExitError, "human tokens are never kept")
	e.run("--human", "--as", "alice", "creds", "migrate").failsWith(t, ExitError, "human tokens are never kept")
	e.run("--human", "--as", "alice", "creds", "reconcile").failsWith(t, ExitError, "human tokens are never kept")
	if after, err := os.ReadFile(e.paths().HumanFile("alice")); err != nil || string(after) != string(before) {
		t.Fatal("refused human migration changed the human file", err)
	}
	if len(records(t, dir)) != 0 {
		t.Fatal("human path reached the keychain backend")
	}
	bob := e.agent("bob")
	saveModel(t, dir, securityModel{Items: map[string]string{bob: e.tokens["bob"]}})
	e.as("bob", "whoami").ok(t)
	if len(records(t, dir)) != 1 {
		t.Fatal("agent control not exercised")
	}
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
}

func TestKeychainRegistrationAndUpsert(t *testing.T) {
	for _, mode := range []string{"create", "existing", "duplicate", "delete-before-update"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			items := map[string]string{}
			if mode == "existing" || mode == "delete-before-update" {
				items["@newbie:test.local"] = "previous-secret"
			}
			dir := fakeKeychain(e, items, mode)
			e.run("register", "newbie").ok(t)
			data, err := os.ReadFile(e.paths().AgentFile("newbie"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("access_token")) || bytes.Contains(data, []byte("password")) {
				t.Fatal("registration persisted a secret")
			}
			m := readModel(t, dir)
			if m.Items["@newbie:test.local"] != "syt_newbie_newtoken_abcdef" {
				t.Fatal("wrong token stored")
			}
			recs := records(t, dir)
			if len(recs) < 3 {
				t.Fatal("missing lookup/write/readback")
			}
			// The backend takes no ACL argument: a new item gets the Keychain
			// default, trusting only the creating binary (phase 2, #138).
			// Existing items are updated in place; creation happens only when the
			// lookup found nothing or the bounded duplicate recovery applies.
			writes := 0
			for _, r := range recs {
				if r.Op == "add" || r.Op == "update" {
					wantUpdate := mode == "existing" || mode == "delete-before-update" || (mode == "duplicate" && writes == 1)
					if (r.Op == "update") != wantUpdate {
						t.Fatalf("write %d is %s, want update=%v", writes, r.Op, wantUpdate)
					}
					writes++
				}
			}
			expectedWrites := 1
			if mode == "duplicate" {
				expectedWrites = 2
			}
			if writes != expectedWrites {
				t.Fatal("unbounded or missing write")
			}
			if recs[len(recs)-1].Op != "find" {
				t.Fatal("last operation is not readback")
			}
			e.as("newbie", "whoami").ok(t)
		})
	}
}

func TestKeychainMigrationRecoverability(t *testing.T) {
	for _, mode := range []string{"valid", "mismatch", "interrupt", "fail-write", "denied", "locked", "empty"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			original, err := os.ReadFile(e.paths().AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			dir := fakeKeychain(e, map[string]string{uid: "old-keychain-secret"}, mode)
			r := e.as("alice", "creds", "migrate")
			data, err := os.ReadFile(e.paths().AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "valid" {
				if r.code == 0 {
					t.Fatal("failed migration admitted")
				}
				if !bytes.Equal(data, original) {
					t.Fatal("failed migration scrubbed recoverable file")
				}
				return
			}
			r.ok(t)
			var c config.Credentials
			if json.Unmarshal(data, &c) != nil {
				t.Fatal("invalid metadata")
			}
			if c.UserID != uid || c.DeviceID != "D" || c.Homeserver != e.hs.srv.URL || c.AccessToken != "" || c.Password != "" {
				t.Fatal("metadata lost or secrets retained")
			}
			if bytes.Contains(data, []byte("access_token")) || bytes.Contains(data, []byte("password")) {
				t.Fatal("secret fields retained")
			}
			st, err := os.Lstat(e.paths().AgentFile("alice"))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Fatal("file permissions changed")
			}
			count := readModel(t, dir).Writes
			e.as("alice", "creds", "migrate").ok(t) // scrubbed retry reads, never recreates retired items
			if readModel(t, dir).Writes != count {
				t.Fatal("scrubbed retry wrote a new item")
			}
			e.as("alice", "whoami").ok(t)
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
		})
	}
}

func TestKeychainMigrationScopeAndFileGuards(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	e.agent("bob")
	bob, err := os.ReadFile(e.paths().AgentFile("bob"))
	if err != nil {
		t.Fatal(err)
	}
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	e.run("creds", "migrate").failsWith(t, ExitUsage, "no identity")
	e.as("alice", "creds", "migrate", "bob").failsWith(t, ExitUsage, "explicit selected identity")
	if len(records(t, dir)) != 0 {
		t.Fatal("unselected migration reached the keychain backend")
	}
	e.as("alice", "creds", "migrate").ok(t)
	after, err := os.ReadFile(e.paths().AgentFile("bob"))
	if err != nil || !bytes.Equal(after, bob) {
		t.Fatal("migration enumerated other agents")
	}
	path := e.paths().AgentFile("alice")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	count := len(records(t, dir))
	e.as("alice", "creds", "migrate").failsWith(t, ExitError, "0600")
	if len(records(t, dir)) != count {
		t.Fatal("insecure file reached keychain")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.paths().AgentFile("bob"), path); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "creds", "migrate").failsWith(t, ExitError, "regular file")
}

// Backend errors may carry anything, including the token (planted here for
// reads and writes). The store reports only a fixed message: nothing from the
// backend error escapes into CLI output or error, and the harness leak check
// runs on every command. Records never contain the secret by construction.
func TestKeychainBackendTextNeverEscapes(t *testing.T) {
	for _, mode := range []string{"echo-error", "write-echo-error"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			token := e.tokens["alice"]
			// An older token, so the write path runs and its error text is planted too.
			fakeKeychain(e, map[string]string{uid: "stale-" + token}, mode)
			r := e.as("alice", "creds", "migrate").failsWith(t, ExitError, "keychain operation failed")
			if strings.Contains(r.stdout+r.stderr, "planted backend text") {
				t.Fatal("backend error text escaped")
			}
		})
	}
}

// An operation that never returns (a blocked backend call) is bounded by the
// hard deadline: the command fails with a recorded cooldown, and the active
// cooldown then refuses further calls without reaching the backend.
func TestKeychainDeadlineBoundsOperation(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "timeout")
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, backend: fakeBackend{dir}, timeout: 400 * time.Millisecond})
		}
	}
	start := time.Now()
	e.as("alice", "whoami").failsWith(t, ExitError, "cooldown recorded")
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline not hard")
	}
	count := len(records(t, dir))
	e.as("alice", "whoami").failsWith(t, ExitError, "cooldown active")
	if len(records(t, dir)) != count {
		t.Fatal("cooldown reached the keychain backend")
	}
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
}

// Independent CLI processes call App.Run with the same on-disk cooldown and
// the same file-backed fake backend. No environment contains any credential.
func TestKeychainCLIProcess(t *testing.T) {
	dir := os.Getenv("A2A_TEST_CLI_DIR")
	if dir == "" {
		return
	}
	modelDir := os.Getenv("A2A_TEST_CLI_MODEL")
	a := NewApp()
	a.Getenv = func(k string) string {
		switch k {
		case config.EnvConfigDir:
			return dir
		case envSecretStore:
			return "keychain"
		}
		return ""
	}
	a.keychain = func(p config.Paths) *keychainStore {
		probe := unlockedProbe
		if path := os.Getenv("A2A_TEST_PROBE"); path != "" {
			probe = func(ctx context.Context, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/bin/sh", append([]string{path}, args...)...)
			}
		}
		return isolatedKeychainStore(keychainStore{paths: p, hostID: os.Getenv("A2A_TEST_HOST"), probeCommand: probe, backend: fakeBackend{modelDir}, timeout: 2 * time.Second, cooldown: time.Second})
	}
	args := []string{"--as", os.Getenv("A2A_TEST_CLI_IDENTITY"), "creds", "migrate"}
	os.Exit(a.Run(context.Background(), args))
}
func runCLIProcess(dir, modelDir, identity string) *exec.Cmd {
	cmd := exec.Command("/usr/bin/env", "GORACE=atexit_sleep_ms=0", os.Args[0], "-test.run=^TestKeychainCLIProcess$")
	cmd.Env = []string{"A2A_TEST_CLI_DIR=" + dir, "A2A_TEST_CLI_MODEL=" + modelDir, "A2A_TEST_CLI_IDENTITY=" + identity, "PATH=/usr/bin:/bin"}
	return cmd
}
func TestKeychainCooldownAcrossProcesses(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	bob := e.agent("bob")
	dir := fakeKeychain(e, map[string]string{alice: e.tokens["alice"], bob: e.tokens["bob"]}, "timeout")
	out, err := runCLIProcess(e.dir, dir, "alice").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cooldown recorded") {
		t.Fatalf("timeout control: %v %s", err, out)
	}
	count := len(records(t, dir))
	saveModel(t, dir, securityModel{Items: map[string]string{alice: e.tokens["alice"], bob: e.tokens["bob"]}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := runCLIProcess(e.dir, dir, "alice").CombinedOutput()
			if err == nil || !strings.Contains(string(out), "cooldown active") {
				t.Errorf("cooldown not shared: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	if len(records(t, dir)) != count {
		t.Fatal("cooldown admitted invocation")
	}
	// Other identity must succeed immediately, and active marker suppresses writes
	// and their preflight/readback too, not only ordinary token reads.
	out, err = runCLIProcess(e.dir, dir, "bob").CombinedOutput()
	if err != nil {
		t.Fatalf("unrelated identity blocked: %v %s", err, out)
	}
	time.Sleep(1100 * time.Millisecond)
	out, err = runCLIProcess(e.dir, dir, "alice").CombinedOutput()
	if err != nil {
		t.Fatalf("expired cooldown did not recover: %v %s", err, out)
	}
}

func TestKeychainInteractionCooldownAllCalls(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "denied")
	e.as("alice", "whoami").failsWith(t, ExitError, "cooldown recorded")
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: fakeBackend{dir}})
	data, err := os.ReadFile(k.identityPath(uid) + ".cooldown")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), uid) || strings.Contains(string(data), e.tokens["alice"]) {
		t.Fatal("cooldown contains identity capability")
	}
	st, err := os.Stat(k.identityPath(uid) + ".cooldown")
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("cooldown not private")
	}
	count := len(records(t, dir))
	e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
	if len(records(t, dir)) != count {
		t.Fatal("write preflight bypassed cooldown")
	}
}

func TestKeychainWriterSerialization(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: fakeBackend{dir}, timeout: 2 * time.Second})
			if err := k.write(context.Background(), uid, fmt.Sprintf("synthetic-token-%d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	recs := records(t, dir)
	if len(recs) != 12 {
		t.Fatalf("calls=%d want 12", len(recs))
	}
	for i := 0; i < len(recs); i += 3 {
		if recs[i].Op != "find" || (recs[i+1].Op != "add" && recs[i+1].Op != "update") || recs[i+2].Op != "find" {
			t.Fatal("writers interleaved")
		}
	}
}

func TestKeychainMigrationInterruptedAndAtomicFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "callback-failure", "write-failure"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			path := e.paths().AgentFile("alice")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			dir := fakeKeychain(e, map[string]string{uid: "stale-" + e.tokens["alice"]}, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old := e.configure
			parent := filepath.Dir(path)
			saved := parent + ".saved"
			e.configure = func(a *App) {
				old(a)
				a.beforeCredentialScrub = func() error {
					switch mode {
					case "cancel":
						cancel()
					case "callback-failure":
						return fmt.Errorf("synthetic interruption after verified readback")
					case "write-failure":
						if err := os.Rename(parent, saved); err != nil {
							return err
						}
						if err := os.Mkdir(parent, 0o700); err != nil {
							return err
						}
						if err := os.WriteFile(path, before, 0o600); err != nil {
							return err
						} // check passes, but a file at temp path namespace would be unpredictable; replace parent after guard instead is tested by config's atomic writer separately.
						if err := os.Chmod(path, 0o644); err != nil {
							return err
						}
					}
					return nil
				}
			}
			r := e.runCtx(ctx, "--as", "alice", "creds", "migrate")
			if r.code == 0 {
				t.Fatal("interrupted migration succeeded")
			}
			if mode == "write-failure" {
				if err := os.RemoveAll(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(saved, parent); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("recoverable original changed")
			}
			if readModel(t, dir).Writes != 1 {
				t.Fatal("test did not reach verified write")
			}
		})
	}
}

// Tokens reach the backend as bytes, with no command line: quotes, backslashes
// and shell syntax are stored exactly, and a long token has no command-length
// limit. Both tokens go through both the create and the update path. Empty
// tokens and tokens with control bytes are refused before any write reaches
// the backend.
func TestKeychainTokenBytesAndRefusals(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	dir := fakeKeychain(e, map[string]string{}, "")
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: fakeBackend{dir}})
	quoted, long := `sentinel ' " \ ; $(not-a-shell) token`, strings.Repeat("x", 8192)
	steps := []struct{ uid, token, op string }{
		{alice, long, "add"}, {alice, quoted, "update"},
		{bob, quoted, "add"}, {bob, long, "update"},
	}
	for _, step := range steps {
		if err := k.write(context.Background(), step.uid, step.token); err != nil {
			t.Fatal(err)
		}
		if got := readModel(t, dir).Items[step.uid]; got != step.token {
			t.Fatalf("token bytes changed on %s", step.op)
		}
		recs := records(t, dir)
		if recs[len(recs)-2].Op != step.op {
			t.Fatalf("write took %s, want %s", recs[len(recs)-2].Op, step.op)
		}
	}
	uid := alice
	for _, bad := range []string{"", "a\nb", "a\x00b", "a\x7fb"} {
		writes := readModel(t, dir).Writes
		if err := k.write(context.Background(), uid, bad); err == nil {
			t.Fatal("unsafe token accepted")
		}
		if readModel(t, dir).Writes != writes {
			t.Fatal("unsafe token reached the writer")
		}
	}
}

func TestKeychainWriteTimeoutsSuppressEveryCall(t *testing.T) {
	for _, mode := range []string{"timeout-write", "timeout-readback"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: "stale-" + e.tokens["alice"]}, mode)
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, backend: fakeBackend{dir}, timeout: 300 * time.Millisecond})
				}
			}
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown recorded")
			count := len(records(t, dir))
			e.as("alice", "whoami").failsWith(t, ExitError, "cooldown active")
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
			if len(records(t, dir)) != count {
				t.Fatal("write timeout did not suppress all calls")
			}
		})
	}
}

func TestKeychainCooldownStateFailClosed(t *testing.T) {
	for _, mode := range []string{"corrupt", "symlink", "public", "private"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
			e.as("alice", "whoami").ok(t)
			k := isolatedKeychainStore(keychainStore{paths: e.paths()})
			path := k.identityPath(uid) + ".cooldown"
			switch mode {
			case "corrupt":
				if err := os.WriteFile(path, []byte("not-a-time"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(dir, "marker")
				if err := os.WriteFile(target, []byte("0"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, []byte("0"), 0o644); err != nil {
					t.Fatal(err)
				}
				// Creation honors umask; force the intended unsafe fixture mode.
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "private":
				if err := os.WriteFile(path, []byte("0"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "public" || mode == "private" {
				want := os.FileMode(0o600)
				if mode == "public" {
					want = 0o644
				}
				st, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !st.Mode().IsRegular() || st.Mode().Perm() != want {
					t.Fatalf("%s fixture must be regular mode %04o; got %v", mode, want, st.Mode())
				}
				t.Logf("%s fixture: regular=%t mode=%04o", mode, st.Mode().IsRegular(), st.Mode().Perm())
			}
			count := len(records(t, dir))
			// The private expired marker must permit the legitimate fake read.
			if mode == "private" {
				e.as("alice", "whoami").ok(t)
				if len(records(t, dir)) != count+1 {
					t.Fatal("private cooldown did not permit backend")
				}
				return
			}
			wantError := "cooldown state unreadable"
			if mode == "corrupt" {
				wantError = "cooldown state invalid"
			}
			e.as("alice", "whoami").failsWith(t, ExitError, wantError)
			if len(records(t, dir)) != count {
				t.Fatal("unsafe state reached the keychain backend")
			}
		})
	}
}

func TestKeychainMetadataIdentityMismatch(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	bob := e.agent("bob")
	dir := fakeKeychain(e, map[string]string{alice: e.tokens["alice"], bob: e.tokens["bob"]}, "")
	e.as("alice", "whoami").ok(t)
	e.writeCred("alice", bob, e.tokens["alice"], 0o600)
	count := len(records(t, dir))
	e.as("alice", "whoami").failsWith(t, ExitError, "does not match")
	if len(records(t, dir)) != count {
		t.Fatal("forged metadata reached the keychain backend")
	}
}

func TestKeychainDuplicateRetryBound(t *testing.T) {
	e := newEnv(t)
	dir := fakeKeychain(e, map[string]string{}, "duplicate-twice")
	r := e.run("register", "newbie")
	if r.code == 0 {
		t.Fatal("second duplicate retried")
	}
	recs := records(t, dir)
	if len(recs) != 4 {
		t.Fatalf("calls=%d, want lookup/create/relookup/update", len(recs))
	}
	if _, err := os.Stat(e.paths().AgentFile("newbie")); !os.IsNotExist(err) {
		t.Fatal("failed registration retained file")
	}
}
