//go:build unix

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func unlockedProbe(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/usr/bin/printf", "unlocked\n")
}

func probeFixture(t *testing.T, mode, dir string) func(context.Context, ...string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(dir, "probe-"+mode)
	body := "#!/bin/sh\nprintf 'call\\n' >> '" + filepath.Join(dir, "probes") + "'\n"
	switch mode {
	case "unlocked":
		body += "printf 'unlocked\\n'"
	case "locked":
		body += "printf 'locked\\n'; exit 3"
	case "badexit":
		body += "printf 'unlocked\\n'; exit 3"
	case "badtoken":
		body += "printf 'locked\\n'"
	case "garbage":
		body += "printf 'unlocked\\ngarbage\\n'"
	case "error":
		body += "printf 'error\\n'; exit 2"
	case "stderr":
		body += "printf 'unlocked\\n'; printf 'danger' >&2"
	case "timeout":
		body += "sleep 60 &\nprintf '%s' $! > '" + filepath.Join(dir, "probe-child") + "'\nwait"
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if mode == "nonexec" {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "missing" {
		path += "-absent"
	}
	return func(ctx context.Context, args ...string) *exec.Cmd {
		// Execute valid synthetic scripts through the system interpreter: a
		// fresh script's first direct exec has substantial macOS admission
		// latency unrelated to the process/pipe contract under test. Keep
		// missing/non-executable controls on the real failing exec boundary.
		if mode == "missing" || mode == "nonexec" {
			return exec.CommandContext(ctx, path, args...)
		}
		return exec.CommandContext(ctx, "/bin/sh", append([]string{path}, args...)...)
	}
}

func TestKeychainProbeRefusals(t *testing.T) {
	for _, mode := range []string{"unlocked", "locked", "missing", "nonexec", "badexit", "badtoken", "garbage", "error", "stderr", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
			probe := probeFixture(t, mode, dir)
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: probe, timeout: 2 * time.Second})
				}
			}
			r := e.as("alice", "whoami")
			if mode == "unlocked" {
				r.ok(t)
				if len(records(t, dir)) != 1 {
					t.Fatal("positive needs one backend read")
				}
				return
			}
			r.failsWith(t, ExitError, "preflight failed")
			if len(records(t, dir)) != 0 {
				t.Fatal("refused probe reached the keychain backend")
			}
			before, _ := os.ReadFile(filepath.Join(dir, "probes"))
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
			after, _ := os.ReadFile(filepath.Join(dir, "probes"))
			if string(before) != string(after) {
				t.Fatal("cooldown launched probe")
			}
			if mode == "timeout" {
				data, err := os.ReadFile(filepath.Join(dir, "probe-child"))
				if err != nil {
					t.Fatal("missing descendant witness", err)
				}
				out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", string(data)).Output()
				if err == nil && strings.TrimSpace(string(out)) != "" && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
					pid, _ := strconv.Atoi(string(data))
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatal("probe descendant survived process group deadline")
				}
			}
		})
	}
}

func TestKeychainProbeEveryWriteStage(t *testing.T) {
	for _, mode := range []string{"create", "update", "readback"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			items := map[string]string{}
			if mode == "update" {
				items[uid] = "stale-" + e.tokens["alice"] // an older token, so the update path runs
			}
			dir := fakeKeychain(e, items, "")
			calls := 0
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd {
						calls++
						stop := 2
						if mode == "readback" {
							stop = 3
						}
						if calls == stop {
							return exec.CommandContext(ctx, "/usr/bin/printf", "locked\n")
						}
						return unlockedProbe(ctx, args...)
					}})
				}
			}
			original, err := os.ReadFile(e.paths().AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "preflight failed")
			want := 1
			if mode == "readback" {
				want = 2
			}
			if len(records(t, dir)) != want {
				t.Fatalf("stage bypass calls=%d want=%d", len(records(t, dir)), want)
			}
			after, _ := os.ReadFile(e.paths().AgentFile("alice"))
			if string(original) != string(after) {
				t.Fatal("probe failure scrubbed recoverable credential")
			}
		})
	}
}

func TestKeychainHostCooldown(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	bob := e.agent("bob")
	dir := fakeKeychain(e, map[string]string{alice: e.tokens["alice"], bob: e.tokens["bob"]}, "")
	now := time.Now()
	host := "host-one"
	probe := probeFixture(t, "locked", dir)
	store := func(p config.Paths) *keychainStore {
		return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: probe, hostID: host, now: func() time.Time { return now }})
	}
	e.configure = func(a *App) { a.keychain = store }
	e.as("alice", "whoami").failsWith(t, ExitError, "preflight failed")
	path, err := store(e.paths()).hostPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path + ".cooldown")
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("host marker not private", err)
	}
	data, _ := os.ReadFile(path + ".cooldown")
	if strings.Contains(string(data), e.tokens["alice"]) || strings.Contains(string(data), alice) {
		t.Fatal("marker not secret free")
	}
	before, _ := os.ReadFile(filepath.Join(dir, "probes"))
	probe = probeFixture(t, "unlocked", dir)
	e.as("bob", "whoami").failsWith(t, ExitError, "cooldown active")
	after, _ := os.ReadFile(filepath.Join(dir, "probes"))
	if string(before) != string(after) || len(records(t, dir)) != 0 {
		t.Fatal("host marker admitted process")
	}
	host = "host-two"
	e.as("bob", "whoami").ok(t)
	host = "host-one"
	now = now.Add(securityCooldown + time.Second)
	e.as("bob", "whoami").ok(t)
}

