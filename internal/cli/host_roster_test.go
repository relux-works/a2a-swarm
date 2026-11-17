//go:build unix

package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/envelopeobserve"
)

// A public BIP-340 test key from the pinned fixtures of the reader package.
const hostRosterBIPKey = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"

// hostRosterTestKey returns a fresh public key identifier; the private half
// is dropped here.
func hostRosterTestKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(public)
}

func hostRosterFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// hostRosterTestProtection stands in for ownership only: the caller's files
// count as root-owned and the directories above the fixture are accepted as
// they are. Modes and file types below the fixture are real.
type hostRosterTestProtection struct {
	root  bool
	above []os.FileInfo
}

func (p hostRosterTestProtection) CallerMayPublish() bool { return p.root }

func (p hostRosterTestProtection) Protected(st os.FileInfo, directory bool) bool {
	for _, ancestor := range p.above {
		if os.SameFile(st, ancestor) {
			return directory && st.IsDir()
		}
	}
	return st.Mode().Perm()&0o022 == 0 &&
		(directory && st.IsDir() || !directory && st.Mode().IsRegular())
}

func selectHostRoster(t *testing.T, e *env, path string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"roster_path": path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "envelope-observe.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHostRosterShow proves show prints only the three reader columns and --check rejects absent, unprotected and malformed rosters.
// Loaded and malformed rows use the loader seam; absent and caller-owned files use the real reader.
func TestHostRosterShow(t *testing.T) {
	e := newEnv(t)
	k1 := hostRosterTestKey(t)
	observer, err := envelopeobserve.ParseRoster([]byte(`{"authors":{"@beta:example.invalid":"` + hostRosterBIPKey + `","@alpha:example.invalid":"` + k1 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`))
	if err != nil {
		t.Fatal(err)
	}
	selectHostRoster(t, e, "/protected/roster.json")
	available := true
	e.configure = func(a *App) {
		a.loadEnvelopeRoster = func(path string) (*envelopeobserve.Observer, error) {
			if path != "/protected/roster.json" || !available {
				return nil, envelopeobserve.ErrRoster
			}
			return observer, nil
		}
	}
	want := "@alpha:example.invalid\ted25519\t" + hostRosterFingerprint(k1) + "\n" +
		"@beta:example.invalid\tbip340\t" + hostRosterFingerprint(hostRosterBIPKey) + "\n"
	for _, args := range [][]string{{"host", "roster", "show"}, {"host", "roster", "show", "--check"}} {
		if got := e.run(args...).ok(t).stdout; got != want {
			t.Fatalf("%v printed %q, want %q", args, got, want)
		}
	}
	available = false
	if got := e.run("host", "roster", "show").ok(t).stdout; got != "roster unavailable\n" {
		t.Fatalf("show of an unavailable roster printed %q", got)
	}
	if r := e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable: absent, unprotected or malformed"); r.stdout != "" {
		t.Fatalf("a failed check printed %q", r.stdout)
	}

	for _, raw := range []string{`{`, `{}`} {
		e.configure = func(a *App) {
			a.loadEnvelopeRoster = func(string) (*envelopeobserve.Observer, error) {
				return envelopeobserve.ParseRoster([]byte(raw))
			}
		}
		e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable")
		if got := e.run("host", "roster", "show").ok(t).stdout; got != "roster unavailable\n" {
			t.Fatalf("show of a malformed roster printed %q", got)
		}
	}

	e.configure = nil
	selectHostRoster(t, e, filepath.Join(e.dir, "absent.json"))
	e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable")
	owned := filepath.Join(e.dir, "caller-owned.json")
	if err := os.WriteFile(owned, []byte(`{"authors":{"@alpha:example.invalid":"`+k1+`"},"facts":{},"version":"a2a-envelope-roster/1"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	selectHostRoster(t, e, owned)
	e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable")
	if err := os.Remove(filepath.Join(e.dir, "envelope-observe.json")); err != nil {
		t.Fatal(err)
	}
	e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable")

	e.run("host", "roster").failsWith(t, ExitUsage, "usage: a2a host roster build")
	e.run("host", "roster", "show", "extra").failsWith(t, ExitUsage, "usage: a2a host roster build")
	e.run("host", "roster", "publish").failsWith(t, ExitUsage, "usage: a2a host roster build")
}

// TestHostRosterPublicationErrors proves CLI error handling exits non-zero and says new bytes are in place for both post-rename reports beside a success control.
func TestHostRosterPublicationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		exit                int
	}{
		{"control", "", "", ExitOK},
		{"unsynced", "published_unsynced", "a2a: host roster published: published_unsynced: the new roster is at the final path; directory sync failed\n", ExitError},
		{"unprotected", "published_unprotected", "a2a: host roster published: published_unprotected: the new roster is at the final path; the reader will treat it as unavailable until the operator fixes the protection\n", ExitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp()
			var stdout, stderr bytes.Buffer
			a.Stdout, a.Stderr = &stdout, &stderr
			var err error
			if tc.code != "" {
				err = &envelopeobserve.RosterPublicationError{Code: tc.code}
			}
			if exit := a.fail(err); exit != tc.exit {
				t.Fatalf("exit %d, want %d", exit, tc.exit)
			}
			if stdout.Len() != 0 || stderr.String() != tc.message {
				t.Fatalf("stdout %q, stderr %q; want only %q", stdout.String(), stderr.String(), tc.message)
			}
		})
	}
}

// TestHostRosterBuild proves real non-root refusal, empty-facts replacement through the ownership seam and byte preservation on later refusals.
func TestHostRosterBuild(t *testing.T) {
	e := newEnv(t)
	k1, k2 := hostRosterTestKey(t), hostRosterTestKey(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := hostRosterTestProtection{root: true}
	for current := base; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			t.Fatal(err)
		}
		p.above = append(p.above, st)
		if current == filepath.Dir(current) {
			break
		}
	}
	dir := filepath.Join(base, "host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "roster.json")
	write := func(name, body string) string {
		path := filepath.Join(e.dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	alpha := `{"mxid":"@alpha:example.invalid","author_key":"` + k1 + `","source":"console"}`
	beta := `{"mxid":"@beta:example.invalid","author_key":"` + k2 + `","source":"console"}`
	valid := write("input.json", `{"version":"a2a-roster-input/1","entries":[`+beta+`,`+alpha+`]}`)
	duplicate := write("duplicate.json", `{"version":"a2a-roster-input/1","entries":[`+beta+`,`+alpha+`,`+alpha+`]}`)

	if os.Geteuid() != 0 {
		e.run("host", "roster", "build", "--from", valid, "--out", out).failsWith(t, ExitError, "host roster refused: not_root")
		if _, err := os.Lstat(out); !os.IsNotExist(err) {
			t.Fatal("a non-root caller created the roster")
		}
	}

	previous := `{"authors":{"@previous:example.invalid":"` + k1 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.rosterProtection = p }
	if got := e.run("host", "roster", "build", "--from", valid, "--out", out).ok(t).stdout; got != "roster "+out+": 2 authors\n" {
		t.Fatalf("build printed %q", got)
	}
	want := `{"authors":{"@alpha:example.invalid":"` + k1 + `","@beta:example.invalid":"` + k2 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	published := func() string {
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if got := published(); got != want {
		t.Fatalf("published %q, want %q", got, want)
	}

	e.run("host", "roster", "build", "--from", duplicate, "--out", out).failsWith(t, ExitError, "host roster refused: mxid_duplicate")
	e.run("host", "roster", "build", "--from", valid, "--out", "roster.json").failsWith(t, ExitError, "host roster refused: out_path")
	e.run("host", "roster", "build", "--from", filepath.Join(e.dir, "absent.json"), "--out", out).failsWith(t, ExitError, "host roster refused: input_unreadable")
	e.run("host", "roster", "build", "--from", valid).failsWith(t, ExitUsage, "usage: a2a host roster build")
	e.run("host", "roster", "build", "--out", out).failsWith(t, ExitUsage, "usage: a2a host roster build")
	e.run("host", "roster", "build", "--from", valid, "--out", out, "extra").failsWith(t, ExitUsage, "usage: a2a host roster build")
	if got := published(); got != want {
		t.Fatalf("a refused build changed the roster: %q", got)
	}
	withFacts := strings.Replace(want, `"facts":{}`, `"facts":{"audience":"installation"}`, 1)
	if err := os.WriteFile(out, []byte(withFacts), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run("host", "roster", "build", "--from", valid, "--out", out).failsWith(t, ExitError, "host roster refused: existing_facts")
	if got := published(); got != withFacts {
		t.Fatal("existing_facts changed the previous roster")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "roster.json" {
		t.Fatalf("directory after refusals: %v, %v", entries, err)
	}
}
