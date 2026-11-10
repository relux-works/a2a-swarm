package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// This inert private Unix socket is never accepted/dialed. The executable is a
// read-only command fixture, not a real tmux server, pane or provider.
func usabilityTarget(t *testing.T, runtime string) (tmuxwake.Target, string, string) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	id, err := tmuxwake.SocketID(socket)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, "trace")
	screen := filepath.Join(dir, "screen")
	if err = os.WriteFile(screen, []byte("PRIVATE_UNRECOGNIZED_SCREEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Paths are TempDir generated; shell quoting also covers arbitrary parent paths.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*' capture-pane '*) printf 'capture\\n' >> %s; cat %s;;\n*' display-message '*) printf 'inspect\\n' >> %s; printf '%%s\\n' '%%0 %d 0 2 0 120';;\n*) printf 'FORBIDDEN\\n' >> %s; exit 1;;\nesac\n", quote(trace), quote(screen), quote(trace), os.Getpid(), quote(trace))
	if err = os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tmuxwake.Target{Socket: socket, SocketID: id, Pane: "%0", PanePID: os.Getpid(), PaneBirth: birth, OwnerPID: os.Getpid(), OwnerBirth: birth, Runtime: runtime}, trace, dir
}

// A refused registry prevents capture: status says not_observed in both formats.
// An idle registry really captures the same invalid layout and retains its
// layout_unrecognized diagnosis. Neither read changes authority or stored data.
func TestBindUsabilityStatusObservation(t *testing.T) {
	target, trace, dir := usabilityTarget(t, "claude")
	e := newEnv(t)
	e.agent("alice")
	root := filepath.Join(dir, "registry")
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	domain, start := localClaudeRegistryFields(t, os.Getpid())
	registry := map[string]any{"cwd": "/synthetic", "entrypoint": "cli", "kind": "interactive", "messagingSocketPath": "/synthetic/not-opened", "name": "synthetic", "nameSince": 1, "nameSource": "user", "peerFeatures": []string{"notify_idle"}, "peerProtocol": 1, "pid": os.Getpid(), "pidDomain": domain, "procStart": start, "sessionId": "owned", "startedAt": 1, "status": "busy", "statusUpdatedAt": 100, "tmux": "synthetic:@0.%0", "updatedAt": 100, "version": "2.1.310"}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "own", SessionID: "owned", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, ClaudeRegistryRoot: root}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"busy", "idle"} {
		for _, format := range []string{"text", "json"} {
			t.Run(status+"/"+format, func(t *testing.T) {
				registry["status"] = status
				raw, _ := json.Marshal(registry)
				if err := os.WriteFile(filepath.Join(root, "sessions", strconv.Itoa(os.Getpid())+".json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(trace, nil, 0600); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(e.paths().StateFile("alice"))
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"bind", "status"}
				if format == "json" {
					args = append(args, "--json")
				}
				got := e.as("alice", args...).ok(t)
				want := "not_observed"
				if status == "idle" {
					want = "layout_unrecognized"
				}
				if format == "json" {
					var x struct{ Input tmuxwake.InputAssessment }
					if err := json.Unmarshal([]byte(got.stdout), &x); err != nil {
						t.Fatal(err)
					}
					if x.Input.State != tmuxwake.InputUnavailable || string(x.Input.Reason) != want {
						t.Fatalf("input=%+v want unavailable/%s", x.Input, want)
					}
				} else if !strings.Contains(got.stdout, "input=unavailable reason="+want+"\n") {
					t.Fatalf("missing diagnostic %s: %s", want, got.stdout)
				}
				ops, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				captures := strings.Count(string(ops), "capture\n")
				if status == "busy" && captures != 0 || status == "idle" && captures != 1 || strings.Contains(string(ops), "FORBIDDEN") {
					t.Fatalf("capture operations=%q", ops)
				}
				if strings.Contains(got.stdout, "PRIVATE_") {
					t.Fatal("private capture leaked")
				}
				after, _ := os.ReadFile(e.paths().StateFile("alice"))
				if !bytes.Equal(before, after) {
					t.Fatal("status mutated state")
				}
			})
		}
	}
}

