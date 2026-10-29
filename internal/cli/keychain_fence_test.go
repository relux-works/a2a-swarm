//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: b, timeout: 300 * time.Millisecond, now: clock,
		boot: func() (string, error) { return sessionA, nil }})
	ctx := context.Background()

	if err := k.write(ctx, uid, "token-a"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatal("writer A did not time out", err)
	}
	<-b.started
	fence, err := k.readFence(uid)
	if err != nil || fence == nil {
		t.Fatal("abandoned add left no fence", err)
	}
	if fence.PID != os.Getpid() || fence.Op != "add" || fence.Digest != tokenDigest([]byte("token-a")) || fence.Boot != sessionA {
		t.Fatalf("fence does not record the writer, op, intended digest and boot session: %+v", fence)
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

// Invalid boot evidence at write time is recorded as none: a fence never
// carries a value that a later session could differ from by accident.
func TestKeychainFenceRecordsNoInvalidBoot(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	b := &blockingBackend{fakeBackend: fakeBackend{dir}, started: make(chan struct{}), release: make(chan struct{})}
	defer close(b.release)
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: b, timeout: 300 * time.Millisecond,
		boot: func() (string, error) { return "garbage-without-error", nil }})
	if err := k.write(context.Background(), uid, "token-a"); err == nil {
		t.Fatal("write did not time out")
	}
	<-b.started
	fence, err := k.readFence(uid)
	if err != nil || fence == nil || fence.Boot != "" {
		t.Fatalf("invalid boot evidence recorded: %+v %v", fence, err)
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

// Boot-session UUIDs (kern.bootsessionuuid): A before a restart, B after.
const (
	sessionA = "11111111-2222-3333-4444-555555555555"
	sessionB = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

// deadFence is the fence of an abandoned write by pid 4242 in boot session A.
func deadFence(intended string) keychainFence {
	return keychainFence{PID: 4242, Birth: "planted-birth", Op: "update", At: 1, Boot: sessionA, Digest: tokenDigest([]byte(intended))}
}

type fenceWorld struct {
	e        *env
	uid, dir string
	k        *keychainStore
	state    *ownerState
	boot     *string
	clock    *time.Time
}

// newFenceWorld plants deadFence(intended) for alice, whose item holds
// current, and controls the recorder's liveness and the boot session. The
// value "<alice>" stands for alice's real homeserver token.
func newFenceWorld(t *testing.T, mode, current, intended string, state ownerState) fenceWorld {
	e := newEnv(t)
	uid := e.agent("alice")
	if current == "<alice>" {
		current = e.tokens["alice"]
	}
	if intended == "<alice>" {
		intended = e.tokens["alice"]
	}
	items := map[string]string{}
	if current != "" {
		items[uid] = current
	}
	dir := fakeKeychain(e, items, mode)
	w := fenceWorld{e: e, uid: uid, dir: dir, state: &state, boot: new(string), clock: new(time.Time)}
	*w.boot = sessionA
	*w.clock = time.Now()
	prior := e.configure
	e.configure = func(a *App) {
		prior(a)
		inner := a.keychain
		a.keychain = func(p config.Paths) *keychainStore {
			k := inner(p)
			k.liveness = func(context.Context, int, string) ownerState { return *w.state }
			k.now = func() time.Time { return *w.clock }
			k.boot = func() (string, error) {
				if *w.boot == "" {
					return "", errors.New("boot session unavailable")
				}
				return *w.boot, nil
			}
			return k
		}
	}
	w.k = isolatedKeychainStore(keychainStore{paths: e.paths()})
	plantFence(t, w.k, uid, deadFence(intended), 0o600)
	return w
}

func (w fenceWorld) fenced() bool { _, err := os.Stat(w.k.fencePath(w.uid)); return err == nil }

// A fence never expires on its own. Reconcile refuses while its recorder is
// alive or of unknown state, without touching the keychain; for a confirmed
// gone recorder it clears the fence only when the intended value is observed
// or the host has restarted since, and keeps it when the value differs or is
// missing, or the keychain does not answer. A fence that is not a private
// regular file, or is malformed, refuses like any other private state.
func TestKeychainFenceReconcile(t *testing.T) {
	t.Run("live-owner", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-a", "token-a", ownerAlive)
		w.e.as("alice", "whoami").failsWith(t, ExitError, "refusing until it completes")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "not reconciled")
		if !w.fenced() || len(records(t, w.dir)) != 0 {
			t.Fatal("live owner's fence touched or backend reached")
		}
		tuitestkit.AssertNotCalled(t, &w.e.hs.MockCallRecorder, routeWhoami)
	})
	t.Run("unknown-owner", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-a", "token-a", ownerUnknown)
		w.e.as("alice", "whoami").failsWith(t, ExitError, "is unknown")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "unknown; fence kept")
		if !w.fenced() || len(records(t, w.dir)) != 0 {
			t.Fatal("unknown owner's fence touched or backend reached")
		}
	})
	t.Run("gone-observed", func(t *testing.T) {
		w := newFenceWorld(t, "", "<alice>", "<alice>", ownerGone)
		w.e.as("alice", "whoami").failsWith(t, ExitError, "creds reconcile")
		r := w.e.as("alice", "creds", "reconcile").ok(t)
		if !strings.Contains(r.stdout, "abandoned write observed completed") || w.fenced() {
			t.Fatal("observed completion not reconciled", r.stdout)
		}
		w.e.as("alice", "whoami").ok(t)
		r = w.e.as("alice", "creds", "reconcile").ok(t)
		if !strings.Contains(r.stdout, "no unresolved keychain write") {
			t.Fatal("reconcile without a fence", r.stdout)
		}
	})
	t.Run("gone-not-observed", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
		if !w.fenced() {
			t.Fatal("fence cleared although the abandoned write is not observed")
		}
	})
	t.Run("gone-missing", func(t *testing.T) {
		w := newFenceWorld(t, "", "", "token-a", ownerGone)
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
		if !w.fenced() {
			t.Fatal("fence cleared for a missing item")
		}
	})
	t.Run("gone-restarted", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		*w.boot = sessionB
		r := w.e.as("alice", "creds", "reconcile").ok(t)
		if !strings.Contains(r.stdout, "host restarted") || w.fenced() || len(records(t, w.dir)) != 0 {
			t.Fatal("restart quiescence not reconciled without a lookup", r.stdout)
		}
	})
	// The boot session is the only restart evidence: unavailable evidence, or a
	// fence recorded without it, never counts as a restart (#138 review r3).
	t.Run("gone-boot-unavailable", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		*w.boot = ""
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
		if !w.fenced() {
			t.Fatal("fence cleared without boot evidence")
		}
	})
	t.Run("gone-fence-without-boot", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		f := deadFence("token-a")
		f.Boot = ""
		plantFence(t, w.k, w.uid, f, 0o600)
		*w.boot = sessionB
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
		if !w.fenced() {
			t.Fatal("fence without boot evidence cleared by a session change")
		}
	})
	t.Run("gone-silent", func(t *testing.T) {
		w := newFenceWorld(t, "locked", "token-a", "token-a", ownerGone)
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "fence kept")
		if !w.fenced() {
			t.Fatal("fence cleared without an answering keychain")
		}
	})
	// Malformed boot evidence is never restart evidence (#138 review r4):
	// a recorded value that is not a UUID makes the fence invalid, and an
	// invalid current value returned without an error does not count.
	t.Run("gone-malformed-recorded-boot", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		f := deadFence("token-a")
		f.Boot = "not-a-uuid"
		plantFence(t, w.k, w.uid, f, 0o600)
		*w.boot = sessionB
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence invalid")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "fence invalid")
		if !w.fenced() {
			t.Fatal("malformed fence removed")
		}
	})
	t.Run("gone-invalid-current-boot", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
		*w.boot = "garbage-without-error"
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
		if !w.fenced() {
			t.Fatal("invalid current boot evidence counted as a restart")
		}
	})
	t.Run("public-fence", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-a", "token-a", ownerGone)
		plantFence(t, w.k, w.uid, deadFence("token-a"), 0o644)
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence unreadable")
		w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "fence unreadable")
	})
	t.Run("symlink-fence", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-a", "token-a", ownerGone)
		target := w.k.fencePath(w.uid) + ".target"
		if err := os.Rename(w.k.fencePath(w.uid), target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, w.k.fencePath(w.uid)); err != nil {
			t.Fatal(err)
		}
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence unreadable")
	})
	t.Run("garbage-fence", func(t *testing.T) {
		w := newFenceWorld(t, "", "token-a", "token-a", ownerGone)
		if err := os.WriteFile(w.k.fencePath(w.uid), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		w.e.as("alice", "whoami").failsWith(t, ExitError, "fence invalid")
	})
}

