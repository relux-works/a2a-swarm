package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Observe only the isolated test process, independently of the registry and
// tmuxwake's collector. Pure cross-platform identity tests retain Darwin cases.
func localClaudeRegistryFields(t *testing.T, pid int) (string, string) {
	t.Helper()
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		end := bytes.LastIndexByte(raw, ')')
		if end < 0 || len(strings.Fields(string(raw[end+1:]))) < 20 {
			t.Fatal("isolated stat unavailable")
		}
		start := strings.Fields(string(raw[end+1:]))[19] // documented field 22.
		machine, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			t.Fatal(err)
		}
		namespace, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid"))
		if err != nil {
			t.Fatal(err)
		}
		return "linux:" + strings.TrimSpace(string(machine)) + ":" + namespace, start
	}
	if runtime.GOOS != "darwin" {
		t.Skip("local registry identity has no supported platform")
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return "darwin", strings.TrimSpace(string(out))
}

// Record values are synthetic except the independently observed local process
// identity fields: PID, platform domain and birth/start token.
func dispatchRegistry(t *testing.T, f *orphanDispatchFixture, status string) (string, map[string]any) {
	t.Helper()
	b := f.binding()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	domain, start := localClaudeRegistryFields(t, b.OwnerPID)
	m := map[string]any{"cwd": "/synthetic", "entrypoint": "cli", "kind": "interactive", "messagingSocketPath": "/synthetic/not-opened", "name": "synthetic", "nameSince": 1, "nameSource": "user", "peerFeatures": []string{"notify_idle"}, "peerProtocol": 1, "pid": b.OwnerPID, "pidDomain": domain, "procStart": start, "sessionId": b.SessionID, "startedAt": 1, "status": status, "statusUpdatedAt": 100, "tmux": "synthetic:@0." + b.Target.Pane, "updatedAt": 100, "version": "2.1.310"}
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
	isolateBindTest(t)
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

// Each Linux identity refusal has a valid named readiness control on the same
// isolated target. Two dispatch cycles must retain mail without input, attempts,
// reservations or changes to binding custody; readiness diagnostics may change.
// Restoring only the record's identity must then deliver exactly one notice.
func TestLinuxRegistryDispatchIdentityRefusals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux collection/dispatch boundary")
	}
	isolateBindTest(t)
	for _, mode := range []string{"wrong-machine-id", "wrong-pid-namespace", "reused-pid-start-ticks", "mixed-darwin-linux-domain"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			mail := f.mail()
			path, m := dispatchRegistry(t, f, "idle")
			f.control(map[string]any{"claude": true, "named": true})
			before := f.binding()
			valid := boundTerminal(*before).AssessReadiness(context.Background())
			if !valid.Eligible() || valid.Key.Route != tmuxwake.RegistryNamed {
				t.Fatal("Linux named positive control refused", valid.Runtime.Reason)
			}
			domain, start := m["pidDomain"].(string), m["procStart"].(string)
			parts := strings.SplitN(domain, ":", 3)
			switch mode {
			case "wrong-machine-id":
				machine := strings.Repeat("a", 32)
				if machine == parts[1] {
					machine = strings.Repeat("b", 32)
				}
				m["pidDomain"] = "linux:" + machine + ":" + parts[2]
			case "wrong-pid-namespace":
				namespace := "pid:[1]"
				if namespace == parts[2] {
					namespace = "pid:[2]"
				}
				m["pidDomain"] = "linux:" + parts[1] + ":" + namespace
			case "reused-pid-start-ticks":
				m["procStart"] = "0"
				if start == "0" {
					m["procStart"] = "1"
				}
			case "mixed-darwin-linux-domain":
				m["pidDomain"] = "darwin"
			}
			writeDispatchRegistry(t, path, m)
			if got := boundTerminal(*before).AssessReadiness(context.Background()); got.Runtime.Reason != tmuxwake.RuntimeIdentityMismatch || got.Eligible() {
				t.Fatal("Linux mismatch did not reach identity refusal", got.Runtime.Reason)
			}
			f.dispatch()
			f.step()
			after := f.binding()
			screen := f.wait(func(s orphanScreen) bool { return s.PID > 1 })
			if screen.Text != "" || len(screen.Submitted) != 0 || len(after.Reservations) != 0 || len(after.Rung) != 0 || len(after.AttemptIDs) != 0 || after.TypingStarted || after.Typed || after.Notice != "" || f.e.received("alice", mail) {
				t.Fatal("identity refusal had input/delivery effects")
			}
			if after.Generation != before.Generation || after.SessionID != before.SessionID || after.Target != before.Target || after.ClaudeRegistryRoot != before.ClaudeRegistryRoot || after.Stopped != before.Stopped {
				t.Fatal("identity refusal changed binding custody")
			}
			m["pidDomain"], m["procStart"] = domain, start
			writeDispatchRegistry(t, path, m)
			f.dispatch()
			f.step()
			screen = f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			if screen.Submitted[0] != tmuxwake.Notice(1) || len(f.binding().Rung) != 1 || f.e.received("alice", mail) {
				t.Fatal("restored identity positive control did not deliver exactly once")
			}
		})
	}
}