// Valid native Claude fallback reaches the ordinary own-target gate; bad or
// conflicting identities fail earlier. No test admits a bind past the process
// scanner or starts a consumer. Explicit A2A and Codex precedence stay intact.
func TestBindUsabilitySessionAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, a2a, codex, claude, explicit string
		flags                                       []string
		want                                        string
	}{
		{name: "claude-default", runtime: "claude", claude: "own", want: "own tmux pane/socket"},
		{name: "claude-explicit-policy", runtime: "claude", claude: "own", flags: []string{"--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "6"}, want: "own tmux pane/socket"},
		{name: "a2a-precedence", runtime: "claude", a2a: "own", claude: "foreign", want: "own tmux pane/socket"},
		{name: "explicit-matches-native", runtime: "claude", claude: "own", explicit: "own", want: "own tmux pane/socket"},
		{name: "explicit-foreign", runtime: "claude", claude: "own", explicit: "foreign", want: "another live session"},
		{name: "a2a-foreign-explicit", runtime: "claude", a2a: "own", explicit: "foreign", want: "another live session"},
		{name: "malformed-native", runtime: "claude", claude: "../foreign", want: "bind requires"},
		{name: "malformed-a2a-no-fallback", runtime: "claude", a2a: "../bad", claude: "own", want: "bind requires"},
		{name: "codex-native", runtime: "codex", codex: "own", claude: "ignored", want: "own tmux pane/socket"},
		{name: "codex-not-claude", runtime: "codex", claude: "own", want: "bind requires"},
		{name: "codex-foreign", runtime: "codex", codex: "own", explicit: "foreign", want: "another live Codex session"},
		{name: "codex-runtime-mismatch", runtime: "claude", codex: "own", want: "another live Codex session"},
		{name: "zero-interval", runtime: "claude", claude: "own", flags: []string{"--interval", "0s"}, want: "bind requires"},
		{name: "long-interval", runtime: "claude", claude: "own", flags: []string{"--interval", "61s"}, want: "bind requires"},
		{name: "retry-short", runtime: "claude", claude: "own", flags: []string{"--retry-max", "1s"}, want: "bind requires"},
		{name: "retry-long", runtime: "claude", claude: "own", flags: []string{"--retry-max", "301s"}, want: "bind requires"},
		{name: "zero-rate", runtime: "claude", claude: "own", flags: []string{"--max-wakes-per-hour", "0"}, want: "bind requires"},
		{name: "large-rate", runtime: "claude", claude: "own", flags: []string{"--max-wakes-per-hour", "3601"}, want: "bind requires"},
		{name: "parse-error", runtime: "claude", claude: "own", flags: []string{"--interval", "invalid"}, want: "invalid value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			e.extra[EnvSessionID] = tc.a2a
			e.extra["CODEX_THREAD_ID"] = tc.codex
			e.extra["CLAUDE_CODE_SESSION_ID"] = tc.claude
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "retained", SessionID: "own", Host: machineLabel()}
				st.Lease = &config.Lease{Holder: "own", Host: machineLabel(), Expires: time.Now().Add(time.Hour)}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = mb.Append([]config.MailEntry{{ID: "$retained", Kind: config.MailEvent}}); err != nil {
				t.Fatal(err)
			}
			mail, err := os.ReadFile(e.paths().MailboxFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			checkEffects := usabilityNoProcessCommands(t)
			state, _ := os.ReadFile(e.paths().StateFile("alice"))
			calls := len(e.hs.Calls())
			args := []string{"bind", "--runtime", tc.runtime, "--owner-pid", "2"}
			if tc.explicit != "" {
				args = append(args, "--session-id", tc.explicit)
			}
			args = append(args, tc.flags...)
			e.as("alice", args...).failsWith(t, ExitUsage, tc.want)
			checkEffects()
			afterMail, err := os.ReadFile(e.paths().MailboxFile("alice"))
			if err != nil || !bytes.Equal(mail, afterMail) {
				t.Fatal("refused bind changed mailbox", err)
			}
			after, _ := os.ReadFile(e.paths().StateFile("alice"))
			if !bytes.Equal(state, after) || len(e.hs.Calls()) != calls {
				t.Fatal("refused bind changed state/lease or contacted server")
			}
		})
	}
}

