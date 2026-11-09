package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testConnection() Connection {
	return Connection{ID: "one-id", URL: "https://api.example.invalid", ServerName: "identity.example.invalid", Store: CredentialStore{Backend: "keychain", AccessTokenService: "test.one.token", PasswordService: "test.one.password"}}
}
func writeRegistry(t *testing.T, p Paths, r ConnectionRegistry) {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p.ConnectionsFile(), b, 0600); e != nil {
		t.Fatal(e)
	}
}

// Proves strict registry rejection alongside a valid endpoint/identity-domain
// split. Rejected updates preserve the original registry bytes.
func TestConnectionRegistryValidation(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	c := testConnection()
	r := ConnectionRegistry{1, "one", map[string]Connection{"one": c}}
	writeRegistry(t, p, r)
	if _, e := LoadConnections(p); e != nil {
		t.Fatal(e)
	}
	original, _ := os.ReadFile(p.ConnectionsFile())
	cases := []string{
		`{"schema_version":1,"schema_version":1,"current":"one","profiles":{}}`,
		strings.Replace(string(original), `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(string(original), `"current":"one"`, `"current":"missing"`, 1),
		strings.Replace(string(original), `"backend":"keychain"`, `"backend":"file"`, 1),
		strings.Replace(string(original), `https://api.example.invalid`, `https://secret@api.example.invalid`, 1),
		strings.Replace(string(original), `"one-id"`, `"../escape"`, 1),
		strings.Replace(string(original), `"connection_id":"one-id"`, `"connection_id":"one-id","connection_id":"other"`, 1),
		string(original) + ` {}`,
	}
	for i, input := range cases {
		if e := os.WriteFile(p.ConnectionsFile(), []byte(input), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadConnections(p); e == nil {
			t.Errorf("case %d admitted", i)
		}
	}
	writeRegistry(t, p, r)
	if e := UpdateConnections(p, func(r *ConnectionRegistry) error { r.Profiles["two"] = c; return nil }); e == nil {
		t.Fatal("duplicate id accepted")
	}
	got, _ := os.ReadFile(p.ConnectionsFile())
	if string(got) != string(original) {
		t.Fatal("refused update changed registry")
	}
	if e := os.Remove(p.ConnectionsFile()); e != nil {
		t.Fatal(e)
	}
	if r, e := LoadConnections(p); e != nil || r != nil {
		t.Fatal("absence is not legacy")
	}
	if e := os.Symlink(filepath.Join(p.Base, "missing"), p.ConnectionsFile()); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadConnections(p); e == nil {
		t.Fatal("broken symlink treated as absence")
	}
}

// Proves identity class/full MXID and stable connection ID isolate mutable
// paths, while registry presentation and bootstrap changes do not break pins.
func TestConnectionNamespaceAndFingerprint(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	c := testConnection()
	a, _ := c.Paths(p, "alice", false)
	h, _ := c.Paths(p, "alice", true)
	other := c
	other.ID = "two-id"
	other.ServerName = "second.invalid"
	b, _ := other.Paths(p, "alice", false)
	if a.Base == h.Base || a.Base == b.Base || a.StateFile("alice") == b.StateFile("alice") || a.AgentFile("alice") == b.AgentFile("alice") {
		t.Fatal("shared state or metadata")
	}
	if a.ConfigRoot() != p.Base || a.HostFile() != p.HostFile() {
		t.Fatal("lost host/registry root")
	}
	bound, _ := c.Binding("alice", false)
	f := bound.Fingerprint()
	bound.Connection.RegistrationTokenRef = &SecretReference{"test.bootstrap", "admitted"}
	if bound.Fingerprint() != f {
		t.Fatal("bootstrap change invalidated operational binding")
	}
	bound.Connection.Store.AccessTokenService = "changed"
	if bound.Fingerprint() == f {
		t.Fatal("store change did not invalidate pin")
	}
}

// Proves complete publication is exclusive under concurrent attempts and
// failed overwrite leaves the published identity bytes intact.
func TestConnectionPublicationExclusive(t *testing.T) {
	c := testConnection()
	p, _ := c.Paths(Paths{Base: t.TempDir()}, "alice", false)
	b, _ := c.Binding("alice", false)
	cred := Credentials{UserID: b.UserID, Homeserver: c.URL, ConnectionBinding: &b}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- PublishBoundCredentials(p, "alice", false, cred) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("published %d times", successes)
	}
	before, _ := os.ReadFile(p.AgentFile("alice"))
	cred.DeviceID = "replacement"
	if e := PublishBoundCredentials(p, "alice", false, cred); e == nil {
		t.Fatal("overwrite admitted")
	}
	after, _ := os.ReadFile(p.AgentFile("alice"))
	if string(before) != string(after) {
		t.Fatal("overwrite changed identity")
	}
	cred.AccessToken = "forbidden"
	if e := ValidateBoundCredentials(cred, b); e == nil {
		t.Fatal("secret-bearing metadata admitted")
	}
}

// Proves an existing symlink cannot redirect connection or identity state into
// another directory, while ordinary missing namespace directories are valid.
func TestConnectionNamespaceSymlinkRefusal(t *testing.T) {
	for _, part := range []string{"connections/one-id", "connections/one-id/agents", "connections/one-id/identities"} {
		t.Run(part, func(t *testing.T) {
			p := Paths{Base: t.TempDir()}
			c := testConnection()
			if _, e := c.Paths(p, "alice", false); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(p.Base, part)
			if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(t.TempDir(), path); e != nil {
				t.Fatal(e)
			}
			if _, e := c.Paths(p, "alice", false); e == nil {
				t.Fatal("symlink selected another namespace")
			}
		})
	}
}

// Card-room normalization depends on the identity server, never URL host or ID
// suffix. Bad configuration is rejected without reproducing the supplied value.
func TestConnectionCardRoom(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"", "#lobby:identity.example.invalid"},
		{"#cards", "#cards:identity.example.invalid"},
		{"#cards:store.example.invalid", "#cards:store.example.invalid"},
		{"!opaque:else.example.invalid", "!opaque:else.example.invalid"},
		{"!opaque", "!opaque"},
	} {
		c := testConnection()
		c.CardRoom = tc.ref
		got, err := c.CardRoomRef()
		if err != nil || got != tc.want {
			t.Fatalf("%q = %q, %v", tc.ref, got, err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		b, _ := c.Binding("alice", false)
		original := testConnection()
		old, _ := original.Binding("alice", false)
		if b.Fingerprint() != old.Fingerprint() {
			t.Fatal("card policy changed credential namespace")
		}
	}
	for _, ref := range []string{"cards", "!", "#", "#cards:", "#cards:bad/path", "#has space", "!has\ncontrol", "#a#b", "#a/b"} {
		c := testConnection()
		c.CardRoom = ref
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "invalid card_room") {
			t.Fatalf("bad card room accepted: %q, %v", ref, err)
		}
	}
}

func TestConnectionCardRoomCaseShadow(t *testing.T) {
	c := testConnection()
	c.CardRoom = "#cards"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Connection
	if err := DecodeConnectionJSON(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"card_room":"#cards"`, `"card_room":"#cards","Card_Room":"#other"`, 1))
	if err := DecodeConnectionJSON(raw, &decoded); err == nil {
		t.Fatal("connection entry admitted a shadow card_room")
	}
}
