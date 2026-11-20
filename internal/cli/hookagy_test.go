package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const agyTestID = "01924444-3333-7222-8111-0123456789ac"

// Native events establish one conversation lease. A fresh hook notice excludes peer text,
// receipts no message, and is not repeated at the next native hook.
func TestAgyConversationDoorbell(t *testing.T) {
	e, room := world(t)
	clk := e.clock()
	e.extra["A2A_AGENT"] = "alice"
	e.extra[EnvSessionID] = agyTestID

	e.stdin = `{"conversationId":"` + agyTestID + `","hookEventName":"SessionStart","lastUserInput":"sensitive"}`
	if s := e.as("alice", "hook", "session-start", "--runtime", "agy").ok(t).stdout; strings.TrimSpace(s) != "{}" {
		t.Fatal(s)
	}
	if out := e.as("alice", "agy", "check", "--session", agyTestID).ok(t).stdout; !strings.Contains(out, "stream and delivery unverified") {
		t.Fatal(out)
	}
	clk.add(time.Minute)
	e.hs.inject(room, "@bob:test.local", toMsg("bob", "notice", "private-title-358"))
	e.stdin = `{"conversationId":"01924444-3333-7222-8111-0123456789ad","hookEventName":"PreInvocation"}`
	wrong := e.as("alice", "hook", "prompt", "--runtime", "agy").ok(t)
	if wrong.stdout != "" || !strings.Contains(wrong.stderr, "identity mismatch") {
		t.Fatal(wrong)
	}
	e.stdin = `{"conversationId":"` + agyTestID + `","hookEventName":"PreInvocation"}`
	out := e.as("alice", "hook", "prompt", "--runtime", "agy").ok(t).stdout
	if !strings.Contains(out, "1 pending mailbox events") || !strings.Contains(out, "injectSteps") || strings.Contains(out, "private-title") || strings.Contains(out, "bob") {
		t.Fatal(out)
	}

	e.stdin = `{"conversationId":"` + agyTestID + `","hookEventName":"PreInvocation"}`
	if s := e.as("alice", "hook", "prompt", "--runtime", "agy").ok(t).stdout; strings.TrimSpace(s) != "{}" {
		t.Fatal(s)
	}
	if s := e.as("alice", "inbox").ok(t).stdout; !strings.Contains(s, "private-title-358") {
		t.Fatal("notice receipted mail")
	}
}

