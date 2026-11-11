package darwinrelease

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type gitFixture struct {
	root, key, sourceKey, commit string
	p                            Policy
}

func fixtureCommand(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = cleanEnvironment()
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("fixture %s failed: %v %s", name, e, b)
	}
	return strings.TrimSpace(string(b))
}

// Fixture keys are generated only by isolated test execution, never read from
// an owner's or installed signer. Source and tag authority use distinct keys.
func fixtureSigner(t *testing.T, root, name string) (key, public, fingerprint string) {
	t.Helper()
	key = filepath.Join(t.TempDir(), name)
	fixtureCommand(t, root, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	pub, e := os.ReadFile(key + ".pub")
	if e != nil {
		t.Fatal(e)
	}
	f := strings.Fields(string(pub))
	blob, e := base64.StdEncoding.DecodeString(f[1])
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(blob)
	return key, f[0] + " " + f[1], "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	key, public, fingerprint := fixtureSigner(t, root, "tag-signing")
	sourceKey, sourcePublic, sourceFingerprint := fixtureSigner(t, root, "source-signing")
	fixtureCommand(t, root, "git", "init", "-q", "-b", "main")
	fixtureCommand(t, root, "git", "config", "user.name", "Release Fixture")
	fixtureCommand(t, root, "git", "config", "user.email", "fixture@example.invalid")
	fixtureCommand(t, root, "git", "config", "tag.gpgsign", "false")
	fixtureCommand(t, root, "git", "config", "gpg.format", "ssh")
	fixtureCommand(t, root, "git", "config", "user.signingkey", key)
	if e := os.Mkdir(filepath.Join(root, ".github"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, ".github/release-signers"), []byte(`release-maintainer namespaces="git" `+public+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, ".github/source-signers"), []byte(`source-maintainer namespaces="git" `+public+"\n"+`source-maintainer namespaces="git" `+sourcePublic+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	fixtureCommand(t, root, "git", "add", ".github")
	fixtureCommand(t, root, "git", "commit", "-q", "-S", "-m", "fixture")
	commit := fixtureCommand(t, root, "git", "rev-parse", "HEAD")
	fixtureCommand(t, root, "git", "tag", "-s", "v0.1.0", "-m", "fixture tag")
	return gitFixture{root: root, key: key, sourceKey: sourceKey, commit: commit,
		p: Policy{HistoryBase: commit, SignerFingerprint: fingerprint, SourceSignerFingerprints: []string{fingerprint, sourceFingerprint}}}
}

// Real isolated SSH-signed Git objects pair a valid tag with each intended
// violation. No fixture key is an owner key; no remote or real tag is changed.
func TestReleaseTagObjects(t *testing.T) {
	cases := []struct {
		name, want string
		mutate     func(*testing.T, gitFixture)
	}{
		{"valid", "", func(*testing.T, gitFixture) {}},
		{"valid-introduced-signed-commit", "", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "signed child")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "new signed tag")
		}},
		// Accepted source authority is deliberately different from tag authority.
		{"distinct-approved-source-bot-tag", "", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "-c", "user.signingkey="+f.sourceKey, "commit", "-q", "-S", "--allow-empty", "-m", "maintainer child")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "bot tag")
		}},
		{"source-only-key-cannot-tag", "tag-signer", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "-c", "user.signingkey="+f.sourceKey, "tag", "-s", "v0.1.0", "-m", "source-only tag")
		}},
		// A later approved child must not hide an untrusted earlier object.
		{"unknown-introduced-source-before-signed-head", "commit-signer", func(t *testing.T, f gitFixture) {
			unknown, _, _ := fixtureSigner(t, f.root, "unknown-source")
			fixtureCommand(t, f.root, "git", "-c", "user.signingkey="+unknown, "commit", "-q", "-S", "--allow-empty", "-m", "unknown child")
			fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "approved descendant")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "bot tag")
		}},
		{"unsigned-source-before-signed-head", "commit-signer", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "commit", "-q", "--no-gpg-sign", "--allow-empty", "-m", "unsigned ancestor")
			fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "approved descendant")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "bot tag")
		}},
		{"lightweight", "tag-annotated", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "v0.1.0")
		}},
		{"unsigned", "tag-signed", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-a", "v0.1.0", "-m", "unsigned")
		}},
		{"wrong-signer", "tag-signer", func(t *testing.T, f gitFixture) {
			other := filepath.Join(t.TempDir(), "other")
			fixtureCommand(t, f.root, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other)
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "-c", "user.signingkey="+other, "tag", "-s", "v0.1.0", "-m", "wrong signer")
		}},
		{"checkout-moved", "tag-checkout", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "next")
		}},
		{"unsigned-introduced-commit", "commit-signer", func(t *testing.T, f gitFixture) {
			fixtureCommand(t, f.root, "git", "commit", "-q", "--no-gpg-sign", "--allow-empty", "-m", "unsigned child")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "new tag")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGitFixture(t)
			c.mutate(t, f)
			_, e := verifyGit(f.root, "v0.1.0", "0.1.0", "", f.p, localGit(f.root))
			if c.want == "" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil || e.Error() != c.want {
				t.Fatalf("wanted %s, got %v", c.want, e)
			}
		})
	}
	t.Run("moved-object", func(t *testing.T) {
		f := newGitFixture(t)
		_, e := verifyGit(f.root, "v0.1.0", "0.1.0", strings.Repeat("0", 40), f.p, localGit(f.root))
		if e == nil || e.Error() != "tag-moved" {
			t.Fatal(e)
		}
	})
	t.Run("version-mismatch", func(t *testing.T) {
		f := newGitFixture(t)
		_, e := verifyGit(f.root, "v0.1.0", "0.2.0", "", f.p, localGit(f.root))
		if e == nil || e.Error() != "tag-version" {
			t.Fatal(e)
		}
	})
	t.Run("tag-name-mismatch", func(t *testing.T) {
		f := newGitFixture(t)
		oid := fixtureCommand(t, f.root, "git", "rev-parse", "refs/tags/v0.1.0")
		fixtureCommand(t, f.root, "git", "update-ref", "refs/tags/v0.2.0", oid)
		_, e := verifyGit(f.root, "v0.2.0", "0.2.0", "", f.p, localGit(f.root))
		if e == nil || e.Error() != "tag-name" {
			t.Fatal(e)
		}
	})
	t.Run("untracked-source", func(t *testing.T) {
		f := newGitFixture(t)
		if e := os.WriteFile(filepath.Join(f.root, "extra.go"), []byte("package extra"), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := verifyGit(f.root, "v0.1.0", "0.1.0", "", f.p, localGit(f.root))
		if e == nil || e.Error() != "source-dirty" {
			t.Fatal(e)
		}
	})

}