// The reviewer's dead-writer schedule (#138 review r2, finding 2): writer A
// submitted an update and died; its request is still queued. Reconcile's
// lookup answers first with the old value, so the fence stays and writer B is
// not admitted. The queued request then lands A's value; only now does
// reconcile observe completion and clear the fence, and B's newer write is
// the one that remains. The boot session stays the same throughout while the
// store's clock moves past the cooldown: elapsed time or a clock change is
// never restart evidence (#138 review r3).
func TestKeychainDeadWriterQueuedRequest(t *testing.T) {
	w := newFenceWorld(t, "", "token-0", "token-a", ownerGone)
	*w.clock = time.Now().Add(10 * 365 * 24 * time.Hour) // a large clock correction, same boot session
	w.e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "may still land")
	k := w.k
	k.backend = fakeBackend{w.dir}
	k.probeCommand = unlockedProbe
	k.liveness = func(context.Context, int, string) ownerState { return ownerGone }
	k.boot = func() (string, error) { return sessionA, nil }
	if err := k.write(context.Background(), w.uid, "token-b"); !errors.Is(err, errKeychainFenced) {
		t.Fatal("writer B admitted while the dead writer's request is queued", err)
	}
	model := readModel(t, w.dir)
	if model.Items[w.uid] != "token-0" {
		t.Fatal("refused B changed the item")
	}
	model.Items[w.uid] = "token-a" // securityd finally processes A's queued update
	saveModel(t, w.dir, model)
	r := w.e.as("alice", "creds", "reconcile").ok(t)
	if !strings.Contains(r.stdout, "observed completed") {
		t.Fatal("completed queued write not reconciled", r.stdout)
	}
	if err := k.write(context.Background(), w.uid, "token-b"); err != nil {
		t.Fatal(err)
	}
	if token, err := k.read(context.Background(), w.uid); err != nil || token != "token-b" {
		t.Fatal("B's verified token was not retained", err)
	}
}

