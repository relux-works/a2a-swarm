package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialScrubCallbackFailurePreservesOriginal(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	c := Credentials{UserID: "@a:test", AccessToken: "synthetic-token", Password: "legacy-password", DeviceID: "D", Homeserver: "https://example.invalid"}
	writeTestCreds(t, p, "a", c)
	before, err := os.ReadFile(p.AgentFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("interrupted before verified readback")
	err = RewriteCredentialMetadata(p, "a", false, func(c *Credentials) error { c.AccessToken, c.Password = "", ""; return fault })
	if !errors.Is(err, fault) {
		t.Fatal("callback refusal lost")
	}
	after, err := os.ReadFile(p.AgentFile("a"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("callback refusal changed file")
	}
}

func TestCredentialAtomicRenameFailurePreservesRecovery(t *testing.T) {
	// A destination becoming a directory forces rename failure after temp write
	// and fsync. The recoverable old file is retained, with no partial scrub.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	recovery := filepath.Join(dir, "original.json")
	original := []byte(`{"user_id":"@a:test","access_token":"synthetic-token","password":"legacy"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, recovery); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretAtomic(path, "a", []byte(`{"user_id":"@a:test"}`)); err == nil {
		t.Fatal("rename failure admitted")
	}
	after, err := os.ReadFile(recovery)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("recovery file changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("temporary scrub left behind")
	}
}
