package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ownerRoundTrip func(*http.Request) (*http.Response, error)

func (f ownerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ownerActiveFixture(t *testing.T) (*env, *ownerDMConfig, ownerDMFiles) {
	t.Helper()
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	ownerActivate(t, e, f, c)
	// Remove setup mirror journal only in this isolated fixture, never production.
	if err := os.Remove(f.path("journal")); err != nil {
		t.Fatal(err)
	}
	return e, ownerLoad(t, f), f
}

// Test 11: exact pinned capture bytes parse for both runtimes; absent/empty/
// mistyped selected fields never send. A FIFO transcript sentinel would block
// any accidental read, so the bounded invocation also proves it is not opened.
func TestOwnerDMPinnedPayloads(t *testing.T) {
	fixtures := map[string]string{"claude": "claude-code-2.1.309-stop.json", "codex": "codex-cli-0.163.0-stop.json"}
	hashes := map[string]string{"claude": "13788893ec64890b9912576bd379e59d7876aada83092e8bbb199916b14a735b", "codex": "ec6f7f738b7ad3b9e2da43d043da0b31a0a528985e70805681da073c1daa6a6b"}
	for runtime, file := range fixtures {
		t.Run(file, func(t *testing.T) {
			b := mustOwnerBytes(t, filepath.Join("testdata", "stop-payloads", file))
			hash := sha256.Sum256(b)
			if hex.EncodeToString(hash[:]) != hashes[runtime] {
				t.Fatal("pinned fixture drift")
			}
			e, c, f := ownerActiveFixture(t)
			before := len(ownerBodies(e, c.Room))
			ownerMirror(t, e, runtime, b)
			if got := ownerBodies(e, c.Room); len(got) != before+1 || got[len(got)-1] != "ok" {
				t.Fatalf("capture not mirrored: %q", got)
			}
			var control map[string]any
			json.Unmarshal(b, &control)
			control[map[string]string{"claude": "prompt_id", "codex": "turn_id"}[runtime]] = "valid-fifo"
			control["transcript_path"] = ownerTranscriptSentinel(t)
			ownerMirrorBounded(t, e, runtime, jsonBytes(t, control))
			if got := ownerBodies(e, c.Room); len(got) != before+2 || got[len(got)-1] != "ok" {
				t.Fatal("valid payload ignored for transcript path")
			}
			for _, field := range []string{"last_assistant_message", "session_id", map[string]string{"claude": "prompt_id", "codex": "turn_id"}[runtime]} {
				for _, bad := range []any{nil, "", 7, false, map[string]any{}, "__missing__"} {
					t.Run(field+fmt.Sprint(bad), func(t *testing.T) {
						e, c, f := ownerActiveFixture(t)
						var obj map[string]any
						json.Unmarshal(b, &obj)
						obj[field] = bad
						if marker, ok := bad.(string); ok && marker == "__missing__" {
							delete(obj, field)
						}
						obj["transcript_path"] = ownerTranscriptSentinel(t)
						payload := jsonBytes(t, obj)
						before := len(ownerBodies(e, c.Room))
						started := time.Now()
						ownerMirrorBounded(t, e, runtime, payload)
						if time.Since(started) > 500*time.Millisecond {
							t.Fatal("payload fallback may have opened transcript")
						}
						if len(ownerBodies(e, c.Room)) != before {
							t.Fatal("invalid payload sent")
						}
						entries, err := f.journal(time.Now())
						if err != nil || len(entries) != 1 || entries[0].State != "skipped:no-payload" {
							t.Fatalf("skip record: %+v %v", entries, err)
						}
					})
				}
			}
			_ = f
		})
	}
}

// Test 16: original-message redaction catches each named class even across
// chunk boundaries, thresholds count original spans, and Unicode character
// caps/chunks preserve clean content and state the exact omitted count.
func TestOwnerDMContent(t *testing.T) {
	secrets := []string{"-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----", "Authorization: Bearer fixture-bearer", "syt_fixture", "mat_fixture", "ghp_fixture", "gho_fixture", "ghs_fixture", "ghu_fixture", "github_pat_fixture", "xoxa-fixture", "xoxb-fixture", "xoxp-fixture", "xoxr-fixture", "AIzaFixture", "sk-fixture", "AKIAABCDEFGHIJKLMNOP", "ASIAABCDEFGHIJKLMNOP", "https://user:fixture@example.invalid/path", `password=fixture`, `"passwd":"fixture"`, `token: fixture`, `secret='fixture'`, `api_key="fixture"`, `access_token: fixture`, `refresh_token=fixture`, `client_secret: fixture`}
	for _, secret := range secrets {
		t.Run(strings.Split(secret, "\n")[0], func(t *testing.T) {
			input := strings.Repeat("x", 3998) + " " + secret
			got := strings.Join(ownerDMContent(input), "")
			if strings.Contains(got, secret) || !strings.Contains(got, "[redacted]") {
				t.Fatalf("not redacted: %q", got[len(got)-100:])
			}
		})
	}
	clean := strings.Repeat("界", 8001)
	chunks := ownerDMContent(clean)
	if len(chunks) != 3 || strings.Join(chunks, "") != clean {
		t.Fatal("clean Unicode changed")
	}
	for _, c := range chunks {
		if len([]rune(c)) > 4000 {
			t.Fatal("chunk too large")
		}
	}
	withheld := ownerDMContent(strings.Repeat(" ghp_fixture", 21))
	if len(withheld) != 1 || withheld[0] != "final message withheld: too many secret-like strings" {
		t.Fatal("threshold not withheld")
	}
	if strings.Contains(strings.Join(ownerDMContent(strings.Repeat(" ghp_fixture", 20)), ""), "withheld") {
		t.Fatal("20 withheld")
	}
	capped := ownerDMContent(strings.Repeat("界", 32001))
	if len(capped) > 8 || !strings.HasSuffix(strings.Join(capped, ""), "\n[omitted 4001 characters]\n") {
		t.Fatal("cap/omission")
	}
}

// Tests 10/15: audience rejects every forbidden state and journals refusal
// without unbinding; restoring valid current state permits the next turn.
func TestOwnerDMAudience(t *testing.T) {
	for _, bad := range []string{"public", "knock", "third-joined", "third-invited", "owner-absent", "identity-invited", "world-readable", "encrypted", "owner-left", "identity-left"} {
		t.Run(bad, func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			before := len(ownerBodies(e, c.Room))
			e.hs.mu.Lock()
			room := e.hs.rooms[c.Room]
			switch bad {
			case "public", "knock":
				room.state["m.room.join_rules|"] = jsonBytes(t, map[string]string{"join_rule": bad})
			case "third-joined":
				room.members["@carol:"+testServer] = "join"
			case "third-invited":
				room.members["@carol:"+testServer] = "invite"
			case "owner-absent", "owner-left":
				delete(room.members, c.Owner)
				delete(room.state, "m.room.member|"+c.Owner)
			case "identity-invited":
				room.members["@bob:"+testServer] = "invite"
			case "identity-left":
				delete(room.members, "@bob:"+testServer)
			case "world-readable":
				room.state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"world_readable"}`)
			case "encrypted":
				room.state["m.room.encryption|"] = json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)
			}
			e.hs.mu.Unlock()
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "bad-state", "x"))
			if len(ownerBodies(e, c.Room)) != before || !ownerLoad(t, f).Active {
				t.Fatal("audience sent or unbound")
			}
			entries, err := f.journal(time.Now())
			if err != nil || len(entries) != 1 || entries[0].State != "refused:audience" {
				t.Fatalf("journal %+v %v", entries, err)
			}
			status := e.as("bob", "owner-dm", "status").ok(t)
			if strings.Contains(status.stdout, "audience: valid") {
				t.Fatal(status.stdout)
			}
			if bad == "encrypted" && !strings.Contains(status.stdout, "v1 cannot read or post") {
				t.Fatal(status.stdout)
			}
			e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
			cause := "audience"
			if bad == "encrypted" {
				cause = "encrypted"
			}
			e.as("bob", "owner-dm", "set", "--room", c.Room).failsWith(t, ExitError, cause)
			e.hs.mu.Lock()
			room.members = map[string]string{"@bob:" + testServer: "join", c.Owner: "join"}
			room.state["m.room.member|"+c.Owner] = json.RawMessage(`{"membership":"join"}`)
			room.state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"invite"}`)
			room.state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"joined"}`)
			delete(room.state, "m.room.encryption|")
			e.hs.mu.Unlock()
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "restored", "valid"))
			if len(ownerBodies(e, c.Room)) != before+1 {
				t.Fatal("restored room did not resume")
			}
		})
	}
}

// Tests 13/14: same-key hooks serialize to one send, pending never resumes,
// partial sends never retry, and the config writer waits through all chunks
// before applying its requested transition (ensure retains the active generation).
// These are isolated fake-network schedules.
func TestOwnerDMJournalAndSynchronization(t *testing.T) {
	t.Run("same-key", func(t *testing.T) {
		e, c, f := ownerActiveFixture(t)
		before := len(ownerBodies(e, c.Room))
		payload := ownerPayload(t, "codex", "one", "hello")
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); ownerMirror(t, e, "codex", payload) }()
		}
		wg.Wait()
		if len(ownerBodies(e, c.Room)) != before+1 {
			t.Fatal("concurrent duplicate sent")
		}
		entries, err := f.journal(time.Now())
		if err != nil || len(entries) != 1 || entries[0].State != "sent 1/1" {
			t.Fatal(entries, err)
		}
		entries[0].State = "pending"
		if err = f.saveJournal(context.Background(), entries); err != nil {
			t.Fatal(err)
		}
		ownerMirror(t, e, "codex", payload)
		if len(ownerBodies(e, c.Room)) != before+1 {
			t.Fatal("pending resumed")
		}
	})
	t.Run("partial", func(t *testing.T) {
		e, c, f := ownerActiveFixture(t)
		before := len(ownerBodies(e, c.Room))
		base := e.hs.srv.Client().Transport
		var sends atomic.Int32
		e.configure = func(a *App) {
			a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") {
					if sends.Add(1) == 2 {
						return nil, fmt.Errorf("bounded injected send failure")
					}
				}
				return base.RoundTrip(r)
			})}
		}
		payload := ownerPayload(t, "claude", "partial", strings.Repeat("z", 9000))
		ownerMirror(t, e, "claude", payload)
		ownerMirror(t, e, "claude", payload)
		if sends.Load() != 2 || len(ownerBodies(e, c.Room)) != before+1 {
			t.Fatal("partial retried")
		}
		entries, err := f.journal(time.Now())
		if err != nil || len(entries) != 1 || entries[0].State != "partial 1/3:send" {
			t.Fatal(entries, err)
		}
	})
	for _, action := range []string{"unset", "ensure", "set"} {
		t.Run("writer-"+action, func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			before := len(ownerBodies(e, c.Room))
			base := e.hs.srv.Client().Transport
			entered := make(chan struct{})
			release := make(chan struct{})
			var sends atomic.Int32
			e.configure = func(a *App) {
				a.ownerConfirm = func(string) error { return nil }
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") {
						if sends.Add(1) == 2 {
							close(entered)
							select {
							case <-release:
							case <-r.Context().Done():
								return nil, r.Context().Err()
							}
						}
					}
					return base.RoundTrip(r)
				})}
			}
			mirrorDone := make(chan struct{})
			go func() {
				ownerMirror(t, e, "claude", ownerPayload(t, "claude", "multi", strings.Repeat("q", 9000)))
				close(mirrorDone)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("send not entered")
			}
			writer := make(chan result, 1)
			go func() {
				args := []string{"owner-dm", action}
				if action == "set" {
					args = append(args, "--room", c.Room)
				}
				writer <- e.as("bob", args...)
			}()
			select {
			case <-writer:
				t.Fatal("writer interleaved before send finished")
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			<-mirrorDone
			(<-writer).ok(t)
			sentAtReturn := sends.Load()
			extra := 0
			if action == "set" {
				extra = 1
			}
			if sentAtReturn != int32(3+extra) || len(ownerBodies(e, c.Room)) != before+3+extra {
				t.Fatal("unchanged generation not fully sent")
			}
			got := ownerLoad(t, f)
			if action == "ensure" {
				if got.Generation != c.Generation || got.Room != c.Room {
					t.Fatal("active ensure repinned")
				}
			} else if got.Generation <= c.Generation {
				t.Fatal("writer did not advance generation")
			}
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "late", "old"))
			if sends.Load() != sentAtReturn {
				t.Fatal("old generation sent after writer")
			}
		})
	}
}

// Tests 13/15: lock timeout, journal read/write refusal, HTTP errors and
// deadline expiry cannot alter native hook output, receipts or cursor; valid
// unblocked Stop is a positive control. No failure record is claimed for an
// unwritable journal; a durable pending record is not retried after timeout.
func TestOwnerDMFailureIsolation(t *testing.T) {
	for _, fault := range []string{"none", "http", "timeout", "lock", "journal-mode", "journal-dir"} {
		t.Run(fault, func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			before := len(ownerBodies(e, c.Room))
			base := e.hs.srv.Client().Transport
			// Quiet native delivery is primed before introducing the mirror fault.
			payload := ownerPayload(t, "claude", "isolation", "final")
			e.hookRT("claude", "bob", "prompt", string(payload)).ok(t)
			before = len(ownerBodies(e, c.Room))
			os.Remove(f.path("journal"))
			baseline := mustOwnerBytes(t, e.paths().StateFile("bob"))
			mailBefore, err := os.ReadFile(e.paths().MailboxFile("bob"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var release func()
			switch fault {
			case "http":
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") {
							return nil, fmt.Errorf("bounded HTTP fault")
						}
						return base.RoundTrip(r)
					})}
				}
			case "timeout":
				e.configure = func(a *App) {
					a.ownerMirrorBudget = 500 * time.Millisecond
					a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/send/m.room.message/a2a-odm-") {
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						return base.RoundTrip(r)
					})}
				}
			case "lock":
				var err error
				release, err = f.lock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				e.configure = func(a *App) { a.ownerMirrorBudget = 500 * time.Millisecond }
			case "journal-mode":
				os.WriteFile(f.path("journal"), []byte("private"), 0400)
			case "journal-dir":
				os.Mkdir(f.path("journal"), 0700)
			}
			r := e.hookRT("claude", "bob", "stop", string(payload)).ok(t)
			if r.stdout != "" {
				t.Fatal("mirror changed native stdout", r.stdout)
			}
			// Compare the state fields that delivery owns, excluding the lease heartbeat.
			var old, newState map[string]any
			json.Unmarshal(baseline, &old)
			json.Unmarshal(mustOwnerBytes(t, e.paths().StateFile("bob")), &newState)
			for _, key := range []string{"next_batch", "pending", "acked", "threads", "muted", "subscriptions", "hook_sessions"} {
				if string(jsonBytes(t, old[key])) != string(jsonBytes(t, newState[key])) {
					t.Fatalf("native %s changed", key)
				}
			}
			mailAfter, err := os.ReadFile(e.paths().MailboxFile("bob"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if string(mailAfter) != string(mailBefore) {
				t.Fatal("mirror changed mailbox/receipts")
			}
			count := len(ownerBodies(e, c.Room))
			if fault == "none" {
				if count != before+1 {
					entries, err := f.journal(time.Now())
					t.Fatalf("positive mirror missing: count=%d before=%d journal=%+v err=%v stdout=%q stderr=%q", count, before, entries, err, r.stdout, r.stderr)
				}
			} else if count != before {
				t.Fatalf("fault sent: %d vs %d", count, before)
			}
		})
	}
}

// Test 12: an a2a-blocked Stop sends nothing; after inbox receipts it mirrors
// once. Same-key replay is suppressed; equal text in another turn and changed
// text in the same turn are separate keys (other hooks may block independently).
func TestOwnerDMStopSelection(t *testing.T) {
	e, c, _ := ownerActiveFixture(t)
	e.sequentialSessions()
	payload := ownerPayload(t, "claude", "turn", "same text")
	e.hookRT("claude", "bob", "prompt", string(payload)).ok(t)
	e.hs.inject(c.Room, c.Owner, map[string]any{"msgtype": "m.text", "body": "mail control"})
	before := len(ownerBodies(e, c.Room))
	blocked := e.hookRT("claude", "bob", "stop", string(payload)).ok(t)
	if !strings.Contains(blocked.stdout, `"decision":"block"`) || len(ownerBodies(e, c.Room)) != before {
		t.Fatal("blocked stop mirrored", blocked)
	}
	e.as("bob", "inbox").ok(t)
	r := e.hookRT("claude", "bob", "stop", string(payload)).ok(t)
	if r.stdout != "" || len(ownerBodies(e, c.Room)) != before+1 {
		t.Fatal("unblocked stop missing mirror", r)
	}
	e.hookRT("claude", "bob", "stop", string(payload)).ok(t)
	if len(ownerBodies(e, c.Room)) != before+1 {
		t.Fatal("same-key replay")
	}
	e.hookRT("claude", "bob", "stop", string(ownerPayload(t, "claude", "another-turn", "same text"))).ok(t)
	e.hookRT("claude", "bob", "stop", string(ownerPayload(t, "claude", "turn", "changed final"))).ok(t)
	if len(ownerBodies(e, c.Room)) != before+3 {
		t.Fatal("different turns/text collapsed")
	}
}

// Test 13: journal retention is bounded by 256 entries and 7 days. A retained
// key never replays; after real eviction the same Matrix transaction ID is
// reused, and a simulated expired server transaction window permits delivery.
func TestOwnerDMJournalRetention(t *testing.T) {
	e, c, f := ownerActiveFixture(t)
	payload := ownerPayload(t, "claude", "evict", "hello")
	before := len(ownerBodies(e, c.Room))
	ownerMirror(t, e, "claude", payload)
	entries, err := f.journal(time.Now())
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	original := entries[0]
	ownerMirror(t, e, "claude", payload)
	if len(ownerBodies(e, c.Room)) != before+1 {
		t.Fatal("retained replay")
	}
	for i := 0; i < 256; i++ {
		entries = append(entries, ownerDMJournalEntry{Key: fmt.Sprint(i), State: "sent 1/1", At: time.Now()})
	}
	if err = f.saveJournal(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	e.hs.mu.Lock()
	e.hs.txns = map[string]string{}
	e.hs.mu.Unlock()
	ownerMirror(t, e, "claude", payload)
	if len(ownerBodies(e, c.Room)) != before+2 {
		t.Fatal("evicted key not eligible again")
	}
	original.At = time.Now().Add(-7*24*time.Hour - time.Second)
	if err = f.saveJournal(context.Background(), []ownerDMJournalEntry{original}); err != nil {
		t.Fatal(err)
	}
	entries, err = f.journal(time.Now())
	if err != nil || len(entries) != 0 {
		t.Fatal("old entry retained", entries, err)
	}
}

// Test 13: failure to create/fsync the initial pending journal (after a valid
// journal read and successful lock) prevents sending. Restoring write access
// allows the identical previously unclaimed key to send.
func TestOwnerDMPendingWriteFence(t *testing.T) {
	e, c, f := ownerActiveFixture(t)
	before := len(ownerBodies(e, c.Room))
	dir := filepath.Dir(f.base)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	payload := ownerPayload(t, "claude", "pending-write", "hello")
	ownerMirror(t, e, "claude", payload)
	if len(ownerBodies(e, c.Room)) != before {
		t.Fatal("sent without durable pending")
	}
	if _, err := os.Stat(f.path("journal")); !os.IsNotExist(err) {
		t.Fatal("claims a failure record despite unwritable journal")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ownerMirror(t, e, "claude", payload)
	if len(ownerBodies(e, c.Room)) != before+1 {
		t.Fatal("valid write control not sent")
	}
}

func ownerMirrorBounded(t *testing.T, e *env, runtime string, payload json.RawMessage) {
	t.Helper()
	done := make(chan struct{})
	go func() { ownerMirror(t, e, runtime, payload); close(done) }()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("payload mirror opened a blocking transcript or exceeded its bounded deadline")
	}
}

// Keeper disposition: confirmation failure leaves activation standing, is
// journaled once, and cannot change the native Stop result. A later turn can mirror.
// A blocked confirmation shares the mirror's total deadline, rather than adding one.
func TestOwnerDMConfirmationFailureAndBudget(t *testing.T) {
	for _, fault := range []string{"send", "deadline"} {
		t.Run(fault, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			c := ownerBind(t, e, room, f)
			ownerJoin(e, c, c.Owner)
			transport := e.hs.srv.Client().Transport
			var attempts atomic.Int32
			e.configure = func(a *App) {
				a.ownerMirrorBudget = 500 * time.Millisecond
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "/send/") {
						attempts.Add(1)
						if fault == "deadline" {
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						return nil, fmt.Errorf("injected confirmation failure")
					}
					return transport.RoundTrip(r)
				})}
			}
			e.stdin = string(ownerPayload(t, "claude", "confirm-failure", "hello"))
			start := time.Now()
			r := e.as("bob", "hook", "stop", "--runtime", "claude").ok(t)
			if r.stdout != "" || time.Since(start) > time.Second || !ownerLoad(t, f).Active || attempts.Load() != 1 {
				t.Fatal("activation/output/deadline", r, attempts.Load())
			}
			entries, err := f.journal(time.Now())
			if err != nil || len(entries) == 0 || entries[0].Key != fmt.Sprintf("%d|confirm", c.Generation) {
				t.Fatal(entries, err)
			}
			if fault == "send" && entries[0].State != "failed:confirm" {
				t.Fatal(entries)
			}
			// Deadline may leave durable pending; no retry is promised in either state.
			e.configure = nil
			e.stdin = string(ownerPayload(t, "claude", "after-confirm", "control"))
			e.as("bob", "hook", "stop", "--runtime", "claude").ok(t)
			bodies := ownerBodies(e, c.Room)
			if len(bodies) != 1 || bodies[0] != "control" {
				t.Fatal("confirmation retried or valid mirror lost", bodies)
			}
			entries, err = f.journal(time.Now())
			if err != nil || entries[0].Key != fmt.Sprintf("%d|confirm", c.Generation) {
				t.Fatal("confirmation reservation lost", entries, err)
			}
		})
	}
}
