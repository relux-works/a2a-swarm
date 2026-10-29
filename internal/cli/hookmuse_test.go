package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func (e *env) muse(event, input string) result {
	e.t.Helper()
	e.stdin = input
	defer func() { e.stdin = "" }()
	return e.as("alice", "hook", event, "--runtime", "muse")
}

const museInput = `{"session_id":"muse-fixture"}`

func museLog(t *testing.T) []map[string]string {
	t.Helper()
	data := readFile(t, filepath.Join(".temp", "a2a-muse-hooks", "alice.jsonl"))
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		var m map[string]string
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if len(m) != 3 || m["class"] == "" || m["event"] == "" {
			t.Fatalf("non-redacted metadata: %s", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, m["timestamp"]); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// Proves the exact parser-qualified envelopes, alongside byte-identical
// legacy controls. This does not prove native model receipt or Stop continuation.
func TestMuseEnvelope(t *testing.T) {
	for _, ev := range hookEvents {
		for _, runtime := range []string{"muse", "claude", "codex"} {
			rt, err := lookupRuntime(runtime)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(rt.render(ev, "COUNTS"))
			want := `{"hookSpecificOutput":{"hookEventName":"` + ev.name + `","additionalContext":"COUNTS"}}`
			if ev.arg == "stop" {
				want = `{"decision":"block","reason":"COUNTS"}`
				if runtime == "muse" {
					want = `{"systemMessage":"COUNTS"}`
				}
			}
			if string(b) != want {
				t.Fatalf("%s/%s: %s want %s", runtime, ev.arg, b, want)
			}
		}
	}
}

// Proves no bodies/forbidden fields, empty/repeated silence, nonblocking Stop
// state and durable pending mail. Printed Rung never constitutes a receipt.
func TestMuseLifecycle(t *testing.T) {
	for _, ev := range hookEvents {
		t.Run(ev.arg, func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			e.extra[EnvSessionID] = "muse-fixture"
			if r := e.muse(ev.arg, museInput).ok(t); r.stdout != "" || r.stderr != "" {
				t.Fatalf("empty: %+v", r)
			}
			e.hs.inject(room, "@bob:test.local", toMsg("bob", "question", "UNTRUSTED_BODY_MARKER"))
			r := e.muse(ev.arg, museInput).ok(t)
			for _, forbidden := range []string{"UNTRUSTED_BODY_MARKER", "decision", "permissionDecision", "updatedInput"} {
				if strings.Contains(r.stdout, forbidden) {
					t.Fatalf("forbidden %s: %+v", forbidden, r)
				}
			}
			if !strings.Contains(r.stdout, "1 new peer event") || r.stderr != "" {
				t.Fatalf("notice: %+v", r)
			}
			st := e.state("alice")
			h := st.HookSession("muse-fixture")
			if h.Blocks != 0 || h.LastBlock != 0 || len(h.Rung) != 1 || len(e.mailbox("alice").Pending()) != 1 || st.Terminal != nil {
				t.Fatalf("false block/binding/receipt: %+v", st)
			}
			if r := e.muse(ev.arg, museInput).ok(t); r.stdout != "" {
				t.Fatalf("repeat: %+v", r)
			}
			e.as("alice", "inbox").ok(t)
			if len(e.mailbox("alice").Pending()) != 0 {
				t.Fatal("inbox did not receipt")
			}
		})
	}
}

// Proves legacy Stop still records an actual block, Muse never does so, even
// with StopHookActive. All runtimes retain pending receipts and notice keys.
func TestMuseStopLegacyStateControls(t *testing.T) {
	for _, runtime := range []string{"muse", "claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			e.stdin = `{"session_id":"fixture","stop_hook_active":true}`
			e.as("alice", "hook", "stop", "--runtime", runtime).ok(t)
			e.hs.inject(room, "@bob:test.local", ping(1))
			r := e.as("alice", "hook", "stop", "--runtime", runtime).ok(t)
			h := e.state("alice").HookSession("fixture")
			if runtime == "muse" {
				if h.Blocks != 0 || h.LastBlock != 0 || !strings.Contains(r.stdout, `"systemMessage"`) {
					t.Fatalf("false block: %+v %+v", h, r)
				}
			} else if h.Blocks != 1 || h.LastBlock == 0 || !strings.Contains(r.stdout, `"decision":"block"`) {
				t.Fatalf("legacy block changed: %+v %+v", h, r)
			}
			if len(h.Rung) != 1 || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatal("receipt/rung changed")
			}
		})
	}
}