// Antigravity names flat groups; updates preserve independent hooks and refuse
// another identity or an operator-disabled group without touching the file.
func TestAgyInstaller(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/bin/a2a")
	p := filepath.Join(t.TempDir(), "hooks.json")
	install := func(name string) result {
		return e.run("hook", "install", "--agent", name, "--runtime", "agy", "--settings", p)
	}
	original := `{"team":{"enabled":false}}`
	if err := os.WriteFile(p, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	install("alice").ok(t)
	first := readFile(t, p)
	install("alice").ok(t)
	if readFile(t, p) != first || !strings.Contains(first, "PreInvocation") || strings.Contains(first, "UserPromptSubmit") {
		t.Fatal(first)
	}
	install("bob").failsWith(t, ExitError, "conflicting")
	if readFile(t, p) != first {
		t.Fatal("identity conflict mutated settings")
	}
	e.as("alice", "hook", "uninstall", "--runtime", "agy", "--settings", p).ok(t)
	var o map[string]json.RawMessage
	if json.Unmarshal([]byte(readFile(t, p)), &o) != nil || len(o) != 1 || o["team"] == nil {
		t.Fatal(o)
	}
	disabled := `{"a2a-mailbox":{"enabled":false}}`
	if err := os.WriteFile(p, []byte(disabled), 0600); err != nil {
		t.Fatal(err)
	}
	install("alice").failsWith(t, ExitError, "modified agy")
	if readFile(t, p) != disabled {
		t.Fatal("changed disabled group")
	}
}

// A mismatched event, malformed UUID or duplicate field cannot ring the hook.
func TestAgyInvalidNativeInput(t *testing.T) {
	e, _ := world(t)
	e.extra["A2A_AGENT"] = "alice"
	for _, tc := range []struct{ input, reason string }{
		{`{"conversationId":"bogus","hookEventName":"SessionStart"}`, "conversation identity mismatch"},
		{`{"conversationId":"` + agyTestID + `","hookEventName":"PreInvocation"}`, "event mismatch"},
		{`{"conversationId":"` + agyTestID + `","conversationId":"` + agyTestID + `","hookEventName":"SessionStart"}`, "invalid Antigravity hook input"},
	} {
		e.stdin = tc.input
		r := e.as("alice", "hook", "session-start", "--runtime", "agy").ok(t)
		if r.stdout != "" || !strings.Contains(r.stderr, tc.reason) {
			t.Fatal(r)
		}
		st, err := config.LoadState(config.Paths{Base: e.dir}, "alice")
		if err != nil || st.Lease != nil {
			t.Fatal("invalid input claimed lease", err)
		}
	}
}

// The native-pipe controller refuses ambiguous init, failed/foreign results,
// and a changed stream; only the matching successful pair reaches clean EOF.
func TestAgyBridgeBoundFrames(t *testing.T) {
	init := `{"event":"init","conversation_id":"` + agyTestID + `","init":{"model":"gemini-4-neon-xhigh"}}`
	ready := `{"event":"result","result":{"conversation_id":"` + agyTestID + `","status":"SUCCESS"}}`
	for _, tc := range []struct{ frames, want string }{
		{init + "\n" + ready, ""},
		{ready, "init binding mismatch"},
		{init + "\n" + ready + "\n" + ready, "unexpected activity"},
		{init + "\n" + `{"event":"step_update","conversation_id":null}`, "invalid agy frame"},
		{init + "\n" + `{"event":"step_update","conversation_id":""}`, "invalid agy frame"},
		{init + "\n" + `{"event":"error"}`, "provider reported an error"},
		{strings.Replace(init, agyQualifiedModel, "other", 1), "init binding mismatch"},
		{init + "\n" + init, "binding changed"},
		{init + "\n" + strings.Replace(ready, agyTestID, "wrong", 1), "did not confirm"},
		{init + "\n" + strings.Replace(ready, "SUCCESS", "ERROR", 1), "did not confirm"},
		{init + "\n" + `{"event":"x","event":"result"}`, "invalid agy frame"},
		{init, "closed before an idle result"},
	} {
		a := NewApp()
		a.Stderr = io.Discard
		err := a.runAgyBridge(context.Background(), io.NopCloser(strings.NewReader(tc.frames)), agyTestID, agyQualifiedModel, time.Hour)
		if tc.want == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, expected %s", err, tc.want)
		}
	}
}

// A bridge poll cannot create a lease, revive an expired claim or supersede a
// terminal owner. A matching current lease admits a poll.
func TestAgyPollCustody(t *testing.T) {
	now := time.Now()
	st := config.State{}
	if err := agyCustody(&st, agyTestID, true, now); err == nil {
		t.Fatal("cold poll acquired custody")
	}
	st.Lease = &config.Lease{Holder: agyTestID, Host: machineLabel(), Expires: now.Add(time.Minute)}
	if err := agyCustody(&st, agyTestID, true, now); err != nil {
		t.Fatal(err)
	}
	if err := agyCustody(&st, "other", true, now); err == nil {
		t.Fatal("foreign conversation")
	}
	if err := agyCustody(&st, agyTestID, true, now.Add(time.Minute)); err == nil {
		t.Fatal("expired lease")
	}
	st.Terminal = &config.TerminalBinding{}
	if err := agyCustody(&st, agyTestID, false, now); err == nil {
		t.Fatal("live terminal")
	}
}

