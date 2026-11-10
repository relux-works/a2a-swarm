package tmuxwake

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This fixture observes only an owned isolated test process. It obtains the
// provider's documented fields directly, without calling the collector/parser
// under test or copying expected identity from the registry being validated.
func localRegistryFields(t *testing.T, pid int) (domain, start string) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
		cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return "darwin", strings.TrimSpace(string(out))
	case "linux":
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		end := bytes.LastIndexByte(raw, ')')
		if end < 0 || len(strings.Fields(string(raw[end+1:]))) < 20 {
			t.Fatal("isolated process stat fixture unavailable")
		}
		start = strings.Fields(string(raw[end+1:]))[19] // documented field 22.
		machine, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			t.Fatal(err)
		}
		namespace, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid"))
		if err != nil {
			t.Fatal(err)
		}
		return "linux:" + strings.TrimSpace(string(machine)) + ":" + namespace, start
	default:
		t.Skip("local registry identity has no supported platform")
		return "", ""
	}
}

// Real local collection must agree with independent fields of this test's own
// process. An incorrect owner-birth pin or canceled context cannot qualify it.
// The complete record uses the production JSON path, including numeric types.
// This does not prove provider responsiveness or eliminate the final I/O race.
func TestLiveRegistryIdentityLocalPlatform(t *testing.T) {
	ctx := context.Background()
	pid := os.Getpid()
	birth, err := Birth(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{OwnerPID: pid, OwnerBirth: birth, Pane: "%1", Runtime: "claude"}
	domain, start := localRegistryFields(t, pid)
	m := registryFixture("3.0.0", "idle")
	m["pid"], m["pidDomain"], m["procStart"] = pid, domain, start
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "wrong-owner-birth", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			owner := target
			probe := ctx
			if mode == "wrong-owner-birth" {
				owner.OwnerBirth = "replaced"
			}
			if mode == "canceled" {
				var cancel context.CancelFunc
				probe, cancel = context.WithCancel(ctx)
				cancel()
			}
			id, err := liveRegistryIdentity(probe, owner, recordString(m, "sessionId"))
			if mode != "valid" {
				if err == nil {
					t.Fatal("uncertain local process admitted")
				}
				return
			}
			if err != nil || id.Domain != domain {
				t.Fatal("independent local identity disagreed", err)
			}
			got := decodeRegistry(raw, id)
			if got.Reason != RuntimeNone || got.State != RuntimeReady {
				t.Fatalf("independent local record reason=%s state=%s want=none/ready", got.Reason, got.State)
			}
		})
	}
}

// Each required Linux negative first associates an otherwise-valid root and
// obtains a named readiness key on the isolated terminal. Changed machine,
// namespace, start ticks or platform then refuse both association and readiness
// as identity mismatches; the old key cannot type, submit or mutate the record.
func TestLinuxRegistryRootIdentityFences(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux local collection boundary; Darwin has explicit pure controls")
	}
	for _, mode := range []string{"wrong-machine-id", "wrong-pid-namespace", "reused-pid-start-ticks", "mixed-darwin-linux-domain", "wrong-pid", "wrong-session", "wrong-pane"} {
		t.Run(mode, func(t *testing.T) {
			f := newBoundFixture(t, "claude-named", "3.0.0", "idle")
			ctx := context.Background()
			root, err := filepath.EvalSymlinks(f.b.ClaudeRegistryRoot)
			if err != nil {
				t.Fatal(err)
			}
			if got := AssociateRegistryRoot(ctx, f.b.Target, f.b.SessionID, root); got != root {
				t.Fatal("valid Linux association control refused")
			}
			valid := f.b.AssessReadiness(ctx)
			if !valid.Eligible() || valid.Key.Route != RegistryNamed {
				t.Fatal("valid Linux named readiness control refused", valid.Runtime.Reason)
			}
			switch mode {
			case "wrong-machine-id":
				parts := strings.SplitN(recordString(f.m, "pidDomain"), ":", 3)
				machine := strings.Repeat("a", 32)
				if parts[1] == machine {
					machine = strings.Repeat("b", 32)
				}
				f.m["pidDomain"] = "linux:" + machine + ":" + parts[2]
			case "wrong-pid-namespace":
				parts := strings.SplitN(recordString(f.m, "pidDomain"), ":", 3)
				namespace := "pid:[1]"
				if parts[2] == namespace {
					namespace = "pid:[2]"
				}
				f.m["pidDomain"] = "linux:" + parts[1] + ":" + namespace
			case "reused-pid-start-ticks":
				start := "0"
				if recordString(f.m, "procStart") == start {
					start = "1"
				}
				f.m["procStart"] = start
			case "mixed-darwin-linux-domain":
				f.m["pidDomain"] = "darwin"
			case "wrong-pid":
				f.m["pid"] = f.b.Target.OwnerPID + 1
			case "wrong-session":
				f.m["sessionId"] = "replacement"
			case "wrong-pane":
				f.m["tmux"] = "synthetic:@0.%99999"
			}
			f.write(t)
			before, err := os.ReadFile(f.record)
			if err != nil {
				t.Fatal(err)
			}
			if got := AssociateRegistryRoot(ctx, f.b.Target, f.b.SessionID, root); got != "" {
				t.Fatal("wrong Linux identity associated a root")
			}
			refused := f.b.AssessReadiness(ctx)
			if refused.Eligible() || refused.Runtime.Reason != RuntimeIdentityMismatch || refused.Key != (ReadinessKey{}) {
				t.Fatal("wrong identity did not refuse through identity gate", refused.Runtime.Reason)
			}
			called := false
			result, err := f.b.Send(ctx, 1, valid.Key, func() { called = true }, nil)
			if err != nil || result.Disposition != SendRefused || called {
				t.Fatal("old readiness key permitted input", err)
			}
			after, err := os.ReadFile(f.record)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("identity refusal changed registry content", err)
			}
			f.empty(t)
		})
	}
}