// Reviewed generic principals and exact source keys must not broaden through
// wildcard/namespace/extra-key file edits. Actual signatures are tested above.
func TestReleaseSourceSignerFileControls(t *testing.T) {
	f := newGitFixture(t)
	path := filepath.Join(f.root, ".github/source-signers")
	valid, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e := checkSignerEntries(path, "source-maintainer", f.p.SourceSignerFingerprints); e != nil {
		t.Fatal(e)
	}
	for _, plant := range []string{
		strings.Replace(string(valid), "source-maintainer", "*", 1),
		strings.Replace(string(valid), `namespaces="git"`, `namespaces="*"`, 1),
		string(valid) + strings.Split(string(valid), "\n")[0] + "\n",
		strings.Split(string(valid), "\n")[0] + "\n",
		strings.Replace(string(valid), "ssh-ed25519", "ssh-rsa", 1),
	} {
		if e := os.WriteFile(path, []byte(plant), 0600); e != nil {
			t.Fatal(e)
		}
		if e := checkSignerEntries(path, "source-maintainer", f.p.SourceSignerFingerprints); e == nil || e.Error() != "signer-policy" {
			t.Fatal("source trust plant not specifically rejected", e)
		}
	}
	if e := os.WriteFile(path, valid, 0600); e != nil {
		t.Fatal(e)
	}
	if e := checkSignerEntries(path, "source-maintainer", f.p.SourceSignerFingerprints); e != nil {
		t.Fatal(e)
	}
}
