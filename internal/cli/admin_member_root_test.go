package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type memberRootTransport struct {
	adminFileTransport
	before       func(*http.Request)
	after        func(*http.Request)
	memberWhoami int
	adminWhoami  int
}

func (rt *memberRootTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if rt.before != nil {
		rt.before(r)
	}
	if strings.HasSuffix(r.URL.Path, "/account/whoami") && r.Header.Get("Authorization") == "Bearer isolated-new-account-access" {
		rt.memberWhoami++
		rec := httptest.NewRecorder()
		_ = json.NewEncoder(rec).Encode(map[string]string{"user_id": "@newbie:test.invalid", "device_id": "new-device"})
		return rec.Result(), nil
	}
	if strings.HasSuffix(r.URL.Path, "/account/whoami") {
		if r.Header.Get("Authorization") != "Bearer isolated-admin-session" {
			return nil, errors.New("fixture administrator authorization mismatch")
		}
		rt.adminWhoami++
	}
	response, err := rt.adminFileTransport.RoundTrip(r)
	if rt.after != nil {
		rt.after(r)
	}
	return response, err
}

func memberRootPath(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve fixture parent")
	}
	return filepath.Join(parent, "member")
}

func memberRootFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	}); err != nil {
		t.Fatal("fixture file inspection failed")
	}
	return files
}

func memberRootApp(f *admin198Fixture, out, stderr io.Writer) (*App, *memberRootTransport) {
	a := f.app(out, stderr)
	rt := &memberRootTransport{adminFileTransport: adminFileTransport{fixture: f}}
	a.HTTP = &http.Client{Transport: rt}
	if f.c.Store.Backend == "file" {
		forbidFileKeychain(a)
	}
	return a, rt
}

func memberRootArgs(target string) []string {
	return []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room", "--into", target}
}

func memberSourceReadCount(f *admin198Fixture) int {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	return len(f.store.reads)
}

// Count only invocation reads: fixture snapshots also load the administrator
// token. Successful/remote-flow Keychain controls must authorize independently.
func assertMemberSourceAuthorized(t *testing.T, f *admin198Fixture, beforeReads int) {
	t.Helper()
	if f.c.Store.Backend == "keychain" && memberSourceReadCount(f) <= beforeReads {
		t.Fatal("source Keychain authorization control did not read the administrator token")
	}
}

// Preservation forbids all creates and non-admin reads, including on early
// rejection. It does not require a credential read from an invalid invocation.
func assertMemberSourceUntouched(t *testing.T, f *admin198Fixture, before map[string]string, subs []byte) {
	t.Helper()
	if !reflect.DeepEqual(before, memberRootFiles(t, f.root)) {
		t.Fatal("member transaction changed source files")
	}
	f.store.mu.Lock()
	creates := len(f.store.creates)
	reads := append([]config.SecretReference(nil), f.store.reads...)
	f.store.mu.Unlock()
	if creates != 0 {
		t.Fatal("member transaction created source secret-store items")
	}
	for _, ref := range reads {
		if ref.Account != admin198User {
			t.Fatal("member transaction read a child/bootstrap secret-store item")
		}
	}
	f.assertSubscriptionsUnchanged(subs)
}

