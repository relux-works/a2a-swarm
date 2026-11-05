//go:build unix

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type presenceFixture struct {
	t        *testing.T
	p        Paths
	c        Connection
	b        ConnectionBinding
	selected Paths
}

func privatePresenceTemp(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func newPresenceFixture(t *testing.T) *presenceFixture {
	t.Helper()
	f := &presenceFixture{t: t, p: Paths{Base: privatePresenceTemp(t)}, c: Connection{ID: "work-id", URL: "https://api.example.invalid", ServerName: "example.invalid", Store: CredentialStore{Backend: "keychain", AccessTokenService: "test.token", PasswordService: "test.password"}, RegistrationTokenRef: &SecretReference{Service: "test.bootstrap", Account: "operator"}}}
	f.b, _ = f.c.Binding("target", false)
	f.selected, _ = f.c.Paths(f.p, "target", false)
	f.write(f.p.ConnectionsFile(), ConnectionRegistry{SchemaVersion: 1, Current: "work", Profiles: map[string]Connection{"work": f.c}})
	return f
}

func (f *presenceFixture) write(path string, value any) {
	f.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	f.raw(path, raw)
}

func (f *presenceFixture) raw(path string, raw []byte) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *presenceFixture) metadata() {
	f.t.Helper()
	// This is the existing publisher's real public shape, not a reader-only mock.
	if err := PublishBoundCredentials(f.selected, "target", false, Credentials{ConnectionBinding: &f.b, UserID: f.b.UserID, DeviceID: "fixture-device", Homeserver: f.c.URL}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *presenceFixture) journal(stage string) {
	f.write(filepath.Join(f.selected.Base, "registration.pending.json"), presenceJournal{Binding: f.b, Stage: stage})
}

func (f *presenceFixture) query(hooks presenceHooks) AgentPresence {
	return inspectAgentPresence(f.p, "target", "work", "", func(string) string { return "" }, hooks)
}

func requirePresence(t *testing.T, result AgentPresence, state, evidence, reason string) {
	t.Helper()
	want := presence(state, evidence, reason)
	if result != want {
		t.Fatalf("got %#v want %#v", result, want)
	}
}

// Proves selected public metadata accepts the existing successful complete
// journal and runtime-state neighbors; missing metadata alone is not ready.
func TestAgentPresenceNamedStates(t *testing.T) {
	for _, tc := range []struct {
		name, stage, state, evidence, reason string
		metadata, residue                    bool
	}{
		{"complete", "complete", "present", "named_bound_metadata", "validated", true, false},
		{"no-journal", "", "present", "named_bound_metadata", "validated", true, false},
		{"runtime-state", "complete", "present", "named_bound_metadata", "validated", true, true},
		{"absent", "", "absent", "none", "local_absence", false, false},
		{"complete-no-metadata", "complete", "inconclusive", "none", "unqualified_state", false, false},
		{"residue", "", "inconclusive", "none", "unqualified_state", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPresenceFixture(t)
			if tc.metadata {
				f.metadata()
			}
			if tc.stage != "" {
				f.journal(tc.stage)
			}
			if tc.residue {
				f.raw(f.selected.StateFile("target"), []byte(`{"synthetic":"untouched"}`))
			}
			requirePresence(t, f.query(presenceHooks{}), tc.state, tc.evidence, tc.reason)
		})
	}
	for _, stage := range []string{"preflight", "secret-preflight-refused", "bootstrap-unavailable", "password-store-failed", "remote-outcome-pending", "remote-outcome-ambiguous", "remote-rejected", "remote-response-mismatch", "remote-created-token-storage-pending", "remote-created-token-store-failed", "remote-created-metadata-publication-pending"} {
		for _, metadata := range []bool{false, true} {
			t.Run(stage+string(rune('0'+btoiPresence(metadata))), func(t *testing.T) {
				f := newPresenceFixture(t)
				if metadata {
					f.metadata()
				}
				f.journal(stage)
				requirePresence(t, f.query(presenceHooks{}), "pending", "registration_journal", "registration_incomplete")
			})
		}
	}
}

