package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Bind status must preserve a recognized publication result but must not echo
// arbitrary persisted text. This exercises both public output formats and
// checks that projecting invalid metadata does not mutate durable authority.
func TestReviewR8StatusPublicationProjection(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, valid := range []bool{true, false} {
			label := "unknown"
			publication := "R8_SYNTHETIC_PUBLICATION_TEXT"
			if valid {
				label = "known"
				publication = "published"
			}
			t.Run(format+"/"+label, func(t *testing.T) {
				e, _, _ := bindDiagnosticEnv(t)
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal.NoticePublication = publication
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				before := readFile(t, e.paths().StateFile("alice"))
				args := []string{"bind", "status"}
				if format == "json" {
					args = append(args, "--json")
				}
				out := e.as("alice", args...).ok(t).stdout
				if readFile(t, e.paths().StateFile("alice")) != before {
					t.Fatal("status wrote state")
				}
				if valid && !strings.Contains(out, "published") {
					t.Fatal("known publication result was lost")
				}
				if !valid && strings.Contains(out, publication) {
					t.Fatal("bind status rendered arbitrary persisted publication text")
				}
			})
		}
	}
}

// A connection guard failure must keep phase=connection when reached at either
// guard in lockedDeliver, not become phase=mailbox. A deterministic injected
// guard models a connection invalidation between existing consumer checks;
// this does not model an end-to-end live registry edit or terminal action.
func TestReviewR8NestedConnectionStage(t *testing.T) {
	for _, failAt := range []int{1, 3, 4} {
		name := map[int]string{1: "entry-control", 3: "mailbox-entry", 4: "mailbox-locked"}[failAt]
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			stateBefore := readFile(t, f.e.paths().StateFile("alice"))
			mailBefore := readFile(t, f.e.paths().MailboxFile("alice"))
			var log bytes.Buffer
			f.a.Stderr = &log
			calls := 0
			f.s.checkBinding = func() error {
				calls++
				if calls >= failAt {
					return errors.New("R8_SYNTHETIC_CONNECTION_FAULT")
				}
				return nil
			}
			err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
			if err == nil {
				t.Fatal("changed connection accepted")
			}
			if stateBefore != readFile(t, f.e.paths().StateFile("alice")) || mailBefore != readFile(t, f.e.paths().MailboxFile("alice")) {
				t.Fatal("changed connection wrote authority")
			}
			d := terminalErrorDiagnostic("dispatch", err, f.now)
			if d.Phase != "connection" {
				t.Fatalf("connection failure relabeled as %s; want connection; log=%q", d.Phase, log.String())
			}
			if strings.Contains(err.Error()+log.String(), "R8_SYNTHETIC_CONNECTION_FAULT") {
				t.Fatal("connection error text leaked")
			}
		})
	}
}

// Rung entries, invites and gaps remain pending until matching durable receipts;
// status derives age from the remaining event origins without changing storage.
func TestReviewR8PendingRungAndKinds(t *testing.T) {
	e, _, _ := bindDiagnosticEnv(t)
	now := e.now()
	bindPendingAppend(t, e,
		config.MailEntry{ID: "event", Kind: config.MailEvent, TS: now.Add(-3 * time.Hour).UnixMilli()},
		config.MailEntry{ID: "invite", Kind: config.MailInvite, TS: now.Add(-2 * time.Hour).UnixMilli()},
		config.MailEntry{ID: "gap", Kind: config.MailGap})
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.Rung = []string{"event", "invite", "gap"}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	p := bindPendingStatus(t, e)
	if p.Count == nil || *p.Count != 3 || p.AgeState != "unknown" || p.MissingTimestamps != 1 || p.OldestKnownEventAgeMillis == nil || *p.OldestKnownEventAgeMillis != int64((3*time.Hour)/time.Millisecond) {
		t.Fatal(p)
	}
	bindPendingReceive(t, e, "event", "not-present")
	p = bindPendingStatus(t, e)
	if p.Count == nil || *p.Count != 2 || p.AgeState != "unknown" || p.OldestKnownEventAgeMillis == nil || *p.OldestKnownEventAgeMillis != int64((2*time.Hour)/time.Millisecond) {
		t.Fatal(p)
	}
}