// A fresh matrix event produces one fixed native frame after the admitted
// result. The bridge sends no second prompt until the provider finishes.
func TestAgyBridgeIdleDelivery(t *testing.T) {
	e, room := world(t)
	var offset atomic.Int64
	e.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	committed := func() {
		t.Helper()
		limit := time.Now().Add(2 * time.Second)
		for time.Now().Before(limit) {
			st, err := config.LoadState(config.Paths{Base: e.dir}, "alice")
			if err == nil && st.Notification.Reservation == nil && st.Notification.NotBefore.After(e.now()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("notification completion was not recorded")
	}
	e.extra[EnvSessionID] = agyTestID
	e.extra["A2A_AGENT"] = "alice"
	e.stdin = `{"conversationId":"` + agyTestID + `","hookEventName":"SessionStart"}`
	e.as("alice", "hook", "session-start", "--runtime", "agy").ok(t)
	e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "private-challenge"))
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	e.configure = func(a *App) { a.Stdin = input; a.Stdout = outputWriter }
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	finished := make(chan result, 1)
	go func() {
		finished <- e.runCtx(ctx, "--as", "alice", "agy", "bridge", "--session", agyTestID, "--model", agyQualifiedModel)
	}()
	fmt.Fprintf(inputWriter, "{\"event\":\"init\",\"conversation_id\":%q,\"init\":{\"model\":%q}}\n", agyTestID, agyQualifiedModel)
	sendResult := func() {
		fmt.Fprintf(inputWriter, "{\"event\":\"result\",\"result\":{\"conversation_id\":%q,\"status\":\"SUCCESS\"}}\n", agyTestID)
	}
	sendResult()
	output.SetReadDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(output)
	frame, err := rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frame, `"event":"user"`) || !strings.Contains(frame, "1 pending mailbox events") || strings.Contains(frame, "private-challenge") {
		t.Fatal(frame)
	}
	committed()
	offset.Add(int64(time.Minute))
	// No second input while this first notice-triggered turn is running.
	e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "second-private"))
	silent := func() {
		t.Helper()
		output.SetReadDeadline(time.Now().Add(5500 * time.Millisecond))
		if _, err := rd.ReadString('\n'); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected silence, got %v", err)
		}
	}
	silent()
	sendResult()
	output.SetReadDeadline(time.Now().Add(8 * time.Second))
	frame, err = rd.ReadString('\n')
	if err != nil || !strings.Contains(frame, "2 pending mailbox events") {
		t.Fatal(frame, err)
	}
	committed()
	offset.Add(int64(time.Minute))
	sendResult()
	silent()
	inputWriter.Close()
	(<-finished).ok(t)
	e.configure = nil
	inbox := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(inbox, "private-challenge") || !strings.Contains(inbox, "second-private") {
		t.Fatal("bridge receipted mail")
	}
}

// A provider indication during a poll invalidates the grant before the write.
// The output callback is never visited, including on a repeated idle result.
func TestAgyCommitAfterInvalidation(t *testing.T) {
	t.Run("buffered suffix", func(t *testing.T) {
		var frame agyObservation
		raw := []byte(`{"event":"result","result":{"conversation_id":"` + agyTestID + `","status":"SUCCESS"}}`)
		if err := frame.decode(raw); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []int{0, 1} {
			g := agyGate{initialized: true, buffered: len(raw) + suffix}
			ready, err := g.observe(frame, agyTestID, agyQualifiedModel, len(raw))
			if suffix == 0 && (!ready || err != nil) || suffix > 0 && (ready || err == nil) {
				t.Fatal("incorrect buffered input verdict", suffix, ready, err)
			}
		}
	})

	for _, cause := range []string{"eof", "error", "repeat-result", "activity"} {
		t.Run(cause, func(t *testing.T) {
			wrote := false
			g := agyGate{initialized: true, idle: true, write: func([]byte) (int, error) { wrote = true; return 1, nil }}
			ctx, ok := g.poll(context.Background())
			if !ok {
				t.Fatal("poll unavailable")
			}
			switch cause {
			case "eof":
				g.finish(nil)
			case "error":
				g.finish(errors.New("provider error"))
			case "repeat-result":
				g.observe(agyObservation{Event: "result"}, agyTestID, agyQualifiedModel, 0)
			case "activity":
				g.observe(agyObservation{Event: "step_update"}, agyTestID, agyQualifiedModel, 0)
			}
			n, err := g.Write([]byte("x"))
			if ctx.Err() == nil || n != 0 || err == nil || wrote {
				t.Fatalf("unfenced %s", cause)
			}
		})
	}
}
