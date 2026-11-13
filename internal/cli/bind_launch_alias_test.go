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
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// launchAliasCustodyRoot returns a private fixture root directly under the real
// home. Shared temporary ancestry must refuse under the production custody
// policy, so the positive cases need an ancestry that passes it; the home is
// a candidate base, checked first, not a promise. The root is recorded in the
// owning bind fixture record so an interrupted run's next sweep removes it.
func launchAliasCustodyRoot(t *testing.T) string {
	t.Helper()
	if bindFixtureHome == "" {
		t.Skip("real home unavailable")
	}
	if err := hookExecutableParents(filepath.Join(bindFixtureHome, "fixture")); err != nil {
		t.Skipf("launch-alias fixture requires home ancestry without Git markers or unsafe mode bits: %v", err)
	}
	root, err := os.MkdirTemp(bindFixtureHome, launchAliasCustodyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

// Admission of a launch alias: only an absolute argv0 that resolves to the
// running executable's file AND passes the installed-command custody policy
// is admitted. Bare names (never searched in PATH), relative paths, dangling
// links, aliases of another image, aliases in a group- or other-writable
// directory and aliases inside a source checkout all refuse; the direct image
// and an alias chain in private directories are the positive controls. The
// verdicts come from the independent policy, not from comparing two
// implementations. Limit: foreign-owned aliases and ancestors need another
// UID and are covered by the shared policy code, not executed here.
func TestAdmittedLaunchAlias(t *testing.T) {
	root := launchAliasCustodyRoot(t)
	images, links, open, group, checkout := filepath.Join(root, "images"), filepath.Join(root, "links"), filepath.Join(root, "open"), filepath.Join(root, "group"), filepath.Join(root, "checkout")
	mkdirs(t, images, links, open, group, filepath.Join(checkout, ".git"))
	if err := os.Chmod(open, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(group, 0770); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(images, "a2a-image")
	other := filepath.Join(images, "a2a-other")
	for _, p := range []string{image, other} {
		if err := os.WriteFile(p, []byte("image"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(links, "a2a")
	chain := filepath.Join(links, "a2a-chain")
	foreign := filepath.Join(links, "a2a-foreign")
	dangling := filepath.Join(links, "a2a-dangling")
	writable := filepath.Join(open, "a2a")
	groupWritable := filepath.Join(group, "a2a")
	inCheckout := filepath.Join(checkout, "a2a")
	for link, target := range map[string]string{alias: image, chain: alias, foreign: other, dangling: filepath.Join(root, "missing"), writable: image, groupWritable: image, inCheckout: image} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", links)
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
		{"alias in other-writable directory", writable, image, false},
		{"alias in group-writable directory", groupWritable, image, false},
		{"alias inside a source checkout", inCheckout, image, false},
		{"empty argv0", "", image, false},
		{"unknown executable", alias, "", false},
	} {
		got, ok := admittedLaunchAlias(tc.argv0, tc.executable)
		if ok != tc.ok || ok && got != tc.argv0 || !ok && got != "" {
			t.Errorf("%s: got %q %v, want admitted=%v", tc.name, got, ok, tc.ok)
		}
	}
}

// launchAliasBind is one registered real-parent bind: the private tmux pane
// of the existing bind fixture runs `<launch> --as alice bind ...` as the
// pane runtime's child, so the parent spawn under test is the production one.
type launchAliasBind struct {
	e     *env
	dir   string
	owned bindFixtureRecord
	pid   int
}

func startLaunchAliasBind(t *testing.T, custody, launch, paneCwd string, consumers ...string) *launchAliasBind {
	t.Helper()
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable; private launch-alias lane unrun")
		}
	}
	dir, owned := newBindFixtureDir(t)
	owned.Launch, owned.Consumers, owned.Custody = launch, consumers, custody
	if err := writeBindFixtureRecord(dir, owned); err != nil {
		t.Fatal(err)
	}
	e, _ := world(t)
	configDir := filepath.Join(dir, "config")
	if err := os.Rename(e.dir, configDir); err != nil {
		t.Fatal(err)
	}
	e.dir = configDir
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(e.dir, "state", "alice.binding.log"))
			t.Logf("synthetic binding log: %s", b)
		}
	})
	e.extra["CODEX_THREAD_ID"] = "bind-fixture"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	script := filepath.Join(dir, "fixture.py")
	if err := os.WriteFile(script, []byte(bindFixture), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	args := []string{"-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24"}
	if paneCwd != "" {
		args = append(args, "-c", paneCwd)
	}
	cmd := exec.Command("tmux", append(args, "python3", script, dir, launch)...)
	cmd.Env = append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_SESSION_ID=", "A2A_AGENT=alice", "CODEX_THREAD_ID=bind-fixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup: private tmux: %v %s", err, out)
	}
	var screen struct{ PID int }
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		b, err := os.ReadFile(filepath.Join(dir, "screen.json"))
		if err == nil && json.Unmarshal(b, &screen) == nil && screen.PID > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if screen.PID <= 0 {
		t.Fatal("setup: fixture not ready")
	}
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if owned.Server, err = bindFixtureIdentity(serverPID); err != nil {
		t.Fatal(err)
	}
	if owned.Fixture, err = bindFixtureIdentity(screen.PID); err != nil {
		t.Fatal(err)
	}
	if err = writeBindFixtureRecord(dir, owned); err != nil {
		t.Fatal(err)
	}
	if err := writeBindingFixtureCommand(dir, []byte("1")); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Code     int
		Out, Err string
		Request  int
	}
	until = time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		b, err := os.ReadFile(filepath.Join(dir, "result.json"))
		if err == nil && json.Unmarshal(b, &r) == nil && r.Request == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Request != 1 || r.Code != 0 {
		t.Fatalf("setup: real bind parent failed: code=%d out=%q err=%q", r.Code, r.Out, r.Err)
	}
	st := awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal != nil && strings.HasPrefix(st.Terminal.Outcome, "armed:")
	})
	return &launchAliasBind{e: e, dir: dir, owned: owned, pid: st.Terminal.DaemonPID}
}

