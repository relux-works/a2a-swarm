package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
)

// Independent consumer regression: an already opened named session must stop
// before native dispatch/state effects when its selected operational binding
// changes. Current/unrelated edits are paired successful dispatch controls.
// Only synthetic stores, loopback fixtures and a fake native RPC are used.
func TestMigrationReviewNativeSelectedChanges(t *testing.T) {
	for _, change := range []string{"unchanged", "current-unrelated", "service", "endpoint", "identity-domain", "connection-id"} {
		t.Run(change, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.profile = "alpha"
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp("", "a2a-review-native-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "fixture.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			if err = os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			endpoint := "unix://" + path
			socket, err := codexwake.SocketIdentity(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			birth, err := ownerBirth(context.Background(), os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			n := &nativeFixture{thread: codexwake.Snapshot{ID: "review-thread", Cwd: cwd, Status: codexwake.Status{Type: "idle"}}, endpoint: endpoint, socket: socket, cwd: cwd}
			dials := 0
			a.nativeDial = func(context.Context, string) (nativeRuntime, error) { dials++; return n, nil }
			binding := codexwake.Binding{Identity: s.name, ThreadID: "review-thread", SessionID: "review-thread", Endpoint: endpoint, EndpointID: socket, ServerVersion: "0.160.1", Generation: "review-generation", Host: machineLabel(), Cwd: cwd, OwnerPID: os.Getpid(), OwnerStart: birth, Gate: "ready", MaxPerHour: 2, HeartbeatEvery: time.Minute}
			if err = binding.Validate(); err != nil {
				t.Fatal(err)
			}
			if err = config.SaveState(s.paths, s.name, config.State{NextBatch: "original-cursor", Runtime: &binding}); err != nil {
				t.Fatal(err)
			}
			mb, err := config.OpenMailbox(s.paths, s.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = mb.Append([]config.MailEntry{{ID: "gap:review", Kind: config.MailGap, Label: "#fixture", From: "old", To: "new"}}); err != nil {
				t.Fatal(err)
			}
			beforeState, err := os.ReadFile(s.paths.StateFile(s.name))
			if err != nil {
				t.Fatal(err)
			}
			beforeMail, err := os.ReadFile(s.paths.MailboxFile(s.name))
			if err != nil {
				t.Fatal(err)
			}
			c := f.r.Profiles["alpha"]
			switch change {
			case "current-unrelated":
				f.r.Current = "beta"
				other := f.r.Profiles["beta"]
				other.Store.AccessTokenService = "test.unrelated.token"
				f.r.Profiles["beta"] = other
			case "service":
				c.Store.AccessTokenService = "test.changed.token"
			case "endpoint":
				c.URL = f.r.Profiles["beta"].URL
			case "identity-domain":
				c.ServerName = "changed.invalid"
			case "connection-id":
				c.ID = "changed-id"
			}
			f.r.Profiles["alpha"] = c
			f.save()
			err = a.dispatchNative(context.Background(), s, binding.Generation)
			afterState, readErr := os.ReadFile(s.paths.StateFile(s.name))
			if readErr != nil {
				t.Fatal(readErr)
			}
			afterMail, readErr := os.ReadFile(s.paths.MailboxFile(s.name))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if change == "unchanged" || change == "current-unrelated" {
				if err != nil || len(n.wakes) != 1 || dials != 1 {
					t.Fatalf("functional dispatch control: err=%v wakes=%d dials=%d", err, len(n.wakes), dials)
				}
				return
			}
			t.Logf("selected change=%s error=%v native_dials=%d native_wakes=%d state_changed=%t mailbox_changed=%t", change, err, dials, len(n.wakes), !bytes.Equal(beforeState, afterState), !bytes.Equal(beforeMail, afterMail))
			if err == nil || dials != 0 || len(n.wakes) != 0 || !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeMail, afterMail) {
				t.Error("stale selected connection must refuse before native RPC and preserve state/lease/mailbox")
			}
		})
	}
}
