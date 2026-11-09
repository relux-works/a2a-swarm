package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
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
	t.Chdir(t.TempDir())
	e := newEnv(t)
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
