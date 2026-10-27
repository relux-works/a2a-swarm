//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
type boundaryRecord struct {
	Args, Env []string
	Input     string
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
func records(t *testing.T, dir string) []boundaryRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "boundary.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []boundaryRecord
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r boundaryRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}
func securityCommand(dir string) func(context.Context, ...string) *exec.Cmd {
	return func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "/usr/bin/env", append([]string{"GORACE=atexit_sleep_ms=0", os.Args[0], "-test.run=^TestSecurityProcess$", "--", "--model-dir=" + dir}, args...)...)
		return cmd
	}
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
			return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, command: securityCommand(dir), timeout: time.Second})
		}
	}
	return dir
}

// This parser intentionally models security(1)'s split_line, not a shell: quoted
// arguments and backslash escape one following byte, no substitutions.
func parseSecurityLine(s string) []string {
	var args []string
	var b strings.Builder
	quote := byte(0)
	escaped, active := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			active = true
			continue
		}
		if c == ' ' || c == '\n' || c == '\t' {
			if active {
				args = append(args, b.String())
				b.Reset()
				active = false
			}
			continue
		}
		active = true
		b.WriteByte(c)
	}
	if active {
		args = append(args, b.String())
	}
	return args
}
func argValue(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func hasArg(args []string, key string) bool {
	for _, a := range args {
		if a == key {
			return true
		}
	}
	return false
}

// Isolated synthetic subprocess; NEVER invokes macOS security. Its stdout/error
// plants test the production runner's pipes and sanitization, not macOS UI.
func TestSecurityProcess(t *testing.T) {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 0 || !strings.HasPrefix(args[0], "--model-dir=") {
		return
	}
	dir := strings.TrimPrefix(args[0], "--model-dir=")
	args = args[1:]
	input, _ := io.ReadAll(os.Stdin)
	record := boundaryRecord{Args: args, Env: os.Environ(), Input: string(input)}
	f, err := os.OpenFile(filepath.Join(dir, "boundary.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(90)
	}
	_ = json.NewEncoder(f).Encode(record)
	_ = f.Close()
	data, err := os.ReadFile(modelFile(dir))
	if err != nil {
		os.Exit(90)
	}
	var m securityModel
	if json.Unmarshal(data, &m) != nil {
		os.Exit(90)
	}
	write := len(args) > 0 && args[0] == "-i"
	if write {
		args = parseSecurityLine(string(input))
	}
	uid := argValue(args, "-a")
	if uid == "" || argValue(args, "-s") != keychainService {
		os.Exit(91)
	}
	persist := func() {
		data, _ := json.Marshal(m)
		if os.WriteFile(modelFile(dir), data, 0o600) != nil {
			os.Exit(92)
		}
	}
	if m.Mode == "denied" {
		fmt.Fprint(os.Stderr, "planted-helper-secret")
		os.Exit(51)
	}
	if m.Mode == "locked" {
		os.Exit(36)
	}
	if m.Mode == "echo-error" || (m.Mode == "write-echo-error" && write) {
		fmt.Fprint(os.Stderr, string(input))
		fmt.Fprint(os.Stdout, string(input))
		os.Exit(7)
	}
	if m.Mode == "timeout" || m.Mode == "grandchild" || (m.Mode == "timeout-write" && write) || (m.Mode == "timeout-readback" && m.Writes > 0) {
		if m.Mode == "grandchild" {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSecurityGrandchild$")
			cmd.Env = []string{"A2A_TEST_GRANDCHILD=" + dir}
			if cmd.Start() != nil {
				os.Exit(93)
			}
			_ = os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	if write {
		if m.Mode == "fail-write" {
			os.Exit(7)
		}
		token := argValue(args, "-w")
		_, exists := m.Items[uid]
		if (m.Mode == "duplicate" && m.Writes == 0) || (m.Mode == "duplicate-twice" && m.Writes < 2) {
			m.Writes++
			m.Items[uid] = "external-token"
			persist()
			os.Exit(45)
		}
		if exists && !hasArg(args, "-U") {
			os.Exit(45)
		}
		// Actual -U is an upsert: deletion between lookup and update recreates it.
		if m.Mode == "delete-before-update" {
			delete(m.Items, uid)
		}
		m.Items[uid] = token
		m.Writes++
		persist()
		if m.Mode == "echo-success" {
			fmt.Fprint(os.Stdout, string(input))
			fmt.Fprint(os.Stderr, string(input))
		}
		os.Exit(0)
	}
	m.Reads++
	persist()
	if m.Mode == "empty" {
		fmt.Println()
		os.Exit(0)
	}
	token, exists := m.Items[uid]
	if !exists {
		os.Exit(44)
	}
	if m.Mode == "mismatch" && m.Writes > 0 {
		token = "mismatching-sentinel"
	}
	if m.Mode == "interrupt" && m.Writes > 0 {
		os.Exit(7)
	}
	fmt.Println(token)
	os.Exit(0)
}
func TestSecurityGrandchild(t *testing.T) {
	dir := os.Getenv("A2A_TEST_GRANDCHILD")
	if dir == "" {
		return
	}
	for {
		_ = os.WriteFile(filepath.Join(dir, "heartbeat"), []byte(time.Now().String()), 0o600)
		time.Sleep(10 * time.Millisecond)
	}
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
				t.Fatal("unknown store invoked security")
			}
			for _, r := range records(t, dir) {
				if argValue(r.Args, "-a") != uid {
					t.Fatal("lookup not bound to full Matrix ID")
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
		t.Fatal("authorization refusal launched security")
	}
	prior := e.configure
	e.configure = func(a *App) { prior(a); a.stdinTTY = func() bool { return true } }
	e.run("--human", "--as", "alice", "whoami").ok(t)
	if len(records(t, dir)) != 1 {
		t.Fatal("valid human path not exercised")
	}
}

func TestKeychainRegistrationAndUpsert(t *testing.T) {
	for _, mode := range []string{"create", "existing", "duplicate", "delete-before-update", "echo-success"} {
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
			writes := 0
			for _, r := range recs {
				if reflect.DeepEqual(r.Args, []string{"-i"}) {
					args := parseSecurityLine(r.Input)
					update := hasArg(args, "-U")
					trust := argValue(args, "-T")
					if hasArg(args, "-A") {
						t.Fatal("allow-all ACL")
					}
					wantUpdate := mode == "existing" || mode == "delete-before-update" || (mode == "duplicate" && writes == 1)
					if update != wantUpdate || (update && hasArg(args, "-T")) || (!update && trust != "/usr/bin/security") {
						t.Fatal("wrong create/update ACL command")
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
			if !hasArg(recs[len(recs)-1].Args, "-w") {
				t.Fatal("last invocation is not readback")
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
		t.Fatal("unselected migration invoked security")
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

func TestKeychainProcessBoundaryLeaks(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	token := e.tokens["alice"]
	t.Setenv("A2A_PLANTED_PROCESS_SECRET", token)
	dir := fakeKeychain(e, map[string]string{uid: token}, "write-echo-error")
	// The write helper echoes its stdin on both outputs and fails. Nothing from
	// either stream may escape to CLI output/error or the file.
	e.as("alice", "creds", "migrate").failsWith(t, ExitError, "keychain operation failed")
	// A successful helper may echo stdin too; its output is also private.
	saveModel(t, dir, securityModel{Items: map[string]string{uid: token}, Mode: "echo-success"})
	e.as("alice", "creds", "migrate").ok(t)
	for _, r := range records(t, dir) {
		if leaksBoundary(token, r.Args, r.Env, "", "") {
			t.Fatal("secret crossed argv/env boundary")
		}
	}
	// Detector controls are bounded planted leaks, not crashes/setup failures.
	for _, plant := range []boundaryRecord{{Args: []string{"-w", token}}, {Env: []string{"TOKEN=" + token}}} {
		if !leaksBoundary(token, plant.Args, plant.Env, "", "") {
			t.Fatal("leak detector missed plant")
		}
	}
	if !leaksBoundary(token, nil, nil, token, "") || !leaksBoundary(token, nil, nil, "", token) {
		t.Fatal("output/error detector missed plant")
	}
	if leaksBoundary(token, []string{"-i"}, []string{"PATH=/usr/bin"}, "safe", "safe") {
		t.Fatal("leak detector false positive")
	}
}
func leaksBoundary(secret string, args, env []string, out, err string) bool {
	return strings.Contains(strings.Join(args, "\x00"), secret) || strings.Contains(strings.Join(env, "\x00"), secret) || strings.Contains(out, secret) || strings.Contains(err, secret)
}

func TestKeychainDeadlineKillsProcessGroup(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "grandchild")
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, command: securityCommand(dir), timeout: 400 * time.Millisecond})
		}
	}
	start := time.Now()
	e.as("alice", "whoami").failsWith(t, ExitError, "cooldown recorded")
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline not hard")
	}
	data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal("grandchild witness missing", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "heartbeat"))
	if err != nil {
		t.Fatal("grandchild never ran", err)
	}
	time.Sleep(80 * time.Millisecond)
	second, err := os.ReadFile(filepath.Join(dir, "heartbeat"))
	if err != nil || !bytes.Equal(first, second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("grandchild survived group kill")
	}
	// On systems with asynchronous orphan reaping a zombie may still have a PID;
	// ps state must be absent or Z, never a live/sleeping descendant.
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z") && len(bytes.TrimSpace(out)) != 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("descendant still live")
	}
	count := len(records(t, dir))
	e.as("alice", "whoami").failsWith(t, ExitError, "cooldown active")
	if len(records(t, dir)) != count {
		t.Fatal("cooldown launched security")
	}
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeWhoami)
}

// Independent CLI processes call App.Run with the same on-disk cooldown and
// isolated security executable. No environment contains any credential.
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
		return isolatedKeychainStore(keychainStore{paths: p, hostID: os.Getenv("A2A_TEST_HOST"), probeCommand: probe, command: securityCommand(modelDir), timeout: 2 * time.Second, cooldown: time.Second})
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
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, command: securityCommand(dir)})
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
			k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, command: securityCommand(dir), timeout: 2 * time.Second})
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
		if recs[i].Args[0] != "find-generic-password" || recs[i+1].Args[0] != "-i" || recs[i+2].Args[0] != "find-generic-password" {
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
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
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

func TestKeychainCommandGrammarAndLimits(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, command: securityCommand(dir)})
	token := `sentinel ' " \ ; $(not-a-shell) token`
	if err := k.write(context.Background(), uid, token); err != nil {
		t.Fatal(err)
	}
	if got := readModel(t, dir).Items[uid]; got != token {
		t.Fatal("stdin quoting changed bytes")
	}
	for _, bad := range []string{"", "a\nb", "a\x00b", strings.Repeat("x", 4096)} {
		writes := readModel(t, dir).Writes
		if err := k.write(context.Background(), uid, bad); err == nil {
			t.Fatal("unsafe command value accepted")
		}
		if readModel(t, dir).Writes != writes {
			t.Fatal("unsafe command reached writer")
		}
	}
}

