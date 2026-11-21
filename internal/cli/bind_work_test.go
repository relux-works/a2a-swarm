//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type terminalWorkFixture struct {
	a       *App
	s       *session
	initial config.TerminalBinding
	errLog  *bytes.Buffer
}

func newTerminalWorkFixture(tb testing.TB) *terminalWorkFixture {
	tb.Helper()
	target, _, _ := usabilityTarget(tb, "codex")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sync"):
			writeJSON(w, 200, map[string]any{"next_batch": "s0", "rooms": map[string]any{}})
		case strings.Contains(r.URL.Path, "/account_data/"):
			writeJSON(w, 200, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			writeJSON(w, 200, map[string]any{"joined_rooms": []string{}})
		default:
			tb.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
			writeErr(w, 404, "M_NOT_FOUND", "fixture route unavailable")
		}
	}))
	tb.Cleanup(srv.Close)
	// Wake-mute admission refuses noncanonical roots; keep this fixture canonical
	// so the work test reaches its cycle and budget assertions.
	p := config.Paths{Base: canonicalRoot(tb)}
	now := time.Now().UTC()
	errLog := &bytes.Buffer{}
	a := &App{as: "fixture", Now: func() time.Time { return now }, Getenv: func(string) string { return "" }, Stdout: io.Discard, Stderr: errLog, HTTP: srv.Client()}
	client := matrix.New(srv.URL, "")
	client.HTTP = srv.Client()
	s := &session{app: a, paths: p, name: "fixture", creds: config.Credentials{UserID: "@fixture:fixture.invalid"}, client: client, server: "fixture.invalid"}
	client.BeforeRequest = s.checkConnection
	fake := &muteTerminal{}
	a.terminalFixture = func(b config.TerminalBinding) terminalEffects { fake.b = b; return fake }
	initial := config.TerminalBinding{Generation: "fixture", SessionID: "fixture", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, DaemonPID: os.Getpid(), DaemonBirth: target.OwnerBirth, Target: target, Interval: time.Second, RetryMax: time.Second, MaxWakes: 6}
	st := config.State{NextBatch: "s0", Terminal: &initial}
	for i := 0; i < 2000; i++ {
		st.ThreadEvents = append(st.ThreadEvents, config.ThreadEvent{Room: fmt.Sprintf("!%043d", i%16), EventID: fmt.Sprintf("$%043d", i), Root: fmt.Sprintf("$%043d", i%64)})
	}
	for i := 0; i < 500; i++ {
		st.Threads = append(st.Threads, fmt.Sprintf("$%063d", i))
		st.Muted = append(st.Muted, fmt.Sprintf("$%063d", i+500))
	}
	if err := config.SaveState(p, s.name, st); err != nil {
		tb.Fatal(err)
	}
	raw, err := os.ReadFile(p.StateFile(s.name))
	if err != nil {
		tb.Fatal(err)
	}
	if len(raw) < 400000 || len(raw) > 580000 {
		tb.Fatalf("State fixture size %d", len(raw))
	}
	var mail bytes.Buffer
	ids := []string{}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("$mail%05d", i)
		ids = append(ids, id)
		event, _ := json.Marshal(map[string]any{"type": "m.room.message", "content": map[string]string{"body": strings.Repeat("x", 9800)}})
		line, _ := json.Marshal(map[string]any{"op": "add", "entry": config.MailEntry{ID: id, Kind: config.MailEvent, Room: "!fixture:fixture.invalid", Event: event}})
		mail.Write(line)
		mail.WriteByte('\n')
	}
	line, _ := json.Marshal(map[string]any{"op": "rcpt", "ids": ids})
	mail.Write(line)
	mail.WriteByte('\n')
	if mail.Len() > 2<<20 {
		tb.Fatalf("mail fixture too large: %d", mail.Len())
	}
	mail.Write(bytes.Repeat([]byte(" "), (2<<20)-mail.Len()-len(`{"op":"padding"}`)-1))
	mail.WriteString("{\"op\":\"padding\"}\n")
	if err := os.WriteFile(p.MailboxFile(s.name), mail.Bytes(), 0600); err != nil {
		tb.Fatal(err)
	}
	tb.Logf("synthetic fixture: State=%d bytes, thread_events=2000, mailbox=%d bytes", len(raw), mail.Len())
	return &terminalWorkFixture{a, s, initial, errLog}
}

func (f *terminalWorkFixture) full(tb testing.TB) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cycles := 0
	f.errLog.Reset()
	reason, err := f.a.runTerminalWithRefresh(ctx, f.s, "fixture", f.initial, func() { cycles++; cancel() })
	if err != nil || reason != "cancelled" || cycles != 1 || f.s.terminalUndelivered != 0 || f.errLog.Len() != 0 {
		tb.Fatalf("full cycle: reason=%s err=%v cycles=%d undelivered=%d diagnostics=%q", reason, err, cycles, f.s.terminalUndelivered, f.errLog.String())
	}
}
func (f *terminalWorkFixture) quiet(tb testing.TB) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if reason := f.a.quiescentExchange(ctx, f.s, &quiescentWriting{}); reason != "" {
		tb.Fatalf("exchange: %s", reason)
	}
}

type terminalWorkCounts struct{ Decodes, Encodes, Digests, Mailbox, Bytes int64 }

func terminalWorkDelta(before, after config.StateDecodeStats) terminalWorkCounts {
	return terminalWorkCounts{after.UnmarshalStatePasses - before.UnmarshalStatePasses, after.StateEncodes - before.StateEncodes, after.StateDigests - before.StateDigests, after.MailboxDecodes - before.MailboxDecodes, after.StateWriteBytes + after.MailboxWriteBytes - before.StateWriteBytes - before.MailboxWriteBytes}
}