// Both administrator backends authorize the same opt-in transaction. All child
// secrets/config live in the fresh member root, and the ordinary member CLI
// works after the fixture administrator root is moved out of reach. Legacy
// no-destination controls remain in TestAdminRegistrationCredentialBackends.
// Only invocation reads supply the Keychain authorization witness.
func TestAdminMemberRootSuccess(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		t.Run(backend, func(t *testing.T) {
			f := newAdmin198Backend(t, "broad-subscription", backend)
			f.c.CardRoom = "#cards:test.invalid"
			if backend == "keychain" {
				f.c.RegistrationTokenRef = &config.SecretReference{Service: "test.bootstrap", Account: "bootstrap"}
			}
			f.save()
			if config.UpdateConnections(config.Paths{Base: f.root}, func(r *config.ConnectionRegistry) error {
				other := f.c
				other.ID = "unselected"
				r.Profiles["unselected"] = other
				return nil
			}) != nil || os.WriteFile(filepath.Join(f.root, "private-note"), []byte("source-only-note"), 0600) != nil {
				t.Fatal("source projection control setup failed")
			}
			f.seedExistingMute()
			subs := f.subscriptionSnapshot()
			before := memberRootFiles(t, f.root)
			target := memberRootPath(t)
			var out, stderr bytes.Buffer
			a, rt := memberRootApp(f, &out, &stderr)
			readsBefore := memberSourceReadCount(f)
			if a.Run(context.Background(), append([]string{"--profile", "test"}, memberRootArgs(target)...)) != 0 {
				t.Fatal("fresh member registration failed")
			}
			if rt.adminWhoami != 1 || f.issues != 1 || f.auths != 1 || f.registrations != 2 || f.redactions != 2 || !strings.Contains(out.String(), "registered newbie;") {
				t.Fatal("member registration/cleanup control missing")
			}
			assertMemberSourceAuthorized(t, f, readsBefore)
			assertMemberSourceUntouched(t, f, before, subs)
			registry, err := config.LoadConnections(config.Paths{Base: target})
			if err != nil || registry == nil || registry.Current != "test" || len(registry.Profiles) != 1 {
				t.Fatal("member registry is not independently selectable")
			}
			c := registry.Profiles["test"]
			if c.ID != f.c.ID || c.URL != f.c.URL || c.ServerName != f.c.ServerName || c.CardRoom != f.c.CardRoom || c.Store != (config.CredentialStore{Backend: "file"}) || c.AdminRegistration != nil || c.RegistrationTokenRef != nil {
				t.Fatal("member projection copied administrator policy/store or lost routing")
			}
			p, err := c.Paths(config.Paths{Base: target}, "newbie", false)
			if err != nil {
				t.Fatal("member paths unavailable")
			}
			creds, err := config.LoadCredentialMetadata(p, "newbie", false)
			binding, bindingErr := c.Binding("newbie", false)
			if err != nil || bindingErr != nil || !reflect.DeepEqual(creds.ConnectionBinding, &binding) || creds.AccessToken != "isolated-new-account-access" || creds.Password != "" {
				t.Fatal("member credentials contain source binding or lack the member token")
			}
			data, err := os.ReadFile(filepath.Join(target, "registration.pending.json"))
			var journal registrationJournal
			if err != nil || json.Unmarshal(data, &journal) != nil || journal.Stage != "complete" {
				t.Fatal("member completion evidence missing")
			}
			files := memberRootFiles(t, target)
			for path, data := range files {
				if strings.Contains(data, rt.password) || strings.Contains(data, admin198Secret) || strings.Contains(data, "isolated-admin-session") || strings.Contains(data, "source-only-note") {
					t.Fatal("source/admission/password material persisted in target")
				}
				if strings.Contains(data, admin198User) || strings.Contains(data, admin198Bot) {
					t.Fatal("administrator principal copied into member root")
				}
				if strings.Contains(data, "isolated-new-account-access") && filepath.Join(target, path) != p.AgentFile("newbie") {
					t.Fatal("member token persisted outside member credential file")
				}
			}
			if err := filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				st, err := os.Lstat(path)
				if err != nil {
					return err
				}
				if d.IsDir() && st.Mode().Perm() != 0700 || !d.IsDir() && (!st.Mode().IsRegular() || st.Mode().Perm() != 0600) {
					t.Fatal("member storage is not private regular-file storage")
				}
				return nil
			}); err != nil {
				t.Fatal("member storage inspection failed")
			}
			f.assertNoExports(out.Bytes(), stderr.Bytes())
			if strings.Contains(out.String()+stderr.String()+f.logs.String(), rt.password) || strings.Contains(out.String()+stderr.String()+f.logs.String(), "isolated-new-account-access") {
				t.Fatal("member credential leaked to output/logs")
			}
			moved := f.root + "-unavailable"
			if os.Rename(f.root, moved) != nil {
				t.Fatal("cannot make admin root unavailable")
			}
			t.Cleanup(func() { _ = os.Rename(moved, f.root) })
			member := NewApp()
			out.Reset()
			stderr.Reset()
			member.Stdout, member.Stderr, member.HTTP = &out, &stderr, &http.Client{Transport: rt}
			member.Getenv = func(k string) string {
				if k == config.EnvConfigDir {
					return target
				}
				if k == config.EnvAgent {
					return "newbie"
				}
				return ""
			}
			forbidFileKeychain(member)
			if member.Run(context.Background(), []string{"whoami"}) != 0 || !strings.Contains(out.String(), "@newbie:test.invalid") || rt.memberWhoami != 1 {
				t.Fatal("ordinary member whoami depends on source root or wrong token/binding")
			}
		})
	}
}

