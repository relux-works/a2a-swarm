package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// All values are synthetic except this isolated fixture's own PID/birth.
func dispatchRegistry(t *testing.T, f *orphanDispatchFixture, status string) (string, map[string]any) {
	t.Helper()
	b := f.binding()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(b.OwnerPID), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"cwd": "/synthetic", "entrypoint": "cli", "kind": "interactive", "messagingSocketPath": "/synthetic/not-opened", "name": "synthetic", "nameSince": 1, "nameSource": "user", "peerFeatures": []string{"notify_idle"}, "peerProtocol": 1, "pid": b.OwnerPID, "pidDomain": "darwin", "procStart": strings.TrimSpace(string(out)), "sessionId": b.SessionID, "startedAt": 1, "status": status, "statusUpdatedAt": 100, "tmux": "synthetic:@0." + b.Target.Pane, "updatedAt": 100, "version": "2.1.310"}
	path := filepath.Join(root, "sessions", strconv.Itoa(b.OwnerPID)+".json")
	writeDispatchRegistry(t, path, m)
	f.update(func(st *config.State) { st.Terminal.Target.Runtime = "claude"; st.Terminal.ClaudeRegistryRoot = root })
	return path, m
}
func writeDispatchRegistry(t *testing.T, path string, m map[string]any) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".next", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

// Real dispatch uses one registry/parser assessment for diagnostics and both
// idle samples. Busy never types; named rootless/unqualified never types; old
// qualified legacy geometry retains fallback; matching unknown releases admit
// named geometry. Shell/idle changes restart even
// when the producer's timestamp is identical. Mail is never receipted here.
func TestReadinessDispatch(t *testing.T) {
	for _, mode := range []string{"named-idle", "named-shell", "named-busy", "named-rootless", "named-unqualified", "named-new-version", "legacy-rootless", "status-transition"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			status := "idle"
			if mode == "named-shell" {
				status = "shell"
			}
			if mode == "named-busy" {
				status = "busy"
			}
			path, m := dispatchRegistry(t, f, status)
			if mode == "named-rootless" || mode == "legacy-rootless" {
				f.update(func(st *config.State) { st.Terminal.ClaudeRegistryRoot = "" })
			}
			if mode == "named-unqualified" {
				delete(m, "cwd")
				writeDispatchRegistry(t, path, m)
			}
			if mode == "named-new-version" {
				m["version"] = "3.0.0"
				writeDispatchRegistry(t, path, m)
			}
			f.control(map[string]any{"claude": true, "named": mode != "legacy-rootless"})
			f.dispatch()
			first := f.binding()
			screen := f.wait(func(s orphanScreen) bool { return s.PID > 1 })
			if screen.Text != "" || len(screen.Submitted) != 0 || len(first.Reservations) != 0 {
				t.Fatal("first observation had effects", screen)
			}
			f.now = f.now.Add(time.Second)
			if mode == "status-transition" {
				m["status"] = "shell"
				writeDispatchRegistry(t, path, m)
			}
			f.dispatch()
			blocked := mode == "named-busy" || mode == "named-rootless" || mode == "named-unqualified" || mode == "status-transition"
			if blocked {
				screen = f.wait(func(s orphanScreen) bool { return s.PID > 1 })
				if screen.Text != "" || len(screen.Submitted) != 0 || len(f.binding().Reservations) != 0 {
					t.Fatal("refused route typed", screen)
				}
				if mode == "status-transition" {
					f.step()
					blocked = false
				}
			}
			if !blocked {
				screen = f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
				if screen.Submitted[0] != tmuxwake.Notice(1) || len(f.binding().Rung) != 1 {
					t.Fatal("valid route failed completion", screen)
				}
			}
			if f.e.received("alice", id) {
				t.Fatal("dispatch receipted mail")
			}
		})
	}
}

// Status projects fresh closed readiness but never saves its observation,
// qualification, attempts, or a hostile stored diagnostic string.
func TestReadinessStatusReadOnly(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	_, _ = dispatchRegistry(t, f, "busy")
	f.update(func(st *config.State) {
		st.Terminal.Readiness = &config.TerminalReadiness{Reason: "private-reason", Version: "/private/root", Since: f.now.Add(time.Hour)}
	})
	before, _ := json.Marshal(f.e.state("alice"))
	var out bytes.Buffer
	f.a.Stdout = &out
	if err := f.a.terminalStatus(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(f.e.state("alice"))
	if string(before) != string(after) {
		t.Fatal("status wrote journal")
	}
	var got struct{ Readiness terminalReadinessStatus }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Readiness.Reason != tmuxwake.RuntimeBusy || got.Readiness.State != tmuxwake.RuntimeRefused || !got.Readiness.Since.Equal(f.now) || strings.Contains(out.String(), "private") {
		t.Fatal("status projection", out.String())
	}
}

// Both public status forms expose only bounded safe key names on an unknown
// release; hostile names/values neither inject output nor change admission.
func TestReadinessUnknownKeysStatus(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	path, m := dispatchRegistry(t, f, "idle")
	m["version"] = "3.0.0"
	m["futureField"] = map[string]any{"private": "NEVER-SHOW-VALUE"}
	m["\x1b[31m\nHOSTILE,"+strings.Repeat("x", 1000)] = "NEVER-SHOW-VALUE"
	for i := 0; i < 25; i++ {
		m["z"+strconv.Itoa(i)] = "NEVER-SHOW-VALUE"
	}
	writeDispatchRegistry(t, path, m)
	f.control(map[string]any{"claude": true, "named": true})
	for _, args := range [][]string{nil, {"--json"}} {
		var out bytes.Buffer
		f.a.Stdout = &out
		if err := f.a.terminalStatus(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "futureField") || strings.Contains(out.String(), "NEVER-SHOW-VALUE") || strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), strings.Repeat("x", 65)) {
			t.Fatal("unsafe or missing drift projection", out.String())
		}
		if len(args) > 0 {
			var got struct{ Readiness terminalReadinessStatus }
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Readiness.State != tmuxwake.RuntimeReady || got.Readiness.Version != "3.0.0" || len(strings.Split(got.Readiness.UnknownKeys, ",")) != 16 {
				t.Fatal("bad drift assessment", got)
			}
			for _, name := range strings.Split(got.Readiness.UnknownKeys, ",") {
				if len(name) == 0 || len(name) > 64 {
					t.Fatal("unbounded name", name)
				}
				for _, c := range name {
					if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-", c) {
						t.Fatal("unsafe name", name)
					}
				}
			}
		}
	}
	sample := boundTerminal(*f.binding()).AssessReadiness(context.Background())
	if !sample.Eligible() {
		t.Fatal("reporting blocked admission", sample)
	}
}
