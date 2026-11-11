package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One isolated recipient bypass, between passing original/restored controls.
// Setup/compile errors and timeouts cannot satisfy the negative assertion.
func TestAgentDMRecipientGateBypassControl(t *testing.T) {
	anchor := "func (g agentDMDelivery) decision(ev matrix.Event, room, me string, principals []string) string {"
	agentDMOverlayControl(t, "agent_dm_delivery.go", anchor, anchor+"\n return \"allow\" // planted recipient bypass", "TestAgentDMRecipientMailboxWake", "recipient wake assertion:")
}

func TestAgentDMHeldCapacityRegressionControl(t *testing.T) {
	anchor := "if heldErr != nil {\n\t\t\ts.warnAgentDelivery()"
	agentDMOverlayControl(t, "delivery.go", anchor, "if heldErr != nil {\n return nil, heldErr", "TestAgentDMHeldCapacityPreservesPrincipalWake", "principal capacity wake assertion:")
}

// Removing only the independent conservative guards must expose the known
// corrupted pair through broad selection, while unbound controls still pass.
func TestAgentDMRetainedContextControl(t *testing.T) {
	anchor := "out.Rooms[room] = agentDMGate{}"
	agentDMOverlayControl(t, "agent_dm_context.go", anchor, "_ = room // planted missing context", "TestAgentDMDeliveryStorageIsolation", "storage isolation assertion: unknown known-pair mail reached mailbox")
}

// Removing the shared refresh must fail fresh-process all-shown, independently
// of the explicit-ID positive control and the sender notification transport.
func TestAgentDMPendingRefreshControl(t *testing.T) {
	anchor := "// Every Pending consumer enters here, including ack, notices and binding\n\t// wakes without a preceding sync. Revalidate remote policy even when the\n\t// local admission stamp did not change; stale snapshots grant no authority.\n\tgate, err := s.refreshAgentGate(ctx)\n\tif err != nil {\n\t\treturn config.State{}, err\n\t}\n\ts.agentDelivery = &gate"
	agentDMOverlayControl(t, "delivery.go", anchor, "// planted stale Pending policy", "TestAgentDMAckShownCurrentPolicy", "shown ack policy assertion:")
}

func agentDMOverlayControl(t *testing.T, source, anchor, replacementText, testName, assertion string) {
	t.Helper()
	if os.Getenv("A2A_RUN_AGENT_DM_CONTROLS") != "1" {
		t.Skip("explicit bounded agent-DM overlay control")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".temp")
	check := exec.Command("git", "-C", root, "check-ignore", "--quiet", filepath.Join(dir, "probe"))
	if err := check.Run(); err != nil {
		t.Fatal("control scratch is not verified ignored")
	}
	dir, err = os.MkdirTemp(dir, "agent-dm-control-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "internal", "cli", source)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(original)
	if strings.Count(string(original), anchor) != 1 {
		t.Fatal("control anchor absent/ambiguous")
	}
	replacement := filepath.Join(dir, "bypass.go")
	if err = os.WriteFile(replacement, []byte(strings.Replace(string(original), anchor, replacementText, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.json")
	raw, _ := json.Marshal(map[string]any{"Replace": map[string]string{path: replacement}})
	if err = os.WriteFile(overlay, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(name, overlay string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		args := []string{"test", "-p=1", "-parallel=1", "-count=1", "-v", "-timeout=60s"}
		if overlay != "" {
			args = append(args, "-overlay", overlay)
		}
		args = append(args, "./internal/cli", "-run", "^"+testName+"$")
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOMAXPROCS=2", "A2A_RUN_AGENT_DM_CONTROLS=0")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".pid"), []byte(fmt.Sprintln(cmd.Process.Pid)), 0600); err != nil {
			t.Error(err)
		}
		err := cmd.Wait()
		data := out.Bytes()
		if e := os.WriteFile(filepath.Join(dir, name+".log"), data, 0600); e != nil {
			t.Error(e)
		}
		if ctx.Err() != nil {
			t.Fatal("overlay control timeout")
		}
		return data, err
	}
	positive, err := run("original", "")
	if err != nil || !bytes.Contains(positive, []byte("--- PASS: "+testName)) {
		t.Fatalf("original control failed %v\n%s", err, positive)
	}
	negative, err := run("planted", overlay)
	if err == nil || !bytes.Contains(negative, []byte("--- FAIL: "+testName)) || !bytes.Contains(negative, []byte(assertion)) || bytes.Contains(negative, []byte("[build failed]")) || bytes.Contains(negative, []byte("test timed out")) {
		t.Fatalf("planted bypass missed mailbox/wake assertion %v\n%s", err, negative)
	}
	restored, err := run("restored", "")
	if err != nil || !bytes.Contains(restored, []byte("--- PASS: "+testName)) {
		t.Fatalf("restored control failed %v\n%s", err, restored)
	}
	current, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(current) != hash {
		t.Fatal("control changed candidate source")
	}
	t.Logf("original/restored PASS; planted regression failed intended %s assertion with nonzero exit; source unchanged", testName)
}