// Destination mistakes refuse before issue, UIA, child store creates or source
// metadata changes. Existing directory/file contents and symlink referents are
// never adopted or overwritten; the success test supplies nearby valid controls.
// Parse/name rejections must add no secret reads, excluding fixture snapshots.
func TestAdminMemberRootPreflight(t *testing.T) {
	for _, scenario := range []string{"existing-empty", "existing-nonempty", "existing-file", "relative", "alias", "source", "nested", "containing", "leaf-symlink", "ancestor-symlink", "unsafe-parent", "missing-parent", "repeated", "conflicting", "empty", "missing-value", "invalid-name", "source-identity", "source-metadata", "source-human", "source-pending", "source-migration"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmin198(t, "positive")
			f.seedExistingMute()
			subs := f.subscriptionSnapshot()
			target := memberRootPath(t)
			args := memberRootArgs(target)
			switch scenario {
			case "existing-empty":
				if os.Mkdir(target, 0700) != nil {
					t.Fatal("fixture mkdir")
				}
			case "existing-nonempty":
				if os.Mkdir(target, 0700) != nil || os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0600) != nil {
					t.Fatal("fixture existing")
				}
			case "existing-file":
				if os.WriteFile(target, []byte("keep"), 0600) != nil {
					t.Fatal("fixture file")
				}
			case "relative":
				args[len(args)-1] = "relative-member"
			case "alias":
				args[len(args)-1] = filepath.Dir(target) + "/unused/../member"
			case "source":
				args[len(args)-1] = f.root
			case "nested":
				args[len(args)-1] = filepath.Join(f.root, "member")
			case "containing":
				args[len(args)-1] = filepath.Dir(f.root)
			case "leaf-symlink":
				if os.Symlink(f.root, target) != nil {
					t.Fatal("fixture link")
				}
			case "ancestor-symlink":
				link := filepath.Join(filepath.Dir(target), "link")
				if os.Symlink(f.root, link) != nil {
					t.Fatal("fixture link")
				}
				args[len(args)-1] = filepath.Join(link, "member")
			case "unsafe-parent":
				if os.Chmod(filepath.Dir(target), 0777) != nil {
					t.Fatal("fixture mode")
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(target), 0700) })
			case "missing-parent":
				args[len(args)-1] = filepath.Join(target, "missing", "member")
			case "repeated":
				args = append(args, "--into", target)
			case "conflicting":
				args = append(args, "--into", target+"-other")
			case "empty":
				args[len(args)-1] = ""
			case "missing-value":
				args = args[:len(args)-1]
			case "invalid-name":
				args[4] = "../invalid"
			case "source-identity":
				args[4] = "admin"
			case "source-metadata", "source-human", "source-pending", "source-migration":
				p, err := f.c.Paths(config.Paths{Base: f.root}, "newbie", false)
				if err != nil {
					t.Fatal("source identity plant paths")
				}
				path := p.AgentFile("newbie")
				data := []byte("existing-source-metadata")
				if scenario == "source-human" {
					path = p.HumanFile("newbie")
				}
				if scenario == "source-pending" {
					path = filepath.Join(p.Base, "registration.pending.json")
				}
				if scenario == "source-migration" {
					path = p.StateFile("newbie")
					data = []byte(`{"migration":{"phase":"migrated"}}`)
				}
				if os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, data, 0600) != nil {
					t.Fatal("source identity plant")
				}
			}
			before := memberRootFiles(t, f.root)
			var out, stderr bytes.Buffer
			a, _ := memberRootApp(f, &out, &stderr)
			readsBefore := memberSourceReadCount(f)
			if a.Run(context.Background(), args) == 0 || !strings.Contains(stderr.String(), "stage=destination errcode=LOCAL_REFUSAL HTTP=0") || out.Len() != 0 || f.issues != 0 || f.auths != 0 || f.registrations != 0 || f.redactions != 0 {
				t.Fatal("destination did not refuse at intended preflight without remote effects")
			}
			switch scenario {
			case "repeated", "conflicting", "empty", "missing-value", "invalid-name":
				if memberSourceReadCount(f) != readsBefore {
					t.Fatal("early parse/name refusal read administrator credentials")
				}
			}
			assertMemberSourceUntouched(t, f, before, subs)
			if scenario == "existing-nonempty" {
				b, e := os.ReadFile(filepath.Join(target, "keep"))
				if e != nil || string(b) != "keep" {
					t.Fatal("existing contents changed")
				}
			}
			if scenario == "existing-file" {
				b, e := os.ReadFile(target)
				if e != nil || string(b) != "keep" {
					t.Fatal("existing file changed")
				}
			}
		})
	}
}

