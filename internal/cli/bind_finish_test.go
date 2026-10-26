package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Same-image one-shot commands remain valid; a different image or a parallel
// waiter has the exact typed refusal. Selector parsing matches global --as.
func TestBindingProcessConflict(t *testing.T) {
	for _, c := range []struct {
		image, cmd string
		want       error
	}{{"current", "inbox", nil}, {"current", "post", nil}, {"old", "inbox", ErrBindingOldImage}, {"old", "wait", ErrBindingOldImage}, {"current", "wait", ErrBindingParallelWait}, {"current", "watch", ErrBindingParallelWait}} {
		got := bindingProcessConflict("current", c.image, c.cmd, 7)
		if !errors.Is(got, c.want) {
			t.Fatalf("%s/%s: %v", c.image, c.cmd, got)
		}
	}
	for _, args := range [][]string{{"a2a", "--as", "mine", "wait"}, {"a2a", "wait", "--as=mine"}, {"a2a", "-as=mine", "wait"}} {
		id, root, cmd := bindingSelectors(args, []string{"A2A_AGENT=other", "A2A_CONFIG_DIR=/synthetic/state", "SECRET=must-not-survive"})
		if id != "mine" || root != "/synthetic/state" || cmd != "wait" {
			t.Fatal("selector mismatch")
		}
	}
}

// The actual OS scanner catches a separately loaded old image; a different
// identity or config root is a positive control. Only synthetic argv/env used.
func TestBindingProcessImageWitness(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "a2a-old-fixture")
	if err = os.WriteFile(exe, raw, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBindingProcessWitnessChild$")
	cmd.Env = append(os.Environ(), "A2A_BIND_WITNESS=1", "A2A_AGENT=synthetic-guard", "A2A_CONFIG_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := make([]byte, 1)
	if _, err = io.ReadFull(out, ready); err != nil {
		t.Fatal(err)
	}
	a := NewApp()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err = a.guardBindingProcesses(ctx, "synthetic-guard", dir); !errors.Is(err, ErrBindingOldImage) {
		t.Fatalf("old image not refused: %v", err)
	}
	if err = a.guardBindingProcesses(ctx, "other-identity", dir); err != nil {
		t.Fatalf("foreign identity blocked: %v", err)
	}
	if err = a.guardBindingProcesses(ctx, "synthetic-guard", dir+"-other"); err != nil {
		t.Fatalf("foreign root blocked: %v", err)
	}
}
func TestBindingProcessWitnessChild(t *testing.T) {
	if os.Getenv("A2A_BIND_WITNESS") != "1" {
		return
	}
	os.Stdout.Write([]byte{'R'})
	time.Sleep(20 * time.Second)
	os.Exit(0)
}

// A live binding refuses BOTH public wait paths without receipts or state loss;
// retirement allows the nearby valid wait to complete normally.
func TestBoundWaitBothDirections(t *testing.T) {
	e, room := world(t)
	e.extra[EnvSessionID] = "own"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	id := runtimeMail(e, room, "SYNTHETIC_PENDING")
	// Seed an unshown pending event: the refusal must preserve mailbox bytes,
	// not merely leave an already-empty mailbox unreceived.
	e.hs.mu.Lock()
	raw, marshalErr := json.Marshal(e.hs.events[len(e.hs.events)-1])
	e.hs.mu.Unlock()
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if _, err := e.mailbox("alice").Append([]config.MailEntry{{ID: id, Kind: config.MailEvent, Room: room, Rel: "to", Event: raw}}); err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "owned", SessionID: "own", DaemonPID: os.Getpid(), DaemonBirth: birth}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(e.state("alice"))
	mailBefore := readFile(t, e.paths().MailboxFile("alice"))
	requests := len(e.hs.Calls())
	for _, command := range []string{"wait", "watch"} {
		r := e.as("alice", command).failsWith(t, ExitError, "same identity has a live binding")
		if !strings.Contains(r.stderr, "a2a bind status") || !strings.Contains(r.stderr, "a2a inbox") || strings.Contains(r.stderr, "stop own waiter") || strings.Contains(r.stderr, "unbind") || strings.Contains(r.stderr, "kill") {
			t.Fatalf("wrong recovery guidance: %s", r.stderr)
		}
		after, _ := json.Marshal(e.state("alice"))
		if string(before) != string(after) || readFile(t, e.paths().MailboxFile("alice")) != mailBefore || len(e.hs.Calls()) != requests || r.stdout != "" {
			t.Fatal("refused consumer changed state, sent traffic or printed mail")
		}
	}
	if e.received("alice", id) || e.state("alice").Terminal.Generation != "owned" {
		t.Fatal("refusal changed mail or generation")
	}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.Stopped = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	valid := e.as("alice", "wait", "--timeout", "1ms").ok(t)
	if !strings.Contains(valid.stdout, id) {
		t.Fatal("retired binding still blocked pending mail")
	}
}

