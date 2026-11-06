package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func predicateContent(text, kind, to string) map[string]any {
	return msg(text, map[string]any{ContentKey: Header{V: 1, Type: "message", From: "bob", Kind: kind, To: []string{to}}})
}

func predicateKeywords(t *testing.T, e *env, words config.WaitKeywords) {
	t.Helper()
	if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error {
		c.WaitKeywords = words
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves both commands reject each predicate's near misses before output/shown state and retain every event for inbox without receipts.
func TestDeliveryPredicates(t *testing.T) {
	type input struct {
		sender  string
		content map[string]any
	}
	note := func(text, kind, to string) input { return input{"@bob:test.local", predicateContent(text, kind, to)} }
	sent := func(sender string) input { return input{sender, predicateContent("ready", "decision", "alice")} }
	subject := func(text, value string) input {
		return input{"@bob:test.local", msg(text, map[string]any{ContentKey: Header{Type: "message", Kind: "decision", Subject: value}})}
	}
	cases := []struct {
		name     string
		flags    []string
		keywords config.WaitKeywords
		bad      []input
		good     input
	}{
		{"regexp", []string{"--match-text", "^READY-[0-9]+$"}, nil,
			[]input{note("READY-x", "decision", "alice"), note("xREADY-1", "decision", "alice"), note("ready-1", "decision", "alice")}, note("READY-17", "decision", "alice")},
		{"regexp full text", []string{"--match-text", "TAIL$"}, nil,
			[]input{note("TAIL followed", "notice", "all")}, note(strings.Repeat("x", 500)+"TAIL", "notice", "all")},
		{"regexp newline", []string{"--match-text", "^first.second$"}, nil,
			[]input{note("first\nsecond", "notice", "all")}, note("first second", "notice", "all")},
		{"regexp explicit modes", []string{"--match-text", "(?is)^first.second$"}, nil,
			[]input{note("FIRST\nTHIRD", "notice", "all")}, note("FIRST\nSECOND", "notice", "all")},
		{"regexp text only", []string{"--match-text", "TASK-56"}, nil,
			[]input{note("- from: TASK-56\n\nother text", "decision", "alice"), subject("other text", "TASK-56")}, note("TASK-56 ready", "decision", "alice")},
		{"sender list first", []string{"--from", "bob, @carol"}, nil,
			[]input{sent("@bob:foreign.test"), sent("@bobx:test.local"), sent("@mallory:test.local")}, sent("@bob:test.local")},
		{"sender list second", []string{"--from", "bob,@carol"}, nil,
			[]input{sent("@carol:foreign.test")}, sent("@carol:test.local")},
		{"sender remote exact", []string{"--from", "@bob:foreign.test"}, nil,
			[]input{sent("@bob:test.local"), sent("@bob:foreign.test.evil")}, sent("@bob:foreign.test")},
		{"kind list first", []string{"--kind", "decision,request"}, nil,
			[]input{note("ready", "notice", "alice"), note("ready", "decision-extra", "alice"), note("ready", "Decision", "alice"), {"@bob:test.local", msg("kind=decision", nil)}, {"@bob:test.local", msg("ready", map[string]any{ContentKey: map[string]any{"kind": "decision", "to": 1}})}}, note("ready", "decision", "alice")},
		{"kind list second", []string{"--kind", "decision,request"}, nil,
			[]input{note("ready", "notice", "alice")}, note("ready", "request", "alice")},
		{"recipient name", []string{"--to-me-only"}, nil,
			[]input{note("ready", "decision", "all"), note("ready", "decision", "alicex"), note("ready", "decision", "@alice:foreign.test"), note("@alice:test.localhost", "decision", "carol")}, note("ready", "decision", "alice")},
		{"recipient full id", []string{"--to-me-only"}, nil,
			[]input{note("ready", "decision", "@alice:test.local.evil")}, note("ready", "decision", "@alice:test.local")},
		{"recipient metadata mention", []string{"--to-me-only"}, nil,
			[]input{{"@bob:test.local", msg("ready", map[string]any{"m.mentions": map[string]any{"user_ids": []string{"@alice:foreign.test"}}})}},
			input{"@bob:test.local", msg("ready", map[string]any{"m.mentions": map[string]any{"user_ids": []string{"@alice:test.local"}}})}},
		{"recipient body mention", []string{"--to-me-only"}, nil,
			[]input{note("@alice:test.localhost", "decision", "carol")}, note("hello @alice:test.local.", "decision", "carol")},
		{"keywords first token", nil, config.WaitKeywords{"LANE-7", "TASK-56"},
			[]input{note("LANE-70", "decision", "alice"), note("xLANE-7", "decision", "alice"), note("lane-7", "decision", "alice"), note("- from: LANE-7\n\nother text", "decision", "alice"), subject("other text", "TASK-560"), {"@bob:test.local", msg("other text", map[string]any{ContentKey: map[string]any{"subject": "TASK-56", "to": 1}})}}, note("(LANE-7).", "decision", "alice")},
		{"keywords second subject", nil, config.WaitKeywords{"LANE-7", "TASK-56"},
			[]input{subject("other text", "TASK-560")}, subject("other text", "TASK-56")},
		{"combined AND", []string{"--match-text", "ready$", "--from", "bob,carol", "--kind", "decision,request", "--to-me-only"}, config.WaitKeywords{"LANE-7", "TASK-56"},
			[]input{note("LANE-7 waiting", "decision", "alice"), {"@mallory:test.local", predicateContent("LANE-7 ready", "decision", "alice")}, note("LANE-7 ready", "notice", "alice"), note("LANE-7 ready", "decision", "carol"), note("LANE-70 ready", "decision", "alice")}, note("TASK-56 ready", "request", "alice")},
	}
	for _, command := range []string{"wait", "watch"} {
		for _, tc := range cases {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				e, room := world(t)
				e.as("alice", "wait", "--timeout", "1ms").ok(t)
				if tc.keywords != nil {
					predicateKeywords(t, e, tc.keywords)
				}
				cursor := e.state("alice").NextBatch
				var rejected []string
				for _, in := range tc.bad {
					id := e.hs.inject(room, in.sender, in.content)
					rejected = append(rejected, id)
					if e.received("alice", id) {
						t.Fatal("fixture already received")
					}
					_, pending := e.pendingEntry("alice", id)
					t.Logf("before filter id=%s pending=%v received=%v", id, pending, e.received("alice", id))
				}
				flags := append([]string{"--room", "proj", "--all", "--json"}, tc.flags...)
				var output string
				var finish func() string
				if command == "wait" {
					// A short wait can time out before its first sync on a loaded
					// host. Repeat it until the cursor moves; every attempt must
					// still time out without output.
					for attempt := 1; ; attempt++ {
						timeout := time.Duration(attempt) * 20 * time.Millisecond
						if timeout > 500*time.Millisecond {
							timeout = 500 * time.Millisecond
						}
						r := e.as("alice", append([]string{"wait", "--timeout", timeout.String()}, flags...)...).ok(t)
						if r.stdout != "{\"type\":\"timeout\"}\n" {
							t.Fatalf("rejected traffic woke wait: %q", r.stdout)
						}
						if e.state("alice").NextBatch != cursor || attempt == 40 {
							break
						}
					}
					finish = func() string {
						return e.as("alice", append([]string{"wait", "--timeout", "2s"}, flags...)...).ok(t).stdout
					}
				} else {
					out := &flushBuffer{}
					e.configure = func(a *App) { a.Stdout = out }
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					done := make(chan result, 1)
					calls := e.hs.SyncCount("@alice:test.local")
					go func() { done <- e.runCtx(ctx, append([]string{"--as", "alice", "watch"}, flags...)...) }()
					e.syncedParked(t, "@alice:test.local", calls+2, done, "rejected traffic")
					if text, _ := out.snapshot(); text != "" {
						t.Fatalf("rejected traffic reached watch: %q", text)
					}
					finish = func() string {
						e.syncedParked(t, "@alice:test.local", calls+3, done, "matching neighbor")
						cancel()
						recv(t, done).ok(t)
						e.configure = nil
						text, _ := out.snapshot()
						return text
					}
				}
				if e.state("alice").NextBatch == cursor {
					t.Fatal("cursor did not advance after retaining rejected traffic")
				}
				for _, id := range rejected {
					entry, ok := e.pendingEntry("alice", id)
					if !ok || entry.Shown || e.received("alice", id) {
						t.Fatalf("rejected entry changed delivery state: pending=%v shown=%v received=%v", ok, entry.Shown, e.received("alice", id))
					}
					t.Logf("after rejection id=%s pending=%v shown=%v received=%v cursor_advanced=%v", id, ok, entry.Shown, e.received("alice", id), e.state("alice").NextBatch != cursor)
				}
				accepted := e.hs.inject(room, tc.good.sender, tc.good.content)
				output = finish()
				var record EventRecord
				if strings.Count(output, "\n") != 1 || json.Unmarshal([]byte(output), &record) != nil || record.EventID != accepted {
					t.Fatalf("matching neighbor delivery = %q, want exactly %s", output, accepted)
				}
				entry, ok := e.pendingEntry("alice", accepted)
				if !ok || !entry.Shown || e.received("alice", accepted) {
					t.Fatal("matched delivery did not retain shown, unreceived mail")
				}
				t.Logf("after match id=%s pending=%v shown=%v received=%v", accepted, ok, entry.Shown, e.received("alice", accepted))
				for _, id := range rejected {
					entry, ok := e.pendingEntry("alice", id)
					if !ok || entry.Shown || e.received("alice", id) {
						t.Fatal("matching delivery consumed or marked a rejected neighbor")
					}
				}
				inbox := e.as("alice", "inbox", "--limit", "50").ok(t).stdout
				for _, id := range append(rejected, accepted) {
					if !strings.Contains(inbox, "id="+id) || !e.received("alice", id) {
						t.Fatalf("inbox did not deliver and receipt retained entry %s", id)
					}
					t.Logf("after inbox id=%s received=%v", id, e.received("alice", id))
				}
				t.Logf("retained %d rejected entries unshown/unreceived and 1 matched entry shown/unreceived; inbox alone receipted all", len(rejected))
			})
		}
	}
}

func predicateSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, root)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Proves empty/invalid CLI selectors fail before any fixture file or HTTP effects, with a valid neighbor for each flag family.
func TestDeliveryPredicatesInvalidFlags(t *testing.T) {
	bad := [][]string{
		{"--match-text", ""}, {"--match-text", "  "}, {"--match-text", "["}, {"--match-text", "(?=x)"},
		{"--match-text", "[", "--match-text", "ok"},
		{"--from", ""}, {"--from", ","}, {"--from", "bob,"}, {"--from", ",bob"}, {"--from", "bob,,carol"},
		{"--from", "bad user"}, {"--from", "@bob:bad server"}, {"--from", "bob", "--from", "carol"},
		{"--kind", ""}, {"--kind", ","}, {"--kind", "decision,"}, {"--kind", "gossip"}, {"--kind", "decision", "--kind", "request"},
		{"--to-me-only="}, {"--to-me-only=perhaps"},
	}
	for _, command := range []string{"wait", "watch"} {
		for _, flags := range bad {
			t.Run(command+"/"+strings.Join(flags, " "), func(t *testing.T) {
				e, _ := world(t)
				before := predicateSnapshot(t, e.dir)
				// Invalid local input must win even with an already canceled context.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				e.runCtx(ctx, append([]string{"--as", "alice", command}, flags...)...).failsWith(t, ExitUsage, "")
				if !reflect.DeepEqual(before, predicateSnapshot(t, e.dir)) || len(e.hs.Calls()) != 0 {
					t.Fatal("invalid predicate caused filesystem or HTTP effects")
				}
			})
		}
	}
}

