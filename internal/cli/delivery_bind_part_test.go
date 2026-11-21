package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func TestMarkedWaitRefusesMissingSidecarAfterRollbackImport(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	p := e.paths()
	legacy := e.state("alice")
	if legacy.Lease == nil {
		t.Fatal("setup: delivery lease missing before migration")
	}
	marked, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Holder: legacy.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(marked) {
		t.Fatalf("activate marked state: active=%t err=%v", config.BindPartActive(marked), err)
	}
	if err := config.ExportBindPartForRollback(p, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateState(p, "alice", func(st *config.State) error {
		st.NextBatch = "rollback-import-cursor"
		return nil
	}); err != nil {
		t.Fatalf("legacy rollback update: %v", err)
	}
	if err := config.ImportBindPartAfterRollback(p, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.BindPartFile("alice")); err != nil {
		t.Fatal(err)
	}
	// Keep an explicit empty durable mailbox so the before/after refusal
	// comparison covers mailbox bytes even when setup delivered no messages.
	if err := os.WriteFile(p.MailboxFile("alice"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	mailboxBefore, err := os.ReadFile(p.MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	syncsBefore := e.hs.CallCount("GET /_matrix/client/v3/sync")
	result := e.as("alice", "wait", "--timeout", "10ms")
	result.failsWith(t, ExitError, "marked bind state sidecar is missing")
	if got := e.hs.CallCount("GET /_matrix/client/v3/sync"); got != syncsBefore {
		t.Fatalf("missing sidecar reached sync: before=%d after=%d", syncsBefore, got)
	}
	stateAfter, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("missing-sidecar refusal changed State: err=%v equal=%t", err, bytes.Equal(stateBefore, stateAfter))
	}
	mailboxAfter, err := os.ReadFile(p.MailboxFile("alice"))
	if err != nil || !bytes.Equal(mailboxBefore, mailboxAfter) {
		t.Fatalf("missing-sidecar refusal changed mailbox: err=%v equal=%t", err, bytes.Equal(mailboxBefore, mailboxAfter))
	}
}

func TestMarkedWaitCursorFailureKeepsOldCursorAndDeduplicatesRetry(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	state := e.state("alice")
	p := e.paths()
	migrated, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Holder: state.Lease.Holder, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !config.BindPartActive(migrated) {
		t.Fatal("migration did not activate the provider")
	}
	oldCursor := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local retry the marked cursor", nil))

	realBirth, err := (&App{}).pidBirth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	run := 0
	commitHookRuns := 0
	e.configure = func(a *App) {
		run++
		birth := realBirth + int64(run-1)
		a.leaseBirth = func(_ context.Context, _ int) (int64, error) { return birth, nil }
		if run == 1 {
			a.beforeBindCursorCommit = func() {
				commitHookRuns++
				if err := os.Chmod(filepath.Dir(p.StateFile("alice")), 0o500); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	first := e.as("alice", "wait", "--timeout", "2s")
	first.failsWith(t, ExitError, "bind state sidecar")
	if commitHookRuns != 1 {
		t.Fatalf("cursor commit hook ran %d times, want exactly once", commitHookRuns)
	}
	if strings.Contains(first.stderr, "lease of alice taken over") {
		t.Fatal("same-process cursor owner was treated as reused before the injected write failure")
	}
	if err := os.Chmod(filepath.Dir(p.StateFile("alice")), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := e.state("alice").NextBatch; got != oldCursor {
		t.Fatalf("cursor advanced despite failed sidecar commit: got %q want %q", got, oldCursor)
	}
	if pending := e.mailbox("alice").Pending(); len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("mailbox after failed cursor commit = %+v", pending)
	}

	retry := e.as("alice", "wait", "--timeout", "2s").ok(t)
	if !strings.Contains(retry.stderr, "lease of alice taken over") {
		t.Fatal("changed process birth did not recover the interrupted cursor owner")
	}
	out := retry.stdout
	if !strings.Contains(out, id) {
		t.Fatalf("retry did not deliver the retained event: %q", out)
	}
	if got := e.state("alice").NextBatch; got != oldCursor {
		t.Fatalf("displaying already-retained mail advanced cursor: got %q want %q", got, oldCursor)
	}
	replay := e.as("alice", "wait", "--timeout", "2s").ok(t)
	if replay.stdout != "timeout\n" {
		t.Fatalf("deduplicated replay output = %q, want only timeout", replay.stdout)
	}
	if got := e.state("alice").NextBatch; got == oldCursor {
		t.Fatal("deduplicated replay left the provider cursor unchanged")
	}
	if pending := e.mailbox("alice").Pending(); len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("deduplicated replay duplicated the mailbox event: %+v", pending)
	}
}
func TestMarkedWatchDeliveryUsesProviderCursor(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	state := e.state("alice")
	p := e.paths()
	migrated, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Holder: state.Lease.Holder, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !config.BindPartActive(migrated) {
		t.Fatal("migration did not activate the provider")
	}
	oldCursor := migrated.NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local provider-backed watch", nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := e.runCtx(ctx, "--as", "alice", "watch")
	if r.code != ExitOK {
		t.Fatalf("watch exit %d; stderr=%q", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, id) {
		t.Fatalf("watch did not print the retained event %s: %q", id, r.stdout)
	}
	if got := e.state("alice").NextBatch; got == oldCursor {
		t.Fatal("watch left the provider cursor unchanged")
	}
	pending := e.mailbox("alice").Pending()
	if len(pending) != 1 || pending[0].ID != id || !pending[0].Shown {
		t.Fatalf("watch pending entry = %+v, want the event marked shown", pending)
	}
	if e.received("alice", id) {
		t.Fatal("watch receipted the event")
	}
}

func TestBindProcessGuardRefusesSameImageProjector(t *testing.T) {
	if err := bindingProcessConflict("same-image", "same-image", "project", 42); !errors.Is(err, ErrBindingParallelProject) {
		t.Fatalf("same-image projector conflict=%v", err)
	}
	if err := bindingProcessConflict("same-image", "same-image", "bind", 42); err != nil {
		t.Fatalf("same-image non-cursor bind conflict=%v", err)
	}
	identity, root, command := bindingSelectors([]string{"a2a-swarm", "--as", "alice", "project", "--system", "!target"}, nil)
	if identity != "alice" || root == "" || command != "project" {
		t.Fatalf("project selector identity=%q root=%q command=%q", identity, root, command)
	}
}
