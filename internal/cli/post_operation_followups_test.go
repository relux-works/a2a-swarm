package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Flag classification agrees with the real parser for flag values, terminators,
// both operation-ID spellings and ordinary input; this is local parsing only.
func TestDurablePostInvocationFlagPosition(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"separate", []string{"ROOM", "--operation-id", "X", "text"}, true},
		{"equals", []string{"ROOM", "--operation-id=X", "text"}, true},
		{"single_dash", []string{"ROOM", "-operation-id", "X", "text"}, true},
		{"after_terminator", []string{"ROOM", "--", "--operation-id", "text"}, false},
		{"ordinary", []string{"ROOM", "--topic", "$root", "text"}, false},
	}
	for _, flag := range []string{"subject", "to", "re", "topic", "kind", "reply-by", "trunk", "subtype", "envelope", "id-file"} {
		for _, token := range []string{"--operation-id", "--operation-id=X"} {
			cases = append(cases, struct {
				name string
				args []string
				want bool
			}{flag + "/" + token, []string{"ROOM", "--" + flag, token, "text"}, false})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := durablePostInvocation(tc.args); got != tc.want {
				t.Fatalf("durable=%v want=%v", got, tc.want)
			}
		})
	}
}

// Ordinary lookalikes still hit missing-metadata preflight and owner-DM warnings;
// real durable posts bypass the warning and acknowledge using inert transport.
func TestDurablePostLookalikesKeepOrdinaryDispatch(t *testing.T) {
	for _, mode := range []string{"ordinary", "subject", "to", "re", "topic", "body", "durable", "durable_equals"} {
		t.Run(mode, func(t *testing.T) {
			w := operationWorldNew(t)
			args := []string{"post", "!room:example.invalid", "--topic", "$root", "text"}
			switch mode {
			case "subject", "to", "re", "topic":
				args = append(args, "--"+mode, "--operation-id")
			case "body":
				args = append(args, "--", "--operation-id")
			case "durable":
				args = operationArgs()
			case "durable_equals":
				args = append(operationArgs()[:4], "--operation-id=op", "synthetic body")
			}
			durable := strings.HasPrefix(mode, "durable")
			metadata, err := os.ReadFile(w.paths.AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(w.paths.AgentFile("alice")); err != nil {
				t.Fatal(err)
			}
			// The neighbor is an independent observable preflight oracle.
			baseline, _, baselineDiag := w.app()
			wantCode := baseline.Run(context.Background(), []string{"post", "!room:example.invalid", "--topic", "$root", "text"})
			if wantCode != 1 || baselineDiag.Len() == 0 {
				t.Fatal("preflight control did not refuse")
			}
			a, _, diag := w.app()
			code := a.Run(context.Background(), args)
			if !durable && (code != wantCode || diag.String() != baselineDiag.String()) {
				t.Fatalf("lookalike bypassed ordinary preflight: %d %s", code, diag)
			}
			if durable && (code != 1 || !strings.Contains(diag.String(), "own_metadata_unavailable")) {
				t.Fatalf("durable preflight control: %d %s", code, diag)
			}
			if w.calls.Load() != 0 || w.store.reads.Load() != 0 {
				t.Fatal("missing-metadata dispatch reached HTTP/provider")
			}
			if err := os.WriteFile(w.paths.AgentFile("alice"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			owner := ownerFiles(w.paths, "alice").path("json")
			if err := os.MkdirAll(filepath.Dir(owner), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(owner, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			a, _, diag = w.app()
			code = a.Run(context.Background(), args)
			if warned := strings.Contains(diag.String(), "owner-dm feature off"); warned == durable {
				t.Fatalf("warning=%v durable=%v: %s", warned, durable, diag)
			}
			if durable && (code != 0 || w.sends.Load() != 1) {
				t.Fatalf("real durable flag failed: %d %s", code, diag)
			}
		})
	}
}

// A full acknowledged journal refuses without mutation, names the explicit
// prune route, then accepts a fresh durable post after local counts-only prune.
// All transport/provider observations are inert, not a live-server proof.
func TestDurablePostCapacityPruneAndResume(t *testing.T) {
	w := operationWorldNew(t)
	a, _, diag := w.app()
	if a.Run(context.Background(), operationArgs()) != 0 {
		t.Fatal(diag)
	}
	j, err := config.OpenPostJournal(w.paths, "alice", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	// Metadata remains outside both budgets, even above the journal byte cap.
	metadataPath := filepath.Join(w.paths.Base, "post-operations", "agents", "alice", ".DS_Store")
	metadata, err := os.OpenFile(metadataPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Truncate(config.PostJournalLimit + 1); err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	seed := w.record()
	for i := 1; i < config.PostRecordCountLimit; i++ {
		r := seed
		r.ID = fmt.Sprintf("retained_%d", i)
		r.TransactionID = "txn_" + r.ID
		r.Outcome, r.EventID, r.HTTPStatus, r.ResponseLength = "not_submitted", "", 0, 0
		r.Request = []byte(`{"body":"inert"}`)
		r.RequestLength = len(r.Request)
		r.ContentDigest = config.PostOperationDigest(r.Request)
		r, created, err := j.Prepare(r)
		if err != nil || !created {
			t.Fatal("seed prepare", err)
		}
		r, err = j.ReserveAttempt(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.RecordACK(r, "$ack", 200, 1); err != nil {
			t.Fatal(err)
		}
	}
	args := operationArgs()
	args[5] = "fresh"
	a, out, diag := w.app()
	if a.Run(context.Background(), args) != 1 || !strings.Contains(diag.String(), "a2a post-operation prune --acknowledged") || out.Len() != 0 || w.sends.Load() != 1 {
		t.Fatalf("capacity refusal: %s %s", out, diag)
	}
	if _, exists, err := j.Read("fresh"); err != nil || exists {
		t.Fatal("capacity refusal retained a new operation", err)
	}
	calls, providers := w.calls.Load(), w.store.reads.Load()
	a, out, diag = w.app()
	if a.Run(context.Background(), []string{"post-operation", "prune", "--acknowledged"}) != 0 || out.String() != "removed=128 retained=0\n" || diag.Len() != 0 {
		t.Fatalf("prune: %s %s", out, diag)
	}
	if w.calls.Load() != calls || w.store.reads.Load() != providers {
		t.Fatal("prune accessed HTTP/provider")
	}
	if st, err := os.Stat(metadataPath); err != nil || st.Size() != config.PostJournalLimit+1 {
		t.Fatal("prune changed metadata", err)
	}
	a, out, diag = w.app()
	if a.Run(context.Background(), args) != 0 || operationResult(t, out).Outcome != "acknowledged" || w.sends.Load() != 2 {
		t.Fatalf("post-prune post failed: %s", diag)
	}
}

// Prune requires explicit acknowledged-only consent and rejects extra modes;
// the valid neighbor succeeds locally without exposing record content.
func TestPostOperationPruneUsage(t *testing.T) {
	w := operationWorldNew(t)
	a, _, diag := w.app()
	if a.Run(context.Background(), operationArgs()) != 0 {
		t.Fatal(diag)
	}
	for _, phase := range []struct{ id, stage string }{{"pending", "after_prepare"}, {"unknown", "after_reservation"}} {
		a, _, _ := w.app()
		a.postOperationFault = func(s string) error {
			if s == phase.stage {
				return errors.New("inert stop")
			}
			return nil
		}
		args := operationArgs()
		args[5] = phase.id
		if a.Run(context.Background(), args) != 1 {
			t.Fatal("failed to seed unresolved state")
		}
	}
	for _, args := range [][]string{
		{"post-operation", "prune"},
		{"post-operation", "prune", "--acknowledged=false"},
		{"post-operation", "prune", "--acknowledged", "extra"},
		{"post-operation", "prune", "--acknowledged", "--json"},
		{"post-operation", "status", "op", "--json", "--acknowledged"},
	} {
		a, out, _ := w.app()
		if a.Run(context.Background(), args) != 2 || out.Len() != 0 || w.record().Outcome != "acknowledged" {
			t.Fatal("invalid prune usage removed evidence")
		}
	}
	unsafe := filepath.Join(w.paths.Base, "post-operations", "agents", "alice", ".DS_Store")
	if err := os.Symlink("op.json", unsafe); err != nil {
		t.Fatal(err)
	}
	a, out, _ := w.app()
	if a.Run(context.Background(), []string{"post-operation", "prune", "--acknowledged"}) != 1 || out.Len() != 0 {
		t.Fatal("unsafe prune printed success counts")
	}
	if err := os.Remove(unsafe); err != nil {
		t.Fatal(err)
	}
	if w.record().Outcome != "acknowledged" {
		t.Fatal("unsafe prune deleted ACK")
	}
	a, out, diag = w.app()
	if a.Run(context.Background(), []string{"post-operation", "prune", "--acknowledged"}) != 0 || out.String() != "removed=1 retained=2\n" {
		t.Fatalf("explicit prune failed: %s", diag)
	}
}