// Unreadable mailbox modes and nonregular paths report unknown availability,
// while a valid mailbox returns its count. The rejected bytes are not rewritten.
func TestReviewR8PendingReadRefusals(t *testing.T) {
	for _, mode := range []string{"valid", "mode", "directory", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _ := bindDiagnosticEnv(t)
			bindPendingAppend(t, e, config.MailEntry{ID: "pending", TS: e.now().UnixMilli()})
			path := e.paths().MailboxFile("alice")
			switch mode {
			case "mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".fixture"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".fixture", path); err != nil {
					t.Fatal(err)
				}
			}
			stateBefore := readFile(t, e.paths().StateFile("alice"))
			syncs := e.hs.CallCount("GET /_matrix/client/v3/sync")
			out := e.as("alice", "bind", "status", "--json").ok(t).stdout
			var status struct{ Pending terminalPending }
			if err := json.Unmarshal([]byte(out), &status); err != nil {
				t.Fatal(err)
			}
			p := status.Pending
			if mode == "valid" {
				if p.Count == nil || *p.Count != 1 || p.State != "available" {
					t.Fatal(p)
				}
			} else if p.Count != nil || p.State != "unavailable" || p.AgeState != "unknown" || p.OldestKnownEventAgeMillis != nil {
				t.Fatal(p)
			}
			if stateBefore != readFile(t, e.paths().StateFile("alice")) || syncs != e.hs.CallCount("GET /_matrix/client/v3/sync") {
				t.Fatal("status changed authority or synced")
			}
		})
	}
}

// Failure reporting cannot write through a replaced/stopped generation or an
// invalid connection, including invalidation between its two guards. Malformed
// state remains unchanged; a valid same-generation control persists fixed codes.
func TestReviewR8DiagnosticWriteAuthority(t *testing.T) {
	for _, mode := range []string{"valid", "replaced", "stopped", "connection", "connection-race", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s := bindDiagnosticEnv(t)
			path := e.paths().StateFile("alice")
			switch mode {
			case "replaced", "stopped":
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					if mode == "replaced" {
						st.Terminal.Generation = "replacement"
					} else {
						st.Terminal.Stopped = true
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			case "connection", "connection-race":
				n := 0
				s.checkBinding = func() error {
					n++
					if mode == "connection" || n >= 2 {
						return errors.New("R8_SYNTHETIC_CONNECTION_FAULT")
					}
					return nil
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := readFile(t, path)
			var log bytes.Buffer
			a.Stderr = &log
			a.recordTerminalFailure(s, "diagnostic-generation", "delivery_sync", context.DeadlineExceeded)
			if mode == "valid" {
				d := e.state("alice").Terminal.Diagnostic
				if d == nil || d.Phase != "delivery_sync" || d.Class != "timeout" {
					t.Fatal(d)
				}
			} else if before != readFile(t, path) {
				t.Fatal("diagnostic wrote through invalid authority")
			}
			if !strings.Contains(log.String(), "phase=delivery_sync class=timeout") || strings.Contains(log.String(), "R8_SYNTHETIC") {
				t.Fatal("original safe failure was lost", log.String())
			}
		})
	}
}

// A connection that changes while status waits for the state lock must refuse
// the snapshot after locking. This bounded callback fixture checks the second
// guard and no-write behavior, not a cross-process registry transaction.
func TestReviewR8StatusConnectionAfterLock(t *testing.T) {
	e, a, s := bindDiagnosticEnv(t)
	bindPendingAppend(t, e, config.MailEntry{ID: "pending", TS: e.now().UnixMilli()})
	state := readFile(t, e.paths().StateFile("alice"))
	mail := readFile(t, e.paths().MailboxFile("alice"))
	release := sync.OnceFunc(e.holdStateLock("alice"))
	defer release()
	first := make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	s.checkBinding = func() error {
		calls++
		if calls == 1 {
			close(first)
			return nil
		}
		return errors.New("R8_SYNTHETIC_CONNECTION_FAULT")
	}
	go func() { _, _, err := a.readTerminalStatus(s); done <- err }()
	<-first
	release()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "binding connection:") || strings.Contains(err.Error(), "R8_SYNTHETIC") {
		t.Fatal("changed connection was accepted or misreported", err)
	}
	if state != readFile(t, e.paths().StateFile("alice")) || mail != readFile(t, e.paths().MailboxFile("alice")) {
		t.Fatal("refused status changed authority")
	}
}
