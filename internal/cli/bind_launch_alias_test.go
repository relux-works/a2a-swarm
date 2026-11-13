//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The private pane runtime records its PID, runs exactly one requested command
// as its own child (so that command's parent is the pinned pane runtime, as for
// a hook or a manual bind), records the result, and stays alive as the owner.
const launchAliasPane = `import os,sys,json,time,subprocess
root=sys.argv[1]
def put(name,data):
    tmp=os.path.join(root,name+'.tmp')
    open(tmp,'w').write(data)
    os.rename(tmp,os.path.join(root,name))
put('pid',str(os.getpid()))
req=os.path.join(root,'request.json')
while not os.path.exists(req): time.sleep(0.02)
r=json.load(open(req))
env=dict(os.environ); env.update(r['env'])
p=subprocess.run(r['argv'],env=env,cwd=r['cwd'] or None,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=120)
put('result.json',json.dumps({'code':p.returncode,'out':p.stdout.decode('utf-8','replace'),'err':p.stderr.decode('utf-8','replace')}))
while True: time.sleep(1)
`

type launchAliasFixture struct {
	e     *env
	dir   string
	owner int
}

// newLaunchAliasFixture starts a private tmux server whose only pane runs the
// pane runtime above, in an isolated config with a fake homeserver.
func newLaunchAliasFixture(t *testing.T) *launchAliasFixture {
	t.Helper()
	isolateBindTest(t)
	if testing.Short() {
		t.Skip("detached executable integration")
	}
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable; private launch-alias lane unrun")
		}
	}
	e, _ := world(t)
	e.extra[EnvSessionID] = "owned-thread"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dir, err := os.MkdirTemp("/tmp", "al-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	script := filepath.Join(dir, "pane.py")
	if err := os.WriteFile(script, []byte(launchAliasPane), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	if out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "python3", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("setup: private tmux: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-S", socket, "kill-server").Run() })
	raw := awaitFile(t, filepath.Join(dir, "pid"))
	owner, err := strconv.Atoi(raw)
	if err != nil || owner <= 1 {
		t.Fatalf("setup: pane runtime PID %q", raw)
	}
	t.Cleanup(func() { syscall.Kill(owner, syscall.SIGKILL) })
	return &launchAliasFixture{e: e, dir: dir, owner: owner}
}

