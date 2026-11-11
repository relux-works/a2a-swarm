//go:build unix

package cli

import (
	"sync"
	"testing"
	"time"
)

// This test-only gate extends the existing stagedBackend timeout's return
// interval. It changes neither the backend result nor any production guard.
// returned means the staged operation is ready to return, NOT that run has
// cleared its fence.
type keychainTimeoutWitnessGate struct {
	entered, release, returned chan struct{}
	holdOnce, releaseOnce      sync.Once
}

func newKeychainTimeoutWitnessGate(t *testing.T, k *keychainStore, uid string) *keychainTimeoutWitnessGate {
	t.Helper()
	g := &keychainTimeoutWitnessGate{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	// Registered after all fixture TempDirs: release and observe return/cleanup
	// before their removal, even when the diagnostic assertion calls Fatal. The
	// staged failure performs no fake I/O after returned; fence absence is
	// also required.
	t.Cleanup(func() { g.finish(t, k, uid) })
	return g
}

func (g *keychainTimeoutWitnessGate) hold() {
	g.holdOnce.Do(func() {
		close(g.entered)
		<-g.release
		close(g.returned)
	})
}

func keychainWitnessSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout witness did not observe %s", description)
	}
}

func (g *keychainTimeoutWitnessGate) finish(t *testing.T, k *keychainStore, uid string) {
	t.Helper()
	g.releaseOnce.Do(func() { close(g.release) })
	keychainWitnessSignal(t, g.returned, "backend return boundary")
	// Poll the actual state with a fail deadline. A context deadline or the
	// returned notification alone cannot establish that run removed its fence.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		fence, err := k.readFence(uid)
		if err != nil {
			t.Fatal("timeout witness cannot read cleanup predicate", err)
		}
		if fence == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timeout witness backend returned but fence did not clear")
		case <-tick.C:
		}
	}
}
