//go:build unix

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression witness for move-human and every public credential writer: both
// durable phases refuse before callbacks, file writes or moves. The neighbour
// exercises actual unfenced writes. Fixtures prove local exclusion, not ACLs.
func TestDaemonCredentialMutationFences(t *testing.T) {
	for _, phase := range []string{"in_progress", "done"} {
		for _, op := range []string{"move", "update", "scrub", "human-scrub", "create", "publish", "human-publish"} {
			t.Run(phase+"/"+op, func(t *testing.T) {
				p, o := daemonFixture(t)
				if phase == "in_progress" {
					o.step = func(s string) error {
						if s == "fenced" {
							return errors.New("fixture interruption")
						}
						return nil
					}
				}
				err := MigrateDaemon(t.Context(), p, daemonFixtureName, o)
				if phase == "done" && err != nil || phase == "in_progress" && err == nil {
					t.Fatal("fixture migration", err)
				}
				original, err := os.ReadFile(p.AgentFile(daemonFixtureName))
				if err != nil {
					t.Fatal(err)
				}
				called := false
				switch op {
				case "move":
					_, err = MoveToHumans(p, daemonFixtureName)
				case "update":
					_, err = UpdateCredentials(p, daemonFixtureName, func(*Credentials) error { called = true; return nil })
				case "scrub", "human-scrub":
					err = RewriteCredentialMetadata(p, daemonFixtureName, op == "human-scrub", func(*Credentials) error { called = true; return nil })
				case "create":
					var f *CredentialFile
					f, err = CreateCredentialFile(p, daemonFixtureName)
					if f != nil {
						f.Abort()
					}
				case "publish", "human-publish":
					err = PublishBoundCredentials(p, daemonFixtureName, op == "human-publish", Credentials{})
				}
				if !errors.Is(err, ErrIdentityMigrated) || called {
					t.Fatal("credential mutation bypassed fence", err)
				}
				after, err := os.ReadFile(p.AgentFile(daemonFixtureName))
				if err != nil || !bytes.Equal(original, after) {
					t.Fatal("source changed")
				}
				if _, err = os.Stat(p.HumanFile(daemonFixtureName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("human bypass created")
				}
				good, _ := daemonFixture(t)
				switch op {
				case "move", "human-scrub":
					if _, err = MoveToHumans(good, daemonFixtureName); err != nil {
						t.Fatal("move neighbour", err)
					}
					if op == "human-scrub" {
						err = RewriteCredentialMetadata(good, daemonFixtureName, true, func(c *Credentials) error { c.AccessToken = ""; c.Password = ""; return nil })
					} else {
						_, err = LoadHumanCredentials(good, daemonFixtureName)
					}
				case "update":
					_, err = UpdateCredentials(good, daemonFixtureName, func(c *Credentials) error { c.DeviceID = "changed"; return nil })
				case "scrub":
					err = RewriteCredentialMetadata(good, daemonFixtureName, false, func(c *Credentials) error { c.AccessToken = ""; c.Password = ""; return nil })
				case "create":
					f, e := CreateCredentialFile(good, "new")
					err = e
					if e == nil {
						err = WriteCredentials(f, Credentials{UserID: "@new:example.invalid", AccessToken: "DUMMY-new"})
					}
				case "publish", "human-publish":
					conn := testConnection()
					conn.Store = CredentialStore{Backend: "file"}
					b, e := conn.Binding("new", op == "human-publish")
					if e != nil {
						t.Fatal(e)
					}
					err = PublishBoundCredentials(good, "new", op == "human-publish", Credentials{UserID: b.UserID, Homeserver: conn.URL, AccessToken: "DUMMY-new", ConnectionBinding: &b})
				}
				if err != nil {
					t.Fatal("unfenced neighbour", err)
				}
			})
		}
	}
}

// The migration owns the credential flock at the fenced hook. move-human must
// remain blocked until release and then observe the fence, rather than moving
// preloaded credentials. Every channel wait has completion/failure/deadline arms.
func TestDaemonMoveHumanWhileCredentialLocked(t *testing.T) {
	p, o := daemonFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	fenced, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
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
	go func() { done <- MigrateDaemon(ctx, p, daemonFixtureName, o) }()
	select {
	case <-fenced:
	case err := <-done:
		t.Fatal("migration did not fence", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	moved := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); _, err := MoveToHumans(p, daemonFixtureName); moved <- err }()
	select {
	case <-started:
	case err := <-done:
		t.Fatal("migration exited early", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-moved:
		t.Fatal("move escaped credential lock", err)
	case err := <-done:
		t.Fatal("migration exited early", err)
	case <-time.After(30 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-moved:
		if !errors.Is(err, ErrIdentityMigrated) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	good, _ := daemonFixture(t)
	if _, err := MoveToHumans(good, daemonFixtureName); err != nil {
		t.Fatal("unfenced neighbour", err)
	}
}

// A reservation retains its lock through provider work; even an otherwise
// valid credential cannot be migrated until publication/abort releases it.
func TestDaemonCredentialReservationLock(t *testing.T) {
	p, o := daemonFixture(t)
	f, err := CreateCredentialFile(p, "reserved")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Abort()
	data := []byte(`{"user_id":"@reserved:example.invalid","access_token":"DUMMY-reserved"}`)
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = MigrateDaemon(t.Context(), p, "reserved", o); !errors.Is(err, ErrDaemonMigration) {
		t.Fatal("reservation not excluded", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = MigrateDaemon(t.Context(), p, "reserved", o); err != nil {
		t.Fatal("closed reservation neighbour", err)
	}
	if err = WriteCredentials(f, Credentials{}); err == nil {
		t.Fatal("closed writer accepted")
	}
}

func daemonCleanKeychain(t *testing.T, p Paths, o DaemonMigrationOptions) (DaemonMigrationOptions, []byte, *daemonFakeSecrets) {
	t.Helper()
	c := Credentials{UserID: "@fixture:example.invalid", DeviceID: "fixture", Homeserver: "https://example.invalid"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.AgentFile(daemonFixtureName), data, 0600); err != nil {
		t.Fatal(err)
	}
	store := &daemonFakeSecrets{present: true, value: "DUMMY-C4-keychain-canary"}
	o.Keychain, o.Secrets = true, store
	return o, data, store
}

// Interrupted creds migrate may leave identical secrets in two backends. Both
// named and legacy metadata must refuse before fencing without scrubbing either
// copy. Clean neighbours delete the fake item, scan every source file and restore.
func TestDaemonLeftoverSourceSecrets(t *testing.T) {
	for _, named := range []bool{false, true} {
		for _, field := range []string{"access_token", "password", "refresh_token", "nested", "duplicate", "case"} {
			t.Run(field+map[bool]string{false: "/legacy", true: "/named"}[named], func(t *testing.T) {
				p, o := daemonFixture(t)
				o, clean, store := daemonCleanKeychain(t, p, o)
				var fields map[string]json.RawMessage
				if json.Unmarshal(clean, &fields) != nil {
					t.Fatal("fixture")
				}
				if named {
					conn := testConnection()
					conn.ServerName = "example.invalid"
					b, e := conn.Binding(daemonFixtureName, false)
					if e != nil {
						t.Fatal(e)
					}
					o.Binding = &b
					fields["connection_binding"], _ = json.Marshal(b)
					fields["homeserver"], _ = json.Marshal(conn.URL)
				}
				key := field
				if field == "case" {
					key = "Access_Token"
				}
				if field == "nested" {
					fields["wait_keywords"] = json.RawMessage(`{"refresh_token":"DUMMY-C4-keychain-canary"}`)
				} else {
					fields[key] = json.RawMessage(`"DUMMY-C4-keychain-canary"`)
				}
				dirty, _ := json.Marshal(fields)
				if field == "duplicate" {
					dirty = append(dirty[:len(dirty)-1], []byte(`,"access_token":"DUMMY-C4-keychain-canary","access_token":""}`)...)
				}
				if err := os.WriteFile(p.AgentFile(daemonFixtureName), dirty, 0600); err != nil {
					t.Fatal(err)
				}
				state, _ := os.ReadFile(p.StateFile(daemonFixtureName))
				err := MigrateDaemon(t.Context(), p, daemonFixtureName, o)
				if !errors.Is(err, ErrDaemonSourceSecrets) || !store.present {
					t.Fatal("leftover accepted", err)
				}
				after, _ := os.ReadFile(p.StateFile(daemonFixtureName))
				source, _ := os.ReadFile(p.AgentFile(daemonFixtureName))
				if !bytes.Equal(after, state) || !bytes.Equal(source, dirty) {
					t.Fatal("preflight lost source evidence")
				}
				if _, err = os.Stat(filepath.Join(o.To, "identities")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("target changed before refusal")
				}
				good, options := daemonFixture(t)
				options, metadata, fake := daemonCleanKeychain(t, good, options)
				daemonRun(t, good, options)
				if fake.present {
					t.Fatal("item not deleted")
				}
				daemonScanCanary(t, good, "DUMMY-C4-keychain-canary")
				options.Rollback = true
				daemonRun(t, good, options)
				restored, err := os.ReadFile(good.AgentFile(daemonFixtureName))
				if err != nil || !bytes.Equal(metadata, restored) || !fake.present {
					t.Fatal("clean rollback neighbour", err)
				}
			})
		}
	}
}

func daemonScanCanary(t *testing.T, p Paths, canary string) {
	t.Helper()
	for _, root := range []string{p.Base, p.metadataRoot()} {
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if bytes.Contains(b, []byte(canary)) {
				t.Error("source secret remains")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// An adversarial fake reintroduces a source field after preflight. Step 3 must
// recheck after Delete and preserve in_progress; a normal adapter is the neighbour.
func TestDaemonKeychainPostDeleteSourceCheck(t *testing.T) {
	p, o := daemonFixture(t)
	o, clean, store := daemonCleanKeychain(t, p, o)
	o.Secrets = daemonDirtyDelete{store: store, p: p}
	if err := MigrateDaemon(t.Context(), p, daemonFixtureName, o); !errors.Is(err, ErrDaemonSourceSecrets) {
		t.Fatal(err)
	}
	s := daemonState(t, p)
	if s.Migration == nil || s.Migration.Phase != "in_progress" || store.present {
		t.Fatal("post-delete check did not fail fenced")
	}
	if err := os.WriteFile(p.AgentFile(daemonFixtureName), clean, 0600); err != nil {
		t.Fatal(err)
	}
	o.Secrets = store
	o.Resume = true
	daemonRun(t, p, o)
	daemonScanCanary(t, p, "DUMMY-C4-keychain-canary")
}

type daemonDirtyDelete struct {
	store *daemonFakeSecrets
	p     Paths
}

func (s daemonDirtyDelete) Read(ctx context.Context, c Credentials) (Credentials, error) {
	return s.store.Read(ctx, c)
}
func (s daemonDirtyDelete) Restore(ctx context.Context, c Credentials) error {
	return s.store.Restore(ctx, c)
}
func (s daemonDirtyDelete) Delete(ctx context.Context, c Credentials) error {
	if err := s.store.Delete(ctx, c); err != nil {
		return err
	}
	return os.WriteFile(s.p.AgentFile(daemonFixtureName), []byte(`{"user_id":"@fixture:example.invalid","refresh_token":"DUMMY-C4-keychain-canary"}`), 0600)
}

// Alias identity uses real stat device/inode fields through a synthetic stat
// view because Unix forbids user-created directory hardlinks. Different devices
// with identical inode numbers remain valid. Real separate paths also pass.
func TestDaemonOverlapFileIdentity(t *testing.T) {
	p, o := daemonFixture(t)
	root, err := os.Stat(p.Base)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := os.Stat(o.To)
	if err != nil {
		t.Fatal(err)
	}
	rs := *root.Sys().(*syscall.Stat_t)
	ds := *dest.Sys().(*syscall.Stat_t)
	alias := filepath.Join(o.To, "alias")
	stat := func(path string) (os.FileInfo, error) {
		if path == alias {
			return daemonStatFixture{FileInfo: dest, stat: rs}, nil
		}
		return os.Stat(path)
	}
	if err = daemonCheckOverlap(filepath.Join(alias, "new"), []string{p.Base}, stat); err == nil {
		t.Fatal("ancestor alias accepted")
	}
	// Reverse containment must refuse too, including a destination alias.
	if err = daemonCheckOverlap(alias, []string{p.Base}, stat); err == nil {
		t.Fatal("equal alias accepted")
	}
	ds.Ino = rs.Ino
	ds.Dev = rs.Dev + 1
	stat = func(path string) (os.FileInfo, error) {
		if path == alias {
			return daemonStatFixture{FileInfo: dest, stat: ds}, nil
		}
		return os.Stat(path)
	}
	if err = daemonCheckOverlap(filepath.Join(alias, "new"), []string{p.Base}, stat); err != nil {
		t.Fatal("different-device neighbour", err)
	}
	daemonRun(t, p, o)
}

// Darwin always folds case for overlap admission, regardless of volume setup.
// This refuses case-only source spellings before writing State; separate target
// spelling remains valid. Other platforms do not claim case-volume coverage.
func TestDaemonOverlapCaseDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin case-fold policy")
	}
	p, o := daemonFixture(t)
	before, _ := os.ReadFile(p.StateFile(daemonFixtureName))
	o.To = filepath.Join(filepath.Dir(p.Base), strings.ToUpper(filepath.Base(p.Base)))
	if err := MigrateDaemon(t.Context(), p, daemonFixtureName, o); err == nil {
		t.Fatal("case alias accepted")
	}
	after, _ := os.ReadFile(p.StateFile(daemonFixtureName))
	if !bytes.Equal(before, after) {
		t.Fatal("case refusal changed State")
	}
	good, options := daemonFixture(t)
	daemonRun(t, good, options)
}

// ACK and quiesced UNKNOWN journals retain exact bytes/digests and are readable
// at the target. not_submitted, malformed/reserved and temporary records refuse
// before the fence. No-journal identities are valid neighbours for every row.
func TestDaemonPostOperationJournals(t *testing.T) {
	for _, outcome := range []string{"acknowledged", "unknown", "not_submitted", "reserved", "temporary"} {
		t.Run(outcome, func(t *testing.T) {
			p, o := daemonFixture(t)
			j, err := OpenPostJournal(p, daemonFixtureName, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			candidate := operationCandidate("operation", 20)
			candidate.Binding.UserID = "@fixture:example.invalid"
			r := mustPrepareOperation(t, j, candidate)
			if outcome == "unknown" || outcome == "acknowledged" {
				r, err = j.ReserveAttempt(r)
				if err != nil {
					t.Fatal(err)
				}
			}
			if outcome == "acknowledged" {
				r, err = j.RecordACK(r, "$ack", 200, 5)
				if err != nil {
					t.Fatal(err)
				}
			}
			source := filepath.Join(p.Base, "post-operations", "agents", daemonFixtureName, r.ID+".json")
			if outcome == "reserved" {
				r.Outcome = "reserved"
				r.RecordDigest = r.digest()
				b, _ := json.Marshal(r)
				if err = os.WriteFile(source, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if outcome == "temporary" {
				if err = os.WriteFile(filepath.Join(filepath.Dir(source), "operation.tmp"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := os.ReadFile(p.StateFile(daemonFixtureName))
			err = MigrateDaemon(t.Context(), p, daemonFixtureName, o)
			if outcome == "acknowledged" || outcome == "unknown" {
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(o.To, "identities", daemonFixtureName)
				copied, e := os.ReadFile(filepath.Join(target, "post-operations", "agents", daemonFixtureName, r.ID+".json"))
				if e != nil || !bytes.Equal(before, copied) || daemonDigest(before) != daemonDigest(copied) {
					t.Fatal("journal digest changed", e)
				}
				tj, e := OpenPostJournal(Paths{Base: target}, daemonFixtureName, false, false)
				if e != nil {
					t.Fatal(e)
				}
				defer tj.Close()
				got, ok, e := tj.Read(r.ID)
				if e != nil || !ok || got.Outcome != outcome || got.RecordDigest != r.RecordDigest {
					t.Fatal("target journal unreadable", e)
				}
				o.Rollback = true
				daemonRun(t, p, o)
			} else {
				if !errors.Is(err, ErrDaemonPostPending) {
					t.Fatal("pending journal accepted", err)
				}
				after, _ := os.ReadFile(p.StateFile(daemonFixtureName))
				retained, _ := os.ReadFile(source)
				if !bytes.Equal(state, after) || !bytes.Equal(before, retained) {
					t.Fatal("refusal changed evidence")
				}
			}
			good, options := daemonFixture(t)
			daemonRun(t, good, options)
		})
	}
}

// A live reservation is distinguished from an interrupted UNKNOWN by the
// attempt lifetime lock. A held journal flock also refuses without waiting.
// Releasing either preserves its inode and admits the separate neighbour.
func TestDaemonPostOperationLocks(t *testing.T) {
	for _, kind := range []string{"attempt", "journal"} {
		t.Run(kind, func(t *testing.T) {
			p, o := daemonFixture(t)
			j, err := OpenPostJournal(p, daemonFixtureName, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			r := mustPrepareOperation(t, j, operationCandidate("op", 8))
			if _, err = j.ReserveAttempt(r); err != nil {
				t.Fatal(err)
			}
			var release func()
			lockPath := postAttemptPath(p, daemonFixtureName, false)
			if kind == "attempt" {
				release, err = AcquirePostAttempt(p, daemonFixtureName, false, time.Now())
			} else {
				lockPath = filepath.Join(p.Base, "post-operations", "agents", daemonFixtureName, ".lock")
				release, err = lockFile(lockPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(lockPath)
			if err != nil {
				release()
				t.Fatal(err)
			}
			err = MigrateDaemon(t.Context(), p, daemonFixtureName, o)
			release()
			if !errors.Is(err, ErrDaemonPostPending) || daemonState(t, p).Migration != nil {
				t.Fatal("reserved journal admitted", err)
			}
			daemonRun(t, p, o)
			after, err := os.Stat(lockPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("lock inode changed", err)
			}
		})
	}
}

// Unknown secret fields also refuse in the file backend without losing the
// only source copy. The admitted legacy file schema still migrates normally.
func TestDaemonFileCredentialSchema(t *testing.T) {
	p, o := daemonFixture(t)
	original, err := os.ReadFile(p.AgentFile(daemonFixtureName))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(original, &fields); err != nil {
		t.Fatal(err)
	}
	fields["refresh_token"] = json.RawMessage(`"DUMMY-refresh"`)
	dirty, _ := json.Marshal(fields)
	if err = os.WriteFile(p.AgentFile(daemonFixtureName), dirty, 0600); err != nil {
		t.Fatal(err)
	}
	if err = MigrateDaemon(t.Context(), p, daemonFixtureName, o); !errors.Is(err, ErrDaemonSourceSecrets) {
		t.Fatal(err)
	}
	source, err := os.ReadFile(p.AgentFile(daemonFixtureName))
	if err != nil || !bytes.Equal(source, dirty) || daemonState(t, p).Migration != nil {
		t.Fatal("preflight lost source copy", err)
	}
	good, options := daemonFixture(t)
	daemonRun(t, good, options)
}

// Compatibility table for the credential-schema boundary: accept the legacy
// and named writers' emitted metadata, including retained duplicate optional
// fields. Inspect each secret occurrence instead of letting later empty fields
// conceal a secret. This proves the wire predicate, not a provider or OS lane.
func TestDaemonCredentialMetadataCompatibility(t *testing.T) {
	base := Credentials{UserID: "@fixture:example.invalid", DeviceID: "fixture", Homeserver: "https://example.invalid"}
	encode := func(c Credentials) []byte {
		b, e := marshalCredentials(c)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	clean := encode(base)
	file := base
	file.AccessToken = "DUMMY-compat-token"
	file.Password = "DUMMY-compat-password"
	optional := base
	optional.OwnRoom = "!fixture:example.invalid"
	optional.WaitKeywords = WaitKeywords{"TASK-2"}
	repeated := optional
	repeated.waitKeywordsRaw = waitKeywordValues{json.RawMessage(`["TASK-1"]`), json.RawMessage(`["TASK-2"]`)}
	conn := testConnection()
	b, e := conn.Binding(daemonFixtureName, false)
	if e != nil {
		t.Fatal(e)
	}
	named := base
	named.ConnectionBinding = &b
	named.UserID = b.UserID
	named.Homeserver = conn.URL
	conn.Store = CredentialStore{Backend: "file"}
	fb, e := conn.Binding(daemonFixtureName, false)
	if e != nil {
		t.Fatal(e)
	}
	namedFile := named
	namedFile.ConnectionBinding = &fb
	namedFile.AccessToken = "DUMMY-named-file"
	withFields := func(extra string) []byte {
		return append(append([]byte(nil), clean[:len(clean)-1]...), []byte(","+extra+"}")...)
	}
	cases := []struct {
		name            string
		data            []byte
		keychain, valid bool
	}{
		{"legacy-file", encode(file), false, true},
		{"legacy-keychain", clean, true, true},
		{"explicit-empty-keychain", withFields(`"access_token":"","password":""`), true, true},
		{"named-keychain", encode(named), true, true},
		{"named-file", encode(namedFile), false, true},
		{"own-room-and-keywords", encode(optional), true, true},
		{"retained-optional-occurrences", encode(repeated), true, true},
		{"secret-before-empty", withFields(`"access_token":"DUMMY-leftover","access_token":""`), true, false},
		{"secret-after-empty", withFields(`"access_token":"","access_token":"DUMMY-leftover"`), true, false},
		{"unknown-refresh", withFields(`"refresh_token":"DUMMY-refresh"`), true, false},
		{"unknown-field-file", withFields(`"refresh_token":"DUMMY-refresh"`), false, false},
		{"case-secret", withFields(`"Access_Token":"DUMMY-leftover"`), true, false},
		{"nested-secret", withFields(`"wait_keywords":{"refresh_token":"DUMMY-refresh"}`), true, false},
		{"trailing-secret-object", append(append([]byte(nil), clean...), []byte(`{"refresh_token":"DUMMY-refresh"}`)...), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := daemonSourceCredentialCheck(c.data, c.keychain)
			if (err == nil) != c.valid {
				t.Fatalf("valid=%t got %v", c.valid, err)
			}
			if err != nil && !errors.Is(err, ErrDaemonSourceSecrets) {
				t.Fatal("unclosed reason", err)
			}
		})
	}
}
