package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

type approvalTestFixture struct {
	e         *env
	room      string
	payload   string
	decision  string
	key       string
	approvers string
	principal string
}

func approvalFixture(t *testing.T) approvalTestFixture {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	e, room := world(t)
	fixed := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return fixed }
	key := filepath.Join(t.TempDir(), "operator")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen keygen: %s", out)
	}
	principal := "op:alexis@relux"
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	approvers := filepath.Join(t.TempDir(), "approvers")
	if err := os.WriteFile(approvers, []byte(principal+" namespaces=\"approve.*\" "+strings.TrimSpace(string(pub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "topic", "new", "proj", "api-design", "API design").ok(t)
	created := e.as("alice", "approval", "request", "--room", "proj", "--topic", "api-design", "--approver", principal, "--scope", "deploy", "--text", "deploy release 7", "--expires", "+24h").ok(t)
	fields := strings.Fields(strings.TrimSpace(created.stdout))
	if len(fields) != 2 {
		t.Fatalf("request output = %q", created.stdout)
	}
	payload := fields[1]
	e.as("alice", "approval", "sign", "--payload", payload, "--key", key, "--post", "proj", "--topic", "api-design").ok(t)
	return approvalTestFixture{e: e, room: room, payload: payload, decision: e.hs.lastEvent().EventID, key: key, approvers: approvers, principal: principal}
}

func (f approvalTestFixture) verify(t *testing.T, approvers string) result {
	t.Helper()
	return f.e.as("alice", "approval", "verify", "--room", "proj", "--event", f.decision, "--approvers", approvers)
}

func mutateApprovalContent(t *testing.T, f approvalTestFixture, eventID, key string, change func(map[string]any)) {
	t.Helper()
	f.e.hs.mu.Lock()
	defer f.e.hs.mu.Unlock()
	for i := range f.e.hs.events {
		if f.e.hs.events[i].EventID != eventID {
			continue
		}
		var content map[string]any
		if err := json.Unmarshal(f.e.hs.events[i].Content, &content); err != nil {
			t.Fatal(err)
		}
		value := content[key].(map[string]any)
		change(value)
		b, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		f.e.hs.events[i].Content = b
		return
	}
	t.Fatalf("event %s not found", eventID)
}

// Proves an operator signature verifies against the request, scope, allowed signer, and unused request id.
func TestApprovalRoundTrip(t *testing.T) {
	f := approvalFixture(t)
	r := f.verify(t, f.approvers).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "VERIFIED ")
	tuitestkit.ContainsStr(t, r.stdout, "scope=deploy approver="+f.principal)
	info, err := os.Stat(f.payload)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("payload mode = %o", info.Mode().Perm())
	}
}

// Proves verification rejects a changed request text beside the valid round-trip control.
func TestApprovalTamperedText(t *testing.T) {
	f := approvalFixture(t)
	mutateApprovalContent(t, f, requestEventID(t, f), approvalContentKey, func(payload map[string]any) { payload["text"] = "deploy release 8" })
	f.verify(t, f.approvers).failsWith(t, ExitError, "text_sha256 mismatch")
}

func requestEventID(t *testing.T, f approvalTestFixture) string {
	t.Helper()
	for _, ev := range f.e.hs.roomMessages(f.room) {
		var c map[string]json.RawMessage
		_ = json.Unmarshal(ev.Content, &c)
		var p approvalPayload
		_ = json.Unmarshal(c[approvalContentKey], &p)
		if p.ID != "" {
			return ev.EventID
		}
	}
	t.Fatal("approval request event missing")
	return ""
}

// Proves allowed_signers must contain the named operator principal beside the valid control.
func TestApprovalWrongAllowedPrincipal(t *testing.T) {
	f := approvalFixture(t)
	other := filepath.Join(t.TempDir(), "approvers")
	pub, err := os.ReadFile(f.key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("op:someone-else@relux namespaces=\"approve.*\" "+strings.TrimSpace(string(pub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.verify(t, other).failsWith(t, ExitError, "SSH signature verification failed")
}

// Proves signatures in a different SSHSIG namespace are refused beside the valid control.
func TestApprovalWrongNamespace(t *testing.T) {
	f := approvalFixture(t)
	if err := os.Remove(f.payload + ".sig"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", f.key, "-n", "other.namespace", f.payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen sign: %s", out)
	}
	sig, err := os.ReadFile(f.payload + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	mutateApprovalContent(t, f, f.decision, approvalSigKey, func(value map[string]any) { value["sshsig"] = string(sig) })
	f.verify(t, f.approvers).failsWith(t, ExitError, "SSH signature verification failed")
}

// Proves an expired request is rejected beside the valid round-trip control.
func TestApprovalExpired(t *testing.T) {
	f := approvalFixture(t)
	mutateApprovalContent(t, f, requestEventID(t, f), approvalContentKey, func(payload map[string]any) {
		payload["issued_at"] = float64(time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC).UnixMilli())
		payload["expires_at"] = float64(time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC).UnixMilli())
	})
	f.verify(t, f.approvers).failsWith(t, ExitError, "approval request expired")
}

// Proves a verified request id is refused on replay beside the first successful verification.
func TestApprovalReplay(t *testing.T) {
	f := approvalFixture(t)
	f.verify(t, f.approvers).ok(t)
	f.verify(t, f.approvers).failsWith(t, ExitError, "approval request id already used")
}

// Proves a decision without a corresponding request event fails beside the valid round-trip control.
func TestApprovalMissingRequestEvent(t *testing.T) {
	f := approvalFixture(t)
	id := f.e.hs.inject(f.room, "@alice:test.local", map[string]any{"msgtype": "m.text", "body": "decision", approvalSigKey: map[string]any{"request_id": "missing-request", "principal": f.principal, "sshsig": "fake"}})
	f.decision = id
	f.verify(t, f.approvers).failsWith(t, ExitError, "request event does not exist")
}

// Proves --approvers is required as a usage error before any homeserver request.
func TestApprovalApproversRequired(t *testing.T) {
	e, _ := world(t)
	before := len(e.hs.urls())
	e.as("alice", "approval", "verify", "--room", "proj", "--event", "$event").failsWith(t, ExitUsage, "--approvers")
	if got := len(e.hs.urls()); got != before {
		t.Fatalf("homeserver calls = %d, want %d", got-before, 0)
	}
}

// Proves verification refuses a request whose payload 'from' is not the
// sender of the request event (someone posting a request in another agent's
// name), beside the valid round trip.
func TestApprovalRequestSenderMismatch(t *testing.T) {
	f := approvalFixture(t)
	id := requestEventID(t, f)
	f.e.hs.mu.Lock()
	for i := range f.e.hs.events {
		if f.e.hs.events[i].EventID == id {
			f.e.hs.events[i].Sender = "@mallory:test.local"
		}
	}
	f.e.hs.mu.Unlock()
	f.verify(t, f.approvers).failsWith(t, ExitError, "does not match the sender")
}
