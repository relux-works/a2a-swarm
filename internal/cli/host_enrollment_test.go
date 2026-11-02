package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func enrolledHostState(t *testing.T, e *env) config.State {
	t.Helper()
	now := e.clock().now().UTC()
	scope := config.HostScope{Agent: "alice", Member: "@alice:test.local", Connection: "test", Configuration: "own", Session: "host-session", NativeThread: "native-thread", Generation: "generation", Consumer: "consumer"}
	lease := config.Lease{Holder: scope.Session, Host: "fixture", PID: 123, Expires: now.Add(time.Minute)}
	st := e.state("alice")
	st.Lease = &lease
	if err := st.EnrollHost(config.HostAttachment{Reference: "admitted", Scope: scope, Lease: lease}, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveState(e.paths(), "alice", st); err != nil {
		t.Fatal(err)
	}
	return e.state("alice")
}

// Persisted Host custody, including expiry, refuses every new legacy consumer
// before lease mutation or provider/process access. A bounded canceled context
// keeps the old wait/watch regression from hanging in the fake homeserver.
func TestHostEnrollmentConsumerAdmission(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, args := range [][]string{
			{"wait", "--timeout", "1ms"}, {"watch"},
			{"bind", "--runtime", "codex", "--owner-pid", "123", "--session-id", "host-session", "--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "2"},
			{"runtime", "bind", "--codex-thread", "host-session", "--endpoint", "unix:///nonexistent-host-enrollment.sock", "--owner-pid", "123", "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none"},
		} {
			t.Run(args[0]+"/"+map[bool]string{false: "valid", true: "expired"}[expired], func(t *testing.T) {
				e, _ := world(t)
				before := enrolledHostState(t, e)
				e.extra[EnvSessionID] = "host-session"
				if expired {
					e.now = func() time.Time { return before.Lease.Expires.Add(time.Second) }
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				r := e.runCtx(ctx, append([]string{"--as", "alice"}, args...)...)
				if r.code != ExitError || !strings.Contains(r.stderr, config.ErrHostEnrollment.Error()) || r.stdout != "" {
					t.Fatalf("consumer not refused: %+v", r)
				}
				if !reflect.DeepEqual(before, e.state("alice")) {
					t.Fatal("refused consumer changed state")
				}
			})
		}
	}
	e, _ := world(t)
	_, s := notificationApp(t, e)
	if err := refuseBoundWait(context.Background(), s); err != nil {
		t.Fatalf("legacy wait guard: %v", err)
	}
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
}

// Doorbell-driven inbox can print mail without replacing its Host-owned lease.
// Foreign and exactly expired holders refuse without extending that lease.
func TestHostEnrollmentInboxLease(t *testing.T) {
	for _, mode := range []string{"same", "foreign", "expired"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			e.extra[EnvSessionID] = "host-session"
			e.as("alice", "inbox").ok(t)
			before := enrolledHostState(t, e)
			if mode == "foreign" {
				e.extra[EnvSessionID] = "foreign-session"
			}
			if mode == "expired" {
				e.now = func() time.Time { return before.Lease.Expires }
			}
			e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "Host inbox witness"))
			r := e.as("alice", "inbox")
			got := e.state("alice")
			if mode == "same" {
				if r.code != ExitOK || !strings.Contains(r.stdout, "Host inbox witness") {
					t.Fatalf("member cannot read: %+v", r)
				}
				attachment := before.HostEnrollment.Attachment
				if err := got.CheckHostEnrollment(attachment, attachment.Scope, e.now()); err != nil {
					t.Fatalf("inbox broke admission: %v", err)
				}
			} else if r.code != ExitError || !strings.Contains(r.stderr, config.ErrHostEnrollment.Error()) || r.stdout != "" {
				t.Fatalf("foreign/expired inbox not refused: %+v", r)
			}
			if !reflect.DeepEqual(before.Lease, got.Lease) || !reflect.DeepEqual(before.HostEnrollment, got.HostEnrollment) {
				t.Fatal("inbox refreshed Host lease or changed attachment")
			}
			if mode != "same" && !reflect.DeepEqual(before, got) {
				t.Fatal("refused inbox changed state")
			}
		})
	}
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
