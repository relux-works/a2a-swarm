package cli

import (
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestMarkedOwnStatusWithoutActiveBinding proves that an identity whose State
// is marked still publishes its own status when its binding is stopped or
// when it has no binding at all, with the same result as an unmarked identity
// in the same situation: the command succeeds, the status is stored, no
// publication is left pending, a stopped binding's notice is recorded as
// published, and a second write succeeds as well.
//
// Regression witness: the sidecar's status reservation refused a stopped
// binding, and the sidecar path refused an identity without a terminal, both
// with "stale bind-state revision or operation token". Every identity that
// had bound once is marked for good, so its `status set` failed from the
// moment its session ended.
//
// The unmarked row is the control for the expected result; the active row is
// the control for the sidecar path. The stopped binding is produced by the
// migration of a stopped State, not by a real unbind.
func TestMarkedOwnStatusWithoutActiveBinding(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		terminal, stopped, mark bool
	}{
		{"unmarked-stopped", true, true, false},
		{"marked-stopped", true, true, true},
		{"marked-no-terminal", false, false, true},
		{"marked-active", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			if tc.terminal {
				if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal = &config.TerminalBinding{Generation: "g1", SessionID: "s1", Host: machineLabel(), Stopped: tc.stopped, Interval: 2 * time.Second, RetryMax: 30 * time.Second}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mark {
				if _, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Now: time.Now()}); err != nil {
					t.Fatalf("marking: %v", err)
				}
			}
			for _, want := range []string{"working", "idle"} {
				e.as("alice", statusArgs(want)...).ok(t)
				st := e.state("alice")
				if config.BindPartMarked(st) != tc.mark {
					t.Fatalf("marked=%t, want %t", config.BindPartMarked(st), tc.mark)
				}
				if st.Liveness == nil || st.Liveness.State != want {
					t.Fatalf("stored status %+v, want %s", st.Liveness, want)
				}
				if st.OwnStatusWrite != nil || st.NoticeWriteUncertain {
					t.Fatalf("a publication is left pending: write=%v uncertain=%t", st.OwnStatusWrite, st.NoticeWriteUncertain)
				}
				if (st.Terminal != nil) != tc.terminal {
					t.Fatalf("terminal present=%t, want %t", st.Terminal != nil, tc.terminal)
				}
				if tc.terminal && (st.Terminal.Stopped != tc.stopped || st.Terminal.Generation != "g1" || st.Terminal.NoticePublication != "published") {
					t.Fatalf("binding after the write: stopped=%t generation=%q publication=%q", st.Terminal.Stopped, st.Terminal.Generation, st.Terminal.NoticePublication)
				}
			}
		})
	}
}
