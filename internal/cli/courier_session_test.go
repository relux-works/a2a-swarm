//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
)

func courierReceiptCount(t *testing.T, f *courierFixture, name, id string) int {
	t.Helper()
	data, err := os.ReadFile(f.paths[name].MailboxFile(name))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var v struct {
			Op  string
			IDs []string
		}
		if json.Unmarshal(line, &v) != nil {
			t.Fatal("mailbox line")
		}
		if v.Op == "rcpt" {
			for _, got := range v.IDs {
				if got == id {
					n++
				}
			}
		}
	}
	return n
}

func courierSessionRun(f *courierFixture, name, session, input string, args ...string) result {
	var out, diag bytes.Buffer
	a := f.app(name, input, &out, &diag)
	base := a.Getenv
	a.Getenv = func(k string) string {
		if k == EnvSessionID {
			return session
		}
		return base(k)
	}
	code := a.Run(context.Background(), args)
	return result{code: code, stdout: out.String(), stderr: diag.String()}
}

func courierOwnerMonitor(t *testing.T, f *courierFixture, name, owner string, fn func()) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"session_id": owner, "hook_event_name": "SessionStart"})
	f.run(name, string(input), "--as", name, "hook", "session-start", "--runtime", "muse").ok(t)
	st, err := config.LoadState(f.paths[name], name)
	if err != nil || st.Lease == nil || st.Lease.Holder != owner || st.MonitorOwner == nil {
		t.Fatal("owner prerequisite", err)
	}
	record := cliMonitorRecord()
	record.SessionID = owner
	record.Host = st.Lease.Host
	record.OwnerPID = st.Lease.PID
	err = config.WithMonitorGeneration(f.paths[name], name, time.Now().Add(time.Second), config.MonitorArm, record, func(g *config.MonitorGeneration) error {
		if err := g.RenewOwnerLease(record.Generation); err != nil {
			return err
		}
		fn()
		return nil
	})
	if err != nil {
		t.Fatal("monitor prerequisite", err)
	}
}

// Rejecting wrong/absent/cross-identity claims preserves both State and receipt
// evidence. Only the exact owner claim for that identity may borrow its lease.
func TestCourierSessionMonitorIsolation(t *testing.T) {
	f := newCourierFixture(t)
	courierOwnerMonitor(t, f, "alice", "owner-alice", func() {
		courierOwnerMonitor(t, f, "bob", "owner-bob", func() {
			aliceBefore, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			bobBefore, err := os.ReadFile(f.paths["bob"].StateFile("bob"))
			if err != nil {
				t.Fatal(err)
			}
			id := f.envs["bob"].hs.inject(f.rooms["bob"], "@alice:"+testServer, msg("@bob:"+testServer+" pending", nil))
			cross := courierSessionRun(f, "alice", "owner-alice", "", "--as", "bob", "inbox")
			if cross.code != ExitError || cross.stdout != "" || cross.stderr != "a2a: courier: identity_mismatch\n" || f.received("bob", id) {
				t.Fatal("authenticated identity binding bypassed", cross)
			}
			for _, claim := range []string{"wrong-owner", "", "owner-alice"} {
				r := courierSessionRun(f, "bob", claim, "", "--as", "bob", "inbox")
				if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: courier: courier_lease_conflict\n" || f.received("bob", id) {
					t.Fatal("monitor conflict class/effects", r)
				}
				after, _ := os.ReadFile(f.paths["bob"].StateFile("bob"))
				aliceAfter, _ := os.ReadFile(f.paths["alice"].StateFile("alice"))
				if !bytes.Equal(bobBefore, after) || !bytes.Equal(aliceBefore, aliceAfter) {
					t.Fatal("foreign session changed State")
				}
			}
			r := courierSessionRun(f, "bob", "owner-bob", "", "--as", "bob", "inbox")
			if r.code != 0 || !strings.Contains(r.stdout, id) || courierReceiptCount(t, f, "bob", id) != 1 {
				t.Fatal("bob owner neighbor", r)
			}
		})
	})
	// Absence outside a monitor keeps the pre-extension PID-borrow behavior.
	plain := newCourierFixture(t)
	id := plain.envs["alice"].hs.inject(plain.rooms["alice"], "@bob:"+testServer, ping(1))
	r := courierSessionRun(plain, "alice", "", "", "--as", "alice", "inbox")
	if r.code != 0 || !strings.Contains(r.stdout, id) || courierReceiptCount(t, plain, "alice", id) != 1 {
		t.Fatal("absent-session neighbor", r)
	}
}

// Poison the actual daemon process environment, not just the fixture Getenv.
// An ambient fallback would make the absent-owner request succeed and fails here.
func TestCourierSessionNoDaemonAmbient(t *testing.T) {
	for _, key := range []string{EnvSessionID, "CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(key, "owner-alice")
	}
	f := newCourierFixture(t)
	courierOwnerMonitor(t, f, "alice", "owner-alice", func() {
		id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(1))
		r := courierSessionRun(f, "alice", "", "", "--as", "alice", "inbox")
		if r.code != ExitError || r.stderr != "a2a: courier: courier_lease_conflict\n" || r.stdout != "" || f.received("alice", id) {
			t.Fatal("daemon borrowed ambient owner", r)
		}
		r = courierSessionRun(f, "alice", "owner-alice", "", "--as", "alice", "inbox")
		if r.code != 0 || !strings.Contains(r.stdout, id) || courierReceiptCount(t, f, "alice", id) != 1 {
			t.Fatal("explicit owner neighbor", r)
		}
	})
}