// Unbind's valid fallback retires only its own synthetic no-daemon binding.
// Foreign/malformed/host/session mismatches preserve exact state and mailbox;
// refusals never reach process signalling (daemon PID is this test's own PID).
func TestBindUsabilityUnbind(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, a2a, codex, claude, holder, host string
		ok                                              bool
	}{
		{name: "native-claude", runtime: "claude", claude: "own", holder: "own", ok: true},
		{name: "native-codex", runtime: "codex", codex: "own", claude: "ignored", holder: "own", ok: true},
		{name: "explicit-a2a", runtime: "claude", a2a: "own", claude: "foreign", holder: "own", ok: true},
		{name: "foreign-claude", runtime: "claude", claude: "foreign", holder: "own"},
		{name: "malformed", runtime: "claude", claude: "../bad", holder: "own"},
		{name: "malformed-even-if-stored", runtime: "claude", claude: "../bad", holder: "../bad"},
		{name: "invalid-a2a-no-fallback", runtime: "claude", a2a: "../bad", claude: "own", holder: "own"},
		{name: "foreign-explicit", runtime: "claude", a2a: "foreign", claude: "own", holder: "own"},
		{name: "wrong-runtime", runtime: "codex", claude: "own", holder: "own"},
		{name: "foreign-host", runtime: "claude", claude: "own", holder: "own", host: "foreign-host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			e.extra[EnvSessionID] = tc.holder
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			id := runtimeMail(e, room, "SYNTHETIC_MAIL")
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			host := machineLabel()
			if tc.host != "" {
				host = tc.host
			}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "own-gen", SessionID: tc.holder, Host: host, Target: tmuxwake.Target{Runtime: tc.runtime}}
				if !tc.ok {
					st.Terminal.DaemonPID = os.Getpid()
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before := e.state("alice")
			mail, _ := os.ReadFile(e.paths().MailboxFile("alice"))
			checkEffects := usabilityNoProcessCommands(t)
			e.extra[EnvSessionID] = tc.a2a
			e.extra["CODEX_THREAD_ID"] = tc.codex
			e.extra["CLAUDE_CODE_SESSION_ID"] = tc.claude
			if tc.ok {
				e.as("alice", "unbind").ok(t)
			} else {
				e.as("alice", "unbind").failsWith(t, ExitError, "another session's binding")
			}
			checkEffects()
			after := e.state("alice")
			if tc.ok {
				if !after.Terminal.Stopped || after.Terminal.Outcome != "unbound" {
					t.Fatal("own binding not retired")
				}
				before.Terminal.Stopped = true
				before.Terminal.Idle = nil
				before.Terminal.Outcome = "unbound"
				before, after = typedStateWithoutWriter(before), typedStateWithoutWriter(after)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unexpected state/lease mutation")
			}
			afterMail, _ := os.ReadFile(e.paths().MailboxFile("alice"))
			if !bytes.Equal(mail, afterMail) || e.received("alice", id) {
				t.Fatal("mail changed or receipted")
			}
		})
	}
}

