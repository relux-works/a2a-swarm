//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// This synthetic CLI witness distinguishes two legal timeout phases: while the
// direct or duplicate-recovery update has not returned, the fence wins over
// cooldown; after its goroutine clears the fence, cooldown wins. Neither retry
// may reach a probe,
// backend or Matrix, or change recoverable credentials. A later clock-controlled
// success proves this fixture does not merely reject every migration. This is
// not a native Keychain, crash-durability or directory-fsync qualification.
func TestKeychainUpdateTimeoutFenceCooldownWitness(t *testing.T) {
	for _, stage := range migrationStages {
		if stage.mutation != "update" {
			continue
		}
		t.Run(stage.name, func(t *testing.T) {
			e := newEnv(t)
			uid := e.agent("alice")
			items := map[string]string{}
			wantWrites, wantItem := 1, "external-token" // duplicate-create preparation
			if stage.existing {
				items[uid] = "stale-" + e.tokens["alice"]
				wantWrites, wantItem = 0, items[uid]
			}
			dir := fakeKeychain(e, items, stage.mode)
			path := e.paths().AgentFile("alice")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originalStat, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			calls, probes := new(atomic.Int32), new(atomic.Int32)
			clock := new(atomic.Int64)
			clock.Store(time.Now().UnixNano())
			makeStore := func(p config.Paths) *keychainStore {
				return isolatedKeychainStore(keychainStore{paths: p, timeout: 300 * time.Millisecond,
					now:      func() time.Time { return time.Unix(0, clock.Load()) },
					birth:    func(context.Context, int) (string, error) { return "witness-birth", nil },
					liveness: func(context.Context, int, string) ownerState { return ownerAlive },
					boot:     func() (string, error) { return sessionA, nil },
				})
			}
			k := makeStore(e.paths())
			gate := newKeychainTimeoutWitnessGate(t, k, uid)
			backend := stagedBackend{fakeBackend: fakeBackend{dir}, stop: int32(stage.stop), fail: "timeout", calls: calls, gate: gate}
			e.configure = func(a *App) {
				a.keychain = func(p config.Paths) *keychainStore {
					store := makeStore(p)
					store.backend = backend
					store.probeCommand = func(ctx context.Context, args ...string) *exec.Cmd {
						probes.Add(1)
						return unlockedProbe(ctx, args...)
					}
					return store
				}
			}
			matrixCalls := len(e.hs.Calls())
			var heldModel []byte
			var heldRecords int
			assertPreserved := func(phase string) {
				t.Helper()
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, original) {
					t.Fatalf("%s changed recoverable credential bytes: %v", phase, err)
				}
				st, err := os.Lstat(path)
				if err != nil || !st.Mode().IsRegular() || st.Mode() != originalStat.Mode() {
					t.Fatalf("%s changed credential file type/mode: %v", phase, err)
				}
				if calls.Load() != int32(stage.stop) || probes.Load() != int32(stage.stop) || len(e.hs.Calls()) != matrixCalls {
					t.Fatalf("%s reached extra backend/probe/Matrix calls: backend=%d probes=%d Matrix=%d", phase, calls.Load(), probes.Load(), len(e.hs.Calls())-matrixCalls)
				}
				if model := readModel(t, dir); model.Writes != wantWrites || model.Items[uid] != wantItem {
					t.Fatalf("%s changed the fake item", phase)
				}
				afterModel, err := os.ReadFile(modelFile(dir))
				if err != nil || !bytes.Equal(afterModel, heldModel) || len(records(t, dir)) != heldRecords {
					t.Fatalf("%s changed fake state or operation records: %v", phase, err)
				}
				if cooldown := k.checkCooldown(uid); transientCredentialReason(cooldown) != "keychain_cooldown" || !strings.Contains(cooldown.Error(), "cause=security_timeout") {
					t.Fatalf("%s omitted typed timeout cooldown: %v", phase, cooldown)
				}
			}

			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cause=security_timeout")
			keychainWitnessSignal(t, gate.entered, "timed-out update held before return")
			fence, err := k.readFence(uid)
			if err != nil || fence == nil || fence.Op != "update" || fence.PID != os.Getpid() || fence.Digest != tokenDigest([]byte(e.tokens["alice"])) {
				t.Fatalf("held timeout omitted the intended update fence: %+v %v", fence, err)
			}
			heldModel, err = os.ReadFile(modelFile(dir))
			if err != nil {
				t.Fatal(err)
			}
			heldRecords = len(records(t, dir))
			assertPreserved("timeout")
			if err := k.checkFence(context.Background(), uid); !errors.Is(err, errKeychainFenced) {
				t.Fatal("held update did not report the outstanding fence", err)
			}
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "keychain write outstanding")
			assertPreserved("held retry")

			gate.finish(t, k, uid) // waits for BOTH return boundary and actual fence absence
			if err := k.checkFence(context.Background(), uid); err != nil {
				t.Fatal("returned update remains fenced", err)
			}
			e.as("alice", "creds", "migrate").failsWith(t, ExitError, "cooldown active")
			assertPreserved("returned retry")

			// This changes only the store's synthetic cooldown clock, never the hard
			// deadline. No elapsed sleep is used to expire cooldown or synchronize return.
			clock.Add((2 * securityCooldown).Nanoseconds())
			e.as("alice", "creds", "migrate").ok(t)
			if calls.Load() != int32(stage.stop+3) || probes.Load() != int32(stage.stop+3) || len(e.hs.Calls()) != matrixCalls {
				t.Fatal("post-cooldown control did not complete lookup/update/readback locally")
			}
			if model := readModel(t, dir); model.Writes != wantWrites+1 || model.Items[uid] != e.tokens["alice"] {
				t.Fatal("post-cooldown control did not store the intended value")
			}
			creds, err := config.LoadCredentialMetadata(e.paths(), "alice", false)
			if err != nil || creds.AccessToken != "" || creds.Password != "" || creds.UserID != uid {
				t.Fatal("successful control did not preserve identity and scrub migrated secrets", err)
			}
			if fence, err := k.readFence(uid); err != nil || fence != nil {
				t.Fatal("successful control left a fence", err)
			}
			if err := k.checkCooldown(uid); err != nil {
				t.Fatal("successful control manufactured cooldown", err)
			}
		})
	}
}
