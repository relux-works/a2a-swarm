package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const registerRoute = "POST /_matrix/client/v3/register"

// Proves host init writes the slug once, keeps it on a repeat, refuses a
// different slug without --force (the file stays), replaces it with --force,
// and host show prints it; an invalid slug is a usage error and host show
// without a slug names the fix.
func TestHostInitShow(t *testing.T) {
	e := newEnv(t)
	e.run("host", "show").failsWith(t, ExitError, "run a2a host init <slug> once per machine")
	e.run("host", "init", "Mini").failsWith(t, ExitUsage, "invalid host")
	e.run("host", "init").failsWith(t, ExitUsage, "usage")
	tuitestkit.ContainsStr(t, e.run("host", "init", "mini").ok(t).stdout, "host mini")
	tuitestkit.ContainsStr(t, e.run("host", "init", "mini").ok(t).stdout, "unchanged")
	e.run("host", "init", "laptop").failsWith(t, ExitError, "pass --force")
	if got := e.run("host", "show").ok(t).stdout; got != "mini\n" {
		t.Fatalf("host show = %q", got)
	}
	e.run("host", "init", "laptop", "--force").ok(t)
	if got := e.run("host", "show").ok(t).stdout; got != "laptop\n" {
		t.Fatalf("host show after --force = %q", got)
	}
	if data := readFile(t, e.paths().HostFile()); data != "laptop\n" {
		t.Fatalf("host file = %q", data)
	}
}

// Proves ensure reuses local credentials: it verifies them with whoami, prints
// the identity, "reusing" and the export line, and never touches /register.
func TestIdentityEnsureReuse(t *testing.T) {
	e := newEnv(t)
	e.run("host", "init", "mini").ok(t)
	e.agent("mini_proj_orch")
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "identity mini_proj_orch (host mini, project proj, role orch)")
	tuitestkit.ContainsStr(t, out, "reusing mini_proj_orch (@mini_proj_orch:test.local)")
	tuitestkit.ContainsStr(t, out, "export A2A_AGENT=mini_proj_orch\n")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "GET /_matrix/client/v3/account/whoami", 1)
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, registerRoute)
}

// Proves ensure registers an identity the server does not know: one
// unauthenticated probe plus the two-step registration, a 0600 credential
// file, and the export line; a second ensure then reuses it.
func TestIdentityEnsureRegisters(t *testing.T) {
	e := newEnv(t)
	e.run("host", "init", "mini").ok(t)
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "registered @mini_proj_orch:test.local")
	tuitestkit.ContainsStr(t, out, "export A2A_AGENT=mini_proj_orch\n")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, registerRoute, 3)
	st, err := os.Stat(e.paths().AgentFile("mini_proj_orch"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", st, err)
	}
	tuitestkit.ContainsStr(t, e.ensure("--role", "orch", "--project", "proj").ok(t).stdout, "reusing mini_proj_orch")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, registerRoute, 3)
}

// Proves ensure refuses (exit 1) when the account exists on the server but
// this machine has no credentials for it: no registration is attempted, no
// credential file is left, and no export line is printed. Positive control:
// the same name with its credentials restored is reused.
func TestIdentityEnsureRefusesRemoteOnly(t *testing.T) {
	e := newEnv(t)
	e.run("host", "init", "mini").ok(t)
	uid, tok := e.hs.addUser("mini_proj_orch")
	e.tokens["mini_proj_orch"] = tok
	r := e.ensure("--role", "orch", "--project", "proj").failsWith(t, ExitError,
		"account @mini_proj_orch:test.local exists but this machine holds no credentials for it; recovery only through the admin")
	tuitestkit.NotContainsStr(t, r.stdout, "export A2A_AGENT")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, registerRoute, 1) // the probe only
	if _, err := os.Stat(e.paths().AgentFile("mini_proj_orch")); !os.IsNotExist(err) {
		t.Fatalf("credential file left behind: %v", err)
	}
	e.writeCred("mini_proj_orch", uid, tok, 0o600)
	tuitestkit.ContainsStr(t, e.ensure("--role", "orch", "--project", "proj").ok(t).stdout, "reusing mini_proj_orch")
}