func TestKeychainProbeResolvedExecutable(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	linkdir := filepath.Join(dir, "launcher")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkdir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(real, "a2a")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkdir, "a2a")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", linkdir)
	t.Setenv("A2A_KEYCHAIN_PROBE", filepath.Join(linkdir, "evil"))
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolvedProbePath(link)
	if err != nil || got != filepath.Join(resolvedReal, keychainProbeName) {
		t.Fatalf("wrong companion %s %v", got, err)
	}
	if _, err := resolvedProbePath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("absent executable admitted")
	}
}

func TestKeychainProbeProductionMissing(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}})
		}
	}
	e.as("alice", "whoami").failsWith(t, ExitError, "preflight failed")
	if len(records(t, dir)) != 0 {
		t.Fatal("missing production companion reached the keychain backend")
	}
}

func TestKeychainProbeTargetPair(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
	fixture := filepath.Join(dir, "fixture.keychain-db")
	e.configure = func(a *App) {
		a.keychain = func(p config.Paths) *keychainStore {
			return isolatedKeychainStore(keychainStore{paths: p, fixture: fixture, backend: fakeBackend{dir}, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd {
				if fmt.Sprint(args) != fmt.Sprint([]string{"--fixture", fixture}) {
					t.Error("probe targeted different keychain")
				}
				return unlockedProbe(ctx, args...)
			}})
		}
	}
	e.as("alice", "whoami").ok(t)
	recs := records(t, dir)
	if len(recs) != 1 || recs[0].Keychain != fixture {
		t.Fatal("backend target differs from probe")
	}
}

func TestKeychainHostCooldownAcrossProcesses(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	bob := e.agent("bob")
	dir := fakeKeychain(e, map[string]string{alice: e.tokens["alice"], bob: e.tokens["bob"]}, "")
	_ = probeFixture(t, "locked", dir)
	_ = probeFixture(t, "unlocked", dir)
	run := func(identity, host, mode string) *exec.Cmd {
		cmd := runCLIProcess(e.dir, dir, identity)
		cmd.Env = append(cmd.Env, "A2A_TEST_HOST="+host, "A2A_TEST_PROBE="+filepath.Join(dir, "probe-"+mode))
		return cmd
	}
	out, err := run("alice", "host-one", "locked").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "preflight failed") {
		t.Fatalf("locked control %v %s", err, out)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "probes"))
	var cmds []*exec.Cmd
	for i := 0; i < 4; i++ {
		cmd := run("bob", "host-one", "unlocked")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err == nil {
			t.Fatal("cross-process host marker admitted")
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "probes"))
	if string(before) != string(after) || len(records(t, dir)) != 0 {
		t.Fatal("active host marker launched process")
	}
	out, err = run("bob", "host-two", "unlocked").CombinedOutput()
	if err != nil {
		t.Fatalf("other host blocked %v %s", err, out)
	}
	time.Sleep(1100 * time.Millisecond)
	out, err = run("bob", "host-one", "unlocked").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry blocked %v %s", err, out)
	}
}

func TestKeychainHostMarkerFailClosed(t *testing.T) {
	for _, mode := range []string{"corrupt", "symlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, "")
			e.as("alice", "whoami").ok(t)
			k := isolatedKeychainStore(keychainStore{paths: e.paths()})
			base, err := k.hostPath()
			if err != nil {
				t.Fatal(err)
			}
			path := base + ".cooldown"
			switch mode {
			case "corrupt":
				err = os.WriteFile(path, []byte("not-a-time"), 0o600)
			case "public":
				err = os.WriteFile(path, []byte("0"), 0o644)
			case "symlink":
				target := filepath.Join(dir, "unsafe")
				err = os.WriteFile(target, []byte("0"), 0o600)
				if err == nil {
					err = os.Symlink(target, path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					return isolatedKeychainStore(keychainStore{paths: p, backend: fakeBackend{dir}, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd { calls++; return unlockedProbe(ctx, args...) }})
				}
			}
			if r := e.as("alice", "whoami"); r.code == 0 {
				t.Fatal("unsafe marker admitted")
			}
			if calls != 0 || len(records(t, dir)) != 1 {
				t.Fatal("unsafe host marker launched process")
			}
		})
	}
}
