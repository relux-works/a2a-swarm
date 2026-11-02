package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func enrolledHostState(t *testing.T, e *env) config.State {
	t.Helper()
	now := e.clock().now().UTC()
	scope := config.HostScope{Agent: "alice", Member: "@alice:test.local", Connection: "test", Configuration: "own", Session: "host-session", NativeThread: "native-thread", Generation: "generation", Consumer: "consumer"}
	lease := config.Lease{Holder: scope.Session, Host: "fixture", PID: 123, Expires: now.Add(time.Minute)}
	st := config.State{Lease: &lease}
	if err := st.EnrollHost(config.HostAttachment{Reference: "admitted", Scope: scope, Lease: lease}, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveState(e.paths(), "alice", st); err != nil {
		t.Fatal(err)
	}
	return e.state("alice")
}

// Proves a selected Host attachment suppresses all native hook entry paths
// before SessionStart export/auto-bind/heal, including expired/held custody.
// The neighboring legacy path still emits its ordinary pending-mail notice.
func TestHostEnrollmentNoHookFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	e.hook("alice", "prompt", `{}`).ok(t)
	e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "pending"))
	before := enrolledHostState(t, e)
	export := filepath.Join(t.TempDir(), "session-env")
	e.extra[envClaudeEnvFile] = export
	for _, runtime := range []string{"codex", "claude", "muse"} {
		for _, event := range []string{"session-start", "prompt", "stop"} {
			r := e.hookRT(runtime, "alice", event, `{"session_id":"foreign-thread"}`).ok(t)
			if r.stdout != "" || r.stderr != "" {
				t.Fatalf("Host hook fallback %s: %+v", event, r)
			}
			if got := e.state("alice"); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s mutated source state", event)
			}
			if _, err := os.Stat(export); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s wrote legacy session export: %v", event, err)
			}
		}
	}
	// Expiry does not authorize routing to a different backend.
	clock := e.clock()
	clock.add(2 * time.Minute)
	r := e.hookRT("codex", "alice", "stop", `{"session_id":"host-session"}`).ok(t)
	if r.stdout != "" || !reflect.DeepEqual(e.state("alice"), before) {
		t.Fatal("expired Host fell back")
	}
	legacy, legacyRoom := world(t)
	legacy.extra[envClaudeEnvFile] = export
	legacy.hook("alice", "session-start", `{"session_id":"legacy"}`).ok(t)
	if b, err := os.ReadFile(export); err != nil || string(b) != "export A2A_SESSION_ID=legacy\n" {
		t.Fatalf("legacy session export control: %q %v", b, err)
	}
	delete(legacy.extra, envClaudeEnvFile)
	legacy.sequentialSessions()
	legacy.hook("alice", "prompt", `{}`).ok(t)
	legacy.hs.inject(legacyRoom, "@bob:test.local", toMsg("bob", "request", "pending"))
	r = legacy.hook("alice", "prompt", `{"session_id":"legacy"}`).ok(t)
	if r.stdout == "" {
		t.Fatalf("legacy positive control suppressed: %+v", r)
	}
}

// Proves hook/native/terminal callers cannot reserve another output while a
// Host record owns custody; the callback and every state field stay unchanged.
func TestHostEnrollmentNoLegacyReservation(t *testing.T) {
	e, _ := world(t)
	before := enrolledHostState(t, e)
	a, s := notificationApp(t, e)
	for _, path := range []string{"hook", "native", "terminal"} {
		called := false
		token, err := a.reserveNotification(context.Background(), s, time.Now().Add(time.Second), path, "host-session", "generation", true, func(*config.State, *config.Mailbox, string) (bool, error) { called = true; return true, nil })
		if !errors.Is(err, config.ErrHostEnrollment) || token != "" || called {
			t.Fatalf("%s reservation token=%q err=%v callback=%v", path, token, err, called)
		}
		if !reflect.DeepEqual(e.state("alice"), before) {
			t.Fatal("rejected reservation mutated custody/rate state")
		}
	}
}
