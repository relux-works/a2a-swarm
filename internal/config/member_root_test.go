package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func memberConfigFixture(t *testing.T) (*MemberRoot, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture parent unavailable")
	}
	source := filepath.Join(parent, "source")
	if os.Mkdir(source, 0700) != nil {
		t.Fatal("fixture source unavailable")
	}
	c := Connection{ID: "member-test", URL: "https://matrix.test.invalid", ServerName: "test.invalid", Store: CredentialStore{Backend: "keychain", AccessTokenService: "test.access", PasswordService: "test.password"}}
	m, err := ReserveMemberRoot(context.Background(), filepath.Join(parent, "member"), source, "chosen", "child", c)
	if err != nil {
		t.Fatal("valid root reservation failed")
	}
	t.Cleanup(m.Close)
	return m, source
}

func memberConfigCredentials(m *MemberRoot) Credentials {
	return Credentials{UserID: m.Binding.UserID, Homeserver: m.Connection.URL, AccessToken: "synthetic-member-access", ConnectionBinding: &m.Binding}
}

// Guarded publication is exclusive and binding-checked. An ordinary consumer
// refuses a pending or corrupted completion journal even with published routing
// and credentials; completing the same bound transaction is the valid control.
func TestMemberRootBindingAndCompletion(t *testing.T) {
	m, _ := memberConfigFixture(t)
	creds := memberConfigCredentials(m)
	wrong := creds
	wrong.UserID = "@other:test.invalid"
	if m.Guard().PublishBoundCredentials(wrong) == nil {
		t.Fatal("wrong member binding published")
	}
	if m.Guard().PublishBoundCredentials(creds) != nil || m.PublishRegistry() != nil || m.Verify() != nil {
		t.Fatal("valid bound member publication failed")
	}
	if m.Guard().PublishBoundCredentials(creds) == nil || m.PublishRegistry() == nil {
		t.Fatal("exclusive publication overwrote an existing file")
	}
	if CheckMemberRegistration(m.Paths, m.Binding) == nil {
		t.Fatal("pending journal admitted ordinary member use")
	}
	journal := func(stage string) []byte {
		b, e := json.Marshal(struct {
			Binding ConnectionBinding `json:"binding"`
			Stage   string            `json:"stage"`
		}{m.Binding, stage})
		if e != nil {
			t.Fatal("fixture journal encoding")
		}
		return b
	}
	if m.WriteJournal(journal("complete")) != nil || CheckMemberRegistration(m.Paths, m.Binding) != nil {
		t.Fatal("complete bound member root refused")
	}
	other, _ := m.Connection.Binding("other", false)
	if CheckMemberRegistration(m.Paths, other) == nil {
		t.Fatal("completion evidence admitted a different member")
	}
	if m.WriteJournal([]byte(`{"stage":"complete"}`)) != nil || CheckMemberRegistration(m.Paths, m.Binding) == nil {
		t.Fatal("incomplete binding journal admitted member use")
	}
}

// A substituted credential directory, imported state/migration subtree or
// existing credential file is never followed or overwritten. The untouched
// binding/completion test above supplies valid publication controls.
func TestMemberRootSubstitutionAndState(t *testing.T) {
	for _, scenario := range []string{"metadata-symlink", "imported-state", "existing-credential"} {
		t.Run(scenario, func(t *testing.T) {
			m, source := memberConfigFixture(t)
			sentinel := filepath.Join(source, "keep")
			if os.WriteFile(sentinel, []byte("keep"), 0600) != nil {
				t.Fatal("fixture sentinel")
			}
			switch scenario {
			case "metadata-symlink":
				path := filepath.Dir(m.Paths.AgentFile("child"))
				if os.Rename(path, path+"-original") != nil || os.Symlink(source, path) != nil {
					t.Fatal("metadata substitution plant")
				}
			case "imported-state":
				if os.Mkdir(filepath.Join(m.Paths.Base, "state"), 0700) != nil {
					t.Fatal("state import plant")
				}
			case "existing-credential":
				if os.WriteFile(m.Paths.AgentFile("child"), []byte("keep"), 0600) != nil {
					t.Fatal("credential plant")
				}
			}
			if m.Validate() == nil || m.Guard().PublishBoundCredentials(memberConfigCredentials(m)) == nil {
				t.Fatal("substitution/import bypassed member guard")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "keep" {
				t.Fatal("substitution changed source contents")
			}
			if _, err := os.Lstat(filepath.Join(source, "child.json")); !os.IsNotExist(err) {
				t.Fatal("member secret followed substituted directory")
			}
			if scenario == "existing-credential" {
				data, err := os.ReadFile(m.Paths.AgentFile("child"))
				if err != nil || string(data) != "keep" {
					t.Fatal("existing credentials overwritten")
				}
			}
		})
	}
}