func TestKeychainWriteTimeoutsSuppressEveryCall(t *testing.T) {
	for _, mode := range []string{"timeout-write", "timeout-readback"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, mode)
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, probeCommand: unlockedProbe, command: securityCommand(dir), timeout: 300 * time.Millisecond})
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
	for _, mode := range []string{"corrupt", "symlink", "public"} {
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
			}
			count := len(records(t, dir))
			if r := e.as("alice", "whoami"); r.code == 0 {
				t.Fatal("unsafe cooldown state admitted")
			}
			if len(records(t, dir)) != count {
				t.Fatal("unsafe state launched security")
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
		t.Fatal("forged metadata reached security")
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

func FuzzSecurityQuoteRoundTrip(f *testing.F) {
	for _, s := range []string{"token", "quote\"slash\\value", "$() ; ' spaces", "", "line\nbreak", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := securityQuote(s)
		invalid := false
		for _, r := range s {
			if r < 32 || r == 127 {
				invalid = true
			}
		}
		if invalid {
			if err == nil {
				t.Fatal("control character admitted")
			}
			return
		}
		if err != nil {
			t.Fatal("safe token refused")
		}
		args := parseSecurityLine("command -w " + q + "\n")
		if len(args) != 3 || args[2] != s {
			t.Fatal("security tokenizer roundtrip differs")
		}
	})
}