func btoiPresence(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Proves strict public decoding refuses secret fields (even empty), aliases,
// duplicates, missing/null fields and nested drift rather than using Credentials.
func TestAgentPresenceStrictPublicRecords(t *testing.T) {
	for _, kind := range []string{"metadata", "journal", "registry"} {
		for _, change := range []string{"unknown", "secret", "duplicate", "alias", "null", "trailing", "nested-secret", "nested-alias", "missing", "mismatch"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f := newPresenceFixture(t)
				f.metadata()
				f.journal("complete")
				path := f.selected.AgentFile("target")
				key := "user_id"
				if kind == "journal" {
					path = filepath.Join(f.selected.Base, "registration.pending.json")
					key = "stage"
				}
				if kind == "registry" {
					path = f.p.ConnectionsFile()
					key = "current"
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, raw); err != nil {
					t.Fatal(err)
				}
				raw = append([]byte(nil), compact.Bytes()...)
				original := append([]byte(nil), raw...)
				switch change {
				case "unknown":
					raw = append([]byte(`{"unrecognized":"not-output",`), raw[1:]...)
				case "secret":
					raw = append([]byte(`{"access_token":"",`), raw[1:]...)
				case "duplicate":
					raw = bytes.Replace(raw, []byte(`"`+key+`":`), []byte(`"`+key+`":"duplicate","`+key+`":`), 1)
				case "alias":
					raw = bytes.Replace(raw, []byte(`"`+key+`":`), []byte(`"`+strings.ToUpper(key)+`":`), 1)
				case "null":
					raw = []byte(`null`)
				case "trailing":
					raw = append(raw, []byte(` {}`)...)
				case "nested-secret":
					raw = bytes.Replace(raw, []byte(`"credential_store":{`), []byte(`"credential_store":{"password":"sensitive-fixture",`), 1)
				case "nested-alias":
					raw = bytes.Replace(raw, []byte(`"connection_id":`), []byte(`"CONNECTION_ID":`), 1)
				case "missing":
					var fields map[string]json.RawMessage
					if json.Unmarshal(raw, &fields) != nil {
						t.Fatal("fixture")
					}
					delete(fields, key)
					raw, _ = json.Marshal(fields)
				case "mismatch":
					if kind == "registry" {
						raw = bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":99`), 1)
					} else {
						raw = bytes.ReplaceAll(raw, []byte(`@target:example.invalid`), []byte(`@other:example.invalid`))
					}
				}
				if bytes.Equal(original, raw) {
					t.Fatal("intended public-record violation was not applied")
				}
				f.raw(path, raw)
				want := "invalid_" + kind
				requirePresence(t, f.query(presenceHooks{}), "inconclusive", "none", want)
			})
		}
	}
	for _, field := range []string{"class", "endpoint", "service", "device-control", "invalid-utf8", "stage", "password-empty"} {
		t.Run(field, func(t *testing.T) {
			f := newPresenceFixture(t)
			f.metadata()
			f.journal("complete")
			path := f.selected.AgentFile("target")
			raw, _ := os.ReadFile(path)
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatal(err)
			}
			raw = append([]byte(nil), compact.Bytes()...)
			original := append([]byte(nil), raw...)
			reason := "invalid_metadata"
			switch field {
			case "class":
				raw = bytes.ReplaceAll(raw, []byte(`"class":"agents"`), []byte(`"class":"humans"`))
			case "endpoint":
				raw = bytes.ReplaceAll(raw, []byte(f.c.URL), []byte("https://wrong.invalid"))
			case "service":
				raw = bytes.ReplaceAll(raw, []byte("test.token"), []byte("test.other"))
			case "device-control":
				raw = bytes.Replace(raw, []byte("fixture-device"), []byte(`bad\nvalue`), 1)
			case "invalid-utf8":
				raw = bytes.Replace(raw, []byte("fixture-device"), []byte{0xff}, 1)
			case "password-empty":
				raw = append([]byte(`{"password":"",`), raw[1:]...)
			case "stage":
				path = filepath.Join(f.selected.Base, "registration.pending.json")
				raw, _ = os.ReadFile(path)
				original = append([]byte(nil), raw...)
				raw = bytes.Replace(raw, []byte(`"complete"`), []byte(`"unknown-sensitive-stage"`), 1)
				reason = "invalid_journal"
			}
			if bytes.Equal(original, raw) {
				t.Fatal("intended binding violation was not applied")
			}
			f.raw(path, raw)
			requirePresence(t, f.query(presenceHooks{}), "inconclusive", "none", reason)
		})
	}
}

