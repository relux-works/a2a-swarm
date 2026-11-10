//go:build unix

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

const daemonFixtureName = "fixture"

func daemonPrivateDir(t *testing.T) string {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "private")
	if e := os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	return path
}

func daemonFixture(t *testing.T) (Paths, DaemonMigrationOptions) {
	t.Helper()
	p := Paths{Base: daemonPrivateDir(t)}
	f, e := CreateCredentialFile(p, daemonFixtureName)
	if e != nil {
		t.Fatal(e)
	}
	// Write dummy legacy bytes directly: WriteCredentials intentionally scrubs
	// passwords, while migration must preserve and remove all legacy secrets.
	data, e := json.Marshal(Credentials{UserID: "@fixture:example.invalid", AccessToken: "DUMMY-C4-file-canary", Password: "DUMMY-C4-password-canary", DeviceID: "fixture", Homeserver: "https://example.invalid"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(append(data, '\n')); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	if e = SaveState(p, daemonFixtureName, State{NextBatch: "fixture-cursor"}); e != nil {
		t.Fatal(e)
	}
	// Include opaque precision-bearing data and record the ordinary L02 fence.
	_, e = UpdateState(p, daemonFixtureName, func(s *State) error {
		s.opaque = map[string]json.RawMessage{"future": json.RawMessage(`{"n":9007199254740993,"v":[1.0,-0]}`)}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	for path, data := range map[string]string{
		p.MailboxFile(daemonFixtureName): "fixture-mailbox\n", filepath.Join(p.Base, "state", daemonFixtureName+".subscription-policy.json"): "fixture-policy\n",
		filepath.Join(p.Base, "state", daemonFixtureName+".runtime-consumer.lock"): "", filepath.Join(p.Base, "state", daemonFixtureName+".runtime-control.lock"): "",
		filepath.Join(p.Base, "agents", "."+daemonFixtureName+".lock"): "",
	} {
		if e = os.WriteFile(path, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.Mkdir(filepath.Join(p.Base, "owner-dm"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"config", "lock"} {
		if e = os.WriteFile(filepath.Join(p.Base, "owner-dm", daemonFixtureName+"."+suffix), []byte("fixture-owner-data"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return p, DaemonMigrationOptions{To: daemonPrivateDir(t), Now: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)}
}
func daemonRun(t *testing.T, p Paths, o DaemonMigrationOptions) {
	t.Helper()
	if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e != nil {
		t.Fatal(e)
	}
}
func daemonState(t *testing.T, p Paths) State {
	t.Helper()
	s, e := LoadState(p, daemonFixtureName)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func daemonAssertFenced(t *testing.T, p Paths) {
	t.Helper()
	if e := CheckIdentityMigration(p, daemonFixtureName); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("expected closed fence", e)
	}
}

// Walk every owned source path, including hidden temporary and renamed files.
// The separate Daemon directory is a fixture stand-in, not a cross-UID proof.
func daemonAssertSourceCustody(t *testing.T, p Paths, o DaemonMigrationOptions, cutover bool) {
	t.Helper()
	for _, root := range []string{p.Base, p.metadataRoot()} {
		e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if e != nil || d.IsDir() {
				return e
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			for _, canary := range []string{"DUMMY-C4-file-canary", "DUMMY-C4-password-canary"} {
				if bytes.Contains(b, []byte(canary)) && (cutover || path != p.AgentFile(daemonFixtureName)) {
					t.Error("source secret custody violation")
				}
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	b, e := daemonRead(p.AgentFile(daemonFixtureName), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	if cutover {
		var tombstone map[string]json.RawMessage
		if !bytes.Equal(b, daemonTombstone(daemonFixtureName, o.Now)) || json.Unmarshal(b, &tombstone) != nil || len(tombstone) != 3 {
			t.Fatal("tombstone is not the exact closed nonsecret schema")
		}
	} else if !bytes.Contains(b, []byte("DUMMY-C4-file-canary")) || !bytes.Contains(b, []byte("DUMMY-C4-password-canary")) {
		t.Fatal("source credentials did not remain in place before cutover")
	}
}

func TestDaemonMigrationRoundTrip(t *testing.T) {
	p, o := daemonFixture(t)

	before := daemonState(t, p)
	credentials, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	locks := map[string]os.FileInfo{}
	ownerLock := filepath.Join(p.Base, "owner-dm", daemonFixtureName+".lock")
	ownerInfo, e := os.Stat(ownerLock)
	if e != nil {
		t.Fatal(e)
	}
	locks[ownerLock] = ownerInfo
	for _, path := range []string{filepath.Join(p.Base, "state", daemonFixtureName+".lock"), filepath.Join(p.Base, "state", daemonFixtureName+".runtime-consumer.lock"), filepath.Join(p.Base, "state", daemonFixtureName+".runtime-control.lock"), filepath.Join(p.Base, "agents", "."+daemonFixtureName+".lock")} {
		info, e := os.Stat(path)
		if e != nil {
			t.Fatal(e)
		}
		locks[path] = info
	}
	daemonRun(t, p, o)
	daemonAssertFenced(t, p)
	done := daemonState(t, p)
	if done.Migration.Phase != "done" || !validWriterSeal(done) || !done.Writer.ForeignSeen.Equal(before.Writer.ForeignSeen) {
		t.Fatal("sealed done state did not preserve L02 writer fence")
	}
	daemonAssertSourceCustody(t, p, o, true)
	backup, e := os.ReadFile(daemonBackup(filepath.Join(o.To, "identities", daemonFixtureName)))
	if e != nil || !bytes.Equal(backup, credentials) {
		t.Fatal("backup mismatch")
	}
	if _, e = LoadCredentials(p, daemonFixtureName); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("credential load bypassed fence")
	}
	o.Rollback = true
	daemonRun(t, p, o)
	after := daemonState(t, p)
	if after.Migration != nil || !validWriterSeal(after) || !reflect.DeepEqual(before.opaque, after.opaque) || after.NextBatch != before.NextBatch || !after.Writer.ForeignSeen.Equal(before.Writer.ForeignSeen) {
		t.Fatal("rollback did not restore sealed state")
	}
	restored, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil || !bytes.Equal(restored, credentials) {
		t.Fatal("credential bytes not restored")
	}
	for path, info := range locks {
		got, e := os.Stat(path)
		if e != nil || !os.SameFile(info, got) {
			t.Fatal("lock inode changed")
		}
	}
	if _, e = LoadCredentials(p, daemonFixtureName); e != nil {
		t.Fatal("unfenced neighbor", e)
	}
}

func TestDaemonMigrationInterruptions(t *testing.T) {
	for _, stage := range []string{"fenced", "staged", "journaled", "copied", "copies_verified", "verified", "rollback_source_copied", "rollback_credential_copied", "rollback_verified", "cutover_ready", "cutover", "secret_deleted", "done"} {
		for _, rollback := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				p, o := daemonFixture(t)
				stopped := false
				o.step = func(label string) error {
					if label == stage && !stopped {
						stopped = true
						return errors.New("fixture interruption")
					}
					return nil
				}
				if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil || !stopped {
					t.Fatal("fault not reached")
				}
				daemonAssertFenced(t, p)
				daemonAssertSourceCustody(t, p, o, stage == "cutover" || stage == "secret_deleted" || stage == "done")
				retry := o
				retry.step = nil
				if e := MigrateDaemon(context.Background(), p, daemonFixtureName, retry); e == nil {
					t.Fatal("implicit resume allowed")
				}
				retry.Rollback = rollback
				retry.Resume = !rollback
				daemonRun(t, p, retry)
				if rollback {
					if e := CheckIdentityMigration(p, daemonFixtureName); e != nil {
						t.Fatal(e)
					}
				} else {
					daemonAssertFenced(t, p)
					retry.Resume = false
					retry.Rollback = true
					daemonRun(t, p, retry)
				}
			})
		}
	}
}

// Proves occupied/unsafe sources refuse without changing State, with a stopped
// valid neighbour per row; directory fixtures do not prove service-user ACLs.
func TestDaemonMigrationRefusals(t *testing.T) {
	for _, kind := range []string{"lease", "host-custody", "active-monitor", "terminal", "runtime", "consumer", "target", "symlink-source", "symlink-parent", "symlink-target", "mode", "credential-mode", "source-hardlink", "historical-secret-copy", "overlap", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			p, o := daemonFixture(t)
			switch kind {
			case "lease":
				_, e := UpdateState(p, daemonFixtureName, func(s *State) error {
					s.Lease = &Lease{Holder: "fixture-holder", Expires: o.Now.Add(-time.Hour)}
					return nil
				})
				if e != nil {
					t.Fatal(e)
				}
			case "host-custody":
				_, attachment, _ := hostFixture()
				_, e := UpdateState(p, daemonFixtureName, func(s *State) error {
					s.HostEnrollment = &HostEnrollment{Version: 1, Attachment: attachment}
					return nil
				})
				if e != nil {
					t.Fatal(e)
				}
			case "active-monitor":
				monitor := monitorRecord()
				state := daemonState(t, p)
				state.Monitor = &monitor
				data, e := sealedState(state)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p.StateFile(daemonFixtureName), data, 0600); e != nil {
					t.Fatal(e)
				}
			case "terminal":
				_, e := UpdateState(p, daemonFixtureName, func(s *State) error { s.Terminal = &TerminalBinding{}; return nil })
				if e != nil {
					t.Fatal(e)
				}
			case "runtime":
				_, e := UpdateState(p, daemonFixtureName, func(s *State) error { s.Runtime = &codexwake.Binding{}; return nil })
				if e != nil {
					t.Fatal(e)
				}
			case "consumer":
				release, e := AcquireConsumerLock(p, daemonFixtureName, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				defer release()
			case "target":
				if e := os.MkdirAll(filepath.Join(o.To, "identities", daemonFixtureName), 0700); e != nil {
					t.Fatal(e)
				}
			case "symlink-source":
				path := p.MailboxFile(daemonFixtureName)
				if e := os.Rename(path, path+".fixture"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(path+".fixture", path); e != nil {
					t.Fatal(e)
				}
			case "symlink-parent":
				parent, e := filepath.EvalSymlinks(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				link := filepath.Join(parent, "link")
				if e := os.Symlink(p.Base, link); e != nil {
					t.Fatal(e)
				}
				p.Base = link
			case "symlink-target":
				root, e := filepath.EvalSymlinks(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				parent, e := filepath.EvalSymlinks(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				link := filepath.Join(parent, "link")
				if e := os.Symlink(root, link); e != nil {
					t.Fatal(e)
				}
				o.To = link
			case "mode":
				if e := os.Chmod(p.Base, 0755); e != nil {
					t.Fatal(e)
				}
			case "credential-mode":
				if e := os.Chmod(p.AgentFile(daemonFixtureName), 0644); e != nil {
					t.Fatal(e)
				}
			case "source-hardlink":
				if e := os.Link(p.AgentFile(daemonFixtureName), filepath.Join(t.TempDir(), "link")); e != nil {
					t.Fatal(e)
				}
			case "historical-secret-copy":
				if e := os.WriteFile(p.AgentFile(daemonFixtureName)+".migrated-old", []byte("DUMMY-C4-file-canary"), 0600); e != nil {
					t.Fatal(e)
				}
			case "overlap":
				o.To = filepath.Join(p.Base, "daemon")
			case "ambiguous":
				p.MetadataBase = daemonPrivateDir(t)
				if e := os.Mkdir(filepath.Join(p.MetadataBase, "agents"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			stateBefore, e := os.ReadFile(p.StateFile(daemonFixtureName))
			if e != nil {
				t.Fatal(e)
			}
			if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); !errors.Is(e, ErrDaemonMigration) {
				t.Fatal("expected closed refusal", e)
			}
			stateAfter, e := os.ReadFile(p.StateFile(daemonFixtureName))
			if e != nil || !bytes.Equal(stateBefore, stateAfter) {
				t.Fatal("refusal changed source state")
			}
			// Every negative has an independent valid stopped-identity neighbour.
			good, options := daemonFixture(t)
			daemonRun(t, good, options)
		})
	}
}

func TestDaemonMigrationDigestMismatch(t *testing.T) {
	p, o := daemonFixture(t)
	o.step = func(stage string) error {
		if stage == "copied" {
			return errors.New("fixture interruption")
		}
		return nil
	}
	if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
		t.Fatal("interruption absent")
	}
	target := filepath.Join(o.To, "identities", daemonFixtureName, "agents", daemonFixtureName+".json")
	if e := os.WriteFile(target, []byte("fixture-corruption"), 0600); e != nil {
		t.Fatal(e)
	}
	o.step = nil
	o.Resume = true
	if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
		t.Fatal("corrupt copy accepted")
	}
	daemonAssertFenced(t, p)
	if _, e := os.Stat(p.AgentFile(daemonFixtureName)); e != nil {
		t.Fatal("unverified credential cutover")
	}
	good, options := daemonFixture(t)
	daemonRun(t, good, options)
}

type daemonFakeSecrets struct {
	present           bool
	value             string
	deletes, restores int
	beforeDelete      func() error
	failRestore       bool
}

func (s *daemonFakeSecrets) Read(_ context.Context, c Credentials) (Credentials, error) {
	if !s.present {
		return Credentials{}, errors.New("fixture missing")
	}
	c.AccessToken = s.value
	return c, nil
}
func (s *daemonFakeSecrets) Delete(_ context.Context, c Credentials) error {
	if s.beforeDelete != nil {
		if e := s.beforeDelete(); e != nil {
			return e
		}
	}
	if s.present && c.AccessToken != s.value {
		return errors.New("fixture mismatch")
	}
	s.present = false
	s.deletes++
	return nil
}
func (s *daemonFakeSecrets) Restore(_ context.Context, c Credentials) error {
	if s.failRestore {
		return errors.New("fixture failure")
	}
	if s.present && s.value != c.AccessToken {
		return errors.New("fixture conflict")
	}
	s.present = true
	s.value = c.AccessToken
	s.restores++
	return nil
}
func TestDaemonMigrationKeychainFixture(t *testing.T) {
	p, o := daemonFixture(t)
	raw, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	var c Credentials
	if e = json.Unmarshal(raw, &c); e != nil {
		t.Fatal(e)
	}
	c.AccessToken = ""
	c.Password = ""
	data, e := marshalCredentials(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p.AgentFile(daemonFixtureName), data, 0600); e != nil {
		t.Fatal(e)
	}
	store := &daemonFakeSecrets{present: true, value: "inert-keychain-fixture"}
	o.Keychain = true
	o.Secrets = store
	store.beforeDelete = func() error {
		target := filepath.Join(o.To, "identities", daemonFixtureName)
		b, e := daemonRead(daemonRollbackCredential(target), 1<<20)
		if e != nil {
			return e
		}
		var saved Credentials
		if json.Unmarshal(b, &saved) != nil || saved.AccessToken != store.value {
			return errors.New("fixture target absent")
		}
		if b, e = daemonRead(daemonBackup(target), 1<<20); e != nil || !bytes.Equal(b, data) {
			return errors.New("fixture private metadata backup absent")
		}
		return nil
	}
	o.step = func(stage string) error {
		if stage == "secret_deleted" {
			return errors.New("fixture interruption")
		}
		return nil
	}
	if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil || store.present {
		t.Fatal("delete interruption not reached")
	}
	daemonAssertFenced(t, p)
	o.step = nil
	o.Resume = true
	daemonRun(t, p, o)
	o.Resume = false
	o.Rollback = true
	store.failRestore = true
	if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
		t.Fatal("failed restore accepted")
	}
	daemonAssertFenced(t, p)
	store.failRestore = false
	daemonRun(t, p, o)
	if !store.present || store.restores != 1 {
		t.Fatal("secret not restored from Daemon copy")
	}
	restored, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil || !bytes.Equal(restored, data) {
		t.Fatal("metadata bytes changed")
	}
	if e = CheckIdentityMigration(p, daemonFixtureName); e != nil {
		t.Fatal(e)
	}
}

// Proves a durable in-progress migration refuses every monitor entry and the
// owner-hook writer without changing State or calling the monitor body. Each
// refusal has an otherwise identical admitted-owner neighbour without migration.
// Equal/older Starts retain #389's owner refusal outside migration; migration
// takes precedence over that refusal. Valid hook neighbours use a newer Start.
// This tests local admission, not live hooks or Daemon station authentication.
func TestDaemonMigrationMonitorAdmission(t *testing.T) {
	for _, entry := range []MonitorEntry{MonitorArm, MonitorAutomatic, MonitorRecovery} {
		for _, migrating := range []bool{false, true} {
			t.Run(fmt.Sprintf("entry-%d/migrating-%t", entry, migrating), func(t *testing.T) {
				p, o := daemonFixture(t)
				record := monitorRecord()
				if err := RecordMonitorOwnerHook(p, daemonFixtureName, time.Now().Add(time.Second), record.Runtime, record.SessionID, "SessionStart", false, time.Now()); err != nil {
					t.Fatal("admitted owner fixture", err)
				}
				if migrating {
					interrupted := errors.New("fixture migration interrupted after fence")
					o.step = func(stage string) error {
						if stage == "fenced" {
							return interrupted
						}
						return nil
					}
					if err := MigrateDaemon(t.Context(), p, daemonFixtureName, o); err == nil {
						t.Fatal("migration did not stop at fence")
					}
				}
				s := daemonState(t, p)
				if !validWriterSeal(s) || !s.monitorOwnerMatches(record, time.Now()) || (s.Migration != nil) != migrating || migrating && s.Migration.Phase != "in_progress" {
					t.Fatal("fixture must have intact seal, admitted owner, and expected migration phase")
				}
				before, err := os.ReadFile(p.StateFile(daemonFixtureName))
				if err != nil {
					t.Fatal(err)
				}
				called := false
				err = WithMonitorGeneration(p, daemonFixtureName, time.Now().Add(time.Second), entry, record, func(g *MonitorGeneration) error {
					called = true
					return g.CheckRenewal(record.Generation)
				})
				if migrating {
					if !errors.Is(err, ErrIdentityMigrated) || called {
						t.Fatal("monitor crossed migration fence", err, called)
					}
				} else if err != nil || !called {
					t.Fatal("valid monitor neighbour was not admitted", err, called)
				}
				for _, at := range []time.Time{s.MonitorOwner.LastHookAt, s.MonitorOwner.LastHookAt.Add(-time.Nanosecond)} {
					beforeHook, err := os.ReadFile(p.StateFile(daemonFixtureName))
					if err != nil {
						t.Fatal(err)
					}
					want := ErrMonitorOwner
					if migrating {
						want = ErrIdentityMigrated
					}
					if err := RecordMonitorOwnerHook(p, daemonFixtureName, time.Now().Add(time.Second), record.Runtime, record.SessionID, "SessionStart", false, at); !errors.Is(err, want) {
						t.Fatal("non-increasing Start did not retain owner/migration refusal", err)
					}
					after, err := os.ReadFile(p.StateFile(daemonFixtureName))
					if err != nil || !bytes.Equal(beforeHook, after) {
						t.Fatal("refused non-increasing Start changed State", err)
					}
				}
				for _, event := range []string{"SessionStart", "UserPromptSubmit", "SessionEnd"} {
					at := time.Now()
					if event == "SessionStart" {
						at = s.MonitorOwner.LastHookAt.Add(time.Nanosecond)
					}
					err = RecordMonitorOwnerHook(p, daemonFixtureName, time.Now().Add(time.Second), record.Runtime, record.SessionID, event, false, at)
					if migrating {
						if !errors.Is(err, ErrIdentityMigrated) {
							t.Fatal("owner hook crossed migration fence", event, err)
						}
						after, err := os.ReadFile(p.StateFile(daemonFixtureName))
						if err != nil || !bytes.Equal(before, after) {
							t.Fatal("refused admission changed State", event, err)
						}
					} else if err != nil {
						t.Fatal("valid owner-hook neighbour was not admitted", event, err)
					}
				}
			})
		}
	}
}

func TestDaemonMigrationFenceEffects(t *testing.T) {
	p, o := daemonFixture(t)
	daemonRun(t, p, o)
	before, e := os.ReadFile(p.StateFile(daemonFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	called := false
	if _, e = UpdateState(p, daemonFixtureName, func(*State) error { called = true; return nil }); !errors.Is(e, ErrIdentityMigrated) || called {
		t.Fatal("ordinary callback ran through fence")
	}
	if e = SaveState(p, daemonFixtureName, State{}); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("stale save cleared fence")
	}
	for _, holder := range []string{"fixture-holder", "fixture-renew"} {
		s := daemonState(t, p)
		s.Lease = &Lease{Holder: holder, Expires: o.Now.Add(time.Hour)}
		if e = s.Acquire(daemonFixtureName, holder, "fixture-host", 1, o.Now, LeaseTTL, nil); !errors.Is(e, ErrIdentityMigrated) {
			t.Fatal("lease acquired/renewed through fence")
		}
	}
	if _, e = AcquireConsumerLock(p, daemonFixtureName, time.Now()); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("consumer admitted through fence")
	}
	if e = WithRuntimeLock(p, daemonFixtureName, "consumer", time.Now(), func() error { called = true; return nil }); !errors.Is(e, ErrIdentityMigrated) || called {
		t.Fatal("runtime consumer admitted")
	}
	after, e := os.ReadFile(p.StateFile(daemonFixtureName))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("fenced operation mutated state")
	}
	good, _ := daemonFixture(t)
	if _, e = UpdateState(good, daemonFixtureName, func(s *State) error {
		return s.Acquire(daemonFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
	}); e != nil {
		t.Fatal("valid lease neighbor", e)
	}
	release, e := AcquireConsumerLock(good, daemonFixtureName, time.Now())
	if e != nil {
		t.Fatal("valid consumer neighbor", e)
	}
	release()
}

// Historical probes established that renaming a consumer lock opens a second
// inode, a consumer lock alone does not exclude leases, and a cached session
// can recreate missing State. Migration must preserve the first inode and keep
// both admissions closed after releasing its locks, without a timing window.
func TestDaemonHistoricalLockRegression(t *testing.T) {
	p, o := daemonFixture(t)
	path := filepath.Join(p.Base, "state", daemonFixtureName+".runtime-consumer.lock")
	inode, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	o.step = func(stage string) error {
		info, e := os.Stat(path)
		if e != nil || !os.SameFile(inode, info) {
			return errors.New("fixture inode moved")
		}
		if _, e = AcquireConsumerLock(p, daemonFixtureName, time.Now()); !errors.Is(e, ErrLockTimeout) {
			return errors.New("fixture concurrent consumer admitted")
		}
		called := false
		if _, e = UpdateStateBy(p, daemonFixtureName, time.Now(), func(s *State) error {
			called = true
			return s.Acquire(daemonFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
		}); !errors.Is(e, ErrLockTimeout) || called {
			return errors.New("fixture lease entered during copy")
		}
		return nil
	}
	daemonRun(t, p, o)
	if _, e = AcquireConsumerLock(p, daemonFixtureName, time.Now()); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("post-release consumer admitted", e)
	}
	if _, e = UpdateState(p, daemonFixtureName, func(s *State) error {
		return s.Acquire(daemonFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
	}); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("post-release lease admitted", e)
	}
	o.Rollback = true
	o.step = nil
	daemonRun(t, p, o)
	release, e := AcquireConsumerLock(p, daemonFixtureName, time.Now())
	if e != nil {
		t.Fatal("unfenced neighbor", e)
	}
	release()
}

// No sleep is used: the channels place the writer precisely between durable
// fence publication and consumer/state lock release.
func TestDaemonMigrationConcurrentWriter(t *testing.T) {
	p, o := daemonFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	fenced, release := make(chan struct{}), make(chan struct{})
	o.step = func(stage string) error {
		if stage == "fenced" {
			close(fenced)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	migrated := make(chan error, 1)
	go func() { migrated <- MigrateDaemon(ctx, p, daemonFixtureName, o) }()
	select {
	case <-fenced:
	case e := <-migrated:
		t.Fatal("migration did not reach fence", e)
	case <-ctx.Done():
		t.Fatal("fixture deadline", ctx.Err())
	}
	mutation := make(chan error, 1)
	called := false
	go func() {
		_, e := UpdateState(p, daemonFixtureName, func(s *State) error { called = true; s.NextBatch = "wrong"; return nil })
		mutation <- e
	}()
	close(release)
	select {
	case e := <-migrated:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("migration completion deadline", ctx.Err())
	}
	select {
	case e := <-mutation:
		if !errors.Is(e, ErrIdentityMigrated) || called {
			t.Fatal("concurrent callback crossed fence", e)
		}
	case <-ctx.Done():
		t.Fatal("mutation completion deadline", ctx.Err())
	}
	o.step = nil
	o.Rollback = true
	daemonRun(t, p, o)
	if _, e := UpdateState(p, daemonFixtureName, func(s *State) error { s.NextBatch = "valid-neighbor"; return nil }); e != nil {
		t.Fatal(e)
	}
}

type daemonStatFixture struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (s daemonStatFixture) Sys() any { return &s.stat }
func TestDaemonOwnership(t *testing.T) {
	p, _ := daemonFixture(t)
	info, e := os.Stat(p.AgentFile(daemonFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	st := *info.Sys().(*syscall.Stat_t)
	st.Uid = uint32(os.Geteuid() + 1)
	if e = daemonOwned(daemonStatFixture{FileInfo: info, stat: st}, false); e == nil {
		t.Fatal("foreign owner accepted")
	}
	if e = daemonOwned(info, false); e != nil {
		t.Fatal("own-file neighbor", e)
	}
}

func TestDaemonMigrationSplitRoots(t *testing.T) {
	p, o := daemonFixture(t)
	metadata := daemonPrivateDir(t)
	if e := os.Mkdir(filepath.Join(metadata, "agents"), 0700); e != nil {
		t.Fatal(e)
	}
	original := p.AgentFile(daemonFixtureName)
	p.MetadataBase = metadata
	if e := os.Rename(original, p.AgentFile(daemonFixtureName)); e != nil {
		t.Fatal(e)
	}
	daemonRun(t, p, o)
	if _, e := os.Stat(filepath.Join(o.To, "identities", daemonFixtureName, "agents", daemonFixtureName+".json")); e != nil {
		t.Fatal(e)
	}
	o.Rollback = true
	daemonRun(t, p, o)
	if _, e := LoadCredentials(p, daemonFixtureName); e != nil {
		t.Fatal("resolved metadata root not restored", e)
	}
	if _, e := os.Stat(original); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("inferred legacy metadata root")
	}
}

func TestDaemonMigrationRollbackDamage(t *testing.T) {
	for _, damage := range []string{"credential", "source-credential", "missing-private-credential", "mailbox", "journal", "restore-interrupted"} {
		t.Run(damage, func(t *testing.T) {
			p, o := daemonFixture(t)
			daemonRun(t, p, o)
			target := filepath.Join(o.To, "identities", daemonFixtureName)
			switch damage {
			case "credential":
				if e := os.WriteFile(daemonRollbackCredential(target), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "source-credential":
				if e := os.WriteFile(daemonBackup(target), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "missing-private-credential":
				if e := os.Rename(daemonBackup(target), filepath.Join(target, "detached-fixture")); e != nil {
					t.Fatal(e)
				}
			case "mailbox":
				if e := os.WriteFile(filepath.Join(target, "state", daemonFixtureName+".mailbox.jsonl"), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "journal":
				if e := os.WriteFile(filepath.Join(target, daemonJournalName), []byte(`{"identity":"someone-else"}`), 0600); e != nil {
					t.Fatal(e)
				}
			case "restore-interrupted":
				o.step = func(stage string) error {
					if stage == "restored" {
						return errors.New("fixture interruption")
					}
					return nil
				}
			}
			o.Rollback = true
			if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
				t.Fatal("damaged rollback cleared fence")
			}
			daemonAssertFenced(t, p)
			if damage == "restore-interrupted" {
				o.step = nil
				daemonRun(t, p, o)
			}
			good, options := daemonFixture(t)
			daemonRun(t, good, options)
			options.Rollback = true
			daemonRun(t, good, options)
		})
	}
}

func TestDaemonTargetPublishNoReplacement(t *testing.T) {
	root := daemonPrivateDir(t)
	from := filepath.Join(root, "staging")
	to := filepath.Join(root, "target")
	for _, path := range []string{from, to} {
		if e := os.Mkdir(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	info, e := os.Stat(to)
	if e != nil {
		t.Fatal(e)
	}
	if e = daemonPublishDirectory(from, to); e == nil {
		t.Fatal("even an empty existing target was replaced")
	}
	after, e := os.Stat(to)
	if e != nil || !os.SameFile(info, after) {
		t.Fatal("target inode changed")
	}
	neighbor := filepath.Join(root, "neighbor")
	if e = daemonPublishDirectory(from, neighbor); e != nil {
		t.Fatal("absent-target neighbor", e)
	}
}

func TestDaemonMissingTargetDoesNotReopen(t *testing.T) {
	for _, phase := range []string{"cutover", "done"} {
		t.Run(phase, func(t *testing.T) {
			p, o := daemonFixture(t)
			if phase == "cutover" {
				o.step = func(stage string) error {
					if stage == "cutover" {
						return errors.New("fixture interruption")
					}
					return nil
				}
				if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
					t.Fatal("interruption absent")
				}
			} else {
				daemonRun(t, p, o)
			}
			target := filepath.Join(o.To, "identities", daemonFixtureName)
			if e := os.Rename(target, filepath.Join(o.To, "fixture-detached-copy")); e != nil {
				t.Fatal(e)
			}
			o.step = nil
			o.Rollback = true
			if e := MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
				t.Fatal("missing target reopened a cut-over identity")
			}
			daemonAssertFenced(t, p)
			// Identical transaction, now with its verified copy available again.
			if e := os.Rename(filepath.Join(o.To, "fixture-detached-copy"), target); e != nil {
				t.Fatal(e)
			}
			daemonRun(t, p, o)
			if e := CheckIdentityMigration(p, daemonFixtureName); e != nil {
				t.Fatal("verified restoration neighbor", e)
			}
		})
	}
}

func TestDaemonPrivateRollbackOnly(t *testing.T) {
	p, o := daemonFixture(t)
	original, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	daemonRun(t, p, o)
	target := filepath.Join(o.To, "identities", daemonFixtureName)
	// Poison redundant snapshots: rollback must read neither credential copy.
	for _, path := range []string{filepath.Join(target, "agents", daemonFixtureName+".json"), filepath.Join(target, "credential.json")} {
		if e = os.WriteFile(path, []byte("DUMMY-poisoned-redundant-copy"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	o.Rollback = true
	daemonRun(t, p, o)
	got, e := os.ReadFile(p.AgentFile(daemonFixtureName))
	if e != nil || !bytes.Equal(got, original) || daemonState(t, p).Migration != nil {
		t.Fatal("private-only rollback failed")
	}
	for _, path := range []string{filepath.Join(target, "rollback"), daemonBackup(target), daemonRollbackCredential(target)} {
		info, e := os.Lstat(path)
		if e != nil || daemonOwned(info, info.IsDir()) != nil {
			t.Fatal("private rollback mode/owner mismatch")
		}
	}
}

func TestDaemonKeychainBoundaryCustody(t *testing.T) {
	for _, stage := range []string{"fenced", "staged", "journaled", "copied", "copies_verified", "verified", "rollback_source_copied", "rollback_credential_copied", "rollback_verified", "cutover_ready", "cutover", "secret_deleted", "done"} {
		for _, rollback := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				p, o := daemonFixture(t)
				c := Credentials{UserID: "@fixture:example.invalid", Homeserver: "https://example.invalid", DeviceID: "fixture"}
				metadata, e := marshalCredentials(c)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p.AgentFile(daemonFixtureName), metadata, 0600); e != nil {
					t.Fatal(e)
				}
				store := &daemonFakeSecrets{present: true, value: "DUMMY-C4-keychain-canary"}
				o.Keychain, o.Secrets = true, store
				o.step = func(label string) error {
					if label == stage {
						return errors.New("DUMMY interruption")
					}
					return nil
				}
				if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
					t.Fatal("boundary was not interrupted")
				}
				daemonAssertFenced(t, p)
				cutover := stage == "cutover" || stage == "secret_deleted" || stage == "done"
				if store.present == cutover {
					t.Fatal("wrong source Keychain custody at boundary")
				}
				if e = filepath.WalkDir(p.Base, func(path string, d os.DirEntry, e error) error {
					if e != nil || d.IsDir() {
						return e
					}
					b, e := os.ReadFile(path)
					if bytes.Contains(b, []byte(store.value)) {
						t.Error("source Keychain secret copy")
					}
					return e
				}); e != nil {
					t.Fatal(e)
				}
				o.step = nil
				o.Rollback, o.Resume = rollback, !rollback
				daemonRun(t, p, o)
				if !rollback {
					if store.present {
						t.Fatal("resumed Keychain cutover did not delete")
					}
					o.Resume, o.Rollback = false, true
					// Runtime snapshots cannot substitute for private rollback data.
					target := filepath.Join(o.To, "identities", daemonFixtureName)
					for _, path := range []string{filepath.Join(target, "credential.json"), filepath.Join(target, "agents", daemonFixtureName+".json")} {
						if e = os.WriteFile(path, []byte("DUMMY redundant corruption"), 0600); e != nil {
							t.Fatal(e)
						}
					}
					daemonRun(t, p, o)
				}
				got, e := os.ReadFile(p.AgentFile(daemonFixtureName))
				if e != nil || !bytes.Equal(got, metadata) || !store.present || store.value != "DUMMY-C4-keychain-canary" || daemonState(t, p).Migration != nil {
					t.Fatal("Keychain rollback did not restore original backend before fence clear")
				}
			})
		}
	}
}

func TestDaemonMissingKeychainTargetDoesNotReopen(t *testing.T) {
	for _, stage := range []string{"cutover_ready", "cutover", "done"} {
		t.Run(stage, func(t *testing.T) {
			p, o := daemonFixture(t)
			metadata, e := marshalCredentials(Credentials{UserID: "@fixture:example.invalid", Homeserver: "https://example.invalid"})
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(p.AgentFile(daemonFixtureName), metadata, 0600); e != nil {
				t.Fatal(e)
			}
			store := &daemonFakeSecrets{present: true, value: "DUMMY-C4-keychain-canary"}
			o.Keychain, o.Secrets = true, store
			o.step = func(label string) error {
				if label == stage {
					return errors.New("DUMMY interruption")
				}
				return nil
			}
			if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil || store.present != (stage == "cutover_ready") {
				t.Fatal("cutover boundary not reached")
			}
			// A recreated source item still must not make a post-intent lost-target
			// transaction look like a pre-cutover abort.
			store.present = true
			target := filepath.Join(o.To, "identities", daemonFixtureName)
			detached := filepath.Join(o.To, "DUMMY-detached-private-copy")
			if e = os.Rename(target, detached); e != nil {
				t.Fatal(e)
			}
			o.step, o.Rollback = nil, true
			if e = MigrateDaemon(context.Background(), p, daemonFixtureName, o); e == nil {
				t.Fatal("metadata alone reopened deleted Keychain backend")
			}
			daemonAssertFenced(t, p)
			if store.restores != 0 || !store.present {
				t.Fatal("missing target altered Keychain")
			}
			if e = os.Rename(detached, target); e != nil {
				t.Fatal(e)
			}
			daemonRun(t, p, o)
			got, e := os.ReadFile(p.AgentFile(daemonFixtureName))
			if e != nil || !bytes.Equal(got, metadata) || !store.present || store.restores != 1 || daemonState(t, p).Migration != nil {
				t.Fatal("private restoration neighbour failed")
			}
		})
	}
}