// The reviewer's live-but-uninspectable schedule (#138 review r2, finding 1):
// writer A is blocked and alive, but its liveness cannot be determined (for
// example ps cannot be run). Reconcile must refuse without querying the
// keychain and keep the fence, B stays excluded, and once A's call returns
// its own goroutine clears the fence and B can write.
func TestKeychainUnknownOwnerKeepsFence(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	dir := fakeKeychain(e, map[string]string{}, "")
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	b := &blockingBackend{fakeBackend: fakeBackend{dir}, started: make(chan struct{}), release: make(chan struct{})}
	k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: b, timeout: 300 * time.Millisecond, now: clock,
		liveness: func(context.Context, int, string) ownerState { return ownerUnknown }})
	ctx := context.Background()
	if err := k.write(ctx, uid, "token-a"); err == nil {
		t.Fatal("writer A did not time out")
	}
	<-b.started
	mu.Lock()
	now = now.Add(2 * securityCooldown)
	mu.Unlock()
	ops := len(records(t, dir))
	if _, err := k.reconcile(ctx, uid); err == nil || !strings.Contains(err.Error(), "unknown; fence kept") {
		t.Fatal("reconcile cleared an uninspectable owner's fence", err)
	}
	if len(records(t, dir)) != ops {
		t.Fatal("reconcile queried the keychain for an uninspectable owner")
	}
	if err := k.write(ctx, uid, "token-b"); !errors.Is(err, errKeychainFenced) {
		t.Fatal("writer B admitted", err)
	}
	close(b.release)
	waitUntil(t, 2*time.Second, func() bool {
		_, err := os.Stat(k.fencePath(uid))
		return errors.Is(err, os.ErrNotExist)
	})
	if err := k.write(ctx, uid, "token-b"); err != nil {
		t.Fatal(err)
	}
	if got := readModel(t, dir).Items[uid]; got != "token-b" {
		t.Fatal("B's token not retained", got)
	}
}