// Proves legacy positive is stat-only even for invalid/token-bearing contents,
// while target residual state, registry failures and forced selection close.
func TestAgentPresenceLegacyUnreadAndAbsence(t *testing.T) {
	for _, positive := range []bool{false, true} {
		t.Run(string(rune('0'+btoiPresence(positive))), func(t *testing.T) {
			p := Paths{Base: privatePresenceTemp(t)}
			if positive {
				if err := os.Mkdir(filepath.Join(p.Base, "agents"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.AgentFile("target"), []byte("token-bearing bytes\xff not even JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			r := inspectAgentPresence(p, "target", "", "", func(string) string { return "" }, presenceHooks{beforeRead: func(string) error { reads++; return errors.New("forbidden content read") }})
			if positive {
				requirePresence(t, r, "present", "legacy_regular_file_only", "regular_file_only")
			} else {
				requirePresence(t, r, "absent", "none", "local_absence")
			}
			if reads != 0 {
				t.Fatal("legacy/absent opened content")
			}
			for _, profile := range []string{"work", "legacy"} {
				requirePresence(t, InspectAgentPresence(p, "target", profile, "", func(string) string { return "" }), "inconclusive", "none", "invalid_selection")
			}
		})
	}
	for _, directory := range []string{"state", "owner-dm"} {
		f := newPresenceFixture(t)
		if err := os.Remove(f.p.ConnectionsFile()); err != nil {
			t.Fatal(err)
		}
		f.raw(filepath.Join(f.p.Base, directory, "target.unrecognized-state"), []byte("never opened"))
		requirePresence(t, inspectAgentPresence(f.p, "target", "", "", func(string) string { return "" }, presenceHooks{}), "inconclusive", "none", "unqualified_state")
	}
	f := newPresenceFixture(t)
	f.raw(f.p.AgentFile("target"), []byte("legacy-does-not-mask-registry"))
	f.raw(f.p.ConnectionsFile(), []byte(`{"schema_version":99}`))
	requirePresence(t, f.query(presenceHooks{}), "inconclusive", "none", "invalid_registry")
}

// Proves unsafe entries and deterministic observed races are refused with exact
// codes; fixture-only hooks do not claim resistance to an undetected same-UID ABA.
func TestAgentPresenceFilesystemRefusals(t *testing.T) {
	for _, variant := range []string{"symlink", "parent-symlink", "directory", "fifo", "public-file", "unreadable-file", "public-parent", "empty", "oversize", "read-error", "human", "root-symlink", "root-public"} {
		t.Run(variant, func(t *testing.T) {
			f := newPresenceFixture(t)
			f.metadata()
			path := f.selected.AgentFile("target")
			hooks := presenceHooks{}
			reason := "invalid_metadata"
			switch variant {
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-original", parent); err != nil {
					t.Fatal(err)
				}
			case "directory", "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if variant == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "unreadable-file":
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
			case "public-parent":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "empty":
				f.raw(path, nil)
			case "oversize":
				f.raw(path, bytes.Repeat([]byte("x"), 65537))
			case "read-error":
				hooks.beforeRead = func(kind string) error {
					if kind == "metadata" {
						return errors.New("secret must not be echoed")
					}
					return nil
				}
			case "human":
				f.raw(f.selected.HumanFile("target"), []byte("human never opened"))
				reason = "identity_class"
			case "root-symlink":
				old := f.p.Base
				f.p.Base = old + "-link"
				if err := os.Symlink(old, f.p.Base); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(f.p.Base) })
				reason = "unsafe_or_unreadable"
			case "root-public":
				if err := os.Chmod(f.p.Base, 0755); err != nil {
					t.Fatal(err)
				}
				reason = "unsafe_or_unreadable"
			}
			requirePresence(t, f.query(hooks), "inconclusive", "none", reason)
		})
	}
	for _, change := range []string{"replace", "remove", "new-metadata", "registry", "parent"} {
		t.Run("race-"+change, func(t *testing.T) {
			f := newPresenceFixture(t)
			if change != "new-metadata" {
				f.metadata()
			}
			h := presenceHooks{beforeRecheck: func() {
				path := f.selected.AgentFile("target")
				switch change {
				case "replace":
					raw, _ := os.ReadFile(path)
					f.raw(path+".new", raw)
					if err := os.Rename(path+".new", path); err != nil {
						t.Fatal(err)
					}
				case "remove":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "new-metadata":
					f.metadata()
				case "registry":
					f.raw(f.p.ConnectionsFile(), []byte(`{}`))
				case "parent":
					if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
				}
			}}
			requirePresence(t, f.query(h), "inconclusive", "none", "snapshot_changed")
		})
	}
}

// Proves a known intended public read failure is observed, and the same fixture
// passes without the injected failure. This is not a product denial plant.
func TestAgentPresenceReadInstrumentControl(t *testing.T) {
	f := newPresenceFixture(t)
	f.metadata()
	calls := 0
	h := presenceHooks{beforeRead: func(kind string) error {
		if kind == "metadata" {
			calls++
			return errors.New("intended metadata read refusal")
		}
		return nil
	}}
	requirePresence(t, f.query(h), "inconclusive", "none", "invalid_metadata")
	if calls != 1 {
		t.Fatal("read-error plant did not reach intended boundary")
	}
	requirePresence(t, f.query(presenceHooks{}), "present", "named_bound_metadata", "validated")
}

// Proves the private-root check, with a valid absent neighbor, independent of
// the host umask. No shared directory or process-wide umask is changed.
func TestAgentPresenceRootModeControl(t *testing.T) {
	root := t.TempDir()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic TempDir initial mode: %04o", info.Mode().Perm())
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	requirePresence(t, InspectAgentPresence(Paths{Base: root}, "target", "", "", getenv), "inconclusive", "none", "unsafe_or_unreadable")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	requirePresence(t, InspectAgentPresence(Paths{Base: root}, "target", "", "", getenv), "absent", "none", "local_absence")
}

type presenceUIDInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i presenceUIDInfo) Sys() any { return &i.stat }

