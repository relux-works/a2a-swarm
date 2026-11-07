package config

import (
	"os"
	"strings"
	"testing"
)

// Both backends admit their own configuration and refuse mixed services,
// invalid Keychain services and unsupported backends; only Keychain admits refs.
func TestConnectionCredentialStoreValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store CredentialStore
		ref   *SecretReference
		want  string
	}{
		{"keychain", testConnection().Store, nil, ""},
		{"keychain-bootstrap", testConnection().Store, &SecretReference{"test.bootstrap", "owner"}, ""},
		{"keychain-no-services", CredentialStore{Backend: "keychain"}, nil, "invalid or unsupported credential_store"},
		{"keychain-no-password", CredentialStore{Backend: "keychain", AccessTokenService: "test.token"}, nil, "invalid or unsupported credential_store"},
		{"keychain-no-token", CredentialStore{Backend: "keychain", PasswordService: "test.password"}, nil, "invalid or unsupported credential_store"},
		{"keychain-same-services", CredentialStore{"keychain", "test.same", "test.same"}, nil, "invalid or unsupported credential_store"},
		{"keychain-bad-service", CredentialStore{"keychain", "bad service", "test.password"}, nil, "invalid or unsupported credential_store"},
		{"keychain-overlapping-bootstrap", testConnection().Store, &SecretReference{testConnection().Store.AccessTokenService, "owner"}, "invalid independent registration_token_ref"},
		{"keychain-empty-account", testConnection().Store, &SecretReference{"test.bootstrap", ""}, "invalid independent registration_token_ref"},
		{"file", CredentialStore{Backend: "file"}, nil, ""},
		{"file-token-service", CredentialStore{Backend: "file", AccessTokenService: "test.token"}, nil, "file credential_store must not declare keychain service names"},
		{"file-password-service", CredentialStore{Backend: "file", PasswordService: "test.password"}, nil, "file credential_store must not declare keychain service names"},
		{"file-both-services", CredentialStore{"file", "test.token", "test.password"}, nil, "file credential_store must not declare keychain service names"},
		{"file-bootstrap", CredentialStore{Backend: "file"}, &SecretReference{"test.bootstrap", "owner"}, "file credential_store cannot use keychain registration_token_ref"},
		{"file-empty-ref", CredentialStore{Backend: "file"}, &SecretReference{}, "file credential_store cannot use keychain registration_token_ref"},
		{"unknown", CredentialStore{Backend: "other"}, nil, "invalid or unsupported credential_store"},
		{"empty", CredentialStore{}, nil, "invalid or unsupported credential_store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConnection()
			c.Store = tc.store
			c.RegistrationTokenRef = tc.ref
			p := Paths{Base: t.TempDir()}
			writeRegistry(t, p, ConnectionRegistry{1, "one", map[string]Connection{"one": c}})
			_, err := LoadConnections(p)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}

// File tokens publish at the existing path with 0600 and exclusive creation;
// passwords and binding changes are refused before any credential is published.
func TestBoundFileCredentials(t *testing.T) {
	for _, mode := range []string{"valid", "password", "binding", "keychain-token"} {
		t.Run(mode, func(t *testing.T) {
			c := testConnection()
			if mode != "keychain-token" {
				c.Store = CredentialStore{Backend: "file"}
			}
			b, err := c.Binding("alice", false)
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.Paths(Paths{Base: t.TempDir()}, "alice", false)
			if err != nil {
				t.Fatal(err)
			}
			creds := Credentials{UserID: b.UserID, Homeserver: c.URL, ConnectionBinding: &b, AccessToken: "file-token-control"}
			if mode == "password" {
				creds.Password = "forbidden-password"
			}
			if mode == "binding" {
				creds.Homeserver = "https://wrong.invalid"
			}
			err = PublishBoundCredentials(p, "alice", false, creds)
			if mode != "valid" {
				if err == nil {
					t.Fatal("invalid credentials published")
				}
				if _, err := os.Lstat(p.AgentFile("alice")); !os.IsNotExist(err) {
					t.Fatal("refused publication left a credential file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(p.AgentFile("alice"))
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("file is not 0600")
			}
			got, err := LoadCredentials(p, "alice")
			if err != nil || got.AccessToken != creds.AccessToken || got.Password != "" {
				t.Fatal("existing file loader did not read token-only credentials")
			}
			before, err := os.ReadFile(p.AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			creds.AccessToken = "replacement"
			if PublishBoundCredentials(p, "alice", false, creds) == nil {
				t.Fatal("overwrite admitted")
			}
			after, err := os.ReadFile(p.AgentFile("alice"))
			if err != nil || string(before) != string(after) {
				t.Fatal("refused overwrite changed credentials")
			}
		})
	}
}