// Nearest runtime ancestry, own UID and a bounded parent chain determine the
// owner. Shell parent, another UID, sibling runtime and parent cycles refuse.
func TestHookRuntimeAncestor(t *testing.T) {
	rows := []bindingProcess{{100, 1, 11, "/vendor/bin/codex"}, {101, 100, 11, "/bin/sh"}, {102, 1, 11, "/bin/claude"}}
	if pid, err := runtimeOwnerPID(rows, 101, "codex", 11); err != nil || pid != 100 {
		t.Fatalf("nearest runtime: %d %v", pid, err)
	}
	for _, runtime := range []string{"claude", "unknown"} {
		if _, err := runtimeOwnerPID(rows, 101, runtime, 11); err == nil {
			t.Fatal("non-ancestor runtime admitted")
		}
	}
	rows[0].UID = 12
	if _, err := runtimeOwnerPID(rows, 101, "codex", 11); err == nil {
		t.Fatal("foreign UID admitted")
	}
	rows[0].UID = 11
	rows[1].PPID = 101
	if _, err := runtimeOwnerPID(rows, 101, "codex", 11); err == nil {
		t.Fatal("parent cycle admitted")
	}
}

// The handover transaction preserves journals/stopped state for an exact
// target; changed owner/pane/socket or another lease cannot partially write.
// Runtime ancestry/UID validation is exercised by the public hook fixture.
func TestTerminalSessionHandover(t *testing.T) {
	for _, name := range []string{"valid", "stopped", "owner", "pane", "socket", "foreign-lease"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			target := tmuxwake.Target{OwnerPID: 123, OwnerBirth: "own-birth", Pane: "%1", SocketID: "own-socket", Runtime: "claude"}
			b := config.TerminalBinding{Generation: "generation", SessionID: "old", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, Stopped: name == "stopped", Reservations: []time.Time{time.Now()}, AttemptIDs: []string{"pending-id"}}
			b.ClaudeRegistryRoot = "/synthetic/previous-root"
			b.Readiness = &config.TerminalReadiness{Reason: tmuxwake.RuntimeBusy}
			b.Idle = &config.TerminalIdle{ReadinessRoute: tmuxwake.RegistryNamed}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &b
				st.Lease = &config.Lease{Holder: "old", Host: machineLabel(), Expires: time.Now().Add(time.Minute)}
				if name == "foreign-lease" {
					st.Lease.Holder = "third-session"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := NewApp()
			a.as = "alice"
			a.Getenv = func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return ""
			}
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "owner":
				target.OwnerPID++
			case "pane":
				target.Pane = "%2"
			case "socket":
				target.SocketID = "foreign-socket"
			}
			before, _ := json.Marshal(e.state("alice"))
			_, err = a.handoverTerminalSession(s, &b, target, "new")
			st := e.state("alice")
			if name == "valid" || name == "stopped" {
				// A session's provider root and qualification never cross handover.
				if st.Terminal.ClaudeRegistryRoot != "" || st.Terminal.Readiness != nil || st.Terminal.Idle != nil {
					t.Fatal("handover retained provider authority")
				}
				if err != nil || st.Lease.Holder != "new" || st.Terminal.SessionID != "new" || st.Terminal.Stopped != b.Stopped || len(st.Terminal.Reservations) != 1 || len(st.Terminal.AttemptIDs) != 1 {
					t.Fatal("valid handover lost state", err)
				}
			} else {
				after, _ := json.Marshal(st)
				if err == nil || string(before) != string(after) {
					t.Fatal("foreign handover admitted or changed state", err)
				}
			}
		})
	}
}
