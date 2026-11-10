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
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Reuse the admin-room server dialect as an in-memory transport. A lost
// authenticated response models a remote creation whose outcome is unknown.
type adminFileTransport struct {
	fixture      *admin198Fixture
	loseResponse bool
	password     string
}

func (rt *adminFileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	authenticated := false
	if strings.HasSuffix(r.URL.Path, "/register") {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var req struct {
			Password string         `json:"password"`
			Auth     map[string]any `json:"auth"`
		}
		if err = json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Password != "" {
			rt.password = req.Password
		}
		authenticated = req.Auth != nil
	}
	recorder := httptest.NewRecorder()
	rt.fixture.serve(recorder, r)
	if rt.loseResponse && authenticated {
		return nil, errors.New("isolated response loss")
	}
	response := recorder.Result()
	response.Request = r
	return response, nil
}

// Both backends complete the real admin-room/UIA flow with the existing fake
// dialect. File stores only a 0600 token; Keychain keeps its metadata/recovery
// password behavior. Rejection, lost response and pre-publication interruption
// retain a secret-free journal, publish no file secrets, and forbid a retry.
// A broad owner-sender subscription is present and unchanged in every scenario.
func TestAdminRegistrationCredentialBackends(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		for _, mode := range []string{"success", "ambiguous", "rejected", "publication-interruption"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				serverMode := "positive"
				if mode == "rejected" {
					serverMode = "expired"
				}
				f := newAdmin198Backend(t, serverMode, backend)
				f.subscriptions = &SubList{V: 1, Subs: []Sub{{ID: "owner-wide", SubSelector: SubSelector{From: admin198Owner}}}}
				beforeSubs := f.subscriptionSnapshot()
				var out, stderr bytes.Buffer
				a := f.app(&out, &stderr)
				rt := &adminFileTransport{fixture: f, loseResponse: mode == "ambiguous"}
				a.HTTP = &http.Client{Transport: rt}
				if backend == "file" {
					forbidFileKeychain(a)
					getenv := a.Getenv
					a.Getenv = func(k string) string {
						if k == envSecretStore {
							return "file"
						}
						return getenv(k)
					}
				}
				if mode == "publication-interruption" {
					a.beforeNamedPublish = func() error { return errors.New("isolated publication interruption") }
				}
				args := []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"}
				code := a.Run(context.Background(), args)
				if (code == 0) != (mode == "success") {
					t.Fatal("unexpected registration verdict")
				}
				if rt.password == "" || f.issues != 1 || f.auths != 1 || f.registrations != 2 || f.redactions != 2 {
					t.Fatal("admin issue/UIA/cleanup control did not execute exactly once")
				}
				p, err := f.c.Paths(config.Paths{Base: f.root}, "newbie", false)
				if err != nil {
					t.Fatal(err)
				}
				b, err := f.c.Binding("newbie", false)
				if err != nil {
					t.Fatal(err)
				}
				journal, err := os.ReadFile(filepath.Join(p.Base, "registration.pending.json"))
				if err != nil {
					t.Fatal(err)
				}
				var j registrationJournal
				if err = json.Unmarshal(journal, &j); err != nil {
					t.Fatal(err)
				}
				wantStage := map[string]string{"success": "complete", "ambiguous": "remote-outcome-ambiguous", "rejected": "remote-rejected", "publication-interruption": "remote-created-metadata-publication-pending"}[mode]
				if j.Stage != wantStage {
					t.Fatalf("journal stage %q, want %q", j.Stage, wantStage)
				}
				if mode == "rejected" && (j.HTTPStatus != 403 || j.ErrCode != "M_FORBIDDEN") {
					t.Fatal("file/keychain rejection evidence changed")
				}
				if mode == "success" {
					creds, err := config.LoadCredentialMetadata(p, "newbie", false)
					if err != nil {
						t.Fatal(err)
					}
					if err = config.ValidateBoundCredentials(creds, b); err != nil {
						t.Fatal(err)
					}
					st, err := os.Stat(p.AgentFile("newbie"))
					if err != nil || st.Mode().Perm() != 0600 {
						t.Fatal("credential mode is not 0600")
					}
					if creds.Password != "" {
						t.Fatal("password written to credentials")
					}
					if backend == "file" {
						if creds.AccessToken != "isolated-new-account-access" {
							t.Fatal("access token missing from file")
						}
						loaded, err := a.loadCredentials(context.Background(), p, "newbie")
						if err != nil || loaded.AccessToken != creds.AccessToken {
							t.Fatal("registered token cannot be loaded")
						}
					} else if creds.AccessToken != "" {
						t.Fatal("Keychain token appeared in metadata")
					}
				} else {
					if _, err = os.Lstat(p.AgentFile("newbie")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed registration published credentials")
					}
					if mode != "publication-interruption" && (!strings.Contains(stderr.String(), "no automatic retry") || backend == "file" && !strings.Contains(stderr.String(), "owner recovery required")) {
						t.Fatal("failure lost recovery/no-retry diagnostic")
					}
					issues, auths, registrations := f.issues, f.auths, f.registrations
					retry := f.app(&out, &stderr)
					retry.HTTP = &http.Client{Transport: rt}
					if backend == "file" {
						forbidFileKeychain(retry)
					}
					if retry.Run(context.Background(), args) == 0 || !strings.Contains(stderr.String(), "owner recovery required") {
						t.Fatal("pending registration admitted retry")
					}
					if f.issues != issues || f.auths != auths || f.registrations != registrations {
						t.Fatal("retry repeated token issue or registration")
					}
				}
				if backend == "keychain" {
					f.store.mu.Lock()
					password := f.store.items[config.SecretReference{Service: f.c.Store.PasswordService, Account: b.UserID}]
					token := f.store.items[config.SecretReference{Service: f.c.Store.AccessTokenService, Account: b.UserID}]
					f.store.mu.Unlock()
					if password != rt.password {
						t.Fatal("Keychain recovery password changed")
					}
					if mode == "success" || mode == "publication-interruption" {
						if token != "isolated-new-account-access" {
							t.Fatal("Keychain token storage changed")
						}
					} else if token != "" {
						t.Fatal("failed remote registration stored a token")
					}
				} else if len(f.store.reads)+len(f.store.creates)+len(f.store.items) != 0 {
					t.Fatal("file registration used Keychain")
				}
				// Inspect every persistent file, including temp files and the journal;
				// only the successful destination credential file may contain its token.
				err = filepath.WalkDir(f.root, func(path string, d os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if d.IsDir() {
						return nil
					}
					data, readErr := os.ReadFile(path)
					if readErr != nil {
						return readErr
					}
					if bytes.Contains(data, []byte(rt.password)) || bytes.Contains(data, []byte(admin198Secret)) {
						t.Fatal("password or admission token persisted")
					}
					if bytes.Contains(data, []byte("isolated-new-account-access")) && !(backend == "file" && mode == "success" && path == p.AgentFile("newbie")) {
						t.Fatal("access token persisted outside successful file credentials")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, data := range [][]byte{out.Bytes(), stderr.Bytes(), f.logs.Bytes()} {
					if bytes.Contains(data, []byte(rt.password)) || bytes.Contains(data, []byte("isolated-new-account-access")) {
						t.Fatal("registration credential leaked to output/logs")
					}
				}
				f.assertNoExports(out.Bytes(), stderr.Bytes())
				f.assertSubscriptionsUnchanged(beforeSubs)
			})
		}
	}
}

// File archive uses the bound file session and retains the existing admin-room
// exclusion with a broad owner-sender row, while an ordinary room still reaches
// the fake history transport.
func TestAdminFileArchive(t *testing.T) {
	for _, room := range []string{admin198Room, admin198Normal} {
		t.Run(room, func(t *testing.T) {
			f := newAdmin198Backend(t, "delivery-owner", "file")
			var out, stderr bytes.Buffer
			a := f.app(&out, &stderr)
			forbidFileKeychain(a)
			a.HTTP = &http.Client{Transport: &adminFileTransport{fixture: f}}
			if a.Run(context.Background(), []string{"--as", "admin", "archive", room, "--dir", filepath.Join(f.root, "archive")}) != 0 {
				t.Fatal("file archive control failed")
			}
			if (f.history > 0) != (room == admin198Normal) {
				t.Fatal("archive history exclusion changed")
			}
			f.assertNoExports(out.Bytes(), stderr.Bytes())
		})
	}
}