// Every invalid input fails before lease/rung mutation. Its nearby valid
// control surfaces the persistent gap and pending event without receipting it.
func TestMuseInputFailures(t *testing.T) {
	for _, input := range []string{"not json", "null", `[]`, `{`, `{"session_id":7}`, `{}`, `{"session_id":"fixture","hook_event_name":"stop"}`, strings.Repeat("x", hookStdinMax+1)} {
		t.Run(string([]byte(input)[:min(len(input), 40)]), func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			e.hs.inject(room, "@bob:test.local", ping(1))
			r := e.muse("prompt", input).ok(t)
			if r.stdout != "" || strings.Count(r.stderr, "\n") != 1 {
				t.Fatalf("not fail-open: %+v", r)
			}
			if logs := museLog(t); len(logs) != 1 || logs[0]["class"] != "invalid_input" {
				t.Fatalf("logs: %+v", logs)
			}
			st := e.state("alice")
			if st.Lease != nil || len(st.HookSessions) != 0 {
				t.Fatalf("failure changed state: %+v", st)
			}
			// Seed sync AFTER injecting the event cannot expose history; ensure
			// the adjacent control has a new event after its initial sync.
			e.muse("prompt", museInput).ok(t)
			e.hs.inject(room, "@bob:test.local", ping(2))
			r = e.muse("prompt", museInput).ok(t)
			if !strings.Contains(r.stdout, "hook gap") || len(museLog(t)) != 1 || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatalf("gap lost/receipted: %+v", r)
			}
		})
	}
}

type museTimeoutTransport struct{}

func (museTimeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// Proves bounded homeserver/deadline failures, one body/token-free log line,
// no rung or receipt, retained lease holder, and gap recovery after an empty
// success. Timeout uses the real context, not an unrelated setup refusal.
func TestMuseDeliveryFailures(t *testing.T) {
	for _, mode := range []string{"server", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			e.muse("prompt", museInput).ok(t)
			lease := *e.lease("alice")
			if mode == "server" {
				e.hs.fail("GET /_matrix/client/v3/sync", 500, "M_UNKNOWN", e.tokens["alice"]+" PRIVATE_BODY")
			} else {
				e.configure = func(a *App) {
					a.hookBudget = 30 * time.Millisecond
					a.HTTP = &http.Client{Transport: museTimeoutTransport{}}
				}
			}
			start := time.Now()
			r := e.muse("prompt", museInput).ok(t)
			if time.Since(start) > time.Second || r.stdout != "" || strings.Count(r.stderr, "\n") != 1 {
				t.Fatalf("not bounded/fail-open: %+v", r)
			}
			logs := museLog(t)
			want := "delivery_error"
			if mode == "timeout" {
				want = "timeout"
			}
			if len(logs) != 1 || logs[0]["class"] != want || strings.Contains(readFile(t, ".temp/a2a-muse-hooks/alice.jsonl"), "PRIVATE_BODY") {
				t.Fatalf("intended failure missing: %+v", logs)
			}
			st := e.state("alice")
			if len(st.HookSessions) != 0 || st.Lease.Holder != lease.Holder || len(e.mailbox("alice").Pending()) != 0 {
				t.Fatalf("failure rang/receipted/stole: %+v", st)
			}
			e.hs.mu.Lock()
			delete(e.hs.failures, "GET /_matrix/client/v3/sync")
			e.hs.mu.Unlock()
			e.configure = nil
			if r := e.muse("prompt", museInput).ok(t); r.stdout != "" {
				t.Fatal("empty gap made context noise")
			}
			e.hs.inject(room, "@bob:test.local", ping(1))
			r = e.muse("prompt", museInput).ok(t)
			if !strings.Contains(r.stdout, "hook gap") || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatalf("lost gap: %+v", r)
			}
		})
	}
}