// Native CLI commands refresh the same Claude lease as hooks. Other holders
// are refused by the real Acquire path; failed acquisition leaves disk intact.
func TestBindUsabilityLeaseFallback(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.extra["CLAUDE_CODE_SESSION_ID"] = "own"
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: time.Now, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.acquireLease(s, "own", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err = a.acquireLease(s, "", time.Time{}); err != nil {
		t.Fatalf("native CLI did not share hook lease: %v", err)
	}
	before, _ := os.ReadFile(e.paths().StateFile("alice"))
	e.extra["CLAUDE_CODE_SESSION_ID"] = "foreign"
	var held *config.LeaseHeldError
	if err = a.acquireLease(s, "", time.Time{}); !errors.As(err, &held) {
		t.Fatalf("foreign lease result=%v", err)
	}
	after, _ := os.ReadFile(e.paths().StateFile("alice"))
	if !bytes.Equal(before, after) {
		t.Fatal("foreign lease mutated state")
	}
}

// A real failed consumer cycle emits one closed cause and no underlying error
// value. Cancellation at that line ends the isolated loop without dispatch or
// spending a wake. This does not test the long-running alarm/refresh workers.
func TestBindUsabilityRetryLog(t *testing.T) {
	for _, stage := range []string{"state", "connection", "lease", "sync"} {
		t.Run(stage, func(t *testing.T) {
			target, trace, _ := usabilityTarget(t, "codex")
			e := newEnv(t)
			e.agent("alice")
			initial := config.TerminalBinding{Generation: "own", SessionID: "own", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 6}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &initial
				st.NextBatch = "s0"
				if stage == "lease" {
					st.Lease = &config.Lease{Holder: "foreign", Host: machineLabel(), Expires: time.Now().Add(time.Hour)}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := &App{as: "alice", Getenv: func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return ""
			}, Now: time.Now, Stdout: io.Discard, HTTP: e.hs.srv.Client()}
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			if stage == "state" {
				if err = os.WriteFile(e.paths().StateFile("alice"), []byte(`{"PRIVATE_ERROR_SENTINEL":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "connection" {
				bad := filepath.Join(t.TempDir(), "not-a-socket")
				if err = os.WriteFile(bad, nil, 0600); err != nil {
					t.Fatal(err)
				}
				_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.Target.Socket = bad; return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "sync" {
				e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "PRIVATE_ERROR_SENTINEL/path/token/body")
			}
			beforeLease, _ := config.LoadState(e.paths(), "alice")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			log := &usabilityLog{cancel: cancel}
			a.Stderr = log
			reason, err := a.runTerminal(ctx, s, "own", initial)
			if err != nil || reason != "cancelled" {
				t.Fatalf("loop exit=%s %v", reason, err)
			}
			want := "binding consumer unavailable; bounded retry cause=" + stage + "\n"
			if log.String() != want {
				t.Fatalf("fixed diagnostic=%q want=%q", log.String(), want)
			}
			if stage != "state" {
				st := e.state("alice")
				if len(st.Terminal.Reservations) != 0 || len(st.Terminal.AttemptIDs) != 0 || st.Terminal.Stopped || !reflect.DeepEqual(st.Lease, beforeLease.Lease) && stage == "lease" {
					t.Fatal("retry spent effect or changed foreign lease")
				}
			}
			ops, _ := os.ReadFile(trace)
			if strings.Contains(string(ops), "capture") || strings.Contains(string(ops), "FORBIDDEN") {
				t.Fatal("failed cycle reached input/dispatch effects")
			}
		})
	}
}

type usabilityLog struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *usabilityLog) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if strings.Contains(string(p), "binding consumer unavailable;") {
		w.cancel()
	}
	return n, err
}

// Commands that should refuse before process/target access must not even probe
// a PID. A buggy path can only execute these task-private tripwires, not signal
// a real consumer. The no-daemon positive unbind needs no process operations.
func usabilityNoProcessCommands(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	trace := filepath.Join(dir, "forbidden")
	quoted := "'" + strings.ReplaceAll(trace, "'", "'\"'\"'") + "'"
	for _, tool := range []string{"ps", "tmux"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\nprintf 'forbidden\\n' >> "+quoted+"\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() {
		t.Helper()
		data, err := os.ReadFile(trace)
		if !os.IsNotExist(err) {
			t.Fatalf("forbidden process probe: %q (%v)", data, err)
		}
	}
}