// Status projects fresh closed readiness but never saves its observation,
// qualification, attempts, or a hostile stored diagnostic string.
func TestReadinessStatusReadOnly(t *testing.T) {
	isolateBindTest(t)
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
	isolateBindTest(t)
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

// Fresh status explains rendered suppression using closed tokens in text and
// JSON, omits a private draft, and leaves the persisted binding unchanged.
func TestReadinessInputStatus(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	for _, mode := range []string{"busy", "draft", "empty"} {
		control := map[string]any{"text": ""}
		want := tmuxwake.InputAssessment{State: tmuxwake.InputEmpty, Reason: tmuxwake.InputReasonNone}
		if mode == "busy" {
			control["busy"] = true
			want = tmuxwake.InputAssessment{State: tmuxwake.InputUnavailable, Reason: tmuxwake.InputReasonBusyOrDialog}
		}
		if mode == "draft" {
			control["text"] = "PRIVATE_SCREEN_SENTINEL"
			control["hint"] = true
			want = tmuxwake.InputAssessment{State: tmuxwake.InputNonempty, Reason: tmuxwake.InputReasonInputNonempty}
		}
		f.control(control)
		for _, format := range []string{"text", "json"} {
			t.Run(mode+"/"+format, func(t *testing.T) {
				before, _ := json.Marshal(f.e.state("alice"))
				var out bytes.Buffer
				f.a.Stdout = &out
				args := []string{}
				if format == "json" {
					args = append(args, "--json")
				}
				if err := f.a.terminalStatus(context.Background(), args); err != nil {
					t.Fatal(err)
				}
				if format == "json" {
					var got struct {
						Input    tmuxwake.InputAssessment
						Location string `json:"busy_match_location"`
					}
					if err := json.Unmarshal(out.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					wantLocation := ""
					if mode == "busy" {
						wantLocation = "at_or_below_composer"
					}
					if got.Location != wantLocation {
						t.Fatalf("location=%q want=%q", got.Location, wantLocation)
					}
					if got.Input != want {
						t.Fatalf("input diagnostic=%+v want=%+v", got.Input, want)
					}
				} else if !strings.Contains(out.String(), "input="+string(want.State)+" reason="+string(want.Reason)+"\n") {
					t.Fatal("input diagnostic absent", out.String())
				}
				if mode != "busy" && strings.Contains(out.String(), "busy_match_location") {
					t.Fatal("location emitted for non-busy input")
				}
				if strings.Contains(out.String(), "PRIVATE_SCREEN_SENTINEL") || strings.Contains(out.String(), "esc to interrupt") || strings.Contains(out.String(), "a2a-fixture-frame") {
					t.Fatal("screen text leaked")
				}
				after, _ := json.Marshal(f.e.state("alice"))
				if string(before) != string(after) {
					t.Fatal("status wrote state")
				}
			})
		}
	}
}
