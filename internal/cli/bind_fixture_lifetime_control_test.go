package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type bindFixtureWitness struct {
	Dir       string
	Namespace string
	Record    bindFixtureRecord
	Daemon    bindFixtureProcess
}

// Only the explicit subprocess regression uses this latch. The ordinary
// lifecycle still executes every original assertion, including failed requests.
func holdBindFixtureControl(t *testing.T, dir string, r bindFixtureRecord) bool {
	control := os.Getenv("A2A_RUN_BIND_FIXTURE_CONTROL")
	if control == "" {
		return false
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	w := bindFixtureWitness{Dir: dir, Namespace: namespace, Record: r, Daemon: bindFixtureDaemon(dir, r)}
	if !w.Daemon.live() {
		t.Fatal("control has no verified detached daemon")
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(control, "ready.next"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(control, "ready.next"), filepath.Join(control, "ready.json")); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(65 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(filepath.Join(control, "release")); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture control release timed out")
	return true
}

// A separate test binary performs the next-run sweep while the observer and
// concurrent neighboring fixture stay in the same namespace throughout.
func TestBindFixtureSweepProcess(t *testing.T) {
	if os.Getenv("A2A_BIND_FIXTURE_SWEEP") != "1" {
		return
	}
	identity, err := bindFixtureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	t.Logf("reaper identity: %+v PID namespace: %s", identity, namespace)
	root, err := bindFixtureRoot()
	if err == nil {
		err = sweepBindFixtures(root)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestBindFixtureKilledOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("detached CLI integration")
	}
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	observer, err := bindFixtureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("independent observer: %+v", observer)
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	t.Logf("observer PID namespace: %s", namespace)
	type child struct {
		cmd     *exec.Cmd
		owner   bindFixtureProcess
		control string
		witness bindFixtureWitness
		log     *os.File
		waited  bool
	}
	var children []*child
	// The safety cleanup deliberately bypasses the sweep entry point, so a plant
	// disabling that entry point fails its assertion without leaving a survivor.
	t.Cleanup(func() {
		for _, c := range children {
			if !c.waited {
				stopBindFixtureProcess(c.owner, syscall.SIGKILL)
				_ = c.cmd.Wait()
				c.waited = true
			}
			if c.witness.Dir != "" {
				if _, err := os.Stat(c.witness.Dir); err == nil {
					if err = reclaimBindFixture(c.witness.Dir, c.witness.Record); err != nil {
						t.Errorf("bounded control cleanup: %v", err)
					}
				}
				for role, p := range map[string]bindFixtureProcess{"server": c.witness.Record.Server, "fixture": c.witness.Record.Fixture, "daemon": c.witness.Daemon} {
					t.Logf("bounded safety cleanup %s: pid=%d absent=%v", role, p.PID, processGone(p.PID))
				}
			}
			_ = c.log.Close()
		}
	})
	start := func(name string) *child {
		t.Helper()
		c := &child{control: t.TempDir()}
		c.log, err = os.OpenFile(filepath.Join(c.control, "child.log"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		c.cmd = exec.Command(os.Args[0], "-test.run=^TestBindDetachedLifecycle$", "-test.v", "-test.timeout=85s")
		c.cmd.Env = append(os.Environ(), "A2A_RUN_BIND_FIXTURE_CONTROL="+c.control)
		c.cmd.Stdout, c.cmd.Stderr = c.log, c.log
		if err = c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
		c.owner, err = bindFixtureIdentity(c.cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(30 * time.Second)
		for time.Now().Before(until) {
			b, e := os.ReadFile(filepath.Join(c.control, "ready.json"))
			if e == nil && json.Unmarshal(b, &c.witness) == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if c.witness.Dir == "" {
			b, _ := os.ReadFile(filepath.Join(c.control, "child.log"))
			t.Fatalf("%s fixture not ready: %s", name, b)
		}
		if c.witness.Record.Owner != c.owner {
			t.Fatal("control owner mismatch")
		}
		if c.witness.Namespace != namespace {
			t.Fatal("fixture changed PID namespace")
		}
		for _, p := range []bindFixtureProcess{c.witness.Record.Owner, c.witness.Record.Server, c.witness.Record.Fixture, c.witness.Daemon} {
			if !p.live() {
				t.Fatalf("%s process not live: %+v", name, p)
			}
		}
		t.Logf("%s actual child identities: %+v", name, c.witness)
		return c
	}
	victim := start("victim")
	neighbor := start("neighbor")
	// Corrupt each persisted daemon authority independently. None may grant a
	// signal target; restore the exact bytes before the real kill-and-reap case.
	paths := config.Paths{Base: filepath.Join(victim.witness.Dir, "config")}
	original, err := os.ReadFile(paths.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	// Pause only the verified synthetic daemon while changing its private state,
	// under the state lock so it cannot be frozen while holding that lock.
	_, err = config.UpdateState(paths, "alice", func(st *config.State) error {
		if bindFixtureDaemon(victim.witness.Dir, victim.witness.Record) != victim.witness.Daemon {
			t.Fatal("daemon identity changed before state controls")
		}
		stopBindFixtureProcess(victim.witness.Daemon, syscall.SIGSTOP)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stopBindFixtureProcess(victim.witness.Daemon, syscall.SIGCONT)
	for _, field := range []string{"birth", "generation", "owner", "socket"} {
		_, err = config.UpdateState(paths, "alice", func(st *config.State) error {
			switch field {
			case "birth":
				st.Terminal.DaemonBirth = "wrong"
			case "generation":
				st.Terminal.Generation = "wrong"
			case "owner":
				st.Terminal.OwnerPID++
			case "socket":
				st.Terminal.Target.Socket += ".other"
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := bindFixtureDaemon(victim.witness.Dir, victim.witness.Record)
		if err = os.WriteFile(paths.StateFile("alice"), original, 0600); err != nil {
			t.Fatal(err)
		}
		if got.PID != 0 {
			t.Fatalf("%s mismatch granted daemon signal authority", field)
		}
		t.Logf("daemon %s mismatch refused", field)
	}
	stopBindFixtureProcess(victim.witness.Daemon, syscall.SIGCONT)
	stopBindFixtureProcess(victim.witness.Record.Owner, syscall.SIGKILL)
	err = victim.cmd.Wait()
	victim.waited = true
	if exit, ok := err.(*exec.ExitError); !ok || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("test binary was not killed: %v", err)
	}
	t.Logf("victim test binary Wait: %v", err)
	for _, p := range []bindFixtureProcess{victim.witness.Record.Server, victim.witness.Record.Fixture, victim.witness.Daemon} {
		if !p.live() {
			t.Fatalf("pre-sweep process did not survive child-test death: %+v", p)
		}
	}
	if !observer.live() {
		t.Fatal("independent observer did not survive")
	}
	sweep := exec.Command(os.Args[0], "-test.run=^TestBindFixtureSweepProcess$", "-test.v", "-test.timeout=15s")
	sweep.Env = append(os.Environ(), "A2A_BIND_FIXTURE_SWEEP=1")
	out, err := sweep.CombinedOutput()
	t.Logf("next-run sweep: %s", out)
	if err != nil {
		t.Fatalf("next-run sweep: %v", err)
	}
	until := time.Now().Add(4 * time.Second)
	allGone := func() bool {
		for _, p := range []bindFixtureProcess{victim.witness.Record.Server, victim.witness.Record.Fixture, victim.witness.Daemon} {
			if !processGone(p.PID) {
				return false
			}
		}
		return true
	}
	for !allGone() && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	for role, p := range map[string]bindFixtureProcess{"server": victim.witness.Record.Server, "fixture": victim.witness.Record.Fixture, "daemon": victim.witness.Daemon} {
		t.Logf("victim %s terminal: pid=%d absent=%v", role, p.PID, processGone(p.PID))
	}
	if !allGone() {
		t.Error("stale fixture processes survived next-run sweep")
	}
	for _, p := range []bindFixtureProcess{neighbor.witness.Record.Owner, neighbor.witness.Record.Server, neighbor.witness.Record.Fixture, neighbor.witness.Daemon} {
		if !p.live() {
			t.Errorf("live neighboring fixture was touched: %+v", p)
		}
	}
	t.Log("live neighboring fixture remained intact after sweep")
	if !observer.live() {
		t.Error("observer died or changed identity")
	}
	if err = os.WriteFile(filepath.Join(neighbor.control, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err = neighbor.cmd.Wait()
	neighbor.waited = true
	b, _ := os.ReadFile(filepath.Join(neighbor.control, "child.log"))
	t.Logf("neighbor normal cleanup: %s", b)
	if err != nil || !strings.Contains(string(b), "--- PASS: TestBindDetachedLifecycle") {
		t.Errorf("neighbor normal cleanup failed: %v", err)
	}
	for _, p := range []bindFixtureProcess{neighbor.witness.Record.Server, neighbor.witness.Record.Fixture, neighbor.witness.Daemon} {
		if !processGone(p.PID) {
			t.Errorf("normal cleanup left neighbor process: %+v", p)
		}
	}
}