// Proves owner checks on copied stat records without chown/sudo, bounded state
// enumeration, and pre-read inode verification. No real credential file is read.
func TestAgentPresenceOwnerBoundsAndOpenRace(t *testing.T) {
	f := newPresenceFixture(t)
	f.metadata()
	info, err := os.Lstat(f.selected.AgentFile("target"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("missing fixture stat")
	}
	foreign := *stat
	foreign.Uid++
	if !privatePresenceInfo(info, false) || privatePresenceInfo(presenceUIDInfo{info, foreign}, false) {
		t.Fatal("owner guard missed foreign stat or rejected own control")
	}
	t.Run("open-race", func(t *testing.T) {
		f := newPresenceFixture(t)
		f.metadata()
		calls := 0
		hooks := presenceHooks{beforeRead: func(kind string) error {
			if kind == "metadata" {
				calls++
				path := f.selected.AgentFile("target")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				f.raw(path+".new", raw)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}}
		requirePresence(t, f.query(hooks), "inconclusive", "none", "snapshot_changed")
		if calls != 1 {
			t.Fatal("pre-read replacement not reached")
		}
	})
	t.Run("bounded-state", func(t *testing.T) {
		f := newPresenceFixture(t)
		for i := 0; i < 1025; i++ {
			f.raw(filepath.Join(f.selected.Base, "synthetic-"+strconv.Itoa(i)), []byte("synthetic"))
		}
		requirePresence(t, f.query(presenceHooks{}), "inconclusive", "none", "unsafe_or_unreadable")
	})
	t.Run("missing-root", func(t *testing.T) {
		root := filepath.Join(privatePresenceTemp(t), "missing")
		requirePresence(t, InspectAgentPresence(Paths{Base: root}, "target", "", "", func(string) string { return "" }), "inconclusive", "none", "unsafe_or_unreadable")
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("query created missing config")
		}
	})
}

// Proves unsafe legacy objects are inconclusive, not clean absence/presence;
// every case has the same valid unopened regular-file neighbor first.
func TestAgentPresenceLegacyUnsafeNeighbors(t *testing.T) {
	for _, variant := range []string{"symlink", "directory", "public", "empty", "human", "pending"} {
		t.Run(variant, func(t *testing.T) {
			f := newPresenceFixture(t)
			if err := os.Remove(f.p.ConnectionsFile()); err != nil {
				t.Fatal(err)
			}
			f.raw(f.p.AgentFile("target"), []byte("synthetic unread token"))
			get := func(string) string { return "" }
			requirePresence(t, InspectAgentPresence(f.p, "target", "", "", get), "present", "legacy_regular_file_only", "regular_file_only")
			path := f.p.AgentFile("target")
			reason := "unsafe_or_unreadable"
			switch variant {
			case "symlink":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "empty":
				f.raw(path, nil)
			case "human":
				f.raw(f.p.HumanFile("target"), []byte("not opened"))
				reason = "identity_class"
			case "pending":
				f.raw(filepath.Join(f.p.Base, "registration.pending.json"), []byte("not opened"))
				reason = "unqualified_state"
			}
			requirePresence(t, InspectAgentPresence(f.p, "target", "", "", get), "inconclusive", "none", reason)
		})
	}
}
