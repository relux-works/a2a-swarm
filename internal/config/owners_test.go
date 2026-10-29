package config

import (
	"os"
	"strings"
	"testing"
)

// Owner IDs are full Matrix user IDs only: a localpart, display name, bare
// server or malformed server name is refused, a real MXID is accepted.
func TestValidOwnerID(t *testing.T) {
	for _, id := range []string{"@alexis:relux.works", "@alexis:gr.relux.works", "@a.b_c=d/e+f-g:example.org:8448"} {
		if !ValidOwnerID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"alexis", "@alexis", "Alexis Display", "@:relux.works", "@Alexis:relux.works", "@alexis:", "@alexis:relux works", "relux.works"} {
		if ValidOwnerID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

// LoadOwners treats only an absent registry as unconfigured; a strict
// registry loads, while unknown fields, duplicate keys or owners, invalid
// entries, a wrong schema and a symlink are errors, never an empty list.
func TestLoadOwners(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	if r, err := LoadOwners(p); r != nil || err != nil {
		t.Fatalf("absent registry: %v %v", r, err)
	}
	write := func(s string) {
		t.Helper()
		os.Remove(p.OwnersFile())
		if err := os.WriteFile(p.OwnersFile(), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"schema_version":1,"owners":["@alexis:relux.works","@alexis:gr.relux.works"]}`)
	r, err := LoadOwners(p)
	if err != nil || len(r.Owners) != 2 {
		t.Fatalf("valid registry: %v %v", r, err)
	}
	for name, body := range map[string]string{
		"unknown field":   `{"schema_version":1,"owners":[],"admin":true}`,
		"duplicate key":   `{"schema_version":1,"owners":[],"owners":[]}`,
		"duplicate owner": `{"schema_version":1,"owners":["@a:b.c","@a:b.c"]}`,
		"localpart owner": `{"schema_version":1,"owners":["alexis"]}`,
		"schema":          `{"schema_version":2,"owners":[]}`,
		"null owners":     `{"schema_version":1,"owners":null}`,
		"trailing":        `{"schema_version":1,"owners":[]} {}`,
	} {
		write(body)
		if r, err := LoadOwners(p); err == nil {
			t.Errorf("%s accepted: %v", name, r)
		}
	}
	os.Remove(p.OwnersFile())
	target := p.OwnersFile() + ".real"
	os.WriteFile(target, []byte(`{"schema_version":1,"owners":[]}`), 0o600)
	if err := os.Symlink(target, p.OwnersFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOwners(p); err == nil {
		t.Fatal("symlinked registry accepted")
	}
}

// UpdateOwners creates the registry on first use with private mode, and a
// callback or validation failure leaves the previous bytes unchanged.
func TestUpdateOwners(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	if err := UpdateOwners(p, func(r *OwnerRegistry) error { r.Owners = append(r.Owners, "@alexis:relux.works"); return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.OwnersFile())
	if err != nil || !strings.Contains(string(before), "@alexis:relux.works") {
		t.Fatalf("registry %q %v", before, err)
	}
	if st, _ := os.Stat(p.OwnersFile()); st.Mode().Perm() != SecretFileMode {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if err := UpdateOwners(p, func(r *OwnerRegistry) error { r.Owners = append(r.Owners, "alexis"); return nil }); err == nil {
		t.Fatal("invalid owner written")
	}
	after, _ := os.ReadFile(p.OwnersFile())
	if string(after) != string(before) {
		t.Fatalf("failed update changed the registry:\n%s", after)
	}
}
