package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func agentRecoverySession(t *testing.T, e *env, tr *agentDMTestTransport, name string) (*session, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	app := &App{as: name, Stdout: &output, Stderr: &output, HTTP: &http.Client{Transport: tr}, Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}}
	s, err := app.session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.closeAdmission)
	s.principals = s.readDeliveryPrincipals()
	return s, &output
}

// Feature-local storage faults cannot suppress ordinary/principal mail or
// turn a retained known pair into an ordinary room, even under --all.
// Failed held persistence preserves the cursor and existing corrupt bytes.
func TestAgentDMDeliveryStorageIsolation(t *testing.T) {
	for _, mode := range []string{"absent-0700", "absent-0755", "records-absent", "records-json", "records-mode", "records-and-held-json", "directory-mode", "guard-json"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, a, b := agentDMFixture(t)
			room := "!unbound"
			known := !strings.HasPrefix(mode, "absent-")
			if known {
				e.as("alice", "agent-dm", "ensure", b).ok(t)
				room = *agentDMEntry(t, e, "alice", b).Room
				e.as("bob", "join", room).ok(t)
				e.as("bob", "agent-dm", "ensure", a).ok(t)
			}
			setAgentCursor(t, e, "bob")
			ownerPut(t, ownerFiles(e.paths(), "bob").path("principals.json"), principalConfig{V: 1, Principals: []string{"@parent:" + testServer}})
			dir := agentDMDir(e.paths(), "bob")
			recordPath := filepath.Join(dir, "records.json")
			heldPath := filepath.Join(dir, "held.json")
			switch mode {
			case "records-absent":
				if err := os.Remove(recordPath); err != nil {
					t.Fatal(err)
				}
			case "records-json", "records-and-held-json":
				if err := os.WriteFile(recordPath, []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "records-and-held-json" {
					if err := os.WriteFile(heldPath, []byte(`{`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "records-mode":
				if err := os.Chmod(recordPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory-mode", "absent-0755":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "guard-json":
				if _, err := config.UpdateState(e.paths(), "bob", func(st *config.State) error { st.AgentDMContexts = json.RawMessage(`{"broken":true}`); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			recordBefore, _ := os.ReadFile(recordPath)
			heldBefore, _ := os.ReadFile(heldPath)
			s, output := agentRecoverySession(t, e, tr, "bob")
			ordinary := agentDirectedEvent(t, "$ordinary-storage", "@other:"+testServer, b)
			principal := agentDirectedEvent(t, "$principal-storage", "@parent:"+testServer, "@wrong:"+testServer)
			pair := agentDirectedEvent(t, "$pair-storage", a, b)
			joined := map[string]any{"!ordinary-storage": map[string]any{"timeline": map[string]any{"events": []matrix.Event{ordinary, principal}}}}
			if known {
				joined[room] = map[string]any{"timeline": map[string]any{"events": []matrix.Event{pair}}}
			}
			raw, _ := json.Marshal(map[string]any{"next_batch": "s100", "rooms": map[string]any{"join": joined}})
			var response matrix.SyncResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			state, err := s.ingest(context.Background(), response, "s0", nil, map[string]bool{"!ordinary-storage": true, room: true}, time.Time{})
			if err != nil {
				t.Fatal("storage isolation assertion: unrelated delivery aborted", err)
			}
			mail := e.mailbox("bob")
			if !mail.Has(ordinary.EventID) || !mail.Has(principal.EventID) {
				t.Fatal("storage isolation assertion: ordinary/principal mail suppressed")
			}
			if known && mode != "guard-json" && mail.Has(pair.EventID) {
				t.Fatal("storage isolation assertion: unknown known-pair mail reached mailbox")
			}
			if mode == "guard-json" && !mail.Has(pair.EventID) {
				t.Fatal("storage isolation assertion: independently valid record/policy hidden by guard-only fault")
			}
			if mode == "records-and-held-json" || mode == "directory-mode" {
				if state.NextBatch != "s0" || !s.agentReplayDeferred {
					t.Fatal("storage replay assertion: cursor advanced over unretained pair mail")
				}
				if after, _ := os.ReadFile(heldPath); string(after) != string(heldBefore) {
					t.Fatal("storage replay assertion: corrupt held input overwritten")
				}
			}
			if after, _ := os.ReadFile(recordPath); string(after) != string(recordBefore) {
				t.Fatal("storage isolation assertion: delivery rewrote records")
			}
			if known && !strings.Contains(output.String(), "unknown") {
				t.Fatal("storage isolation assertion: missing unknown diagnostic")
			}
			if mode == "absent-0755" {
				before := tr.count("createRoom")
				e.as("bob", "agent-dm", "ensure", a).failsWith(t, ExitError, "0700")
				if tr.count("createRoom") != before {
					t.Fatal("delivery exception weakened pair command")
				}
			}
		})
	}
}

// An absent file in a physical broad-mode directory is absent for delivery;
// symlinked directories/files and wrong types remain errors, never absence.
func TestAgentDMDeliveryAbsenceBoundary(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "pair")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := agentDeliveryRead(dir, "records.json"); !os.IsNotExist(err) {
		t.Fatal("physical absence refused", err)
	}
	link := filepath.Join(base, "redirect")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := agentDeliveryRead(link, "records.json"); err == nil || os.IsNotExist(err) {
		t.Fatal("symlink namespace became absence", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "records.json")
	if err := os.Symlink(filepath.Join(base, "missing"), file); err != nil {
		t.Fatal(err)
	}
	if _, err := agentDeliveryRead(dir, "records.json"); err == nil || os.IsNotExist(err) {
		t.Fatal("dangling file became absence", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := agentDeliveryRead(dir, "records.json"); err == nil || os.IsNotExist(err) {
		t.Fatal("directory file became absence", err)
	}
}

// Actual state-writer refusal occurs after valid admission/alias observation
// and before JOIN or mapping publication. A cancelled writer also preserves
// the original state; no weaker alternate writer is used.
func TestAgentDMContextWriterRefusal(t *testing.T) {
	e, tr, _, b := agentDMFixture(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	room := *agentDMEntry(t, e, "alice", b).Room
	s, _ := agentRecoverySession(t, e, tr, "bob")
	path := e.paths().StateFile("bob")
	var observations atomic.Int32
	s.app.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := tr.RoundTrip(r)
		if strings.Contains(r.URL.Path, "/directory/room/#a2a-dm-v1-") {
			observations.Add(1)
			// Inject only after session migration checks and real alias lookup,
			// so this exercises the dependent context writer, not setup refusal.
			if err := os.MkdirAll(path, 0700); err != nil {
				return nil, err
			}
		}
		return resp, err
	})}
	before := tr.count("/join/")
	err := cmdJoin(context.Background(), s.app, []string{room})
	if err == nil || !strings.Contains(err.Error(), "regular") || observations.Load() == 0 || tr.count("/join/") != before {
		t.Fatalf("context writer refusal assertion: err=%v observations=%d joins=%d", err, observations.Load(), tr.count("/join/")-before)
	}
	if _, err := os.Stat(filepath.Join(agentDMDir(e.paths(), "bob"), "records.json")); !os.IsNotExist(err) {
		t.Fatal("writer refusal published mapping", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.retainAgentContexts(ctx, []string{room}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled context write not refused", err)
	}
	if st := e.state("bob"); len(st.AgentDMContexts) != 0 {
		t.Fatal("cancelled writer changed contexts")
	}
}

// Capacity stops the dependent create or explicit JOIN without guard eviction;
// a neighboring available slot permits the normal creator operation.
func TestAgentDMContextCapacityBeforeEffects(t *testing.T) {
	for _, mode := range []string{"available-create", "full-create", "full-join"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, a, b := agentDMFixture(t)
			name, caller, peer := "alice", a, b
			room := ""
			if mode == "full-join" {
				e.as("alice", "agent-dm", "ensure", b).ok(t)
				room = *agentDMEntry(t, e, "alice", b).Room
				name, caller, peer = "bob", b, a
			}
			s, _ := agentRecoverySession(t, e, tr, name)
			connection, err := s.agentConnection()
			if err != nil {
				t.Fatal(err)
			}
			guards := agentDMContexts{Contract: agentDMVersion, Caller: caller, Connection: connection, Rooms: []string{}}
			n := 64
			if mode == "available-create" {
				n = 63
			}
			for i := 0; i < n; i++ {
				guards.Rooms = append(guards.Rooms, fmt.Sprintf("!retained-%d", i))
			}
			raw, _ := json.Marshal(guards)
			if _, err := config.UpdateState(e.paths(), name, func(st *config.State) error { st.AgentDMContexts = raw; return nil }); err != nil {
				t.Fatal(err)
			}
			beforeCreate, beforeJoin := tr.count("createRoom"), tr.count("/join/")
			var result result
			if room != "" {
				result = e.as(name, "join", room)
			} else {
				result = e.as(name, "agent-dm", "ensure", peer)
			}
			if mode == "available-create" {
				result.ok(t)
				if tr.count("createRoom") != beforeCreate+1 {
					t.Fatal("available context did not create")
				}
				return
			}
			result.failsWith(t, ExitError, "context capacity")
			if tr.count("createRoom") != beforeCreate || tr.count("/join/") != beforeJoin {
				t.Fatal("context capacity assertion: dependent remote effect")
			}
			st := e.state(name)
			var retained agentDMContexts
			if agentDecode(st.AgentDMContexts, &retained) != nil || !reflect.DeepEqual(retained, guards) {
				t.Fatal("context capacity assertion: guard eviction/write on refusal")
			}
			if _, err := os.Stat(filepath.Join(agentDMDir(e.paths(), name), "records.json")); !os.IsNotExist(err) {
				t.Fatal("context capacity assertion: partial mapping", err)
			}
		})
	}
}

// The replay pause is bounded by the original wait deadline and cancellable;
// the normal unblocked path does not add a timer delay.
func TestAgentDMReplayPause(t *testing.T) {
	s := &session{app: &App{Now: time.Now}}
	start := time.Now()
	if err := s.pauseAgentReplay(context.Background(), time.Time{}); err != nil || time.Since(start) > time.Second {
		t.Fatal("normal delivery delayed", err)
	}
	s.agentReplayDeferred = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.pauseAgentReplay(ctx, time.Time{}); err != context.Canceled {
		t.Fatal("replay pause ignored cancellation", err)
	}
	start = time.Now()
	if err := s.pauseAgentReplay(context.Background(), start.Add(40*time.Millisecond)); err != errWaitTimeout || time.Since(start) < 25*time.Millisecond || time.Since(start) > time.Second {
		t.Fatal("replay pause deadline assertion", err, time.Since(start))
	}
}
