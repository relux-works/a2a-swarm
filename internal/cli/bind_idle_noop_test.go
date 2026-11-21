package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func shortBindIdleTarget(t *testing.T, runtime string) tmuxwake.Target {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bdi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	id, err := tmuxwake.SocketID(socket)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return tmuxwake.Target{Socket: socket, SocketID: id, Pane: "%0", PanePID: os.Getpid(), PaneBirth: birth, OwnerPID: os.Getpid(), OwnerBirth: birth, Runtime: runtime}
}

func bindIdleSession(t *testing.T, e *env, name string) (*App, *session) {
	t.Helper()
	a := &App{as: name, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client(), Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}}
	if e.now != nil {
		a.Now = e.now
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	s.principals = s.readDeliveryPrincipals()
	return a, s
}

func markedBindDeliveryFixture(t *testing.T, legacy bool) (*env, *App, *session, string, string) {
	t.Helper()
	isolateBindTest(t)
	e, room := world(t)
	e.extra[EnvSessionID] = "bind-session"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	target := shortBindIdleTarget(t, "codex")
	generation := strings.Repeat("a", 32)
	_, err := config.UpdateState(e.paths(), "alice", func(state *config.State) error {
		state.Terminal = &config.TerminalBinding{
			Generation: generation, SessionID: "bind-session", Host: machineLabel(),
			OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target,
			Outcome: "armed: no new mail", Interval: time.Second, RetryMax: 2 * time.Second,
		}
		if legacy {
			state.LegacySubscriptions = map[string][]string{config.LegacyAllRooms: {"m.room.message"}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := e.state("alice")
	holder := "fixture"
	if prior.Lease != nil && prior.Lease.Expires.After(time.Now()) {
		holder = prior.Lease.Holder
	}
	marked, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(marked) || marked.Terminal == nil || marked.Terminal.Generation != generation {
		t.Fatalf("activate bind fixture: active=%t terminal=%+v err=%v", config.BindPartActive(marked), marked.Terminal, err)
	}
	if err := os.WriteFile(e.paths().MailboxFile("alice"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	a, s := bindIdleSession(t, e, "alice")
	s.paths = s.paths.WithReadCache(config.NewReadCache("alice"))
	scope := config.NewStateDecodeScope("alice")
	s.paths = s.paths.WithStateDecodeScope(scope)
	scope.Open()
	t.Cleanup(scope.Close)
	if _, err := config.LoadState(s.paths, "alice"); err != nil {
		t.Fatal(err)
	}
	return e, a, s, generation, room
}

// TestMarkedBindDeliverySteadyStartSkipsStateWriter proves a steady
// delivery start of a marked binding with nothing expired returns the cached
// view, changes neither State nor the subscription list on the server, and
// does not enter the State writer.
func TestMarkedBindDeliverySteadyStartSkipsStateWriter(t *testing.T) {
	e, a, s, _, _ := markedBindDeliveryFixture(t, false)
	beforeState, err := os.ReadFile(s.paths.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	beforeStats := config.ReadStateDecodeStats()
	beforePuts := e.hs.CallCount(accountDataPut)
	start, list, err := s.startBindDelivery(context.Background(), a.Now())
	if err != nil || !start.cached || !start.view.Marked || !start.view.Active || start.view.NextBatch == "" || len(list.Subs) != 0 {
		t.Fatalf("steady bind start=%+v subscriptions=%+v err=%v", start, list, err)
	}
	afterStats := config.ReadStateDecodeStats()
	afterState, readErr := os.ReadFile(s.paths.StateFile("alice"))
	if readErr != nil || string(beforeState) != string(afterState) || e.hs.CallCount(accountDataPut) != beforePuts {
		t.Fatalf("steady start changed State or subscriptions: readErr=%v StateSame=%t PUTs=%d->%d", readErr, string(beforeState) == string(afterState), beforePuts, e.hs.CallCount(accountDataPut))
	}
	// The view's writer seal is checked once. That costs at most one digest:
	// none when the seal-digest memo already holds these bytes.
	if afterStats.WriterClones != beforeStats.WriterClones || afterStats.StateDigests-beforeStats.StateDigests > 1 || afterStats.WriterSealChecks-beforeStats.WriterSealChecks != 1 || afterStats.StateWriteBytes != beforeStats.StateWriteBytes {
		t.Fatalf("steady start entered State writer: before=%+v after=%+v", beforeStats, afterStats)
	}
}

// TestMarkedBindDeliveryLegacySubscriptionsUseWriterPath proves a
// marked binding that still holds legacy subscriptions in State takes the
// ordinary writer path: the migration to the server list is persisted and the
// State copy cleared.
func TestMarkedBindDeliveryLegacySubscriptionsUseWriterPath(t *testing.T) {
	e, a, s, _, _ := markedBindDeliveryFixture(t, true)
	start, list, err := s.startBindDelivery(context.Background(), a.Now())
	if err != nil || start.cached || len(list.Subs) != 1 || list.Subs[0].Source != "legacy" {
		t.Fatalf("legacy bind start=%+v subscriptions=%+v err=%v", start, list, err)
	}
	var remote SubList
	if err := json.Unmarshal(e.hs.accountDataOf("@alice:"+testServer, SubsType), &remote); err != nil || len(remote.Subs) != 1 || remote.Subs[0].Source != "legacy" {
		t.Fatalf("legacy subscription migration not persisted: list=%+v err=%v", remote, err)
	}
	state, err := config.LoadState(s.paths, "alice")
	if err != nil || len(state.LegacySubscriptions) != 0 {
		t.Fatalf("legacy subscription shadow not cleared: %v err=%v", state.LegacySubscriptions, err)
	}
}

// TestMarkedBindEmptySyncAdvancesCursorAndKeepsMailboxEmpty proves an
// empty sync of a marked binding advances the cursor in the sidecar only:
// State and mailbox bytes are unchanged, the State writer is not used, and the
// cycle reports the mailbox as proven empty.
func TestMarkedBindEmptySyncAdvancesCursorAndKeepsMailboxEmpty(t *testing.T) {
	e, a, s, _, _ := markedBindDeliveryFixture(t, false)
	if _, err := config.UpdateState(s.paths, "alice", func(state *config.State) error {
		state.Terminal.Outcome = "starting"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An event in a room Alice has not joined advances the server cursor while
	// producing an empty joined/invited response.
	outside := e.hs.addRoom("outside", "@carol:"+testServer)
	e.hs.inject(outside, "@carol:"+testServer, msg("not joined", nil))
	start, subs, err := s.startBindDelivery(context.Background(), a.Now())
	if err != nil || !start.cached {
		t.Fatalf("start empty-sync fixture=%+v err=%v", start, err)
	}
	viewBefore, hitBefore, err := config.TryLoadCachedBindIdleDeliveryState(s.paths, "alice")
	if err != nil || !hitBefore || viewBefore.NextBatch == "" {
		t.Fatalf("read cursor before empty sync: view=%+v err=%v", viewBefore, err)
	}
	stateBefore, err := os.ReadFile(s.paths.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	mailboxBefore, err := os.ReadFile(s.paths.MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	beforeStats := config.ReadStateDecodeStats()
	syncResult, err := s.syncIdleBindNow(context.Background(), start.view, subs.Subs, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !syncResult.mailboxProvenEmpty {
		t.Fatal("empty mailbox sync did not prove the steady idle path")
	}
	viewAfter, hitAfter, err := config.TryLoadCachedBindIdleDeliveryState(s.paths, "alice")
	if err != nil || !hitAfter || viewAfter.NextBatch == viewBefore.NextBatch {
		t.Fatalf("empty sync did not advance cursor: before=%+v after=%+v err=%v", viewBefore, viewAfter, err)
	}
	stateAfter, stateErr := os.ReadFile(s.paths.StateFile("alice"))
	mailboxAfter, mailErr := os.ReadFile(s.paths.MailboxFile("alice"))
	if stateErr != nil || mailErr != nil || string(stateBefore) != string(stateAfter) || string(mailboxBefore) != string(mailboxAfter) {
		t.Fatalf("empty sync changed State/mailbox: stateErr=%v mailErr=%v stateSame=%t mailboxSame=%t", stateErr, mailErr, string(stateBefore) == string(stateAfter), string(mailboxBefore) == string(mailboxAfter))
	}
	afterStats := config.ReadStateDecodeStats()
	if afterStats.WriterSealChecks != beforeStats.WriterSealChecks || afterStats.StateDigests != beforeStats.StateDigests || afterStats.WriterClones != beforeStats.WriterClones || afterStats.StateWriteBytes != beforeStats.StateWriteBytes {
		t.Fatalf("empty sync used State writer: before=%+v after=%+v", beforeStats, afterStats)
	}
}

// TestMarkedBindNonEmptySyncFallsBackToDeliveryPersistence proves a
// sync that carries mail does not claim an empty mailbox: the mail is appended
// through the ordinary delivery persistence and only then the cursor is
// committed through the sidecar.
func TestMarkedBindNonEmptySyncFallsBackToDeliveryPersistence(t *testing.T) {
	e, a, s, _, room := markedBindDeliveryFixture(t, false)
	start, subs, err := s.startBindDelivery(context.Background(), a.Now())
	if err != nil || !start.cached {
		t.Fatalf("start non-empty sync fixture=%+v err=%v", start, err)
	}
	id := e.hs.inject(room, "@bob:"+testServer, msg("@alice:"+testServer+" addressed control", nil))
	syncResult, err := s.syncIdleBindNow(context.Background(), start.view, subs.Subs, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if syncResult.mailboxProvenEmpty {
		t.Fatal("non-empty sync incorrectly proved an empty mailbox")
	}
	if !e.mailbox("alice").Has(id) {
		t.Fatal("non-empty sync skipped normal mailbox persistence")
	}
	view, hit, err := config.TryLoadCachedBindIdleDeliveryState(s.paths, "alice")
	if err != nil || !hit || view.NextBatch == start.view.NextBatch {
		t.Fatalf("non-empty sync did not commit marked cursor through sidecar: before=%+v after=%+v err=%v", start.view, view, err)
	}
}

// TestMarkedBindHeldStorageFailureRetainsCursor proves that when held
// peer mail cannot be stored, principal mail is still delivered, the replay
// signal is kept and the marked cursor does not advance.
func TestMarkedBindHeldStorageFailureRetainsCursor(t *testing.T) {
	e, a, s, _, room := markedBindDeliveryFixture(t, false)
	start, subs, err := s.startBindDelivery(context.Background(), a.Now())
	if err != nil || !start.cached {
		t.Fatalf("start held-storage fixture=%+v err=%v", start, err)
	}
	heldPath := filepath.Join(agentDMDir(s.paths, s.name), "held.json")
	if err := os.MkdirAll(filepath.Dir(heldPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(heldPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	id := e.hs.inject(room, "@bob:"+testServer, msg("@alice:"+testServer+" addressed control", nil))
	if _, err := s.syncIdleBindNow(context.Background(), start.view, subs.Subs, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !e.mailbox("alice").Has(id) || !s.agentReplayDeferred {
		t.Fatal("held-storage refusal suppressed principal delivery or replay signal")
	}
	view, hit, err := config.TryLoadCachedBindIdleDeliveryState(s.paths, "alice")
	if err != nil || !hit || view.NextBatch != start.view.NextBatch {
		t.Fatalf("held-storage refusal advanced marked cursor: before=%+v after=%+v err=%v", start.view, view, err)
	}
}

// TestMarkedTerminalStaleHandover proves a terminal update from a
// generation that has been replaced is refused as retired and leaves the new
// binding unchanged, while the current generation's update is stored.
func TestMarkedTerminalStaleHandover(t *testing.T) {
	isolateBindTest(t)
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	p := e.paths()
	target, _, _ := usabilityTarget(t, "codex")
	oldGeneration := strings.Repeat("a", 32)
	newGeneration := strings.Repeat("b", 32)
	_, err := config.UpdateState(p, "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{
			Generation: oldGeneration, SessionID: "owned-thread", Host: machineLabel(),
			OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target,
			Outcome: "armed: no new mail", Interval: time.Second, RetryMax: 2 * time.Second,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := e.state("alice")
	marked, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Holder: prior.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(marked) {
		t.Fatalf("activate bind-state fixture: active=%t err=%v", config.BindPartActive(marked), err)
	}
	newNotice := schemaNotice("false")
	staleNotice := schemaNotice("true")
	testRecordBindNotice(t, p, "alice", newNotice, "unavailable: own notice changed")

	a := &App{as: "alice", ctx: context.Background()}
	s := &session{app: a, paths: p, name: "alice"}
	changed := false
	err = a.terminalUpdate(s, oldGeneration, func(b *config.TerminalBinding) {
		if !changed {
			changed = true
			proof := config.BindControlProof{
				OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth,
				Target: target, TargetVerified: true,
			}
			testSetBindControl(t, p, "alice", config.BindControlTarget{
				Generation: newGeneration, SessionID: "next-thread", Kind: "handover",
			}, proof)
		}
		b.OwnNotice = staleNotice
	})
	if err == nil || err.Error() != "binding retired" {
		t.Fatalf("stale terminal update error=%v, want binding retired", err)
	}
	current, err := config.LoadState(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	view, err := config.ReadBindPartView(p, "alice", current)
	if err != nil || view.Generation != newGeneration || view.SessionID != "next-thread" || !reflect.DeepEqual(view.OwnNotice, newNotice) || view.NoticePublication != "unavailable: own notice changed" {
		t.Fatalf("stale update changed new binding: view=%+v err=%v", view, err)
	}

	// The current generation remains writable, so the stale-generation guard
	// does not turn into a blanket refusal of valid terminal updates.
	if err := a.terminalUpdate(s, newGeneration, func(b *config.TerminalBinding) { b.OwnNotice = staleNotice }); err != nil {
		t.Fatalf("current generation update: %v", err)
	}
	current, err = config.LoadState(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	view, err = config.ReadBindPartView(p, "alice", current)
	if err != nil || view.Generation != newGeneration || view.SessionID != "next-thread" || !reflect.DeepEqual(view.OwnNotice, staleNotice) {
		t.Fatalf("current-generation update was not stored: view=%+v err=%v", view, err)
	}
}

// TestMarkedTerminalUpdateNoopSkipsLargeStateWriter proves three
// terminal updates of a marked binding on a large State: a no-op and a change
// of sidecar-held fields alone do not decode, clone, digest, reseal, encode or
// write State and leave its bytes equal, and a change of a State-held terminal
// field is still persisted.
func TestMarkedTerminalUpdateNoopSkipsLargeStateWriter(t *testing.T) {
	isolateBindTest(t)
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	p := e.paths()
	generation := strings.Repeat("a", 32)
	_, err := config.UpdateState(p, "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{
			Generation: generation, SessionID: "owned-thread", Host: machineLabel(),
			Outcome: "armed: no new mail", Interval: time.Second, RetryMax: 2 * time.Second,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := e.state("alice")
	marked, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Holder: prior.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(marked) || marked.Terminal == nil || marked.Terminal.Generation != generation {
		t.Fatalf("activate bind-state fixture: active=%t terminal=%+v err=%v", config.BindPartActive(marked), marked.Terminal, err)
	}

	a := &App{as: "alice", ctx: context.Background()}
	s := &session{app: a, paths: p.WithReadCache(config.NewReadCache("alice")), name: "alice"}
	scope := config.NewStateDecodeScope("alice")
	defer scope.Close()
	s.paths = s.paths.WithStateDecodeScope(scope)
	scope.Open()
	if _, err := config.LoadState(s.paths, "alice"); err != nil { // warm the exact cache used by the consumer
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	before := config.ReadStateDecodeStats()
	if err := a.terminalUpdate(s, generation, func(*config.TerminalBinding) {}); err != nil {
		t.Fatal(err)
	}
	after := config.ReadStateDecodeStats()
	unchanged, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || string(unchanged) != string(beforeBytes) {
		t.Fatalf("marked no-op changed State bytes: err=%v equal=%t", err, string(unchanged) == string(beforeBytes))
	}
	// The cache hit confirms the State bytes, so a read is expected; decoding,
	// cloning, digesting or rewriting the unchanged State is not.
	if d := after.UnmarshalStatePasses - before.UnmarshalStatePasses; d != 0 {
		t.Errorf("marked no-op decoded large State %d times", d)
	}
	if d := after.WriterClones - before.WriterClones; d != 0 {
		t.Errorf("marked no-op cloned large State %d times", d)
	}
	if d := after.StateDigests - before.StateDigests; d != 0 {
		t.Errorf("marked no-op digested large State %d times", d)
	}
	if d := after.CanonicalStates - before.CanonicalStates; d != 0 {
		t.Errorf("marked no-op canonically parsed large State %d times", d)
	}
	if d := after.SealedStates - before.SealedStates; d != 0 {
		t.Errorf("marked no-op resealed large State %d times", d)
	}
	if d := after.StateEncodes - before.StateEncodes; d != 0 {
		t.Errorf("marked no-op encoded large State %d times", d)
	}
	if d := after.StateWriteBytes - before.StateWriteBytes; d != 0 {
		t.Errorf("marked no-op wrote %d State bytes", d)
	}

	// A fresh notice observation belongs to the sidecar. It must be visible on
	// the next merged read without invoking the large State writer.
	sidecarNotice := schemaNotice("false")
	before = config.ReadStateDecodeStats()
	if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.OwnNotice = sidecarNotice }); err != nil {
		t.Fatal(err)
	}
	after = config.ReadStateDecodeStats()
	unchanged, err = os.ReadFile(p.StateFile("alice"))
	if err != nil || string(unchanged) != string(beforeBytes) {
		t.Fatalf("sidecar-only observation changed State bytes: err=%v equal=%t", err, string(unchanged) == string(beforeBytes))
	}
	if d := after.WriterClones - before.WriterClones; d != 0 {
		t.Errorf("sidecar-only observation cloned large State %d times", d)
	}
	if d := after.StateDigests - before.StateDigests; d != 0 {
		t.Errorf("sidecar-only observation digested large State %d times", d)
	}
	if d := after.CanonicalStates - before.CanonicalStates; d != 0 {
		t.Errorf("sidecar-only observation canonically parsed large State %d times", d)
	}
	if d := after.SealedStates - before.SealedStates; d != 0 {
		t.Errorf("sidecar-only observation resealed large State %d times", d)
	}
	if d := after.StateWriteBytes - before.StateWriteBytes; d != 0 {
		t.Errorf("sidecar-only observation wrote %d State bytes", d)
	}
	updatedNotice, err := config.LoadState(s.paths, "alice")
	if err != nil || !reflect.DeepEqual(updatedNotice.TerminalOwnNotice(), sidecarNotice) {
		t.Fatalf("sidecar-only observation missing from merged State: notice=%+v err=%v", updatedNotice.TerminalOwnNotice(), err)
	}

	if err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Outcome = "updated control" }); err != nil {
		t.Fatal(err)
	}
	updated, err := config.LoadState(p, "alice")
	if err != nil || updated.Terminal == nil || updated.Terminal.Outcome != "updated control" {
		t.Fatalf("changed marked terminal metadata was lost: terminal=%+v err=%v", updated.Terminal, err)
	}
	finalBytes, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || string(finalBytes) == string(beforeBytes) {
		t.Fatalf("changed marked terminal metadata did not persist: err=%v", err)
	}
}
