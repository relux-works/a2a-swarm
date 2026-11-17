//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Claim: on the launch alias means argv0 is a custody-admitted symlink
// alias of the running image. Beside that positive control, a direct image
// launch, a bare name, and an alias in an other-writable directory are all
// off the alias, so an alias retarget cannot reach those consumers.
func TestOnLaunchAliasVerdict(t *testing.T) {
	root := launchAliasCustodyRoot(t)
	images, links, open := filepath.Join(root, "images"), filepath.Join(root, "links"), filepath.Join(root, "open")
	mkdirs(t, images, links, open)
	if err := os.Chmod(open, 0777); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(images, "a2a-image")
	if err := os.WriteFile(image, []byte("image"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(links, "a2a")
	writable := filepath.Join(open, "a2a")
	for link, target := range map[string]string{alias: image, writable: image} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, argv0 string
		want        bool
	}{
		{"admitted symlink alias", alias, true},
		{"direct image", image, false},
		{"bare name", "a2a", false},
		{"alias in other-writable directory", writable, false},
		{"empty argv0", "", false},
	} {
		if got := onLaunchAlias(tc.argv0, image); got != tc.want {
			t.Errorf("%s: onLaunchAlias=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// busyLockObserveFixture installs a previous image's record (same
// generation and PID, as an in-place re-exec leaves it) and stubs the new
// image, so a missed record write would leave the old revision in place.
func busyLockObserveFixture(t *testing.T) (*env, *App, *session) {
	t.Helper()
	e, _ := world(t)
	a, s := notificationApp(t, e)
	rev1 := strings.Repeat("1", 40)
	notificationState(t, e, func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "gen", DaemonPID: os.Getpid(), Observe: &config.TerminalObserve{
			Executable: "/installed/a2a-one", Revision: rev1, PID: os.Getpid(), Generation: "gen",
			CPUAlerted: true, CPUJobID: "a2a-mail-alarm-job",
		}}
	})
	stubRunning(t, "/installed/a2a-two", "/installed/a2a-two", strings.Repeat("2", 40))
	return e, a, s
}

// Claim: a State lock held by another writer on the first record attempt
// (the moment after a re-exec) does not leave the previous image's
// revision recorded for the life of the process: the per-cycle recorder
// reports the attempt uncommitted, the next cycle after the lock frees
// lands the new revision with the CPU fence preserved, and later cycles
// make no further write. The control is the free lock, where the first
// attempt already commits. A single-attempt recorder fails this test.
func TestRecordRunningInfoRetriesAfterBusyLock(t *testing.T) {
	e, a, s := busyLockObserveFixture(t)
	rev2 := strings.Repeat("2", 40)

	control := &runningInfoRecorder{}
	if !control.ensure(a, s, "gen") {
		t.Fatal("free-lock control did not commit on the first attempt")
	}
	if got := e.state("alice").Terminal.Observe; got == nil || got.Revision != rev2 {
		t.Fatalf("free-lock control record: %+v", got)
	}
	// Restore the previous image's record for the busy-lock attempt.
	notificationState(t, e, func(st *config.State) {
		st.Terminal.Observe.Executable, st.Terminal.Observe.Revision = "/installed/a2a-one", strings.Repeat("1", 40)
	})

	recorder := &runningInfoRecorder{}
	release := e.holdStateLock("alice")
	if recorder.ensure(a, s, "gen") {
		t.Fatal("busy-lock attempt reported committed")
	}
	if got := e.state("alice").Terminal.Observe; got == nil || got.Revision != strings.Repeat("1", 40) {
		t.Fatalf("busy-lock attempt changed the record: %+v", got)
	}
	release()
	if !recorder.ensure(a, s, "gen") {
		t.Fatal("record did not land on the cycle after the lock freed")
	}
	got := e.state("alice").Terminal.Observe
	if got == nil || got.Executable != "/installed/a2a-two" || got.Revision != rev2 || got.PID != os.Getpid() || got.Generation != "gen" {
		t.Fatalf("record after retry: %+v", got)
	}
	if !got.CPUAlerted || got.CPUJobID != "a2a-mail-alarm-job" {
		t.Fatalf("retry lost the cpu episode fence: %+v", got)
	}
	before := readFile(t, e.paths().StateFile("alice"))
	if !recorder.ensure(a, s, "gen") || readFile(t, e.paths().StateFile("alice")) != before {
		t.Fatal("later cycles kept rewriting a committed record")
	}
}

// Claim: the startup-transaction form of the record waits briefly for a
// busy State lock and commits within that one attempt once the holder
// releases, beside the single non-blocking attempt, which reports
// uncommitted against the same held lock and changes nothing.
func TestRecordRunningInfoBoundedWaitAtStartup(t *testing.T) {
	e, a, s := busyLockObserveFixture(t)
	release := e.holdStateLock("alice")
	if a.recordRunningInfo(s, "gen") {
		t.Fatal("non-blocking attempt reported committed under a held lock")
	}
	done := make(chan bool, 1)
	go func() { done <- a.recordRunningInfoBy(s, "gen", time.Now().Add(10*time.Second)) }()
	select {
	case <-done:
		t.Fatal("bounded-wait attempt finished while the lock was still held")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case committed := <-done:
		if !committed {
			t.Fatal("bounded-wait attempt did not commit after the lock freed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded-wait attempt did not finish after the lock freed")
	}
	if got := e.state("alice").Terminal.Observe; got == nil || got.Revision != strings.Repeat("2", 40) {
		t.Fatalf("record after bounded wait: %+v", got)
	}
}
