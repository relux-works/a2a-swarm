//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// This test uses only the admitted C5 fixture, fake Matrix and the landed
// L02/L03 API. It does not depend on the draft CM implementation.
func cmPrerequisiteOwnerInbox(t *testing.T, requireStationSuccess bool) {
	f := newDaemonFixture(t)
	const ownerSession = "cm-prerequisite-session"
	f.run("alice", `{"session_id":"cm-prerequisite-session","hook_event_name":"SessionStart"}`, "--as", "alice", "hook", "session-start", "--runtime", "muse").ok(t)
	st, err := config.LoadState(f.paths["alice"], "alice")
	if err != nil || st.Lease == nil || st.Lease.Holder != ownerSession || st.MonitorOwner == nil {
		t.Fatal("valid Daemon hook did not establish the owner and existing lease")
	}
	// Valid neighbour: the same station inbox can borrow the owner lease while
	// no monitor is active. No mail is injected yet.
	f.run("alice", "", "--as", "alice", "inbox").ok(t)
	record := cliMonitorRecord()
	record.SessionID = ownerSession
	record.Host = st.Lease.Host
	record.OwnerPID = st.Lease.PID
	err = config.WithMonitorGeneration(f.paths["alice"], "alice", time.Now().Add(time.Second), config.MonitorArm, record, func(g *config.MonitorGeneration) error {
		if err := g.RenewOwnerLease(record.Generation); err != nil {
			return err
		}
		id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(1))
		var stationOut, stationDiag bytes.Buffer
		station := f.app("alice", "", &stationOut, &stationDiag)
		getenv := station.Getenv
		station.Getenv = func(k string) string {
			if k == EnvSessionID {
				if !requireStationSuccess {
					return "wrong-session"
				}
				return ownerSession
			}
			return getenv(k)
		}
		before, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
		if err != nil {
			t.Fatal(err)
		}
		r := result{code: station.Run(context.Background(), []string{"--as", "alice", "inbox"}), stdout: stationOut.String(), stderr: stationDiag.String()}
		if requireStationSuccess {
			if r.code != ExitOK || strings.Count(r.stdout, id) != 1 || !f.received("alice", id) {
				t.Fatalf("CM required owner inbox through station: exit=%d, closed diagnostic=%q", r.code, r.stderr)
			}
			if daemonReceiptCount(t, f, "alice", id) != 1 {
				t.Fatal("first owner inbox did not receipt exactly once")
			}
			stationOut.Reset()
			stationDiag.Reset()
			if code := station.Run(context.Background(), []string{"--as", "alice", "inbox"}); code != ExitOK || strings.Contains(stationOut.String(), id) {
				t.Fatal("second station inbox was not empty")
			}
			if daemonReceiptCount(t, f, "alice", id) != 1 {
				t.Fatal("second inbox duplicated receipt")
			}
			return nil
		}
		after, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("refused session changed State", err)
		}
		if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: daemon: daemon_lease_conflict\n" || f.received("alice", id) {
			t.Fatal("C5 failure differs from the isolated closed refusal")
		}
		// Valid neighbour with actual same-session evidence: the engine's local
		// inbox can borrow the exact owner lease and receipt the exact event.
		var out, diag bytes.Buffer
		local := &App{Stdout: &out, Stderr: &diag, Now: time.Now, HTTP: f.envs["alice"].hs.srv.Client(), connectionSecretStore: f.store, Getenv: func(k string) string {
			switch k {
			case config.EnvConfigDir:
				return filepath.Join(f.home, "identities", "alice")
			case EnvSessionID:
				return ownerSession
			}
			return ""
		}}
		if code := local.Run(context.Background(), []string{"--as", "alice", "inbox"}); code != 0 || !strings.Contains(out.String(), id) || !f.received("alice", id) {
			t.Fatal("same-session local neighbour did not print and receipt")
		}
		out.Reset()
		diag.Reset()
		if code := local.Run(context.Background(), []string{"--as", "alice", "inbox"}); code != 0 || strings.Contains(out.String(), id) {
			t.Fatal("second local inbox did not remain empty")
		}
		return nil
	})
	if err != nil {
		t.Fatal("valid monitor prerequisite failed", err)
	}
}

func TestCMPrerequisiteOwnerInboxDiagnostic(t *testing.T) { cmPrerequisiteOwnerInbox(t, false) }
func TestCMPrerequisiteOwnerInboxRequired(t *testing.T)   { cmPrerequisiteOwnerInbox(t, true) }