func (b *launchAliasBind) log(t *testing.T) string {
	return readFile(t, filepath.Join(b.e.dir, "state", "alice.binding.log"))
}

// awaitSyncs waits until the consumer has issued n more /sync requests.
func (b *launchAliasBind) awaitSyncs(t *testing.T, n int) {
	t.Helper()
	from := b.e.hs.SyncCount("@alice:" + testServer)
	until := time.Now().Add(testDeadline)
	for b.e.hs.SyncCount("@alice:"+testServer) < from+n && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if b.e.hs.SyncCount("@alice:"+testServer) < from+n {
		t.Fatalf("setup: consumer stopped cycling: log=%s", b.log(t))
	}
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

func retargetLink(t *testing.T, link, target string) {
	t.Helper()
	next := link + ".next"
	if err := os.Symlink(target, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatal(err)
	}
}

func copyImage(t *testing.T, from, to string, mode os.FileMode) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(to, mode); err != nil {
		t.Fatal(err)
	}
}

// A real bind parent started through an absolute, custody-admitted alias
// spawns a consumer that runs the validated image with that alias as
// argv[0]. A retarget of the alias to an unsafe (group-writable) copy of the
// same image is refused with no exec and an unchanged image; a safe retarget
// while the alias directory is other-writable (custody changed after
// admission) is refused as well; restoring custody lets that retarget be
// adopted by exactly one in-place re-exec (same PID, birth and generation,
// still bound). Base-red on Linux, where os.Executable
// resolves the alias; on Darwin os.Executable keeps the exec path, so there it
// is a control. Hook stdin parsing is excluded (the hook route reaches the
// same spawn lines); incompatible candidates stay covered by the refresh tests.
// A direct absolute image remains the argv[0] of its own consumer; a relative
// invocation gains no alias and runs only the validated image; an alias in an
// other-writable directory is not retained and its retarget is not adopted
// (Linux; Darwin's exec path is outside this change).
func TestBindSpawnLaunchAlias(t *testing.T) {
	isolateBindTest(t)
	if testing.Short() {
		t.Skip("detached executable integration")
	}
	custody := launchAliasCustodyRoot(t)
	images := filepath.Join(custody, "images")
	mkdirs(t, images)
	exe := filepath.Join(images, "a2a")
	buildRefreshLifecycleCLI(t, exe)
	if err := os.Chmod(exe, 0700); err != nil {
		t.Fatal(err)
	}
	current, err := observeTerminalExecutable(exe)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("absolute alias", func(t *testing.T) {
		installed := filepath.Join(custody, "installed")
		mkdirs(t, installed)
		alias := filepath.Join(installed, "a2a")
		if err := os.Symlink(exe, alias); err != nil {
			t.Fatal(err)
		}
		b := startLaunchAliasBind(t, custody, alias, "", alias)
		if err := launchArgvError(b.pid, alias); err != nil {
			t.Fatal(err)
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("consumer is not running the validated image: %v", err)
		}
		if holdBindFixtureControl(t, b.dir, b.owned) {
			return
		}
		before := b.e.state("alice").Terminal
		unsafe := filepath.Join(images, "a2a-unsafe")
		copyImage(t, exe, unsafe, 0770)
		retargetLink(t, alias, unsafe)
		b.awaitSyncs(t, 3)
		if strings.Contains(b.log(t), "binding re-exec: executable changed\n") {
			t.Fatalf("unsafe alias target crossed exec: log=%s", b.log(t))
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("unsafe alias target changed the running image: %v", err)
		}
		next := filepath.Join(images, "a2a-next")
		copyImage(t, exe, next, 0700)
		candidate, err := observeTerminalExecutable(next)
		if err != nil {
			t.Fatal(err)
		}
		// Changed ancestry after admission: with the alias directory made
		// other-writable, the safe retarget grants no execution authority.
		if err := os.Chmod(installed, 0777); err != nil {
			t.Fatal(err)
		}
		retargetLink(t, alias, next)
		b.awaitSyncs(t, 3)
		if strings.Contains(b.log(t), "binding re-exec: executable changed\n") {
			t.Fatalf("an alias whose custody changed was adopted: log=%s", b.log(t))
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("an alias whose custody changed changed the running image: %v", err)
		}
		// Paired control: restoring custody lets the same safe retarget be adopted.
		if err := os.Chmod(installed, 0700); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(testDeadline)
		for time.Now().Before(until) {
			image, err := bindingImage(context.Background(), b.pid)
			if err == nil && image == candidate.Image && strings.Count(b.log(t), "binding re-exec: executable changed\n") == 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != candidate.Image || strings.Count(b.log(t), "binding re-exec: executable changed\n") != 1 {
			t.Fatalf("alias retarget not adopted by one in-place re-exec: %v log=%s", err, b.log(t))
		}
		if err := launchArgvError(b.pid, alias); err != nil {
			t.Fatalf("re-exec dropped the alias: %v", err)
		}
		after := b.e.state("alice").Terminal
		if after == nil || after.DaemonPID != b.pid || after.DaemonBirth != before.DaemonBirth || after.Generation != before.Generation || after.Stopped {
			t.Fatal("re-exec changed consumer identity or stopped the binding")
		}
	})

	t.Run("direct image", func(t *testing.T) {
		b := startLaunchAliasBind(t, custody, exe, "", exe)
		if err := launchArgvError(b.pid, exe); err != nil {
			t.Fatal(err)
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("direct consumer is not running the validated image: %v", err)
		}
	})

	t.Run("relative invocation", func(t *testing.T) {
		relative := filepath.Join(custody, "relative")
		mkdirs(t, relative)
		link := filepath.Join(relative, "a2a")
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		// Linux records the resolved image; Darwin's os.Executable keeps the
		// absolute exec path of the relative invocation.
		b := startLaunchAliasBind(t, custody, "./a2a", relative, exe, link)
		args, _, err := bindingArguments(b.pid)
		if err != nil || len(args) == 0 {
			t.Fatalf("inspect consumer argv: %v", err)
		}
		if args[0] == "./a2a" || !filepath.IsAbs(args[0]) {
			t.Fatalf("relative launch name reached the consumer: %q", args[0])
		}
		if resolved, err := filepath.EvalSymlinks(args[0]); err != nil || resolved != exe {
			t.Fatalf("consumer argv0 does not name the validated image: %q", args[0])
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("relative invocation executed another image: %v", err)
		}
	})

	t.Run("other-writable alias directory", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("Darwin's os.Executable keeps the exec path; this change concerns the Linux parent path")
		}
		open := filepath.Join(custody, "open")
		mkdirs(t, open)
		if err := os.Chmod(open, 0777); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(open, "a2a")
		if err := os.Symlink(exe, alias); err != nil {
			t.Fatal(err)
		}
		b := startLaunchAliasBind(t, custody, alias, "", exe)
		if err := launchArgvError(b.pid, exe); err != nil {
			t.Fatalf("an alias in an other-writable directory was retained: %v", err)
		}
		next := filepath.Join(images, "a2a-open-next")
		copyImage(t, exe, next, 0700)
		retargetLink(t, alias, next)
		b.awaitSyncs(t, 3)
		if strings.Contains(b.log(t), "binding re-exec:") {
			t.Fatalf("an unadmitted alias retarget was adopted: log=%s", b.log(t))
		}
		if image, err := bindingImage(context.Background(), b.pid); err != nil || image != current.Image {
			t.Fatalf("unadmitted alias changed the running image: %v", err)
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
	custody := launchAliasCustodyRoot(t)
	images, installed := filepath.Join(custody, "images"), filepath.Join(custody, "installed")
	mkdirs(t, images, installed)
	exe := filepath.Join(images, "a2a")
	buildRefreshLifecycleCLI(t, exe, overlayEdit{file: "bind.go", anchor: "\t\t\t\tchild.Args[0] = alias\n", replacement: "\t\t\t\t_ = alias\n"})
	if err := os.Chmod(exe, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(installed, "a2a")
	if err := os.Symlink(exe, alias); err != nil {
		t.Fatal(err)
	}
	b := startLaunchAliasBind(t, custody, alias, "", alias, exe)
	err := launchArgvError(b.pid, alias)
	if err == nil || !strings.Contains(err.Error(), "want launch alias") {
		t.Fatalf("witness did not detect the dropped alias: %v", err)
	}
}

// The registry signals only the exact recorded process instance. A record
// whose identity no longer matches a live private control (a stale or reused
// instance, injected as a wrong birth) is retired without any signal and the
// control survives; the paired record carrying the control's true identity is
// reclaimed. Both records have a dead owner, so the sweep is entitled to act.
func TestBindFixtureSweepRefusesReusedInstance(t *testing.T) {
	root, err := bindFixtureRoot()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command("sleep", "0.2")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	dead, err := bindFixtureIdentity(owner.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact-%t", exact), func(t *testing.T) {
			control := exec.Command("sleep", "60")
			if err := control.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan struct{})
			go func() { control.Wait(); close(waited) }() // reap promptly
			t.Cleanup(func() {
				control.Process.Kill()
				<-waited
			})
			identity, err := bindFixtureIdentity(control.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			recorded := identity
			if !exact {
				recorded.Birth = "stale-" + recorded.Birth
			}
			dir, err := os.MkdirTemp(root, "fixture-")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeBindFixtureRecord(dir, bindFixtureRecord{Owner: dead, Fixture: recorded}); err != nil {
				t.Fatal(err)
			}
			if err := sweepBindFixtures(root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("dead-owner record was not retired: %v", err)
			}
			if !exact {
				if !identity.live() {
					t.Fatal("the sweep signalled a process whose recorded identity did not match")
				}
				return
			}
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
				t.Fatal("positive control: the exact recorded instance was not reclaimed")
			}
		})
	}
}
