//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// blockingBackend models a native mutation that ignores its context: add and
// update wait until release is closed, then apply to the file-backed fake.
type blockingBackend struct {
	fakeBackend
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (b *blockingBackend) hold() {
	b.once.Do(func() { close(b.started) })
	<-b.release
}

func (b *blockingBackend) add(ctx context.Context, keychain, service, account string, secret []byte) error {
	b.hold()
	return b.fakeBackend.add(ctx, keychain, service, account, secret)
}

func (b *blockingBackend) update(ctx context.Context, keychain, service, account string, secret []byte) error {
	b.hold()
	return b.fakeBackend.update(ctx, keychain, service, account, secret)
}

func waitUntil(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(limit); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("condition not reached")
		}
	}
}

// The reviewer's schedule for an abandoned mutation (#138 review, finding 1):
// writer A's add ignores its deadline and completes only when released. A
// times out; even after the cooldown has passed, writer B and every read
// refuse while A is outstanding. Once A's native call returns, its fence
// clears; B then writes and keeps its token, which A can no longer overwrite.
func TestKeychainAbandonedWriteStaysFenced(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	b := &blockingBackend{fakeBackend: fakeBackend{dir}, started: make(chan struct{}), release: make(chan struct{})}
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: b, timeout: 300 * time.Millisecond, now: clock})
	ctx := context.Background()

	if err := k.write(ctx, uid, "token-a"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatal("writer A did not time out", err)
	}
	<-b.started
	if _, err := os.Stat(k.fencePath(uid)); err != nil {
		t.Fatal("abandoned add left no fence", err)
	}
	mu.Lock()
	now = now.Add(2 * securityCooldown)
	mu.Unlock()
	if err := k.write(ctx, uid, "token-b"); !errors.Is(err, errKeychainFenced) {
		t.Fatal("writer B admitted while A is outstanding", err)
	}
	if _, err := k.read(ctx, uid); !errors.Is(err, errKeychainFenced) {
		t.Fatal("read admitted while A is outstanding", err)
	}
	if readModel(t, dir).Writes != 0 {
		t.Fatal("a refused operation reached the writer")
	}

	close(b.release)
	waitUntil(t, 2*time.Second, func() bool {
		_, err := os.Stat(k.fencePath(uid))
		return errors.Is(err, os.ErrNotExist)
	})
	if got := readModel(t, dir).Items[uid]; got != "token-a" {
		t.Fatal("A's late add did not land before its fence cleared")
	}
	if err := k.write(ctx, uid, "token-b"); err != nil {
		t.Fatal(err)
	}
	if token, err := k.read(ctx, uid); err != nil || token != "token-b" {
		t.Fatal("B's verified token was not retained", err)
	}
}

// A completed mutation leaves no fence (positive control for the test above).
func TestKeychainCompletedWriteLeavesNoFence(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: fakeBackend{dir}})
	if err := k.write(context.Background(), uid, "token-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(k.fencePath(uid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed write left a fence", err)
	}
}

func plantFence(t *testing.T, k *keychainStore, uid string, fence keychainFence, mode os.FileMode) {
	t.Helper()
	path := k.fencePath(uid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(fence)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A fence whose writer is gone never expires. Every command refuses until
// `creds reconcile`. Reconcile keeps the fence while the owner lives or while
// the keychain does not answer, and clears it once a lookup answers. A fence
// that is not a private regular file refuses like any other private state.
func TestKeychainFenceReconcile(t *testing.T) {
	type world struct {
		e     *env
		uid   string
		dir   string
		k     *keychainStore
		alive *bool
	}
	setup := func(t *testing.T, mode string) world {
		e := newEnv(t)
		uid := e.agent("alice")
		dir := fakeKeychain(e, map[string]string{uid: e.tokens["alice"]}, mode)
		alive := new(bool)
		birth := func(_ context.Context, pid int) (string, error) {
			if pid == 4242 && *alive {
				return "planted-birth", nil
			}
			if pid == os.Getpid() {
				return "test-process", nil
			}
			return "", errors.New("process gone")
		}
		prior := e.configure
		e.configure = func(a *App) {
			prior(a)
			inner := a.keychain
			a.keychain = func(p config.Paths) *keychainStore { k := inner(p); k.birth = birth; return k }
		}
		k := isolatedKeychainStore(keychainStore{paths: e.paths()})
		plantFence(t, k, uid, keychainFence{PID: 4242, Birth: "planted-birth", Op: "update", At: 1}, 0o600)
		return world{e, uid, dir, k, alive}
	}
	fenceExists := func(w world) bool { _, err := os.Stat(w.k.fencePath(w.uid)); return err == nil }

	t.Run("live-owner", func(t *testing.T) {
		w := setup(t, "")
		*w.alive = true
		w.e.as("alice", "whoami").failsWith(t, ExitError, "refusing until it completes")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "not reconciled")
		if !fenceExists(w) || len(records(t, w.dir)) != 0 {
			t.Fatal("live owner's fence touched or backend reached")
		}
		tuitestkit.AssertNotCalled(t, &w.e.hs.MockCallRecorder, routeWhoami)
	})
	t.Run("keychain-silent", func(t *testing.T) {
		w := setup(t, "locked")
		w.e.as("alice", "whoami").failsWith(t, ExitError, "creds reconcile")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "fence kept")
		if !fenceExists(w) {
			t.Fatal("fence cleared without an answering keychain")
		}
	})
	t.Run("dead-owner-answered", func(t *testing.T) {
		w := setup(t, "")
		w.e.as("alice", "whoami").failsWith(t, ExitError, "creds reconcile")
		if len(records(t, w.dir)) != 0 {
			t.Fatal("fenced read reached the backend")
		}
		r := w.e.as("alice", "creds", "reconcile").ok(t)
		if !strings.Contains(r.stdout, "keychain answered; item present") || fenceExists(w) {
			t.Fatal("dead owner's fence not reconciled", r.stdout)
		}
		w.e.as("alice", "whoami").ok(t)
		tuitestkit.AssertCalledN(t, &w.e.hs.MockCallRecorder, routeWhoami, 1)
		r = w.e.as("alice", "creds", "reconcile").ok(t)
		if !strings.Contains(r.stdout, "no unresolved keychain write") {
			t.Fatal("reconcile without a fence", r.stdout)
		}
	})
	t.Run("public-fence", func(t *testing.T) {
		w := setup(t, "")
		plantFence(t, w.k, w.uid, keychainFence{PID: 4242, Birth: "planted-birth", Op: "update", At: 1}, 0o644)
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence unreadable")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "fence unreadable")
	})
	t.Run("garbage-fence", func(t *testing.T) {
		w := setup(t, "")
		if err := os.WriteFile(w.k.fencePath(w.uid), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence invalid")
	})
}