// Exercise the actual station Request, including long consumers, with a handler
// that records evidence and finishes immediately. No sleeping watch is needed.
func TestCourierStationSessionPrecedence(t *testing.T) {
	seen := make(chan string, 1)
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		select {
		case seen <- inv.Request.Session:
			return 0
		case <-ctx.Done():
			return 1
		}
	})
	for _, tc := range []struct {
		name        string
		args        []string
		env         map[string]string
		input, want string
	}{
		{"explicit", []string{"inbox"}, map[string]string{EnvSessionID: "explicit", "CODEX_THREAD_ID": "codex", "CLAUDE_CODE_SESSION_ID": "claude"}, "", "explicit"},
		{"codex", []string{"inbox"}, map[string]string{"CODEX_THREAD_ID": "codex", "CLAUDE_CODE_SESSION_ID": "claude"}, "", "codex"},
		{"claude", []string{"inbox"}, map[string]string{"CLAUDE_CODE_SESSION_ID": "claude"}, "", "claude"},
		{"invalid-codex-no-claude-fallback", []string{"inbox"}, map[string]string{"CODEX_THREAD_ID": "not a key", "CLAUDE_CODE_SESSION_ID": "claude"}, "", ""},
		{"watch-ambient", []string{"watch"}, map[string]string{"CODEX_THREAD_ID": "codex", "CLAUDE_CODE_SESSION_ID": "claude"}, "", ""},
		{"watch-explicit", []string{"watch"}, map[string]string{EnvSessionID: "explicit", "CODEX_THREAD_ID": "codex"}, "", "explicit"},
		{"wait-unbounded", []string{"wait", "--timeout=0"}, map[string]string{"CODEX_THREAD_ID": "codex"}, "", ""},
		{"wait-unbounded-explicit", []string{"wait", "--timeout", "0"}, map[string]string{EnvSessionID: "explicit", "CODEX_THREAD_ID": "codex", "CLAUDE_CODE_SESSION_ID": "claude"}, "", "explicit"},
		{"wait-default", []string{"wait"}, map[string]string{"CLAUDE_CODE_SESSION_ID": "claude"}, "", "claude"},
		{"wait-bounded", []string{"wait", "--timeout", "1s"}, map[string]string{"CODEX_THREAD_ID": "codex"}, "", "codex"},
		{"hook-input", []string{"hook", "prompt"}, map[string]string{EnvSessionID: "explicit", "CODEX_THREAD_ID": "codex"}, `{"session_id":"hook-owner"}`, "hook-owner"},
		{"hook-absent", []string{"hook", "prompt"}, map[string]string{EnvSessionID: "explicit"}, `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diag bytes.Buffer
			a := &App{Stdout: &out, Stderr: &diag, Stdin: strings.NewReader(tc.input), Getenv: func(k string) string {
				if k == config.EnvConfigDir {
					return s.home
				}
				return tc.env[k]
			}}
			if code := a.Run(context.Background(), tc.args); code != 0 || diag.Len() != 0 {
				t.Fatal("station session neighbor", code, diag.String())
			}
			select {
			case got := <-seen:
				if got != tc.want {
					t.Fatal("wrong session precedence")
				}
			case <-s.done:
				t.Fatal("daemon stopped")
			case <-time.After(courier.LockReleaseBound):
				t.Fatal("no invocation")
			}
		})
	}
}

// Invalid explicit/hook IDs fail at the station before execution. Empty typed
// Request.Session is absent, but empty-present hook/wire strings are refused.
func TestCourierStationSessionGrammar(t *testing.T) {
	var calls atomic.Int32
	s := newC5Station(t, func(context.Context, courier.Invocation) int { calls.Add(1); return 0 })
	for _, id := range []string{strings.Repeat("x", 129), "a\n", "a\x00", "a b", "pid:1@host", "-", ""} {
		before := calls.Load()
		input, _ := json.Marshal(map[string]string{"session_id": id})
		var out, diag bytes.Buffer
		a := &App{Stdout: &out, Stderr: &diag, Stdin: bytes.NewReader(input), Getenv: func(k string) string {
			if k == config.EnvConfigDir {
				return s.home
			}
			return ""
		}}
		if code := a.Run(context.Background(), []string{"hook", "prompt"}); code != 0 || out.Len() != 0 || calls.Load() != before {
			t.Fatal("invalid hook evidence executed", code)
		}
		log, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
		if err != nil || !bytes.HasSuffix(log, []byte(" UserPromptSubmit not_allowed\n")) {
			t.Fatal("hook grammar closed reason", err)
		}
		if id != "" {
			a.Stdin = nil
			out.Reset()
			diag.Reset()
			a.Getenv = func(k string) string {
				if k == config.EnvConfigDir {
					return s.home
				}
				if k == EnvSessionID {
					return id
				}
				return ""
			}
			if code := a.Run(context.Background(), []string{"inbox"}); code != ExitError || diag.String() != "a2a: courier: not_allowed\n" || calls.Load() != before {
				t.Fatal("explicit grammar refusal", code, diag.String())
			}
		}
		good := s.run(context.Background(), `{"session_id":"owner-1"}`, "hook", "prompt")
		if good.code != 0 || calls.Load() != before+1 {
			t.Fatal("grammar success neighbor", good)
		}
	}
}
