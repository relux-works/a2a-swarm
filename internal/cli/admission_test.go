package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/admission"
	"github.com/relux-works/a2a-swarm/internal/admission/testdata/policy"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func admissionEntry(id string) config.MailEntry {
	raw, _ := json.Marshal(firstSyncMessage(id, "@"+policy.Leak+":test.local", toMsg("bob", "question", policy.Leak)))
	return config.MailEntry{ID: id, Kind: config.MailEvent, Room: "!private:test.local", Sender: "@" + policy.Leak + ":test.local", TS: 42, Event: raw, Rel: "to"}
}
func admissionSession(t *testing.T, name string) (*App, *session, *policy.Fixture) {
	t.Helper()
	p := config.Paths{Base: t.TempDir()}
	a := NewApp()
	a.Stdout, a.Stderr = io.Discard, io.Discard
	a.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	s := &session{app: a, paths: p, name: "alice", creds: config.Credentials{AdmissionPolicy: name}, client: matrix.New("https://example.invalid", "")}
	if err := s.openAdmission(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAdmission)
	return a, s, policy.Get(filepath.Join(p.Base, "admission", "alice"))
}
func admissionPersist(t *testing.T, s *session, entries []config.MailEntry) {
	t.Helper()
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.persist(context.Background(), batch{entries: entries}, st.NextBatch, "next", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
}
func admissionRead(t *testing.T, path string) []byte {
	t.Helper()
	v, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func setAdmissionPolicy(t *testing.T, p config.Paths, name, selected string) {
	t.Helper()
	file := p.AgentFile(name)
	raw := admissionRead(t, file)
	var c config.Credentials
	if json.Unmarshal(raw, &c) != nil {
		t.Fatal("credentials")
	}
	c.AdmissionPolicy = selected
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Proves none's sync mailbox and State bytes equal the bypass run, including replay and redaction.
func TestAdmissionNoneSyncGolden(t *testing.T) {
	var goldenMailbox, goldenState []byte
	for _, bypass := range []bool{true, false} {
		_, s, _ := admissionSession(t, "none")
		if bypass {
			s.admission = nil
		}
		entries := []config.MailEntry{admissionEntry("$one"), admissionEntry("$two")}
		admissionPersist(t, s, entries)
		admissionPersist(t, s, entries)
		b := batch{redactions: []config.Redaction{{Room: entries[1].Room, EventID: "$two"}}}
		if _, err := s.persist(context.Background(), b, "next", "done", time.Time{}); err != nil {
			t.Fatal(err)
		}
		mailbox := admissionRead(t, s.paths.MailboxFile(s.name))
		state := admissionRead(t, s.paths.StateFile(s.name))
		if bypass {
			goldenMailbox, goldenState = mailbox, state
		} else if !bytes.Equal(mailbox, goldenMailbox) || !bytes.Equal(state, goldenState) {
			t.Fatal("none changed golden delivery bytes")
		}
		if _, err := os.Stat(filepath.Join(s.paths.Base, "admission", s.name, "held.json")); !os.IsNotExist(err) {
			t.Fatal("none wrote policy state")
		}
	}
}

// Proves a prepared post through none sends the bypass's exact request and writes byte-identical follow State.
func TestAdmissionNonePostGolden(t *testing.T) {
	var requestGolden, stateGolden []byte
	w := operationWorldNew(t)
	for _, bypass := range []bool{true, false} {
		a, _, _ := w.app()
		s, err := a.session()
		if err != nil {
			t.Fatal(err)
		}
		if bypass {
			s.admission = nil
			s.client.BeforeSend = nil
		}
		h := Header{V: 1, Type: "message", From: "alice", Topic: "$root", Kind: "notice", At: "2026-01-01T00:00:00Z"}
		content, err := textContent("golden post", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.sendThreadContent(context.Background(), "!room:example.invalid", "room", h, content); err != nil {
			t.Fatal(err)
		}
		state := admissionRead(t, w.paths.StateFile("alice"))
		request := w.requests[len(w.requests)-1]
		if bypass {
			requestGolden, stateGolden = bytes.Clone(request), state
		} else if !bytes.Equal(requestGolden, request) || !bytes.Equal(stateGolden, state) {
			t.Fatal("none changed golden post bytes")
		}
		if err := os.Remove(w.paths.StateFile("alice")); err != nil {
			t.Fatal(err)
		}
		a.closeAdmission()
	}
}

// Proves held release plus a replay after mailbox persistence produces exactly one durable mailbox add.
func TestAdmissionHeldReleaseExactlyOnce(t *testing.T) {
	a, s, f := admissionSession(t, policy.Name)
	f.In = admission.Decision{Verdict: admission.Refused, Retryable: true, Reason: "dependency_unavailable"}
	entry := admissionEntry("$held")
	admissionPersist(t, s, []config.MailEntry{entry})
	mb, err := config.OpenMailbox(s.paths, s.name)
	if err != nil || len(mb.Pending()) != 0 {
		t.Fatal("held reached mailbox")
	}
	f.In = admission.Decision{Verdict: admission.Live, Reason: "allowed"}
	a.afterAdmissionAppend = func() error { return errors.New("interrupted after append") }
	if _, err := s.persist(context.Background(), batch{entries: []config.MailEntry{entry}}, "next", "later", time.Time{}); err == nil {
		t.Fatal("crash witness did not fire")
	}
	a.afterAdmissionAppend = nil
	admissionPersist(t, s, []config.MailEntry{entry})
	admissionPersist(t, s, nil)
	mb, err = config.OpenMailbox(s.paths, s.name)
	if err != nil || len(mb.Pending()) != 1 || mb.Pending()[0].ID != entry.ID {
		t.Fatal("held release lost or duplicated entry")
	}
	raw := admissionRead(t, s.paths.MailboxFile(s.name))
	if strings.Count(string(raw), `"op":"add"`) != 1 {
		t.Fatal("mailbox dedup did not survive held replay")
	}
}

// Proves definite refusal produces no mailbox write and persists only a closed-code count, scoped to this identity.
func TestAdmissionDefiniteAndNoLeaks(t *testing.T) {
	a, s, f := admissionSession(t, policy.Name)
	f.In = admission.Decision{Verdict: admission.Refused, Reason: policy.Leak}
	f.Err = true
	admissionPersist(t, s, []config.MailEntry{admissionEntry("$refused")})
	if _, err := os.Stat(s.paths.MailboxFile(s.name)); !os.IsNotExist(err) {
		t.Fatal("definite refusal wrote mailbox")
	}
	c, err := s.admission.store.Counts(context.Background())
	if err != nil || c["policy_error"] != 1 {
		t.Fatal("missing error count")
	}
	out := new(bytes.Buffer)
	a.Stdout = out
	a.Stderr = out
	a.fail(fmt.Errorf("room %s: %w", policy.Leak, &admissionRefusal{reason: "denied"}))
	raw, _ := json.Marshal(c)
	if strings.Contains(out.String()+string(raw), policy.Leak) {
		t.Fatal("private policy text leaked")
	}
	other := admission.Store{Dir: filepath.Join(s.paths.Base, "admission", "bob")}
	counts, err := other.Counts(context.Background())
	if err != nil || len(counts) != 0 {
		t.Fatal("cross-identity counters")
	}
}

// Proves held/exchange never reach inbox, wait, watch or hook surfaces, on initial and later syncs.
func TestAdmissionPrivateSurfaces(t *testing.T) {
	for _, verdict := range []admission.Verdict{admission.Refused, admission.Exchange} {
		t.Run(fmt.Sprint(verdict), func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			setAdmissionPolicy(t, e.paths(), "alice", policy.Name)
			f := policy.Get(filepath.Join(e.dir, "admission", "alice"))
			f.In = admission.Decision{Verdict: verdict, Retryable: verdict == admission.Refused, Reason: "dependency_unavailable"}
			entry := firstSyncMessage("$private", "@"+policy.Leak+":test.local", toMsg("bob", "question", policy.Leak))
			firstSyncFixture(t, e, room, false, "", firstSyncJoin("@alice:test.local", "$join"), entry)
			outputs := []result{e.hook("alice", "prompt", `{"session_id":"admission"}`).ok(t), e.as("alice", "inbox", "--json").ok(t), e.as("alice", "wait", "--timeout", "10ms", "--json").ok(t)}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			outputs = append(outputs, e.runCtx(ctx, "--as", "alice", "watch", "--json"))
			outputs = append(outputs, e.as("alice", "status", "admission", "--json").ok(t))
			for _, r := range outputs {
				if strings.Contains(r.stdout+r.stderr, policy.Leak) || strings.Contains(r.stdout, "$private") {
					t.Fatalf("private surface leaked: %+v", r)
				}
			}
			if len(e.mailbox("alice").Pending()) != 0 {
				t.Fatal("private item counted unread")
			}
			if verdict == admission.Exchange {
				if raw := admissionRead(t, filepath.Join(e.dir, "admission", "alice", "exchange.jsonl")); !bytes.Contains(raw, []byte(`"event_id":"$private"`)) {
					t.Fatal("exchange not persisted")
				}
			}
		})
	}
}

// Proves policy refusal/error/exchange closes durable UNKNOWN to refused before any server send.
func TestAdmissionOutboundJournalRefused(t *testing.T) {
	for _, mode := range []string{"denied", "exchange", "error", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			w := operationWorldNew(t)
			setAdmissionPolicy(t, w.paths, "alice", policy.Name)
			f := policy.Get(filepath.Join(w.paths.Base, "admission", "alice"))
			f.Out = admission.Decision{Verdict: admission.Refused, Reason: "denied"}
			want := "denied"
			switch mode {
			case "exchange":
				f.Out = admission.Decision{Verdict: admission.Exchange, Reason: "exchange"}
				want = "exchange_outbound"
			case "error":
				f.Err = true
				want = "policy_error"
			case "invalid":
				f.Out.Reason = policy.Leak
				want = "policy_invalid"
			}
			a, out, diag := w.app()
			if code := a.Run(context.Background(), operationArgs()); code != ExitError {
				t.Fatalf("refused post exit %d", code)
			}
			record := w.record()
			if w.sends.Load() != 0 || record.Outcome != "refused" || record.RefusalReason != want || len(record.Request) != 0 || f.OutCalls != 1 {
				t.Fatalf("bad refusal: %+v sends=%d calls=%d", record, w.sends.Load(), f.OutCalls)
			}
			if strings.Contains(out.String()+diag.String(), policy.Leak) {
				t.Fatal("policy text leaked")
			}
			a, _, _ = w.app()
			a.Run(context.Background(), operationArgs())
			if f.OutCalls != 1 || w.sends.Load() != 0 {
				t.Fatal("retained refused record was resent")
			}
		})
	}
}

// Proves each retry of the same transaction invokes the checker again; an earlier Live cannot authorize a later attempt.
func TestAdmissionOutboundRechecked(t *testing.T) {
	w := operationWorldNew(t)
	setAdmissionPolicy(t, w.paths, "alice", policy.Name)
	a, _, _ := w.app()
	defer a.closeAdmission()
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	f := policy.Get(filepath.Join(w.paths.Base, "admission", "alice"))
	content := map[string]any{"body": policy.Leak, "msgtype": "m.text"}
	if _, err := s.client.SendMessageTxnReceipt(context.Background(), "!room:example.invalid", "same-transaction", content); err != nil {
		t.Fatal(err)
	}
	f.Out = admission.Decision{Verdict: admission.Refused, Reason: "denied"}
	if _, err := s.client.WithoutRedirects().SendMessageTxnReceipt(context.Background(), "!room:example.invalid", "same-transaction", content); err == nil {
		t.Fatal("retry reused Live")
	}
	if f.OutCalls != 2 || w.sends.Load() != 1 || f.Requests[0].TransactionID != f.Requests[1].TransactionID || !bytes.Equal(f.Requests[0].Request, f.Requests[1].Request) {
		t.Fatal("retry gate mismatch")
	}
}

// Proves unknown and Nostr-only credential settings refuse before any HTTP and retain a closed diagnostic.
func TestAdmissionSessionStartupRefused(t *testing.T) {
	for _, name := range []string{"unregistered-" + policy.Leak, policy.NostrName} {
		t.Run(name, func(t *testing.T) {
			w := operationWorldNew(t)
			setAdmissionPolicy(t, w.paths, "alice", name)
			a, out, diag := w.app()
			if code := a.Run(context.Background(), []string{"whoami"}); code != ExitError || w.calls.Load() != 0 || strings.Contains(out.String()+diag.String(), policy.Leak) {
				t.Fatal("startup did not fail closed")
			}
		})
	}
}

// Proves changing the host policy to none releases previously held mail without losing mailbox dedup.
func TestAdmissionNoneReleasesExistingHeld(t *testing.T) {
	_, s, f := admissionSession(t, policy.Name)
	f.In = admission.Decision{Verdict: admission.Refused, Retryable: true, Reason: "dependency_unavailable"}
	entry := admissionEntry("$held")
	admissionPersist(t, s, []config.MailEntry{entry})
	c, err := admission.Open("none", admission.Config{Identity: s.name, StateDir: s.admission.store.Dir})
	if err != nil {
		t.Fatal(err)
	}
	s.admission = &identityAdmission{checker: c, store: s.admission.store, none: true}
	defer c.Close()
	admissionPersist(t, s, []config.MailEntry{entry})
	mb, err := config.OpenMailbox(s.paths, s.name)
	if err != nil || len(mb.Pending()) != 1 {
		t.Fatal("none lost existing held mail")
	}
}

// Proves an ordinary post refusal returns only the code, without wrapping the private room label.
func TestAdmissionOutboundClosedDiagnostic(t *testing.T) {
	w := operationWorldNew(t)
	setAdmissionPolicy(t, w.paths, "alice", policy.Name)
	a, _, _ := w.app()
	defer a.closeAdmission()
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	f := policy.Get(filepath.Join(w.paths.Base, "admission", "alice"))
	f.Out = admission.Decision{Verdict: admission.Refused, Reason: "denied"}
	_, err = s.sendThreadContent(context.Background(), "!room:example.invalid", policy.Leak, Header{}, map[string]any{"msgtype": "m.text", "body": policy.Leak})
	if err == nil || err.Error() != "admission: denied" || w.sends.Load() != 0 {
		t.Fatalf("unsafe refusal: %v", err)
	}
}
