package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type ownerNoNetwork struct{ t *testing.T }

func (n ownerNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Error("owner-only hook contacted HTTP")
	return nil, errors.New("HTTP forbidden")
}

func ownerCLI(t *testing.T) *env {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	e := newEnv(t)
	e.dir, err = filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.agent("alice")
	e.agent("bob")
	e.extra[config.EnvAgent] = "alice"
	if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.HooksPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.HTTP = &http.Client{Transport: ownerNoNetwork{t}} }
	return e
}

func cliOwnerHash(session string) string {
	h := sha256.Sum256([]byte(session))
	return hex.EncodeToString(h[:])
}

// Proves each supported local runtime's admitted CLI Start/refresh/replacement
// and end path, even when notices are paused. It does not qualify installation.
func TestMonitorOwnerHookAdmission(t *testing.T) {
	for _, runtime := range []string{"muse", "claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := ownerCLI(t)
			now := time.Now().UTC()
			e.now = func() time.Time { return now }
			run := func(arg, event, session, extra string) {
				t.Helper()
				e.stdin = `{"session_id":"` + session + `","hook_event_name":"` + event + `"` + extra + `}`
				r := e.as("alice", "hook", arg, "--runtime", runtime).ok(t)
				if r.stdout != "" || r.stderr != "" {
					t.Fatalf("owner-only hook: %+v", r)
				}
			}
			run("session-start", "SessionStart", "native-A", "")
			first := e.state("alice").MonitorOwner
			if first == nil || first.Runtime != runtime || first.SessionIDHash != cliOwnerHash("native-A") || !first.FirstSeen.Equal(now) {
				t.Fatal("start did not admit stdin owner", first)
			}
			if e.state("alice").Lease != nil {
				t.Fatal("association acquired a lease")
			}
			now = now.Add(time.Hour)
			run("prompt", "UserPromptSubmit", "native-A", "")
			refreshed := e.state("alice").MonitorOwner
			if !refreshed.FirstSeen.Equal(first.FirstSeen) || !refreshed.LastHookAt.Equal(now) {
				t.Fatal("refresh", refreshed)
			}
			now = now.Add(time.Hour)
			run("session-start", "SessionStart", "native-B", "")
			if e.state("alice").MonitorOwner.SessionIDHash != cliOwnerHash("native-B") {
				t.Fatal("replacement")
			}
			now = now.Add(time.Hour)
			run("stop", "Stop", "native-A", `,"stop_reason":"exit"`)
			if e.state("alice").MonitorOwner.SessionIDHash != cliOwnerHash("native-B") {
				t.Fatal("old owner exit ended replacement")
			}
			run("stop", "Stop", "native-B", "")
			if e.state("alice").MonitorOwner == nil {
				t.Fatal("normal Stop ended owner")
			}
			run("session-end", "SessionEnd", "native-B", "")
			if e.state("alice").MonitorOwner != nil {
				t.Fatal("SessionEnd retained owner")
			}
			now = now.Add(time.Hour)
			run("session-start", "SessionStart", "native-C", "")
			run("stop", "Stop", "native-C", `,"stop_reason":"exit"`)
			if e.state("alice").MonitorOwner != nil {
				t.Fatal("explicit exit retained owner")
			}
		})
	}
}