// Refuses ambient-only/wrong identity and foreign live lease; valid explicit
// selection still works. Neither refusal steals the holder or creates binding.
func TestMuseIdentityLeaseAndExport(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	e.extra[config.EnvAgent] = "alice"
	e.stdin = museInput
	if r := e.run("hook", "prompt", "--runtime", "muse").ok(t); r.stdout != "" || !strings.Contains(r.stderr, "explicit_identity_required") {
		t.Fatalf("ambient: %+v", r)
	}
	e.extra[config.EnvAgent] = "bob"
	if r := e.muse("prompt", museInput).ok(t); r.stdout != "" || !strings.Contains(r.stderr, "identity_refused") {
		t.Fatalf("foreign: %+v", r)
	}
	delete(e.extra, config.EnvAgent)
	export := filepath.Join(t.TempDir(), "claude-env")
	e.extra[envClaudeEnvFile] = export
	e.muse("session-start", museInput).ok(t)
	if _, err := os.Stat(export); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Muse wrote Claude env export")
	}
	lease := *e.lease("alice")
	e.hs.inject(room, "@bob:test.local", ping(1))
	if r := e.muse("stop", `{"session_id":"foreign"}`).ok(t); r.stdout != "" || !strings.Contains(r.stderr, "lease_conflict") {
		t.Fatalf("foreign lease: %+v", r)
	}
	if !reflect.DeepEqual(&lease, e.lease("alice")) || len(e.state("alice").HookSessions) != 0 {
		t.Fatal("foreign hook changed ownership/rung")
	}
	if r := e.muse("prompt", museInput).ok(t); !strings.Contains(r.stdout, "1 new peer event") {
		t.Fatalf("own hook refused: %+v", r)
	}
}

// Proves a known read-gap entry remains pending after a printed Muse notice;
// a crash after printing also retains the diagnostic offset for later recovery.
func TestMusePendingReadGapAndPrintFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	e, _ := world(t)
	e.muse("prompt", museInput).ok(t)
	mb := e.mailbox("alice")
	if _, err := mb.Append([]config.MailEntry{{ID: "gap:fixture", Kind: config.MailGap, Label: "#fixture", From: "old", To: "new"}}); err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.afterPrint = func() error { return errors.New("PRIVATE_BODY") } }
	r := e.muse("stop", museInput).ok(t)
	if !strings.Contains(r.stdout, "gap") || len(museLog(t)) != 1 || len(e.mailbox("alice").Pending()) != 1 || len(e.state("alice").HookSessions) != 0 {
		t.Fatalf("lost obligation: %+v", r)
	}
	e.configure = nil
	// Reservation cooldown remains real; advance the existing fake clock.
	c := e.clock()
	c.add(2 * time.Minute)
	if r := e.muse("prompt", museInput).ok(t); !strings.Contains(r.stdout, "hook gap") {
		t.Fatalf("gap not recovered: %+v", r)
	}
	if len(e.mailbox("alice").Pending()) != 1 {
		t.Fatal("gap receipted by notice")
	}
}