// The first invocation is paused after exclusive target reservation and before
// issue. A second invocation must refuse without any issue/register; releasing
// the first then completes exactly one registration transaction.
func TestAdminMemberRootConcurrentReservation(t *testing.T) {
	f := newAdmin198(t, "positive")
	target := memberRootPath(t)
	reserved, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var out, stderr bytes.Buffer
	a, rt := memberRootApp(f, &out, &stderr)
	rt.before = func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/state") {
			if _, e := os.Stat(target); e == nil {
				once.Do(func() { close(reserved); <-release })
			}
		}
	}
	result := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { result <- a.Run(ctx, memberRootArgs(target)) }()
	select {
	case <-reserved:
	case <-ctx.Done():
		close(release)
		<-result
		t.Fatal("fixture did not reach reserved pre-issue control")
	}
	var secondOut, secondErr bytes.Buffer
	second, _ := memberRootApp(f, &secondOut, &secondErr)
	code := second.Run(context.Background(), memberRootArgs(target))
	noEffects := f.issues == 0 && f.registrations == 0
	close(release)
	first := <-result
	if code == 0 || !strings.Contains(secondErr.String(), "stage=destination") || secondOut.Len() != 0 || !noEffects || first != 0 || f.issues != 1 || f.registrations != 2 || f.auths != 1 {
		t.Fatal("exclusive reservation did not fence duplicate remote effects")
	}
}