// Proves ambient IDs, malformed/ambiguous stdin, missing --as and another
// identity's invocation never write an owner, with a valid adjacent Start.
func TestMonitorOwnerHookRefusals(t *testing.T) {
	for _, runtime := range []string{"muse", "claude", "codex"} {
		for _, mode := range []string{"env", "bad-shape", "missing-event", "wrong-event", "duplicate-session", "case-session", "unicode-session", "duplicate-unicode-session", "unicode-event", "identity", "implicit-identity", "human", "canceled"} {
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				e := ownerCLI(t)
				e.extra[EnvSessionID] = "ambient"
				e.extra["CLAUDE_CODE_SESSION_ID"] = "ambient"
				e.extra["CODEX_THREAD_ID"] = "ambient"
				e.stdin = `{"session_id":"native","hook_event_name":"SessionStart"}`
				switch mode {
				case "env":
					e.stdin = `{"hook_event_name":"SessionStart"}`
				case "bad-shape":
					e.stdin = `{"session_id":"bad/session","hook_event_name":"SessionStart"}`
				case "missing-event":
					e.stdin = `{"session_id":"native"}`
				case "wrong-event":
					e.stdin = `{"session_id":"native","hook_event_name":"Stop"}`
				case "duplicate-session":
					e.stdin = `{"session_id":"forged","session_id":"native","hook_event_name":"SessionStart"}`
				case "case-session":
					e.stdin = `{"Session_ID":"native","hook_event_name":"SessionStart"}`
				case "unicode-session":
					e.stdin = `{"ſession_id":"native","hook_event_name":"SessionStart"}`
				case "duplicate-unicode-session":
					e.stdin = `{"session_id":"forged","ſession_id":"native","hook_event_name":"SessionStart"}`
				case "unicode-event":
					e.stdin = `{"session_id":"native","hook_event_name":"Stop","hooK_event_name":"SessionStart"}`
				case "identity":
					e.extra[config.EnvAgent] = "bob"
				}
				switch mode {
				case "implicit-identity":
					e.run("hook", "session-start", "--runtime", runtime)
				case "human":
					e.run("--human", "--as", "alice", "hook", "session-start", "--runtime", runtime)
				case "canceled":
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					e.runCtx(ctx, "--as", "alice", "hook", "session-start", "--runtime", runtime)
				default:
					e.as("alice", "hook", "session-start", "--runtime", runtime)
				}
				if e.state("alice").MonitorOwner != nil || e.state("bob").MonitorOwner != nil {
					t.Fatal("unadmitted owner persisted")
				}
				e.extra[config.EnvAgent] = "alice"
				e.stdin = `{"session_id":"native","hook_event_name":"SessionStart"}`
				e.as("alice", "hook", "session-start", "--runtime", runtime).ok(t)
				if o := e.state("alice").MonitorOwner; o == nil || o.SessionIDHash != cliOwnerHash("native") {
					t.Fatal("valid neighbour not admitted", o)
				}
			})
		}
	}
}

