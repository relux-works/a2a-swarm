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

const courierFixtureName = "fixture"

func courierPrivateDir(t *testing.T) string {
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

func courierFixture(t *testing.T) (Paths, CourierMigrationOptions) {
	t.Helper()
	p := Paths{Base: courierPrivateDir(t)}
	f, e := CreateCredentialFile(p, courierFixtureName)
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
	if e = SaveState(p, courierFixtureName, State{NextBatch: "fixture-cursor"}); e != nil {
		t.Fatal(e)
	}
	// Include opaque precision-bearing data and record the ordinary L02 fence.
	_, e = UpdateState(p, courierFixtureName, func(s *State) error {
		s.opaque = map[string]json.RawMessage{"future": json.RawMessage(`{"n":9007199254740993,"v":[1.0,-0]}`)}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	for path, data := range map[string]string{
		p.MailboxFile(courierFixtureName): "fixture-mailbox\n", filepath.Join(p.Base, "state", courierFixtureName+".subscription-policy.json"): "fixture-policy\n",
		filepath.Join(p.Base, "state", courierFixtureName+".runtime-consumer.lock"): "", filepath.Join(p.Base, "state", courierFixtureName+".runtime-control.lock"): "",
		filepath.Join(p.Base, "agents", "."+courierFixtureName+".lock"): "",
	} {
		if e = os.WriteFile(path, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.Mkdir(filepath.Join(p.Base, "owner-dm"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"config", "lock"} {
		if e = os.WriteFile(filepath.Join(p.Base, "owner-dm", courierFixtureName+"."+suffix), []byte("fixture-owner-data"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return p, CourierMigrationOptions{To: courierPrivateDir(t), Now: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)}
}
func courierRun(t *testing.T, p Paths, o CourierMigrationOptions) {
	t.Helper()
	if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e != nil {
		t.Fatal(e)
	}
}
func courierState(t *testing.T, p Paths) State {
	t.Helper()
	s, e := LoadState(p, courierFixtureName)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func courierAssertFenced(t *testing.T, p Paths) {
	t.Helper()
	if e := CheckIdentityMigration(p, courierFixtureName); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("expected closed fence", e)
	}
}

// Walk every owned source path, including hidden temporary and renamed files.
// The separate Courier directory is a fixture stand-in, not a cross-UID proof.
func courierAssertSourceCustody(t *testing.T, p Paths, o CourierMigrationOptions, cutover bool) {
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
				if bytes.Contains(b, []byte(canary)) && (cutover || path != p.AgentFile(courierFixtureName)) {
					t.Error("source secret custody violation")
				}
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	b, e := courierRead(p.AgentFile(courierFixtureName), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	if cutover {
		var tombstone map[string]json.RawMessage
		if !bytes.Equal(b, courierTombstone(courierFixtureName, o.Now)) || json.Unmarshal(b, &tombstone) != nil || len(tombstone) != 3 {
			t.Fatal("tombstone is not the exact closed nonsecret schema")
		}
	} else if !bytes.Contains(b, []byte("DUMMY-C4-file-canary")) || !bytes.Contains(b, []byte("DUMMY-C4-password-canary")) {
		t.Fatal("source credentials did not remain in place before cutover")
	}
}

func TestCourierMigrationRoundTrip(t *testing.T) {
	p, o := courierFixture(t)

	before := courierState(t, p)
	credentials, e := os.ReadFile(p.AgentFile(courierFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	locks := map[string]os.FileInfo{}
	ownerLock := filepath.Join(p.Base, "owner-dm", courierFixtureName+".lock")
	ownerInfo, e := os.Stat(ownerLock)
	if e != nil {
		t.Fatal(e)
	}
	locks[ownerLock] = ownerInfo
	for _, path := range []string{filepath.Join(p.Base, "state", courierFixtureName+".lock"), filepath.Join(p.Base, "state", courierFixtureName+".runtime-consumer.lock"), filepath.Join(p.Base, "state", courierFixtureName+".runtime-control.lock"), filepath.Join(p.Base, "agents", "."+courierFixtureName+".lock")} {
		info, e := os.Stat(path)
		if e != nil {
			t.Fatal(e)
		}
		locks[path] = info
	}
	courierRun(t, p, o)
	courierAssertFenced(t, p)
	done := courierState(t, p)
	if done.Migration.Phase != "done" || !validWriterSeal(done) || !done.Writer.ForeignSeen.Equal(before.Writer.ForeignSeen) {
		t.Fatal("sealed done state did not preserve L02 writer fence")
	}
	courierAssertSourceCustody(t, p, o, true)
	backup, e := os.ReadFile(courierBackup(filepath.Join(o.To, "identities", courierFixtureName)))
	if e != nil || !bytes.Equal(backup, credentials) {
		t.Fatal("backup mismatch")
	}
	if _, e = LoadCredentials(p, courierFixtureName); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("credential load bypassed fence")
	}
	o.Rollback = true
	courierRun(t, p, o)
	after := courierState(t, p)
	if after.Migration != nil || !validWriterSeal(after) || !reflect.DeepEqual(before.opaque, after.opaque) || after.NextBatch != before.NextBatch || !after.Writer.ForeignSeen.Equal(before.Writer.ForeignSeen) {
		t.Fatal("rollback did not restore sealed state")
	}
	restored, e := os.ReadFile(p.AgentFile(courierFixtureName))
	if e != nil || !bytes.Equal(restored, credentials) {
		t.Fatal("credential bytes not restored")
	}
	for path, info := range locks {
		got, e := os.Stat(path)
		if e != nil || !os.SameFile(info, got) {
			t.Fatal("lock inode changed")
		}
	}
	if _, e = LoadCredentials(p, courierFixtureName); e != nil {
		t.Fatal("unfenced neighbor", e)
	}
}

func TestCourierMigrationInterruptions(t *testing.T) {
	for _, stage := range []string{"fenced", "staged", "journaled", "copied", "copies_verified", "verified", "rollback_source_copied", "rollback_credential_copied", "rollback_verified", "cutover_ready", "cutover", "secret_deleted", "done"} {
		for _, rollback := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				p, o := courierFixture(t)
				stopped := false
				o.step = func(label string) error {
					if label == stage && !stopped {
						stopped = true
						return errors.New("fixture interruption")
					}
					return nil
				}
				if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil || !stopped {
					t.Fatal("fault not reached")
				}
				courierAssertFenced(t, p)
				courierAssertSourceCustody(t, p, o, stage == "cutover" || stage == "secret_deleted" || stage == "done")
				retry := o
				retry.step = nil
				if e := MigrateCourier(context.Background(), p, courierFixtureName, retry); e == nil {
					t.Fatal("implicit resume allowed")
				}
				retry.Rollback = rollback
				retry.Resume = !rollback
				courierRun(t, p, retry)
				if rollback {
					if e := CheckIdentityMigration(p, courierFixtureName); e != nil {
						t.Fatal(e)
					}
				} else {
					courierAssertFenced(t, p)
					retry.Resume = false
					retry.Rollback = true
					courierRun(t, p, retry)
				}
			})
		}
	}
}

func TestCourierMigrationRefusals(t *testing.T) {
	for _, kind := range []string{"lease", "terminal", "runtime", "consumer", "target", "symlink-source", "symlink-parent", "symlink-target", "mode", "credential-mode", "source-hardlink", "historical-secret-copy", "overlap", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			p, o := courierFixture(t)
			switch kind {
			case "lease":
				_, e := UpdateState(p, courierFixtureName, func(s *State) error {
					s.Lease = &Lease{Holder: "fixture-holder", Expires: o.Now.Add(-time.Hour)}
					return nil
				})
				if e != nil {
					t.Fatal(e)
				}
			case "terminal":
				_, e := UpdateState(p, courierFixtureName, func(s *State) error { s.Terminal = &TerminalBinding{}; return nil })
				if e != nil {
					t.Fatal(e)
				}
			case "runtime":
				_, e := UpdateState(p, courierFixtureName, func(s *State) error { s.Runtime = &codexwake.Binding{}; return nil })
				if e != nil {
					t.Fatal(e)
				}
			case "consumer":
				release, e := AcquireConsumerLock(p, courierFixtureName, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				defer release()
			case "target":
				if e := os.MkdirAll(filepath.Join(o.To, "identities", courierFixtureName), 0700); e != nil {
					t.Fatal(e)
				}
			case "symlink-source":
				path := p.MailboxFile(courierFixtureName)
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
				if e := os.Chmod(p.AgentFile(courierFixtureName), 0644); e != nil {
					t.Fatal(e)
				}
			case "source-hardlink":
				if e := os.Link(p.AgentFile(courierFixtureName), filepath.Join(t.TempDir(), "link")); e != nil {
					t.Fatal(e)
				}
			case "historical-secret-copy":
				if e := os.WriteFile(p.AgentFile(courierFixtureName)+".migrated-old", []byte("DUMMY-C4-file-canary"), 0600); e != nil {
					t.Fatal(e)
				}
			case "overlap":
				o.To = filepath.Join(p.Base, "courier")
			case "ambiguous":
				p.MetadataBase = courierPrivateDir(t)
				if e := os.Mkdir(filepath.Join(p.MetadataBase, "agents"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			stateBefore, e := os.ReadFile(p.StateFile(courierFixtureName))
			if e != nil {
				t.Fatal(e)
			}
			if e = MigrateCourier(context.Background(), p, courierFixtureName, o); !errors.Is(e, ErrCourierMigration) {
				t.Fatal("expected closed refusal", e)
			}
			stateAfter, e := os.ReadFile(p.StateFile(courierFixtureName))
			if e != nil || !bytes.Equal(stateBefore, stateAfter) {
				t.Fatal("refusal changed source state")
			}
			// Every negative has an independent valid stopped-identity neighbour.
			good, options := courierFixture(t)
			courierRun(t, good, options)
		})
	}
}

func TestCourierMigrationDigestMismatch(t *testing.T) {
	p, o := courierFixture(t)
	o.step = func(stage string) error {
		if stage == "copied" {
			return errors.New("fixture interruption")
		}
		return nil
	}
	if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
		t.Fatal("interruption absent")
	}
	target := filepath.Join(o.To, "identities", courierFixtureName, "agents", courierFixtureName+".json")
	if e := os.WriteFile(target, []byte("fixture-corruption"), 0600); e != nil {
		t.Fatal(e)
	}
	o.step = nil
	o.Resume = true
	if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
		t.Fatal("corrupt copy accepted")
	}
	courierAssertFenced(t, p)
	if _, e := os.Stat(p.AgentFile(courierFixtureName)); e != nil {
		t.Fatal("unverified credential cutover")
	}
	good, options := courierFixture(t)
	courierRun(t, good, options)
}

type courierFakeSecrets struct {
	present           bool
	value             string
	deletes, restores int
	beforeDelete      func() error
	failRestore       bool
}

func (s *courierFakeSecrets) Read(_ context.Context, c Credentials) (Credentials, error) {
	if !s.present {
		return Credentials{}, errors.New("fixture missing")
	}
	c.AccessToken = s.value
	return c, nil
}
func (s *courierFakeSecrets) Delete(_ context.Context, c Credentials) error {
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
func (s *courierFakeSecrets) Restore(_ context.Context, c Credentials) error {
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
func TestCourierMigrationKeychainFixture(t *testing.T) {
	p, o := courierFixture(t)
	raw, e := os.ReadFile(p.AgentFile(courierFixtureName))
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
	if e = os.WriteFile(p.AgentFile(courierFixtureName), data, 0600); e != nil {
		t.Fatal(e)
	}
	store := &courierFakeSecrets{present: true, value: "inert-keychain-fixture"}
	o.Keychain = true
	o.Secrets = store
	store.beforeDelete = func() error {
		target := filepath.Join(o.To, "identities", courierFixtureName)
		b, e := courierRead(courierRollbackCredential(target), 1<<20)
		if e != nil {
			return e
		}
		var saved Credentials
		if json.Unmarshal(b, &saved) != nil || saved.AccessToken != store.value {
			return errors.New("fixture target absent")
		}
		if b, e = courierRead(courierBackup(target), 1<<20); e != nil || !bytes.Equal(b, data) {
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
	if e = MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil || store.present {
		t.Fatal("delete interruption not reached")
	}
	courierAssertFenced(t, p)
	o.step = nil
	o.Resume = true
	courierRun(t, p, o)
	o.Resume = false
	o.Rollback = true
	store.failRestore = true
	if e = MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
		t.Fatal("failed restore accepted")
	}
	courierAssertFenced(t, p)
	store.failRestore = false
	courierRun(t, p, o)
	if !store.present || store.restores != 1 {
		t.Fatal("secret not restored from Courier copy")
	}
	restored, e := os.ReadFile(p.AgentFile(courierFixtureName))
	if e != nil || !bytes.Equal(restored, data) {
		t.Fatal("metadata bytes changed")
	}
	if e = CheckIdentityMigration(p, courierFixtureName); e != nil {
		t.Fatal(e)
	}
}

// Proves a durable in-progress migration refuses every monitor entry and the
// owner-hook writer without changing State or calling the monitor body. Each
// refusal has an otherwise identical admitted-owner neighbour without migration.
// This tests local admission, not live hooks or Courier station authentication.
func TestCourierMigrationMonitorAdmission(t *testing.T) {
	for _, entry := range []MonitorEntry{MonitorArm, MonitorAutomatic, MonitorRecovery} {
		for _, migrating := range []bool{false, true} {
			t.Run(fmt.Sprintf("entry-%d/migrating-%t", entry, migrating), func(t *testing.T) {
				p, o := courierFixture(t)
				record := monitorRecord()
				if err := RecordMonitorOwnerHook(p, courierFixtureName, time.Now().Add(time.Second), record.Runtime, record.SessionID, "SessionStart", false, time.Now()); err != nil {
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
					if err := MigrateCourier(t.Context(), p, courierFixtureName, o); err == nil {
						t.Fatal("migration did not stop at fence")
					}
				}
				s := courierState(t, p)
				if !validWriterSeal(s) || !s.monitorOwnerMatches(record, time.Now()) || (s.Migration != nil) != migrating || migrating && s.Migration.Phase != "in_progress" {
					t.Fatal("fixture must have intact seal, admitted owner, and expected migration phase")
				}
				before, err := os.ReadFile(p.StateFile(courierFixtureName))
				if err != nil {
					t.Fatal(err)
				}
				called := false
				err = WithMonitorGeneration(p, courierFixtureName, time.Now().Add(time.Second), entry, record, func(g *MonitorGeneration) error {
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
				for _, event := range []string{"SessionStart", "UserPromptSubmit", "SessionEnd"} {
					at := time.Now()
					if event == "SessionStart" {
						at = s.MonitorOwner.LastHookAt // delayed/replayed Start must also refuse
					}
					err = RecordMonitorOwnerHook(p, courierFixtureName, time.Now().Add(time.Second), record.Runtime, record.SessionID, event, false, at)
					if migrating {
						if !errors.Is(err, ErrIdentityMigrated) {
							t.Fatal("owner hook crossed migration fence", event, err)
						}
						after, err := os.ReadFile(p.StateFile(courierFixtureName))
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

func TestCourierMigrationFenceEffects(t *testing.T) {
	p, o := courierFixture(t)
	courierRun(t, p, o)
	before, e := os.ReadFile(p.StateFile(courierFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	called := false
	if _, e = UpdateState(p, courierFixtureName, func(*State) error { called = true; return nil }); !errors.Is(e, ErrIdentityMigrated) || called {
		t.Fatal("ordinary callback ran through fence")
	}
	if e = SaveState(p, courierFixtureName, State{}); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("stale save cleared fence")
	}
	for _, holder := range []string{"fixture-holder", "fixture-renew"} {
		s := courierState(t, p)
		s.Lease = &Lease{Holder: holder, Expires: o.Now.Add(time.Hour)}
		if e = s.Acquire(courierFixtureName, holder, "fixture-host", 1, o.Now, LeaseTTL, nil); !errors.Is(e, ErrIdentityMigrated) {
			t.Fatal("lease acquired/renewed through fence")
		}
	}
	if _, e = AcquireConsumerLock(p, courierFixtureName, time.Now()); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("consumer admitted through fence")
	}
	if e = WithRuntimeLock(p, courierFixtureName, "consumer", time.Now(), func() error { called = true; return nil }); !errors.Is(e, ErrIdentityMigrated) || called {
		t.Fatal("runtime consumer admitted")
	}
	after, e := os.ReadFile(p.StateFile(courierFixtureName))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("fenced operation mutated state")
	}
	good, _ := courierFixture(t)
	if _, e = UpdateState(good, courierFixtureName, func(s *State) error {
		return s.Acquire(courierFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
	}); e != nil {
		t.Fatal("valid lease neighbor", e)
	}
	release, e := AcquireConsumerLock(good, courierFixtureName, time.Now())
	if e != nil {
		t.Fatal("valid consumer neighbor", e)
	}
	release()
}

// Historical probes established that renaming a consumer lock opens a second
// inode, a consumer lock alone does not exclude leases, and a cached session
// can recreate missing State. Migration must preserve the first inode and keep
// both admissions closed after releasing its locks, without a timing window.
func TestCourierHistoricalLockRegression(t *testing.T) {
	p, o := courierFixture(t)
	path := filepath.Join(p.Base, "state", courierFixtureName+".runtime-consumer.lock")
	inode, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	o.step = func(stage string) error {
		info, e := os.Stat(path)
		if e != nil || !os.SameFile(inode, info) {
			return errors.New("fixture inode moved")
		}
		if _, e = AcquireConsumerLock(p, courierFixtureName, time.Now()); !errors.Is(e, ErrLockTimeout) {
			return errors.New("fixture concurrent consumer admitted")
		}
		called := false
		if _, e = UpdateStateBy(p, courierFixtureName, time.Now(), func(s *State) error {
			called = true
			return s.Acquire(courierFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
		}); !errors.Is(e, ErrLockTimeout) || called {
			return errors.New("fixture lease entered during copy")
		}
		return nil
	}
	courierRun(t, p, o)
	if _, e = AcquireConsumerLock(p, courierFixtureName, time.Now()); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("post-release consumer admitted", e)
	}
	if _, e = UpdateState(p, courierFixtureName, func(s *State) error {
		return s.Acquire(courierFixtureName, "fixture-holder", "fixture-host", 1, o.Now, LeaseTTL, nil)
	}); !errors.Is(e, ErrIdentityMigrated) {
		t.Fatal("post-release lease admitted", e)
	}
	o.Rollback = true
	o.step = nil
	courierRun(t, p, o)
	release, e := AcquireConsumerLock(p, courierFixtureName, time.Now())
	if e != nil {
		t.Fatal("unfenced neighbor", e)
	}
	release()
}

// No sleep is used: the channels place the writer precisely between durable
// fence publication and consumer/state lock release.
func TestCourierMigrationConcurrentWriter(t *testing.T) {
	p, o := courierFixture(t)
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
	go func() { migrated <- MigrateCourier(ctx, p, courierFixtureName, o) }()
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
		_, e := UpdateState(p, courierFixtureName, func(s *State) error { called = true; s.NextBatch = "wrong"; return nil })
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
	courierRun(t, p, o)
	if _, e := UpdateState(p, courierFixtureName, func(s *State) error { s.NextBatch = "valid-neighbor"; return nil }); e != nil {
		t.Fatal(e)
	}
}

type courierStatFixture struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (s courierStatFixture) Sys() any { return &s.stat }
func TestCourierOwnership(t *testing.T) {
	p, _ := courierFixture(t)
	info, e := os.Stat(p.AgentFile(courierFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	st := *info.Sys().(*syscall.Stat_t)
	st.Uid = uint32(os.Geteuid() + 1)
	if e = courierOwned(courierStatFixture{FileInfo: info, stat: st}, false); e == nil {
		t.Fatal("foreign owner accepted")
	}
	if e = courierOwned(info, false); e != nil {
		t.Fatal("own-file neighbor", e)
	}
}

func TestCourierMigrationSplitRoots(t *testing.T) {
	p, o := courierFixture(t)
	metadata := courierPrivateDir(t)
	if e := os.Mkdir(filepath.Join(metadata, "agents"), 0700); e != nil {
		t.Fatal(e)
	}
	original := p.AgentFile(courierFixtureName)
	p.MetadataBase = metadata
	if e := os.Rename(original, p.AgentFile(courierFixtureName)); e != nil {
		t.Fatal(e)
	}
	courierRun(t, p, o)
	if _, e := os.Stat(filepath.Join(o.To, "identities", courierFixtureName, "agents", courierFixtureName+".json")); e != nil {
		t.Fatal(e)
	}
	o.Rollback = true
	courierRun(t, p, o)
	if _, e := LoadCredentials(p, courierFixtureName); e != nil {
		t.Fatal("resolved metadata root not restored", e)
	}
	if _, e := os.Stat(original); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("inferred legacy metadata root")
	}
}

func TestCourierMigrationRollbackDamage(t *testing.T) {
	for _, damage := range []string{"credential", "source-credential", "missing-private-credential", "mailbox", "journal", "restore-interrupted"} {
		t.Run(damage, func(t *testing.T) {
			p, o := courierFixture(t)
			courierRun(t, p, o)
			target := filepath.Join(o.To, "identities", courierFixtureName)
			switch damage {
			case "credential":
				if e := os.WriteFile(courierRollbackCredential(target), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "source-credential":
				if e := os.WriteFile(courierBackup(target), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "missing-private-credential":
				if e := os.Rename(courierBackup(target), filepath.Join(target, "detached-fixture")); e != nil {
					t.Fatal(e)
				}
			case "mailbox":
				if e := os.WriteFile(filepath.Join(target, "state", courierFixtureName+".mailbox.jsonl"), []byte("fixture damage"), 0600); e != nil {
					t.Fatal(e)
				}
			case "journal":
				if e := os.WriteFile(filepath.Join(target, courierJournalName), []byte(`{"identity":"someone-else"}`), 0600); e != nil {
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
			if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
				t.Fatal("damaged rollback cleared fence")
			}
			courierAssertFenced(t, p)
			if damage == "restore-interrupted" {
				o.step = nil
				courierRun(t, p, o)
			}
			good, options := courierFixture(t)
			courierRun(t, good, options)
			options.Rollback = true
			courierRun(t, good, options)
		})
	}
}

func TestCourierTargetPublishNoReplacement(t *testing.T) {
	root := courierPrivateDir(t)
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
	if e = courierPublishDirectory(from, to); e == nil {
		t.Fatal("even an empty existing target was replaced")
	}
	after, e := os.Stat(to)
	if e != nil || !os.SameFile(info, after) {
		t.Fatal("target inode changed")
	}
	neighbor := filepath.Join(root, "neighbor")
	if e = courierPublishDirectory(from, neighbor); e != nil {
		t.Fatal("absent-target neighbor", e)
	}
}

func TestCourierMissingTargetDoesNotReopen(t *testing.T) {
	for _, phase := range []string{"cutover", "done"} {
		t.Run(phase, func(t *testing.T) {
			p, o := courierFixture(t)
			if phase == "cutover" {
				o.step = func(stage string) error {
					if stage == "cutover" {
						return errors.New("fixture interruption")
					}
					return nil
				}
				if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
					t.Fatal("interruption absent")
				}
			} else {
				courierRun(t, p, o)
			}
			target := filepath.Join(o.To, "identities", courierFixtureName)
			if e := os.Rename(target, filepath.Join(o.To, "fixture-detached-copy")); e != nil {
				t.Fatal(e)
			}
			o.step = nil
			o.Rollback = true
			if e := MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
				t.Fatal("missing target reopened a cut-over identity")
			}
			courierAssertFenced(t, p)
			// Identical transaction, now with its verified copy available again.
			if e := os.Rename(filepath.Join(o.To, "fixture-detached-copy"), target); e != nil {
				t.Fatal(e)
			}
			courierRun(t, p, o)
			if e := CheckIdentityMigration(p, courierFixtureName); e != nil {
				t.Fatal("verified restoration neighbor", e)
			}
		})
	}
}

func TestCourierPrivateRollbackOnly(t *testing.T) {
	p, o := courierFixture(t)
	original, e := os.ReadFile(p.AgentFile(courierFixtureName))
	if e != nil {
		t.Fatal(e)
	}
	courierRun(t, p, o)
	target := filepath.Join(o.To, "identities", courierFixtureName)
	// Poison redundant snapshots: rollback must read neither credential copy.
	for _, path := range []string{filepath.Join(target, "agents", courierFixtureName+".json"), filepath.Join(target, "credential.json")} {
		if e = os.WriteFile(path, []byte("DUMMY-poisoned-redundant-copy"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	o.Rollback = true
	courierRun(t, p, o)
	got, e := os.ReadFile(p.AgentFile(courierFixtureName))
	if e != nil || !bytes.Equal(got, original) || courierState(t, p).Migration != nil {
		t.Fatal("private-only rollback failed")
	}
	for _, path := range []string{filepath.Join(target, "rollback"), courierBackup(target), courierRollbackCredential(target)} {
		info, e := os.Lstat(path)
		if e != nil || courierOwned(info, info.IsDir()) != nil {
			t.Fatal("private rollback mode/owner mismatch")
		}
	}
}

func TestCourierKeychainBoundaryCustody(t *testing.T) {
	for _, stage := range []string{"fenced", "staged", "journaled", "copied", "copies_verified", "verified", "rollback_source_copied", "rollback_credential_copied", "rollback_verified", "cutover_ready", "cutover", "secret_deleted", "done"} {
		for _, rollback := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/resume", true: "/rollback"}[rollback], func(t *testing.T) {
				p, o := courierFixture(t)
				c := Credentials{UserID: "@fixture:example.invalid", Homeserver: "https://example.invalid", DeviceID: "fixture"}
				metadata, e := marshalCredentials(c)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p.AgentFile(courierFixtureName), metadata, 0600); e != nil {
					t.Fatal(e)
				}
				store := &courierFakeSecrets{present: true, value: "DUMMY-C4-keychain-canary"}
				o.Keychain, o.Secrets = true, store
				o.step = func(label string) error {
					if label == stage {
						return errors.New("DUMMY interruption")
					}
					return nil
				}
				if e = MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
					t.Fatal("boundary was not interrupted")
				}
				courierAssertFenced(t, p)
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
				courierRun(t, p, o)
				if !rollback {
					if store.present {
						t.Fatal("resumed Keychain cutover did not delete")
					}
					o.Resume, o.Rollback = false, true
					// Runtime snapshots cannot substitute for private rollback data.
					target := filepath.Join(o.To, "identities", courierFixtureName)
					for _, path := range []string{filepath.Join(target, "credential.json"), filepath.Join(target, "agents", courierFixtureName+".json")} {
						if e = os.WriteFile(path, []byte("DUMMY redundant corruption"), 0600); e != nil {
							t.Fatal(e)
						}
					}
					courierRun(t, p, o)
				}
				got, e := os.ReadFile(p.AgentFile(courierFixtureName))
				if e != nil || !bytes.Equal(got, metadata) || !store.present || store.value != "DUMMY-C4-keychain-canary" || courierState(t, p).Migration != nil {
					t.Fatal("Keychain rollback did not restore original backend before fence clear")
				}
			})
		}
	}
}

func TestCourierMissingKeychainTargetDoesNotReopen(t *testing.T) {
	for _, stage := range []string{"cutover_ready", "cutover", "done"} {
		t.Run(stage, func(t *testing.T) {
			p, o := courierFixture(t)
			metadata, e := marshalCredentials(Credentials{UserID: "@fixture:example.invalid", Homeserver: "https://example.invalid"})
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(p.AgentFile(courierFixtureName), metadata, 0600); e != nil {
				t.Fatal(e)
			}
			store := &courierFakeSecrets{present: true, value: "DUMMY-C4-keychain-canary"}
			o.Keychain, o.Secrets = true, store
			o.step = func(label string) error {
				if label == stage {
					return errors.New("DUMMY interruption")
				}
				return nil
			}
			if e = MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil || store.present != (stage == "cutover_ready") {
				t.Fatal("cutover boundary not reached")
			}
			// A recreated source item still must not make a post-intent lost-target
			// transaction look like a pre-cutover abort.
			store.present = true
			target := filepath.Join(o.To, "identities", courierFixtureName)
			detached := filepath.Join(o.To, "DUMMY-detached-private-copy")
			if e = os.Rename(target, detached); e != nil {
				t.Fatal(e)
			}
			o.step, o.Rollback = nil, true
			if e = MigrateCourier(context.Background(), p, courierFixtureName, o); e == nil {
				t.Fatal("metadata alone reopened deleted Keychain backend")
			}
			courierAssertFenced(t, p)
			if store.restores != 0 || !store.present {
				t.Fatal("missing target altered Keychain")
			}
			if e = os.Rename(detached, target); e != nil {
				t.Fatal(e)
			}
			courierRun(t, p, o)
			got, e := os.ReadFile(p.AgentFile(courierFixtureName))
			if e != nil || !bytes.Equal(got, metadata) || !store.present || store.restores != 1 || courierState(t, p).Migration != nil {
				t.Fatal("private restoration neighbour failed")
			}
		})
	}
}