// Proves ensure needs the host slug: without one it fails (exit 1) with the
// fix and no traffic.
func TestIdentityEnsureNeedsHost(t *testing.T) {
	e := newEnv(t)
	e.run("identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "claude", "--dir", t.TempDir()).failsWith(t, ExitError, "run a2a host init <slug> once per machine")
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("%d requests without a host slug", n)
	}
}

// Proves project detection: a directory without git is a usage error asking
// for --project (no traffic); a git repo whose origin is
// git@host:Owner/My_Repo.git gives the project "my-repo"; --project wins over
// the remote. The remote URL is never printed.
func TestIdentityEnsureProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git:", err)
	}
	e := newEnv(t)
	e.run("host", "init", "mini").ok(t)
	plain := t.TempDir()
	e.ensure("--role", "orch", "--dir", plain).failsWith(t, ExitUsage, "pass --project P")
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("%d requests without a project", n)
	}
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@example.com:Owner/My_Repo.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	e.ensure("--role", "orch", "--dir", plain+"-missing").failsWith(t, ExitUsage, "pass --project P")
	e.agent("mini_my-repo_orch")
	out := e.ensure("--role", "orch", "--dir", repo).ok(t).stdout
	tuitestkit.ContainsStr(t, out, "reusing mini_my-repo_orch")
	tuitestkit.NotContainsStr(t, out, "example.com")
	e.agent("mini_other_orch")
	tuitestkit.ContainsStr(t, e.ensure("--role", "orch", "--project", "other", "--dir", repo).ok(t).stdout, "reusing mini_other_orch")
}

// Proves repoSlug maps remote URLs to the repo name (last element, no .git,
// lowercase, other characters to '-') and rejects what gives no valid project.
func TestRepoSlug(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/relux-works/a2a-swarm.git": "a2a-swarm",
		"git@github.com:relux-works/A2A_Swarm.git":     "a2a-swarm",
		"ssh://git@host:22/team/my.repo/":              "my-repo",
		"git@host:repo.git":                            "repo",
		"/srv/git/Local Repo":                          "local-repo",
	} {
		if got, ok := repoSlug(url); !ok || got != want {
			t.Errorf("repoSlug(%q) = %q, %v; want %q", url, got, ok, want)
		}
	}
	for _, bad := range []string{"", ".git", "https://host/.git", "https://host/" + strings.Repeat("x", 41)} {
		if got, ok := repoSlug(bad); ok {
			t.Errorf("repoSlug(%q) = %q, want rejected", bad, got)
		}
	}
}

// Proves an invalid or missing role and an invalid --project are usage errors
// (exit 2) before any traffic, and a session bound to another identity
// (A2A_AGENT) cannot ensure this one; the valid call works (positive control).
func TestIdentityEnsureUsage(t *testing.T) {
	e := newEnv(t)
	e.run("host", "init", "mini").ok(t)
	e.ensure("--project", "proj").failsWith(t, ExitUsage, "--role is required")
	e.ensure("--role", "Orch", "--project", "proj").failsWith(t, ExitUsage, "invalid role")
	e.ensure("--role", "a_b", "--project", "proj").failsWith(t, ExitUsage, "invalid role")
	e.ensure("--role", "orch", "--project", "p.q").failsWith(t, ExitUsage, "invalid project")
	e.ensure("--role", strings.Repeat("r", 24), "--project", strings.Repeat("p", 40)).failsWith(t, ExitUsage, "longer than the 64")
	e.ensure("--role", "orch", "extra").failsWith(t, ExitUsage, "usage")
	e.run("identity").failsWith(t, ExitUsage, "usage")
	e.extra[config.EnvAgent] = "someone-else"
	e.ensure("--role", "orch", "--project", "proj").failsWith(t, ExitUsage, "never act as another identity")
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("%d requests from usage errors", n)
	}
	e.extra[config.EnvAgent] = "mini_proj_orch"
	tuitestkit.ContainsStr(t, e.ensure("--role", "orch", "--project", "proj").ok(t).stdout, "export A2A_AGENT=mini_proj_orch")
}

