package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A transient pair-only directory failure or unreadable admission refuses
// before JOIN/subscription; the valid neighbor holds pair mail until ensure.
// These are synthetic transport/filesystem failures, not federation tests.
func TestAgentDMJoinCaptureRecovery(t *testing.T) {
	for _, mode := range []string{"valid", "alias-429", "alias-502", "admission-json", "admission-mode", "admission-peer"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, a, b := agentDMFixture(t)
			e.as("alice", "agent-dm", "ensure", b).ok(t)
			room := *agentDMEntry(t, e, "alice", b).Room
			admission := filepath.Join(agentDMDir(e.paths(), "bob"), "admission.json")
			switch mode {
			case "admission-json":
				if err := os.WriteFile(admission, []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			case "admission-mode":
				if err := os.Chmod(admission, 0644); err != nil {
					t.Fatal(err)
				}
			case "admission-peer":
				raw, err := os.ReadFile(admission)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(admission, []byte(strings.Replace(string(raw), `"orchestrator"`, `"unknown"`, 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var faults atomic.Int32
			e.configure = func(app *App) {
				app.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasPrefix(mode, "alias-") && strings.Contains(r.URL.Path, "/directory/room/#a2a-dm-v1-") {
						faults.Add(1)
						if mode == "alias-429" {
							return waitResponse(r, 429, `{"errcode":"M_LIMIT_EXCEEDED","error":"transient"}`)
						}
						return waitResponse(r, 502, `{"errcode":"M_UNKNOWN","error":"transient"}`)
					}
					return tr.RoundTrip(r)
				})}
			}
			before := tr.count("/join/")
			join := e.as("bob", "join", room)
			if mode != "valid" {
				if join.code != ExitError || !strings.Contains(join.stderr, "agent-dm:") || tr.count("/join/") != before {
					t.Fatalf("join capture refusal assertion: code=%d joins=%d stderr=%q", join.code, tr.count("/join/")-before, join.stderr)
				}
				if strings.HasPrefix(mode, "alias-") && faults.Load() == 0 {
					t.Fatal("pair-only alias fault not applied")
				}
				if _, err := os.Stat(filepath.Join(agentDMDir(e.paths(), "bob"), "records.json")); !os.IsNotExist(err) {
					t.Fatalf("refused capture wrote records: %v", err)
				}
				assertAgentMailbox(t, e, "bob", []string{})
				return
			}
			join.ok(t)
			if d := agentDMEntry(t, e, "bob", a); d.Room == nil || *d.Room != room || d.Qualified {
				t.Fatal("join failed to retain unqualified pair")
			}
			setAgentCursor(t, e, "bob")
			heldID := appendAgentEvent(e, room, agentDirectedEvent(t, "$capture-held", a, b))
			e.hook("bob", "stop", `{"session_id":"capture-recovery"}`).ok(t)
			assertAgentMailbox(t, e, "bob", []string{})
			e.as("bob", "agent-dm", "ensure", a).ok(t)
			e.hook("bob", "stop", `{"session_id":"capture-qualified"}`).ok(t)
			assertAgentMailbox(t, e, "bob", []string{heldID})
			if tr.count("/join/") != before+1 {
				t.Fatal("valid capture repeated join")
			}
		})
	}
}

// Only an explicit not-found pair alias permits an ordinary join. A fresh
// identity with no pair admission also retains the ordinary join behavior.
func TestAgentDMJoinCaptureOrdinary(t *testing.T) {
	for _, admission := range []bool{true, false} {
		t.Run(map[bool]string{true: "admitted-no-pair-alias", false: "absent-admission"}[admission], func(t *testing.T) {
			e, tr, a, b := agentDMFixture(t)
			if !admission {
				if err := os.Remove(filepath.Join(agentDMDir(e.paths(), "bob"), "admission.json")); err != nil {
					t.Fatal(err)
				}
			}
			room := e.hs.addRoom("ordinary-capture", a)
			e.hs.mu.Lock()
			e.hs.rooms[room].members[b] = "invite"
			e.hs.mu.Unlock()
			before := tr.count("/join/")
			e.as("bob", "join", room).ok(t)
			if tr.count("/join/") != before+1 {
				t.Fatal("ordinary join not executed")
			}
			if _, err := os.Stat(filepath.Join(agentDMDir(e.paths(), "bob"), "records.json")); !os.IsNotExist(err) {
				t.Fatalf("ordinary join established pair context: %v", err)
			}
		})
	}
}