// Proves the actual sink seam and status fail closed at TTL expiry, leaving
// durable mail and the existing lease untouched; a later Start re-admits only
// the association. This does not enable a live monitor or Courier path.
func TestMonitorOwnerExpiryKeepsPending(t *testing.T) {
	e, _, s := cliMonitorFixture(t)
	mb := e.mailbox("alice")
	if _, err := mb.Append([]config.MailEntry{{ID: "gap:owner", Kind: config.MailGap, Label: "#fixture", From: "before", To: "after"}}); err != nil {
		t.Fatal(err)
	}
	err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, cliMonitorRecord(), func(g *config.MonitorGeneration) error {
		writes := 0
		sink, err := admitMonitorSink(g, cliMonitorRecord().Generation, func() error { writes++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.announce(); err != nil || writes != 1 {
			t.Fatal("valid sink neighbour", err)
		}
		lease := *e.state("alice").Lease
		if _, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
			st.MonitorConfig = &config.MonitorConfig{OwnerTTL: time.Nanosecond}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := sink.announce(); !errors.Is(err, config.ErrMonitorOwner) || writes != 1 {
			t.Fatal("expired sink emitted", err, writes)
		}
		if active, reason := g.Status(); active || reason != "owner_unknown" {
			t.Fatal("expired status", active, reason)
		}
		if err := g.RenewOwnerLease(cliMonitorRecord().Generation); !errors.Is(err, config.ErrMonitorGeneration) {
			t.Fatal("expired renewal", err)
		}
		if len(e.mailbox("alice").Pending()) != 1 || !reflect.DeepEqual(&lease, e.state("alice").Lease) {
			t.Fatal("expiry changed pending/lease")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A normal hook after expiry is insufficient; the next Start is the only
	// re-admission path. It does not reactivate the invalidated generation.
	if err := config.RecordMonitorOwnerHook(s.paths, s.name, time.Now().Add(time.Second), "muse", "monitor-session", "UserPromptSubmit", false, time.Now()); !errors.Is(err, config.ErrMonitorOwner) {
		t.Fatal("ordinary hook re-admitted expired owner", err)
	}
	if err := config.RecordMonitorOwnerHook(s.paths, s.name, time.Now().Add(time.Second), "muse", "replacement", "SessionStart", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if e.state("alice").MonitorOwner.SessionIDHash != cliOwnerHash("replacement") {
		t.Fatal("Start did not re-admit")
	}
}

// Proves a canceled automatic watch cannot turn ambient IDs into owner proof,
// alongside the actual admitted hook writer; sweep is not a CLI command here.
func TestMonitorOwnerWatchNeverAdmits(t *testing.T) {
	e := ownerCLI(t)
	e.extra[EnvSessionID] = "ambient"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.runCtx(ctx, "--as", "alice", "watch", "--room", "proj")
	if e.state("alice").MonitorOwner != nil {
		t.Fatal("watch admitted owner")
	}
	e.stdin = `{"session_id":"native","hook_event_name":"SessionStart"}`
	e.as("alice", "hook", "session-start", "--runtime", "muse").ok(t)
	if o := e.state("alice").MonitorOwner; o == nil || !strings.EqualFold(o.SessionIDHash, cliOwnerHash("native")) {
		t.Fatal("valid neighbour", o)
	}
}

// Equal/backwards Start must fail at the CLI, never claim an admission that
// silently leaves a different (or indistinguishable same-session) owner.
func TestMonitorOwnerHookNonIncreasingStart(t *testing.T) {
	for _, runtime := range []string{"muse", "claude", "codex"} {
		for _, delta := range []time.Duration{0, -time.Hour} {
			t.Run(runtime+"/"+delta.String(), func(t *testing.T) {
				e := ownerCLI(t)
				now := time.Now().UTC()
				e.now = func() time.Time { return now }
				e.stdin = `{"session_id":"native-A","hook_event_name":"SessionStart"}`
				e.as("alice", "hook", "session-start", "--runtime", runtime).ok(t)
				before := e.state("alice").MonitorOwner
				now = now.Add(delta)
				e.stdin = `{"session_id":"native-B","hook_event_name":"SessionStart"}`
				r := e.as("alice", "hook", "session-start", "--runtime", runtime)
				diagnostic := "owner_unknown"
				if runtime == "muse" {
					diagnostic = "delivery_error"
				}
				// Hooks intentionally exit zero; a closed diagnostic is their failure signal.
				if !strings.Contains(r.stderr, diagnostic) || !reflect.DeepEqual(before, e.state("alice").MonitorOwner) {
					t.Fatal("ambiguous Start silently accepted", r)
				}
			})
		}
	}
}

// The shim deliberately lets non-Start owner refusals, including wrapped
// malformed owner State, reach legacy delivery. It never repairs the bytes;
// Start still aborts. Delivery's own State checks remain in force.
func TestMonitorOwnerHookWrappedRefusal(t *testing.T) {
	e := ownerCLI(t)
	path := e.paths().StateFile("alice")
	bad := []byte(`{"monitor_owner":{}}`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	in := hookInput{SessionID: "native", HookEventName: "UserPromptSubmit", Payload: []byte(`{"session_id":"native","hook_event_name":"UserPromptSubmit"}`)}
	a := eApp(e)
	a.as = "alice"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.recordHookOwner(ctx, hookRuntime{name: "muse"}, hookEvent{arg: "prompt", name: "UserPromptSubmit"}, in); err != nil {
		t.Fatal("wrapped non-Start refusal did not fall through", err)
	}
	in.HookEventName = "SessionStart"
	in.Payload = []byte(`{"session_id":"native","hook_event_name":"SessionStart"}`)
	if err := a.recordHookOwner(ctx, hookRuntime{name: "muse"}, hookEvent{arg: "session-start", name: "SessionStart"}, in); !errors.Is(err, config.ErrMonitorOwner) {
		t.Fatal("malformed Start did not abort", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(bad, after) {
		t.Fatal("shim repaired malformed State", err)
	}
	e.stdin = `{"session_id":"native","hook_event_name":"UserPromptSubmit"}`
	if r := e.as("alice", "hook", "prompt", "--runtime", "muse"); !strings.Contains(r.stderr, "delivery_error") || r.stdout != "" {
		t.Fatal("legacy delivery ignored malformed State")
	}
}

// A successful native bind, terminal bind refusal and unsupported sweep entry
// points cannot manufacture or refresh owner evidence from ambient/session IDs.
// There is no production sweep command; its negative CLI admission is explicit.
func TestMonitorOwnerBindAndSweepNeverWrite(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("admitted=%t", admitted), func(t *testing.T) {
			e, _, n, _ := runtimeWorld(t)
			canonical, err := filepath.EvalSymlinks(e.dir)
			e.dir = canonical
			if err != nil {
				t.Fatal(err)
			}
			if admitted {
				if err := config.RecordMonitorOwnerHook(e.paths(), "alice", time.Now().Add(time.Second), "muse", "native", "SessionStart", false, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			before := e.state("alice").MonitorOwner
			e.as("alice", append([]string{"runtime", "bind"}, runtimeStatusBindArgs(n)...)...).ok(t)
			if !reflect.DeepEqual(before, e.state("alice").MonitorOwner) {
				t.Fatal("native bind wrote owner")
			}
			e.extra[EnvSessionID] = "ambient"
			for _, args := range [][]string{{"bind", "--runtime", "muse", "--session-id", "ambient"}, {"sweep"}, {"hook", "sweep", "--runtime", "muse"}} {
				e.stdin = `{"session_id":"ambient","hook_event_name":"SessionStart"}`
				r := e.as("alice", args...)
				if (r.code == 0 && r.stderr == "") || !reflect.DeepEqual(before, e.state("alice").MonitorOwner) {
					t.Fatal("unsupported/refused invocation admitted owner", args, r)
				}
			}
		})
	}
}
