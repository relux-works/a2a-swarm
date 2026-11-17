//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/envelopeobserve"
)

func hostRosterV2Fixture(t *testing.T) (string, hostRosterTestProtection) {
	t.Helper()
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
	return filepath.Join(dir, "roster.json"), p
}

// TestHostRosterV2Build proves explicit version 2 builds both shared-carrier bindings while default version 1 bytes and refusal preservation remain unchanged.
func TestHostRosterV2Build(t *testing.T) {
	e := newEnv(t)
	key := hostRosterTestKey(t)
	out, p := hostRosterV2Fixture(t)
	write := func(name, body string) string {
		path := filepath.Join(e.dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	alpha := `{"author_key":"` + key + `","author":"alpha","publishers":["@bridge:example.invalid"],"source":"public record"}`
	beta := `{"author_key":"` + hostRosterBIPKey + `","author":"beta","publishers":["@bridge:example.invalid"],"source":"public record"}`
	input := write("input.json", `{"version":"a2a-roster-input/2","entries":[`+alpha+`,`+beta+`]}`)
	duplicate := write("duplicate.json", `{"version":"a2a-roster-input/2","entries":[`+alpha+`,`+alpha+`]}`)
	legacy := write("legacy.json", `{"version":"a2a-roster-input/1","entries":[{"mxid":"@author:example.invalid","author_key":"`+key+`","source":"console"}]}`)
	if os.Geteuid() != 0 {
		e.run("host", "roster", "build", "--version", "2", "--from", input, "--out", out).failsWith(t, ExitError, "host roster refused: not_root")
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("non-root version 2 build created output")
		}
	}
	e.configure = func(a *App) { a.rosterProtection = p }
	if got := e.run("host", "roster", "build", "--version", "2", "--from", input, "--out", out).ok(t).stdout; got != "roster "+out+": 2 authors\n" {
		t.Fatalf("version 2 success output %q", got)
	}
	published, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"authors":[{"author":"beta","author_key":"` + hostRosterBIPKey + `","publishers":["@bridge:example.invalid"]},{"author":"alpha","author_key":"` + key + `","publishers":["@bridge:example.invalid"]}],"facts":{},"version":"a2a-envelope-roster/2"}`
	if string(published) != want {
		t.Fatal("CLI did not publish canonical version 2 bytes")
	}
	for _, args := range [][]string{
		{"--from", input, "--out", out},
		{"--version", "1", "--from", input, "--out", out},
		{"--version", "2", "--from", legacy, "--out", out},
	} {
		e.run(append([]string{"host", "roster", "build"}, args...)...).failsWith(t, ExitError, "host roster refused: input_version")
	}
	e.run("host", "roster", "build", "--version", "2", "--from", duplicate, "--out", out).failsWith(t, ExitError, "host roster refused: key_duplicate")
	for _, version := range []string{"0", "3"} {
		e.run("host", "roster", "build", "--version", version, "--from", input, "--out", out).failsWith(t, ExitUsage, "usage: a2a host roster build")
	}
	after, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(after, published) {
		t.Fatalf("CLI refusal changed output: %v", err)
	}
	withFacts := strings.Replace(want, `"facts":{}`, `"facts":{"audience":"installation"}`, 1)
	if err := os.WriteFile(out, []byte(withFacts), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run("host", "roster", "build", "--version", "2", "--from", input, "--out", out).failsWith(t, ExitError, "host roster refused: existing_facts")
	if after, err := os.ReadFile(out); err != nil || string(after) != withFacts {
		t.Fatalf("version 2 flag bypassed existing-facts preservation: %v", err)
	}
	if err := os.WriteFile(out, published, 0o644); err != nil {
		t.Fatal(err)
	}
	legacyWant := `{"authors":{"@author:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	for _, args := range [][]string{{}, {"--version", "1"}} {
		args = append(args, "--from", legacy, "--out", out)
		e.run(append([]string{"host", "roster", "build"}, args...)...).ok(t)
		if data, err := os.ReadFile(out); err != nil || string(data) != legacyWant {
			t.Fatalf("default/explicit version 1 bytes changed: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil || len(entries) != 1 || entries[0].Name() != "roster.json" {
		t.Fatalf("CLI left temporary entries: %v", err)
	}
}

// TestHostRosterV2Show proves the unchanged three-column view lists both keys behind one carrier and retains the existing unavailable check behavior.
func TestHostRosterV2Show(t *testing.T) {
	e := newEnv(t)
	key := hostRosterTestKey(t)
	raw := `{"version":"a2a-envelope-roster/2","authors":[{"author_key":"` + key + `","author":"alpha","publishers":["@bridge:example.invalid"]},{"author_key":"` + hostRosterBIPKey + `","author":"beta","publishers":["@bridge:example.invalid"]}],"facts":{}}`
	o, err := envelopeobserve.ParseRoster([]byte(raw))
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
			return o, nil
		}
	}
	want := "@bridge:example.invalid\tbip340\t" + hostRosterFingerprint(hostRosterBIPKey) + "\n" +
		"@bridge:example.invalid\ted25519\t" + hostRosterFingerprint(key) + "\n"
	for _, args := range [][]string{{}, {"--check"}} {
		if got := e.run(append([]string{"host", "roster", "show"}, args...)...).ok(t).stdout; got != want {
			t.Fatalf("version 2 three-column view %q, want %q", got, want)
		}
	}
	available = false
	e.run("host", "roster", "show", "--check").failsWith(t, ExitError, "host roster unavailable: absent, unprotected or malformed")
	if got := e.run("host", "roster", "show").ok(t).stdout; got != "roster unavailable\n" {
		t.Fatalf("unavailable view %q", got)
	}
}