func awaitFile(t *testing.T, path string) string {
	t.Helper()
	until := time.Now().Add(testDeadline)
	for time.Now().Before(until) {
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("setup: %s never appeared", filepath.Base(path))
	return ""
}

// bind runs `<argv0> --as alice bind ...` from the pane runtime and returns the
// detached consumer PID reported by the real parent.
func (f *launchAliasFixture) bind(t *testing.T, argv0, cwd string) int {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"argv": []string{argv0, "--as", "alice", "bind", "--runtime", "codex", "--owner-pid", strconv.Itoa(f.owner), "--session-id", "owned-thread"},
		"env": map[string]string{
			"A2A_CONFIG_DIR": f.e.dir, "A2A_SESSION_ID": "owned-thread", "A2A_SECRET_STORE": "file",
			"A2A_MATRIX_URL": f.e.hs.srv.URL, "A2A_MATRIX_SERVER_NAME": testServer,
		},
		"cwd": cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(f.dir, "request.tmp")
	if err := os.WriteFile(tmp, request, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(f.dir, "request.json")); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Code     int
		Out, Err string
	}
	if err := json.Unmarshal([]byte(awaitFile(t, filepath.Join(f.dir, "result.json"))), &result); err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(result.Out, "alice bound pid=")
	pid, convErr := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, ";", 2)[0]))
	if result.Code != 0 || !found || convErr != nil || pid <= 1 {
		t.Fatalf("setup: real bind parent failed: code=%d out=%q err=%q", result.Code, result.Out, result.Err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func (f *launchAliasFixture) log(t *testing.T) string {
	return readFile(t, filepath.Join(f.e.dir, "state", "alice.binding.log"))
}

// launchArgvError is the witness assertion: the live consumer's argv[0] must
// be exactly want. The calibration mutant requires this same check to fail.
func launchArgvError(pid int, want string) error {
	args, _, err := bindingArguments(pid)
	if err != nil {
		return fmt.Errorf("inspect consumer argv: %w", err)
	}
	if len(args) == 0 || args[0] != want {
		return fmt.Errorf("consumer argv0 %q, want launch alias %q", strings.Join(args[:min(len(args), 1)], ""), want)
	}
	return nil
}

func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Admission of a launch alias: only an absolute argv0 that currently resolves
// to the running executable's file is admitted. Bare names (never searched in
// PATH), relative paths, dangling links and aliases of another image refuse;
// the direct absolute image and a symlink chain to it are the positive controls.
func TestAdmittedLaunchAlias(t *testing.T) {
	dir := physicalTempDir(t)
	image := filepath.Join(dir, "a2a-image")
	other := filepath.Join(dir, "a2a-other")
	for _, p := range []string{image, other} {
		if err := os.WriteFile(p, []byte("image"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(dir, "a2a")
	chain := filepath.Join(dir, "a2a-chain")
	foreign := filepath.Join(dir, "a2a-foreign")
	dangling := filepath.Join(dir, "a2a-dangling")
	for link, target := range map[string]string{alias: image, chain: alias, foreign: other, dangling: filepath.Join(dir, "missing")} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		name, argv0, executable string
		ok                      bool
	}{
		{"absolute alias", alias, image, true},
		{"absolute alias chain", chain, image, true},
		{"direct image", image, image, true},
		{"executable is alias too", alias, alias, true},
		{"bare name in PATH", "a2a", image, false},
		{"relative path", "./a2a", image, false},
		{"alias of another image", foreign, image, false},
		{"dangling alias", dangling, image, false},
		{"empty argv0", "", image, false},
		{"unknown executable", alias, "", false},
	} {
		got, ok := admittedLaunchAlias(tc.argv0, tc.executable)
		if ok != tc.ok || ok && got != tc.argv0 || !ok && got != "" {
			t.Errorf("%s: got %q %v, want admitted=%v", tc.name, got, ok, tc.ok)
		}
	}
}

// A real bind parent started through an absolute alias spawns a consumer that
// runs the validated image with that alias as argv[0]; an atomic retarget of
// the alias is then adopted by exactly one in-place re-exec (same PID and
// generation, still bound), after a prior retarget to an unsafe
// (group-writable) copy was refused with no exec and an unchanged image. Base-red on Linux, where os.Executable resolves the
// alias; on Darwin os.Executable keeps the exec path, so there it is a control.
// Excludes: hook stdin parsing (the hook route reaches the same spawn code) and
// incompatible candidates (TestTerminalRefreshOldCandidateContinues).
// A direct absolute image remains the argv[0] of its own consumer; a relative
// invocation gains no alias and still runs only the validated image.
func TestBindSpawnLaunchAlias(t *testing.T) {
	isolateBindTest(t)
	if testing.Short() {
		t.Skip("detached executable integration")
	}
	build := physicalTempDir(t)
	exe := filepath.Join(build, "a2a")
	buildRefreshLifecycleCLI(t, exe)
	current, err := observeTerminalExecutable(exe)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("absolute alias", func(t *testing.T) {
		f := newLaunchAliasFixture(t)
		alias := filepath.Join(physicalTempDir(t), "a2a")
		if err := os.Symlink(exe, alias); err != nil {
			t.Fatal(err)
		}
		pid := f.bind(t, alias, "")
		if err := launchArgvError(pid, alias); err != nil {
			t.Fatal(err)
		}
		if image, err := bindingImage(context.Background(), pid); err != nil || image != current.Image {
			t.Fatalf("consumer is not running the validated image: %v", err)
		}
		before := f.e.state("alice").Terminal
		if before == nil || before.DaemonPID != pid || before.Stopped {
			t.Fatalf("setup: binding not armed: %+v", before)
		}
		raw, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		// Unsafe retarget: the same image bytes, but group-writable. The alias
		// grants no execution authority, so the refresh gates refuse it: no
		// re-exec and the same running image over several consumer cycles.
		unsafe := filepath.Join(build, "a2a-unsafe")
		if err := os.WriteFile(unsafe, raw, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafe, 0770); err != nil {
			t.Fatal(err)
		}
		retarget := func(target string) {
			t.Helper()
			link := alias + ".next"
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(link, alias); err != nil {
				t.Fatal(err)
			}
		}
		retarget(unsafe)
		syncs := f.e.hs.SyncCount("@alice:" + testServer)
		until := time.Now().Add(testDeadline)
		for f.e.hs.SyncCount("@alice:"+testServer) < syncs+3 && time.Now().Before(until) {
			time.Sleep(20 * time.Millisecond)
		}
		if f.e.hs.SyncCount("@alice:"+testServer) < syncs+3 {
			t.Fatalf("setup: consumer stopped cycling: log=%s", f.log(t))
		}
		if strings.Contains(f.log(t), "binding re-exec: executable changed\n") {
			t.Fatalf("unsafe alias target crossed exec: log=%s", f.log(t))
		}
		if image, err := bindingImage(context.Background(), pid); err != nil || image != current.Image {
			t.Fatalf("unsafe alias target changed the running image: %v", err)
		}
		next := filepath.Join(build, "a2a-next")
		if err := os.WriteFile(next, raw, 0700); err != nil {
			t.Fatal(err)
		}
		candidate, err := observeTerminalExecutable(next)
		if err != nil {
			t.Fatal(err)
		}
		// Paired control: a safe retarget of the same alias is adopted.
		retarget(next)
		until = time.Now().Add(testDeadline)
		for time.Now().Before(until) {
			image, err := bindingImage(context.Background(), pid)
			if err == nil && image == candidate.Image && strings.Count(f.log(t), "binding re-exec: executable changed\n") == 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if image, err := bindingImage(context.Background(), pid); err != nil || image != candidate.Image || strings.Count(f.log(t), "binding re-exec: executable changed\n") != 1 {
			t.Fatalf("alias retarget not adopted by one in-place re-exec: %v log=%s", err, f.log(t))
		}
		if err := launchArgvError(pid, alias); err != nil {
			t.Fatalf("re-exec dropped the alias: %v", err)
		}
		after := f.e.state("alice").Terminal
		if after == nil || after.DaemonPID != pid || after.DaemonBirth != before.DaemonBirth || after.Generation != before.Generation || after.Stopped {
			t.Fatal("re-exec changed consumer identity or stopped the binding")
		}
	})

	t.Run("direct image", func(t *testing.T) {
		f := newLaunchAliasFixture(t)
		pid := f.bind(t, exe, "")
		if err := launchArgvError(pid, exe); err != nil {
			t.Fatal(err)
		}
		if image, err := bindingImage(context.Background(), pid); err != nil || image != current.Image {
			t.Fatalf("direct consumer is not running the validated image: %v", err)
		}
	})

	t.Run("relative invocation", func(t *testing.T) {
		f := newLaunchAliasFixture(t)
		aliasDir := physicalTempDir(t)
		if err := os.Symlink(exe, filepath.Join(aliasDir, "a2a")); err != nil {
			t.Fatal(err)
		}
		pid := f.bind(t, "./a2a", aliasDir)
		args, _, err := bindingArguments(pid)
		if err != nil || len(args) == 0 {
			t.Fatalf("inspect consumer argv: %v", err)
		}
		if args[0] == "./a2a" || !filepath.IsAbs(args[0]) {
			t.Fatalf("relative launch name reached the consumer: %q", args[0])
		}
		resolved, err := filepath.EvalSymlinks(args[0])
		if err != nil || resolved != exe {
			t.Fatalf("consumer argv0 does not name the validated image: %q", args[0])
		}
		if image, err := bindingImage(context.Background(), pid); err != nil || image != current.Image {
			t.Fatalf("relative invocation executed another image: %v", err)
		}
	})
}

// Calibration: the same witness against a CLI built without the retained
// argv[0] must fail at the argv assertion (the reported failure class), while
// the bind itself still arms. Linux only: Darwin's os.Executable keeps the exec
// path, so the mutant is indistinguishable there.
func TestBindSpawnLaunchAliasCalibration(t *testing.T) {
	isolateBindTest(t)
	if testing.Short() {
		t.Skip("detached executable integration")
	}
	if runtime.GOOS != "linux" {
		t.Skip("os.Executable keeps the exec path on " + runtime.GOOS + "; mutant not distinguishable")
	}
	exe := filepath.Join(physicalTempDir(t), "a2a")
	buildRefreshLifecycleCLI(t, exe, overlayEdit{file: "bind.go", anchor: "\t\t\t\tchild.Args[0] = alias\n", replacement: "\t\t\t\t_ = alias\n"})
	f := newLaunchAliasFixture(t)
	alias := filepath.Join(physicalTempDir(t), "a2a")
	if err := os.Symlink(exe, alias); err != nil {
		t.Fatal(err)
	}
	pid := f.bind(t, alias, "")
	err := launchArgvError(pid, alias)
	if err == nil || !strings.Contains(err.Error(), "want launch alias") {
		t.Fatalf("witness did not detect the dropped alias: %v", err)
	}
}