// Reuses the public pause/resume switch: pause prevents network and lease
// acquisition, install must not clear it, and resume restores normal notices.
func TestMusePauseResume(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
	r := e.muse("prompt", museInput).ok(t)
	if r.stdout != "" || r.stderr != "" || e.lease("alice") != nil {
		t.Fatalf("pause: %+v", r)
	}
	e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
	e.muse("prompt", museInput).ok(t)
	e.hs.inject(room, "@bob:test.local", ping(1))
	if r := e.muse("prompt", museInput).ok(t); !strings.Contains(r.stdout, "1 new peer event") {
		t.Fatalf("resume: %+v", r)
	}
}

// Exact valid-JSON size neighbors narrow the input ceiling: one MiB is
// accepted, one additional byte is refused before lease/rung mutation. The
// padding is untrusted input and never appears in the notice or diagnostic.
func TestMuseInputSizeBoundary(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	base := `{"session_id":"muse-fixture","padding":""}`
	input := strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("Z", hookStdinMax-len(base))+`"`, 1)
	if len(input) != hookStdinMax {
		t.Fatal("incorrect boundary fixture")
	}
	if r := e.muse("prompt", input).ok(t); r.stderr != "" {
		t.Fatalf("valid boundary rejected: %+v", r)
	}
	e.hs.inject(room, "@bob:test.local", ping(1))
	before := e.state("alice")
	oversized := strings.Replace(input, `"padding":"`, `"padding":"Z`, 1)
	if r := e.muse("prompt", oversized).ok(t); r.stdout != "" || !strings.Contains(r.stderr, "invalid_input") {
		t.Fatalf("oversize accepted: %+v", r)
	}
	if !reflect.DeepEqual(before, e.state("alice")) || len(museLog(t)) != 1 {
		t.Fatal("oversize changed state")
	}
	r := e.muse("prompt", input).ok(t)
	if !strings.Contains(r.stdout, "1 new peer event") || !strings.Contains(r.stdout, "hook gap") || strings.Contains(r.stdout, "ZZZZ") {
		t.Fatalf("adjacent control: %+v", r)
	}
}

// A known durable gap survives a network failure and an empty/suppressed
// notification cycle. Successful context prints it without acknowledging it.
func TestMuseNetworkFailurePreservesPending(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	e.muse("prompt", museInput).ok(t)
	mb := e.mailbox("alice")
	if _, err := mb.Append([]config.MailEntry{{ID: "gap:pending", Kind: config.MailGap, Label: "#fixture", From: "a", To: "b"}}); err != nil {
		t.Fatal(err)
	}
	e.hs.fail("GET /_matrix/client/v3/sync", 500, "M_UNKNOWN", "PRIVATE_BODY "+e.tokens["alice"])
	if r := e.muse("prompt", museInput).ok(t); r.stdout != "" || !strings.Contains(r.stderr, "delivery_error") {
		t.Fatalf("wrong failure: %+v", r)
	}
	if len(e.mailbox("alice").Pending()) != 1 || len(e.state("alice").HookSessions) != 0 {
		t.Fatal("failure consumed pending gap")
	}
	log := readFile(t, ".temp/a2a-muse-hooks/alice.jsonl")
	if len(museLog(t)) != 1 || strings.Contains(log, e.tokens["alice"]) || strings.Contains(log, "PRIVATE_BODY") {
		t.Fatal("error journal leaked")
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, "GET /_matrix/client/v3/sync")
	e.hs.mu.Unlock()
	if r := e.muse("prompt", museInput).ok(t); !strings.Contains(r.stdout, "1 gap") || !strings.Contains(r.stdout, "hook gap") {
		t.Fatalf("pending recovery: %+v", r)
	}
	if len(e.mailbox("alice").Pending()) != 1 {
		t.Fatal("successful notice receipted gap")
	}
	c := e.clock()
	c.add(time.Minute)
	e.hs.inject(room, "@bob:test.local", ping(1))
	if r := e.muse("prompt", museInput).ok(t); !strings.Contains(r.stdout, "1 new peer event") || strings.Contains(r.stdout, "hook gap") {
		t.Fatalf("surfaced failure repeated or new mail lost: %+v", r)
	}
}