// Proves malformed identity keywords fail before delivery effects and a restored valid config accepts the same message.
func TestDeliveryPredicatesInvalidConfig(t *testing.T) {
	for _, command := range []string{"wait", "watch"} {
		for _, value := range []string{`null`, `[]`, `[""]`, `[" "]`, `[null]`, `[1]`, `"TASK-56"`, `{}`, `["TASK 56"]`, `["TASK-56",false]`, `["` + strings.Repeat("x", 129) + `"]`} {
			t.Run(command+"/"+value, func(t *testing.T) {
				e, room := world(t)
				path := e.paths().AgentFile("alice")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				malformed := append(bytes.Clone(bytes.TrimSuffix(original, []byte("}"))), []byte(`,"wait_keywords":`+value+`}`)...)
				if err := os.WriteFile(path, malformed, 0o600); err != nil {
					t.Fatal(err)
				}
				before := predicateSnapshot(t, e.dir)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r := e.runCtx(ctx, "--as", "alice", command).failsWith(t, ExitError, "credential JSON")
				if r.stdout != "" || len(e.hs.Calls()) != 0 || !reflect.DeepEqual(before, predicateSnapshot(t, e.dir)) {
					t.Fatal("malformed config produced output, HTTP or file effects")
				}
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				predicateKeywords(t, e, config.WaitKeywords{"TASK-56"})
				e.as("alice", "wait", "--timeout", "1ms").ok(t)
				id := e.hs.inject(room, "@bob:test.local", predicateContent("TASK-56 ready", "decision", "alice"))
				if out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout; !strings.Contains(out, id) || e.received("alice", id) {
					t.Fatal("valid keyword config did not restore unreceipted delivery")
				}
			})
		}
	}
}

