//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/daemonsvc"
)

// W-FRESH/C-RESTART: only the newest SessionStart can admit a monitor; ending
// the old session cannot disarm the new one. A fresh handler after a simulated
// crash has no owner even if disk evidence or the caller repeats the old ID.
// CM's disk/lease tests independently cover actual owner renewal and disarm.
func TestServiceFreshOwnerGate(t *testing.T) {
	policy := daemonsvc.Policy{Owner: "@ivan:fixture.invalid", Identities: []string{"alice"}}
	calls := 0
	next := func(context.Context, daemon.Invocation) int { calls++; return 0 }
	epoch := newDaemonOwnerEpoch()
	h := freshServiceHandler(policy, epoch, next)
	activeEpoch := epoch
	run := func(handler daemon.Handler, id, session, event string, argv ...string) int {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"session_id": session, "hook_event_name": event})
		if event != "" {
			if e := activeEpoch.hook(context.Background(), id, session, event, false, func() error { return nil }); e != nil {
				t.Fatal(e)
			}
		}
		return handler(context.Background(), daemon.Invocation{Identity: id, Request: daemon.Request{Argv: argv, Session: session, Stdin: b}, Stderr: io.Discard, Refuse: func(class daemon.Diagnostic) int {
			if class != daemon.NotAllowed {
				t.Fatal("wrong refusal", class)
			}
			return 1
		}})
	}
	watch := []string{"watch", "--notices"}
	if run(h, "alice", "old", "", watch...) != 1 || calls != 0 {
		t.Fatal("owner resurrected")
	}
	if run(h, "alice", "old", "SessionStart", "hook", "session-start", "--runtime", "muse") != 0 || run(h, "alice", "old", "", watch...) != 0 {
		t.Fatal("valid old neighbour")
	}
	if run(h, "alice", "new", "SessionStart", "hook", "session-start", "--runtime", "muse") != 0 {
		t.Fatal("new Start")
	}
	before := calls
	if run(h, "alice", "old", "", watch...) != 1 || calls != before {
		t.Fatal("old served")
	}
	if run(h, "alice", "old", "SessionEnd", "hook", "session-end", "--runtime", "muse") != 0 || run(h, "alice", "new", "", watch...) != 0 {
		t.Fatal("old End disarmed new")
	}
	restartEpoch := newDaemonOwnerEpoch()
	restart := freshServiceHandler(policy, restartEpoch, next)
	before = calls
	if run(restart, "alice", "new", "", watch...) != 1 || calls != before {
		t.Fatal("restart renewed without fresh hook")
	}
	activeEpoch = restartEpoch
	if run(restart, "alice", "new", "SessionStart", "hook", "session-start", "--runtime", "muse") != 0 || run(restart, "alice", "new", "", watch...) != 0 {
		t.Fatal("restart positive")
	}
	activeEpoch = epoch
	before = calls
	if run(h, "mallory", "new", "", "inbox") != 1 || calls != before {
		t.Fatal("allowed set writable by request")
	}
	if run(h, "alice", "new", "SessionEnd", "hook", "session-end", "--runtime", "muse") != 0 {
		t.Fatal("End")
	}
	before = calls
	if run(h, "alice", "new", "", watch...) != 1 || calls != before {
		t.Fatal("ended owner renewed")
	}
}

// W-AUTHORITY/C-LOCAL-PIN: immutable policy is propagated through named/local
// path resolution and overrides a daemon-owned mutable owner file. Local agent
// commands retain their previous own pin behaviour.
func TestServiceOwnerOverridesMutablePin(t *testing.T) {
	base := t.TempDir()
	p := config.Paths{Base: base}
	f := ownerFiles(p, "alice")
	os.MkdirAll(filepath.Dir(f.path("pin")), 0700)
	if e := os.WriteFile(f.path("pin"), []byte(`{"mode":"host","owner":"@mallory:fixture.invalid"}`), 0600); e != nil {
		t.Fatal(e)
	}
	app := &App{as: "alice", daemonEngine: true, daemonOwner: "@ivan:fixture.invalid", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return base
		}
		return ""
	}}
	resolved, e := app.paths()
	if e != nil {
		t.Fatal(e)
	}
	pin := ownerFiles(resolved, "alice").pin()
	principals, e := ownerFiles(resolved, "alice").principals()
	if e != nil || pin.Owner != app.daemonOwner || len(principals) != 1 || principals[0] != app.daemonOwner {
		t.Fatal("mutable daemon pin overrode root snapshot")
	}
	if f.pin().Owner != "@mallory:fixture.invalid" {
		t.Fatal("local pin changed")
	}
}

// W-CLI/C-ROOT: service commands reject missing authority/invalid argv locally
// with credential and HTTP traps. Tests never call launchctl or systemctl.
func TestServiceCLIRefusals(t *testing.T) {
	for _, args := range [][]string{{"service"}, {"service", "install"}, {"service", "status", "--token", "secret-control"}, {"prepare-run-dir", "--home", "relative"}, {"sweep"}, {"rescue"}, {"soak"}} {
		var out, diag bytes.Buffer
		a := localDaemonApp(&out, &diag)
		code := a.Run(context.Background(), append([]string{"daemon"}, args...))
		if code == 0 || out.Len() != 0 || bytes.Contains(diag.Bytes(), []byte("secret-control")) {
			t.Fatal("invalid service command leaked or succeeded", args[0])
		}
	}
	for _, argv := range [][]string{{"daemon", "service", "install"}, {"daemon", "prepare-run-dir"}, {"daemon", "sweep"}, {"daemon", "rescue"}, {"daemon", "soak"}} {
		if _, e := daemon.Authorize(argv, "alice"); e == nil {
			t.Fatal("station gained service authority", argv)
		}
	}
}

// W-HOOK-COMMIT/C-FAILOPEN: epoch admission follows the actual committed
// owner-hook boundary. Malformed/legacy hooks and cancelled commits cannot
// turn a fail-open exit into fresh monitor ownership.
func TestServiceEpochRequiresCommittedOwnerHook(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	epoch := newDaemonOwnerEpoch()
	a := &App{as: "alice", daemonEngine: true, daemonOwnerEpoch: epoch, Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return root
		}
		return ""
	}}
	call := func(session, event, payload string, ctx context.Context) error {
		t.Helper()
		return a.recordHookOwner(ctx, hookRuntime{name: "muse"}, hookEvent{arg: "session-start", name: event}, hookInput{SessionID: session, HookEventName: event, Payload: json.RawMessage(payload)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = call("old", "SessionStart", `{"session_id":"old"}`, ctx); e != nil || epoch.owner("alice") != "" {
		t.Fatal("legacy event created epoch", e)
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if e = call("old", "SessionStart", `{"session_id":"old","hook_event_name":"SessionStart"}`, stopped); e == nil || epoch.owner("alice") != "" {
		t.Fatal("cancelled event admitted")
	}
	if e = call("new", "SessionStart", `{"session_id":"new","hook_event_name":"SessionStart"}`, ctx); e != nil || epoch.owner("alice") != "new" {
		t.Fatal("committed positive", e)
	}
	if e = call("old", "SessionStart", `{"session_id":"old","session_id":"old","hook_event_name":"SessionStart"}`, ctx); e != nil || epoch.owner("alice") != "new" {
		t.Fatal("duplicate event changed epoch", e)
	}
}
