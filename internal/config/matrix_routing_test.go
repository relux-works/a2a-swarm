package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMatrixRoutingSecretIsolation(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	if err := os.WriteFile(p.EnvFile(), []byte("export A2A_MATRIX_URL='https://selected.invalid/'\nA2A_MATRIX_SERVER_NAME=Selected.invalid\nA2A_MATRIX_REGISTRATION_TOKEN=synthetic-file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	get := func(k string) string {
		switch k {
		case EnvMatrixURL:
			return "https://override.invalid/"
		case EnvServerName:
			return ""
		default:
			t.Fatalf("routing requested non-public environment key %s", k)
			return ""
		}
	}
	m, err := LoadMatrixRouting(p, get)
	if err != nil || m.URL != "https://override.invalid" || m.ServerName != "Selected.invalid" || m.RegistrationToken != "" {
		t.Fatalf("routing: %+v %v", m, err)
	}
	// The general loader's existing secret behavior remains available to its
	// authenticated callers; only the new public loader excludes it.
	ordinary, err := LoadMatrix(p, func(string) string { return "" })
	if err != nil || ordinary.RegistrationToken != "synthetic-file-secret" {
		t.Fatal("ordinary loader changed")
	}
	if err := os.Remove(p.EnvFile()); err != nil {
		t.Fatal(err)
	}
	m, err = LoadMatrixRouting(p, get)
	if err != nil || m.URL != "https://override.invalid" || m.RegistrationToken != "" {
		t.Fatal("environment-only routing")
	}
	if err := os.Mkdir(filepath.Join(p.Base, "matrix.env"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadMatrixRouting(p, get); err == nil {
		t.Fatal("unreadable routing accepted")
	}
}