// Definite rejection, response loss and cancellation never retry a remote
// attempt. Publication interruptions preserve the conservative journal and
// never report success; even valid member metadata/routing cannot bypass the
// incomplete-root fence in the ordinary session path.
// Each remote-flow Keychain invocation must read its administrator token.
func TestAdminMemberRootRecovery(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		for _, scenario := range []string{"rejected", "ambiguous", "cancel-after-remote", "credential-publication", "registry-publication"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				mode := "positive"
				if scenario == "rejected" {
					mode = "expired"
				}
				f := newAdmin198Backend(t, mode, backend)
				f.seedExistingMute()
				subs := f.subscriptionSnapshot()
				before := memberRootFiles(t, f.root)
				target := memberRootPath(t)
				var out, stderr bytes.Buffer
				a, rt := memberRootApp(f, &out, &stderr)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "ambiguous" {
					rt.loseResponse = true
				}
				if scenario == "cancel-after-remote" {
					rt.after = func(r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/register") && f.auths == 1 {
							cancel()
						}
					}
				}
				if scenario == "credential-publication" {
					a.beforeNamedPublish = func() error { return errors.New("fixture interruption") }
				}
				var planted []byte
				if scenario == "registry-publication" {
					a.beforeMemberRegistry = func() error {
						c := config.Connection{ID: f.c.ID, URL: f.c.URL, ServerName: f.c.ServerName, Store: config.CredentialStore{Backend: "file"}}
						planted, _ = json.Marshal(config.ConnectionRegistry{SchemaVersion: 1, Current: "test", Profiles: map[string]config.Connection{"test": c}})
						return os.WriteFile(filepath.Join(target, "connections.json"), planted, 0600)
					}
				}
				readsBefore := memberSourceReadCount(f)
				if a.Run(ctx, memberRootArgs(target)) == 0 || strings.Contains(out.String(), "registered") || !strings.Contains(stderr.String(), "owner recovery required") {
					t.Fatal("incomplete transaction reported success or lost recovery diagnostic")
				}
				if f.issues != 1 || f.auths != 1 || f.registrations != 2 || f.redactions != 2 {
					t.Fatal("failure did not exercise one remote attempt and cleanup")
				}
				if scenario == "rejected" && !strings.Contains(stderr.String(), "stage=register errcode=M_FORBIDDEN HTTP=403") {
					t.Fatal("definite rejection diagnostic lost")
				}
				data, err := os.ReadFile(filepath.Join(target, "registration.pending.json"))
				var j registrationJournal
				if err != nil || json.Unmarshal(data, &j) != nil {
					t.Fatal("recovery journal missing")
				}
				want := map[string]string{"rejected": "remote-rejected", "ambiguous": "remote-outcome-ambiguous", "cancel-after-remote": "remote-outcome-ambiguous", "credential-publication": "remote-created-metadata-publication-pending", "registry-publication": "remote-created-registry-publication-pending"}[scenario]
				if j.Stage != want || j.HTTPStatus != 0 && (scenario != "rejected" || j.HTTPStatus != 403 || j.ErrCode != "M_FORBIDDEN") {
					t.Fatal("failure journal claims wrong outcome")
				}
				if scenario == "rejected" && (j.HTTPStatus != 403 || j.ErrCode != "M_FORBIDDEN") {
					t.Fatal("rejection journal lacks closed server evidence")
				}
				assertMemberSourceAuthorized(t, f, readsBefore)
				assertMemberSourceUntouched(t, f, before, subs)
				memberConnection := config.Connection{ID: f.c.ID, URL: f.c.URL, ServerName: f.c.ServerName, Store: config.CredentialStore{Backend: "file"}}
				p, _ := memberConnection.Paths(config.Paths{Base: target}, "newbie", false)
				_, credentialErr := os.Lstat(p.AgentFile("newbie"))
				if scenario == "registry-publication" {
					if credentialErr != nil {
						t.Fatal("registry failure control did not reach credential publication")
					}
					got, err := os.ReadFile(filepath.Join(target, "connections.json"))
					if err != nil || !bytes.Equal(got, planted) {
						t.Fatal("substituted registry overwritten")
					}
					member := NewApp()
					var mo, me bytes.Buffer
					member.Stdout, member.Stderr, member.HTTP = &mo, &me, &http.Client{Transport: rt}
					member.Getenv = func(k string) string {
						if k == config.EnvConfigDir {
							return target
						}
						if k == config.EnvAgent {
							return "newbie"
						}
						return ""
					}
					if member.Run(context.Background(), []string{"whoami"}) == 0 || !strings.Contains(me.String(), "owner recovery required") || rt.memberWhoami != 0 {
						t.Fatal("ordinary member use bypassed incomplete-root fence")
					}
				} else if !errors.Is(credentialErr, os.ErrNotExist) {
					t.Fatal("failed transaction unexpectedly published credentials")
				}
				for path, data := range memberRootFiles(t, target) {
					if strings.Contains(data, rt.password) || strings.Contains(data, admin198Secret) {
						t.Fatal("recovery evidence leaked password/admission token")
					}
					if strings.Contains(data, "isolated-new-account-access") && !(scenario == "registry-publication" && filepath.Join(target, path) == p.AgentFile("newbie")) {
						t.Fatal("recovery token outside member credentials")
					}
				}
				if strings.Contains(out.String()+stderr.String()+f.logs.String(), rt.password) || strings.Contains(out.String()+stderr.String()+f.logs.String(), "isolated-new-account-access") {
					t.Fatal("failure leaked member credentials")
				}
				f.assertNoExports(out.Bytes(), stderr.Bytes())
				issues, registers := f.issues, f.registrations
				retry, _ := memberRootApp(f, io.Discard, &stderr)
				if retry.Run(context.Background(), memberRootArgs(target)) == 0 || f.issues != issues || f.registrations != registers {
					t.Fatal("retained root admitted automatic reissue/re-registration")
				}
			})
		}
	}
}