// Proves message predicates preserve invite/gap delivery and room boundaries without introducing receipts or consuming an outside-room gap.
func TestDeliveryPredicatesControlEntries(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	predicateKeywords(t, e, config.WaitKeywords{"TASK-56"})
	entries := []config.MailEntry{
		{ID: "invite:outside", Kind: config.MailInvite, Room: "!outside", Label: "#outside", Sender: "@bob:test.local"},
		{ID: "gap:inside", Kind: config.MailGap, Room: "!projRoomID", Label: "#proj", From: "s1", To: "s2"},
		{ID: "gap:outside", Kind: config.MailGap, Room: "!outside", Label: "#outside", From: "s1", To: "s2"},
	}
	if _, err := e.mailbox("alice").Append(entries); err != nil {
		t.Fatal(err)
	}
	out := e.as("alice", "wait", "--room", "proj", "--match-text", "^never$", "--from", "carol", "--kind", "question", "--to-me-only", "--json").ok(t).stdout
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, `"type":"invite"`) || !strings.Contains(out, `"event_id":"gap:inside"`) || strings.Contains(out, "gap:outside") {
		t.Fatalf("control delivery or room selection changed: %q", out)
	}
	for _, want := range entries {
		entry, ok := e.pendingEntry("alice", want.ID)
		if !ok || e.received("alice", want.ID) || entry.Shown != (want.ID != "gap:outside") {
			t.Fatalf("control state changed for %s", want.ID)
		}
	}
}

// Proves filtering retains subscription and followed-thread entries and a later unfiltered wait can show them without any ACK.
func TestDeliveryPredicatesRetainSubscriptionsAndThreads(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	e.subscribe("alice", "--room", "proj", "--kind", "system")
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	beforeSubs, beforeThreads := e.serverSubs("alice"), e.state("alice").Threads
	thread := e.hs.inject(room, "@bob:test.local", msg("skip thread", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}))
	sub := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "skip subscription"))
	// Repeat the short wait until both entries are ingested (a loaded host can
	// time out before the first sync); every attempt must still time out.
	for attempt := 1; attempt <= 40; attempt++ {
		timeout := min(time.Duration(attempt)*20*time.Millisecond, 500*time.Millisecond)
		if out := e.as("alice", "wait", "--match-text", "ready", "--timeout", timeout.String()).ok(t).stdout; out != "timeout\n" {
			t.Fatalf("subscription/thread bypassed text predicate: %q", out)
		}
		_, okThread := e.pendingEntry("alice", thread)
		_, okSub := e.pendingEntry("alice", sub)
		if okThread && okSub {
			break
		}
	}
	for id, rel := range map[string]string{thread: "thread", sub: "sub"} {
		entry, ok := e.pendingEntry("alice", id)
		if !ok || entry.Rel != rel || entry.Shown || e.received("alice", id) {
			t.Fatalf("filtered %s entry lost or consumed", rel)
		}
	}
	if !reflect.DeepEqual(beforeSubs, e.serverSubs("alice")) || !reflect.DeepEqual(beforeThreads, e.state("alice").Threads) {
		t.Fatal("filtering changed subscriptions or followed threads")
	}
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	for _, id := range []string{thread, sub} {
		if !strings.Contains(out, id) || e.received("alice", id) {
			t.Fatal("unfiltered rearm did not show retained unreceived entries")
		}
	}
}
