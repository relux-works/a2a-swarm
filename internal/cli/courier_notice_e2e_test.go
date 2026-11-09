//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func cmFixtureListener(t *testing.T, f *courierFixture, name, session string) *cmListenerRun {
	t.Helper()
	return cmStart(t, f.stations[name], func(ctx context.Context, diag io.Writer) result {
		a := f.app(name, "", io.Discard, diag)
		get := a.Getenv
		a.Getenv = func(k string) string {
			if k == EnvSessionID {
				return session
			}
			return get(k)
		}
		return result{code: a.Run(ctx, []string{"--as", name, "watch", "--notices", "--listen"})}
	})
}

func cmSeedOwner(t *testing.T, f *courierFixture, name, session, owner string) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"session_id": session, "hook_event_name": "SessionStart"})
	f.run(name, string(input), "--as", name, "hook", "session-start", "--runtime", "muse").ok(t)
	if err := ownerWrite(context.Background(), ownerFiles(f.paths[name], name).path("pin"), ownerDMPin{Mode: "host", Owner: owner}); err != nil {
		t.Fatal(err)
	}
}

func cmAwait(t *testing.T, done <-chan result, check func() bool) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !check() {
		select {
		case <-tick.C:
		case <-done:
			t.Fatal("listener ended before state boundary")
		case <-timer.C:
			t.Fatal("state boundary timed out")
		}
	}
}

func TestCMStationNoticeInbox(t *testing.T) {
	f := newCourierFixture(t)
	cmSeedOwner(t, f, "alice", "alice-monitor", "@bob:"+testServer)
	l := cmFixtureListener(t, f, "alice", "alice-monitor")
	cmRefusal(t, l, strings.Repeat("0", 64), "not_allowed", 403)
	st, err := config.LoadState(f.paths["alice"], "alice")
	if err != nil || st.Monitor != nil {
		t.Fatal("refused client created idle wake evidence")
	}
	c := cmConnect(t, l)
	cmAwait(t, l.done, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor != nil
	})
	st, _ = config.LoadState(f.paths["alice"], "alice")
	if !st.Monitor.LastNoticeAt.IsZero() {
		t.Fatal("empty watch stamped idle wake")
	}
	id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(427))
	data := cmRead(t, c)
	var n notice
	if json.Unmarshal(data, &n) != nil || n.Total != 1 || n.validate() != nil || strings.Contains(string(data), id) || strings.Contains(string(data), "ping") {
		t.Fatal("notice leaked event or lost count")
	}
	cmAwait(t, l.done, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && !st.Monitor.LastNoticeAt.IsZero()
	})
	if f.received("alice", id) || courierReceiptCount(t, f, "alice", id) != 0 {
		t.Fatal("watch wrote a receipt")
	}
	r := courierSessionRun(f, "alice", "alice-monitor", "", "--as", "alice", "inbox").ok(t)
	if !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, "BEGIN UNTRUSTED PEER EVENTS") || !strings.Contains(r.stdout, "ping 427") || courierReceiptCount(t, f, "alice", id) != 1 {
		t.Fatal("first owner inbox did not print and receipt exact event")
	}
	r = courierSessionRun(f, "alice", "alice-monitor", "", "--as", "alice", "inbox").ok(t)
	if strings.Contains(r.stdout, id) || courierReceiptCount(t, f, "alice", id) != 1 {
		t.Fatal("second inbox repeated or added a receipt")
	}
	st, _ = config.LoadState(f.paths["alice"], "alice")
	if st.Monitor.LastNoticeAt.IsZero() {
		t.Fatal("inbox lost notice timestamp")
	}
	// The exact read ID lives in the receipt index; not a notice receipt.
	if !f.received("alice", id) {
		t.Fatal("read ID not retained")
	}
	c.CloseNow()
	cmAwait(t, nil, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor.Phase == "stopped"
	})
}

func TestCMOwnerLeaseIsolation(t *testing.T) {
	f := newCourierFixture(t)
	for _, name := range []string{"alice", "bob"} {
		cmSeedOwner(t, f, name, "owner-"+name, "@alice:"+testServer)
	}
	before, _ := os.ReadFile(f.paths["bob"].StateFile("bob"))
	r := courierSessionRun(f, "bob", "owner-alice", "", "--as", "bob", "watch", "--notices", "--json")
	after, _ := os.ReadFile(f.paths["bob"].StateFile("bob"))
	if r.code != 1 || r.stdout != "" || r.stderr != "a2a: courier: courier_lease_conflict\n" || !bytesEqual(before, after) {
		t.Fatal("foreign owner session acquired monitor or lease")
	}
	l := cmFixtureListener(t, f, "alice", "owner-alice")
	c := cmConnect(t, l)
	cmAwait(t, l.done, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor != nil
	})
	// Other identities remain byte-identical; an active owner is renewed only
	// by its own handle. Closing the station releases that identity's lock.
	after, _ = os.ReadFile(f.paths["bob"].StateFile("bob"))
	if !bytesEqual(before, after) {
		t.Fatal("alice monitor changed bob")
	}
	c.CloseNow()
	cmAwait(t, nil, func() bool {
		st, err := config.LoadState(f.paths["alice"], "alice")
		return err == nil && st.Monitor.Phase == "stopped"
	})
	if _, err := os.Stat(filepath.Join(f.stations["alice"], "state", noticeEndpoint)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