// Replacing the root or its parent cannot redirect writes. A pre-issue change
// has no remote effects; post-remote replacement retains uncertainty without
// publishing into the substitute or source. Success is the unchanged-path test.
// Source authorization is still required before these later replacement faults.
func TestAdminMemberRootReplacement(t *testing.T) {
	for _, scenario := range []string{"before-issue", "after-remote", "ancestor-after-remote"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdmin198(t, "positive")
			f.seedExistingMute()
			subs := f.subscriptionSnapshot()
			before := memberRootFiles(t, f.root)
			target := memberRootPath(t)
			moved := target + "-moved"
			var out, stderr bytes.Buffer
			a, rt := memberRootApp(f, &out, &stderr)
			replace := func() error {
				from := target
				if scenario == "ancestor-after-remote" {
					from = filepath.Dir(target)
					moved = from + "-moved"
				}
				if err := os.Rename(from, moved); err != nil {
					return err
				}
				if err := os.Symlink(f.root, from); err != nil {
					return err
				}
				t.Cleanup(func() { _ = os.Remove(from); _ = os.Rename(moved, from) })
				return nil
			}
			if scenario == "before-issue" {
				var once sync.Once
				rt.before = func(r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						once.Do(func() {
							if replace() != nil {
								t.Fatal("replacement plant failed")
							}
						})
					}
				}
			} else {
				a.beforeNamedPublish = replace
			}
			readsBefore := memberSourceReadCount(f)
			if a.Run(context.Background(), memberRootArgs(target)) == 0 || out.Len() != 0 {
				t.Fatal("replaced root reported success")
			}
			if scenario == "before-issue" {
				if f.issues != 0 || f.registrations != 0 || !strings.Contains(stderr.String(), "stage=destination") {
					t.Fatal("pre-issue replacement passed destination guard")
				}
			} else if f.issues != 1 || f.registrations != 2 || f.redactions != 2 || !strings.Contains(stderr.String(), "owner recovery required") {
				t.Fatal("post-remote replacement lost recovery/cleanup bounds")
			}
			assertMemberSourceAuthorized(t, f, readsBefore)
			assertMemberSourceUntouched(t, f, before, subs)
			original := moved
			if scenario == "ancestor-after-remote" {
				original = filepath.Join(moved, "member")
			}
			c := config.Connection{ID: f.c.ID, URL: f.c.URL, ServerName: f.c.ServerName, Store: config.CredentialStore{Backend: "file"}}
			p, err := c.Paths(config.Paths{Base: original}, "newbie", false)
			if err != nil {
				t.Fatal("original destination inspection failed")
			}
			if _, err := os.Lstat(p.AgentFile("newbie")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("replaced root published member credentials")
			}
			if _, err := os.Lstat(filepath.Join(original, "registration.pending.json")); err != nil {
				t.Fatal("replacement discarded recovery fence")
			}
			f.assertNoExports(out.Bytes(), stderr.Bytes())
		})
	}
}
