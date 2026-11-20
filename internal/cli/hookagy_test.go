package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	for _, input := range []string{`{"conversationId":"bogus","hookEventName":"SessionStart"}`, `{"conversationId":"` + agyTestID + `","hookEventName":"PreInvocation"}`, `{"conversationId":"` + agyTestID + `","conversationId":"` + agyTestID + `","hookEventName":"SessionStart"}`} {
		e.stdin = input
		r := e.as("alice", "hook", "session-start", "--runtime", "agy").ok(t)
		if r.stdout != "" || !strings.Contains(r.stderr, "nothing new was acknowledged") {
			t.Fatal(r)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	sendResult()
	inputWriter.Close()
	(<-finished).ok(t)
}