// TestTerminalCycleWork bounds warmed cycle/exchange work and requires the quiescent exchange to load State and verify a writer seal.
func TestTerminalCycleWork(t *testing.T) {
	f := newTerminalWorkFixture(t)
	f.full(t)
	for _, tc := range []struct {
		name string
		run  func(testing.TB)
	}{{"full", f.full}, {"quiescent", f.quiet}} {
		t.Run(tc.name, func(t *testing.T) {
			before := config.ReadStateDecodeStats()
			tc.run(t)
			after := config.ReadStateDecodeStats()
			got := terminalWorkDelta(before, after)
			loads := after.LoadStateCalls - before.LoadStateCalls
			checks := after.WriterSealChecks - before.WriterSealChecks
			t.Logf("work: %+v; State loads=%d seal checks=%d", got, loads, checks)
			if tc.name == "quiescent" && (loads < 1 || checks < 1) {
				t.Fatalf("quiescent exchange did not reach State load and seal verification: loads=%d checks=%d", loads, checks)
			}
			limit := terminalWorkBudget(tc.name)
			if err := checkTerminalWork(got, limit); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func terminalWorkBudget(name string) terminalWorkCounts {
	if name == "full" {
		return terminalWorkCounts{2, 6, 4, 0, 960000}
	}
	return terminalWorkCounts{0, 0, 1, 0, 0}
}

// TestTerminalCycleWorkBudgetControl proves the checker accepts each frozen budget and rejects one-unit overruns of every measured dimension.
func TestTerminalCycleWorkBudgetControl(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget terminalWorkCounts
	}{{"full", terminalWorkCounts{2, 6, 4, 0, 960000}}, {"quiescent", terminalWorkCounts{0, 0, 1, 0, 0}}} {
		limit := terminalWorkBudget(tc.name)
		if err := checkTerminalWork(tc.budget, limit); err != nil {
			t.Fatal("valid budget refused", err)
		}
		for i := range 5 {
			got := tc.budget
			fields := []*int64{&got.Decodes, &got.Encodes, &got.Digests, &got.Mailbox, &got.Bytes}
			*fields[i]++
			if checkTerminalWork(got, limit) == nil {
				t.Fatalf("%s budget silently accepts dimension %d above its frozen bound", tc.name, i)
			}
		}
	}
}

func checkTerminalWork(got, limit terminalWorkCounts) error {
	if got.Decodes > limit.Decodes || got.Encodes > limit.Encodes || got.Digests > limit.Digests || got.Mailbox > limit.Mailbox || got.Bytes > limit.Bytes {
		return fmt.Errorf("work %+v exceeds %+v", got, limit)
	}
	return nil
}

// TestTerminalCycleWorkIncrementalMailbox proves a warmed full cycle over a
// 30 MB mailbox parses only the bytes appended since the previous cycle:
// appending about 1 KB costs at most 64 KB of parsed mailbox bytes and no
// full mailbox decode, while the warmed control cycle already ran on the
// same 30 MB file with nothing appended.
func TestTerminalCycleWorkIncrementalMailbox(t *testing.T) {
	f := newTerminalWorkFixture(t)
	var mail bytes.Buffer
	ids := []string{}
	body := strings.Repeat("x", 9800)
	for i := 0; i < 3200; i++ {
		id := fmt.Sprintf("$mail%05d", i)
		ids = append(ids, id)
		event, _ := json.Marshal(map[string]any{"type": "m.room.message", "content": map[string]string{"body": body}})
		line, _ := json.Marshal(map[string]any{"op": "add", "entry": config.MailEntry{ID: id, Kind: config.MailEvent, Room: "!fixture:fixture.invalid", Event: event}})
		mail.Write(line)
		mail.WriteByte('\n')
	}
	line, _ := json.Marshal(map[string]any{"op": "rcpt", "ids": ids})
	mail.Write(line)
	mail.WriteByte('\n')
	if mail.Len() < 30<<20 {
		t.Fatalf("large mailbox fixture = %d bytes, want at least %d", mail.Len(), 30<<20)
	}
	if err := os.WriteFile(f.s.paths.MailboxFile(f.s.name), mail.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f.full(t) // warmed control: first cycle on the 30 MB mailbox
	path := f.s.paths.MailboxFile(f.s.name)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	pad := append([]byte(`{"op":"padding"}`), bytes.Repeat([]byte(" "), 1024)...)
	pad = append(pad, '\n')
	if _, err := file.Write(pad); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before := config.ReadStateDecodeStats()
	f.full(t)
	after := config.ReadStateDecodeStats()
	parsed := after.MailboxParseBytes - before.MailboxParseBytes
	t.Logf("incremental cycle: parsed=%d bytes appended=%d file=%d", parsed, len(pad), mail.Len())
	if after.MailboxDecodes != before.MailboxDecodes {
		t.Fatal("appended cycle took a full mailbox decode")
	}
	if parsed > 64<<10 {
		t.Fatalf("appended cycle parsed %d mailbox bytes, want near the %d appended", parsed, len(pad))
	}
	if after.MailboxIncrementalReads <= before.MailboxIncrementalReads {
		t.Fatal("appended cycle was not served incrementally")
	}
}

// BenchmarkTerminalCycleWork measures one warmed full cycle or cursor-stable quiescent exchange, including the in-process HTTP and file paths.
func BenchmarkTerminalCycleWork(b *testing.B) {
	for _, name := range []string{"full", "quiescent"} {
		b.Run(name, func(b *testing.B) {
			f := newTerminalWorkFixture(b)
			f.full(b)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if name == "full" {
					f.full(b)
				} else {
					f.quiet(b)
				}
			}
		})
	}
}