// A value already present is never written again, so an abandoned write's
// intended value can only appear through that write: an existing equal item
// and a duplicate-create race that already holds the value both finish
// without any add or update and leave no fence. A different value is still
// updated (control).
func TestKeychainWriteSkipsPresentValue(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, mode, current, token string
		mutations                  int
	}{
		{"existing-equal", "", "token-x", "token-x", 0},
		{"duplicate-race-equal", "duplicate", "", "external-token", 1},
		{"existing-different", "", "token-old", "token-x", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			items := map[string]string{}
			if c.current != "" {
				items[uid] = c.current
			}
			dir := fakeKeychain(e, items, c.mode)
			k := isolatedKeychainStore(keychainStore{paths: e.paths(), probeCommand: unlockedProbe, backend: fakeBackend{dir}})
			if err := k.write(ctx, uid, c.token); err != nil {
				t.Fatal(err)
			}
			if got := countOps(records(t, dir), "add", "update"); got != c.mutations {
				t.Fatalf("mutations=%d want %d", got, c.mutations)
			}
			if got := readModel(t, dir).Items[uid]; got != c.token {
				t.Fatal("value not as written", got)
			}
			if _, err := os.Stat(k.fencePath(uid)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fence left behind", err)
			}
		})
	}
}

// Liveness: only ESRCH or a replaced pid proves the recorder gone. Every
// inspection failure is unknown, and the recorded identity matching is alive.
func TestKeychainLivenessClassification(t *testing.T) {
	for _, c := range []struct {
		name    string
		killErr error
		current string
		idErr   error
		want    ownerState
	}{
		{"no-such-process", syscall.ESRCH, "", nil, ownerGone},
		{"same-identity", nil, "id-1", nil, ownerAlive},
		{"other-user-same-identity", syscall.EPERM, "id-1", nil, ownerAlive},
		{"replaced-pid", nil, "id-2", nil, ownerGone},
		{"ps-unavailable", nil, "", errors.New("exec: no ps"), ownerUnknown},
		{"empty-identity", nil, "", nil, ownerUnknown},
		{"probe-error", syscall.EINVAL, "id-1", nil, ownerUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyLiveness(c.killErr, c.current, c.idErr, "id-1"); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
	// Positive control on the real process table: this test process is alive
	// under its own identity and gone under any other.
	id, err := processIdentity(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal("real process identity:", err)
	}
	if processLiveness(context.Background(), os.Getpid(), id) != ownerAlive || processLiveness(context.Background(), os.Getpid(), "other") != ownerGone {
		t.Fatal("real liveness misclassified this process")
	}
}

// On macOS the real boot session is a valid UUID, which a fence records.
func TestKeychainBootSessionReadable(t *testing.T) {
	id, err := keychainBootSession()
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatal("boot session claimed off macOS")
		}
		return
	}
	if err != nil || !validBootSession(id) {
		t.Fatalf("boot session %q: %v", id, err)
	}
	for _, bad := range []string{"", "7", "1761700000", "11111111-2222-3333-4444-55555555555", "zzzzzzzz-2222-3333-4444-555555555555"} {
		if validBootSession(bad) {
			t.Fatalf("invalid boot session accepted: %q", bad)
		}
	}
}