// lease returns alice's recorded lease (nil when none).
func (e *env) lease(name string) *config.Lease {
	e.t.Helper()
	st, err := config.LoadState(e.paths(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return st.Lease
}

// Proves the lease holder refreshes its own lease: a second inbox of the same
// session (A2A_SESSION_ID) succeeds and moves the expiry to now+LeaseTTL.
func TestLeaseSameHolderRefreshes(t *testing.T) {
	e, _ := world(t)
	clk := e.clock()
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	first := e.lease("alice")
	if first == nil || first.Holder != "s1" || first.PID != os.Getpid() || !first.Expires.Equal(clk.now().Add(config.LeaseTTL)) {
		t.Fatalf("lease = %+v", first)
	}
	clk.add(5 * time.Minute)
	e.as("alice", "inbox").ok(t)
	if l := e.lease("alice"); l.Holder != "s1" || !l.Expires.Equal(clk.now().Add(config.LeaseTTL)) {
		t.Fatalf("lease not refreshed: %+v", l)
	}
}

// Native Codex thread metadata shares the hook lease without an agent export;
// another thread is refused before sync. Explicit A2A_SESSION_ID keeps priority.
func TestLeaseCodexThreadFallback(t *testing.T) {
	e, _ := world(t)
	e.extra["CODEX_THREAD_ID"] = "codex-own"
	e.stdin = `{"session_id":"codex-own"}`
	e.as("alice", "hook", "prompt", "--runtime", "codex").ok(t)
	e.as("alice", "inbox").ok(t)
	if l := e.lease("alice"); l == nil || l.Holder != "codex-own" {
		t.Fatalf("native lease missing: %+v", l)
	}
	n := len(e.hs.CallsFor("GET /_matrix/client/v3/sync"))
	e.extra["CODEX_THREAD_ID"] = "codex-other"
	e.as("alice", "inbox").failsWith(t, ExitError, "identity alice is held by codex-own")
	if len(e.hs.CallsFor("GET /_matrix/client/v3/sync")) != n {
		t.Fatal("refused native thread synced")
	}
	e.extra[EnvSessionID] = "codex-own"
	e.as("alice", "inbox").ok(t)
}

// Proves a second session is refused while the first one's lease is live:
// inbox, wait and watch exit 1 naming the holder and the expiry, before any
// sync; the lease is unchanged, and the first session still works (positive control).
func TestLeaseSecondHolderRefused(t *testing.T) {
	e, _ := world(t)
	clk := e.clock()
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	until := clk.now().Add(config.LeaseTTL).UTC().Format(time.RFC3339)
	e.extra[EnvSessionID] = "s2"
	syncs := len(e.hs.CallsFor("GET /_matrix/client/v3/sync"))
	for _, args := range [][]string{{"inbox"}, {"wait", "--timeout", "1s"}, {"watch"}} {
		e.as("alice", args...).failsWith(t, ExitError, "identity alice is held by s1 until "+until)
	}
	if n := len(e.hs.CallsFor("GET /_matrix/client/v3/sync")); n != syncs {
		t.Fatalf("a refused session synced %d times", n-syncs)
	}
	if l := e.lease("alice"); l.Holder != "s1" {
		t.Fatalf("lease = %+v", l)
	}
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
}

// Proves an expired lease is taken over (injected clock): refused just before
// the expiry, taken just after it; the old holder is then refused in turn.
func TestLeaseTakeoverAfterExpiry(t *testing.T) {
	e, _ := world(t)
	clk := e.clock()
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	e.extra[EnvSessionID] = "s2"
	clk.add(config.LeaseTTL - time.Second)
	e.as("alice", "inbox").failsWith(t, ExitError, "held by s1")
	clk.add(2 * time.Second)
	e.as("alice", "inbox").ok(t)
	if l := e.lease("alice"); l.Holder != "s2" {
		t.Fatalf("lease = %+v", l)
	}
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").failsWith(t, ExitError, "held by s2")
}

// Proves a "pid:" holder on this host whose process has exited is taken over
// at once, while one whose process runs is refused (positive control of the
// liveness check).
// (Leases written before the Host field, as here, still rely on the liveness check.)
func TestLeaseDeadPidTakenOver(t *testing.T) {
	e, _ := world(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	host := machineLabel()
	put := func(pid int) {
		if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error {
			s.Lease = &config.Lease{Holder: "pid:" + strconv.Itoa(pid) + "@" + host, PID: pid, Expires: time.Now().Add(time.Hour)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	e.extra[EnvSessionID] = "s2"
	put(os.Getppid())
	e.as("alice", "inbox").failsWith(t, ExitError, "held by pid:"+strconv.Itoa(os.Getppid()))
	put(dead.Process.Pid)
	e.as("alice", "inbox").ok(t)
	if l := e.lease("alice"); l.Holder != "s2" {
		t.Fatalf("lease = %+v", l)
	}
}

// Proves identity release clears my own lease so another session can take
// it at once, refuses (exit 1) to clear another session's live lease, and
// clears it with --force.
func TestLeaseRelease(t *testing.T) {
	e, _ := world(t)
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	e.extra[EnvSessionID] = "s2"
	e.as("alice", "identity", "release").failsWith(t, ExitError, "held by s1")
	e.extra[EnvSessionID] = "s1"
	tuitestkit.ContainsStr(t, e.as("alice", "identity", "release").ok(t).stdout, "released alice")
	if l := e.lease("alice"); l != nil {
		t.Fatalf("lease after release = %+v", l)
	}
	tuitestkit.ContainsStr(t, e.as("alice", "identity", "release").ok(t).stdout, "alice has no lease")
	e.extra[EnvSessionID] = "s2"
	e.as("alice", "inbox").ok(t)
	e.extra[EnvSessionID] = "s3"
	e.as("alice", "identity", "release", "--force").ok(t)
	e.as("alice", "inbox").ok(t)
	e.as("alice", "identity", "release", "x").failsWith(t, ExitUsage, "usage")
}

// Proves a hook of a second session is quiet while another session holds the
// lease: no stdout, one stderr line naming the holder, exit 0, and it takes
// nothing (the lease and the pending event stay). The holding session's hook
// rings (positive control).
func TestLeaseHookRefusalQuiet(t *testing.T) {
	e, room := world(t)
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	e.hs.inject(room, "@bob:test.local", ping(1))
	r := e.hook("alice", "prompt", `{"session_id":"other"}`)
	if r.code != ExitOK || r.stdout != "" || strings.Count(r.stderr, "\n") != 1 || !strings.Contains(r.stderr, "identity alice is held by s1 until") {
		t.Fatalf("refused hook: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if l := e.lease("alice"); l.Holder != "s1" {
		t.Fatalf("lease = %+v", l)
	}
	tuitestkit.ContainsStr(t, bellContext(t, e.hook("alice", "prompt", `{"session_id":"s1"}`).ok(t).stdout), "a2a: 1 new peer event (mention: 1)")
}

// Proves the SessionStart hook appends `export A2A_SESSION_ID=<id>` to
// CLAUDE_ENV_FILE, so the session's own inbox holds its hooks' lease (the
// inbox with that id succeeds), and writes nothing for an id that is not a
// plain token.
func TestHookExportsSessionID(t *testing.T) {
	e, _ := world(t)
	envFile := filepath.Join(t.TempDir(), "claude.env")
	e.extra[envClaudeEnvFile] = envFile
	e.hook("alice", "session-start", `{"session_id":"abc-123"}`).ok(t)
	if got := readFile(t, envFile); got != "export A2A_SESSION_ID=abc-123\n" {
		t.Fatalf("env file = %q", got)
	}
	e.hook("alice", "session-start", `{"session_id":"x; rm -rf ~"}`).ok(t)
	if got := readFile(t, envFile); got != "export A2A_SESSION_ID=abc-123\n" {
		t.Fatalf("env file after a hostile id = %q", got)
	}
	delete(e.extra, envClaudeEnvFile)
	e.extra[EnvSessionID] = "abc-123"
	e.as("alice", "inbox").ok(t)
}

// Proves the lease host does not change with the naming slug (regression from
// the self-migration of a2a-swarm-ac): a session holds the lease, then
// 'a2a host init' sets the slug mid-session, and a session-less process on the
// same machine is still compatible; a second known session is still refused.
func TestLeaseHostStableAcrossHostInit(t *testing.T) {
	e, _ := world(t)
	e.clock()
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	e.run("host", "init", "renamedslug").ok(t)
	delete(e.extra, EnvSessionID)
	e.as("alice", "inbox").ok(t)
	if l := e.lease("alice"); l.Holder != "s1" {
		t.Fatalf("a session-less process replaced the session's lease: %+v", l)
	}
	e.extra[EnvSessionID] = "s2"
	e.as("alice", "inbox").failsWith(t, ExitError, "is held by s1")
}
