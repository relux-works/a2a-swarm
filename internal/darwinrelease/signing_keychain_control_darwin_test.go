//go:build darwin && arm64 && cgo

package darwinrelease

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// signingControlDirEnv names a directory prepared by
// .scripts/signing-keychain-fixture.sh: a throwaway self-signed code-signing
// identity (identity.p12, identity.pass). It is never a release credential.
// Without it the control is skipped.
const signingControlDirEnv = "A2A_SIGNING_KEYCHAIN_CONTROL_DIR"

// errorClass maps codesign stderr to a bounded vocabulary. Raw stderr is
// never logged: only the class and the exit status leave the test.
func errorClass(stderr string) string {
	s := strings.ToLower(stderr)
	for _, c := range []struct{ needle, class string }{
		{"errsecinternalcomponent", "internal-component"},
		{"user interaction is not allowed", "interaction-not-allowed"},
		{"unable to build chain", "chain-build"},
		{"not trusted", "not-trusted"},
		{"timestamp", "timestamp"},
		{"no identity found", "identity-not-found"},
		{"could not be found", "identity-not-found"},
		{"cannot be used for signing", "identity-unusable"},
		{"test-requirement", "requirement-unsatisfied"},
		{"does not satisfy", "requirement-unsatisfied"},
	} {
		if strings.Contains(s, c.needle) {
			return c.class
		}
	}
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return "other"
}

// step is one executed command. The zero value is UNRUN: a command that was
// never started is never reported or admitted as success.
type step struct {
	ran     bool
	exit    int
	class   string
	timeout bool
}

func (s step) ok() bool { return s.ran && s.exit == 0 && !s.timeout }

func (s step) String() string {
	if !s.ran {
		return "UNRUN"
	}
	if s.timeout {
		return "timeout"
	}
	return "exit=" + strconv.Itoa(s.exit) + " class=" + s.class
}

func run(t *testing.T, limit time.Duration, name string, args ...string) step {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = cleanEnvironment()
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return step{ran: true, timeout: true, class: "timeout"}
	}
	out := step{ran: true, class: errorClass(stderr.String())}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.exit = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s did not start: %v", filepath.Base(name), err)
	}
	return out
}

// runStdout is run with a bounded capture of stdout (at most 64 KiB), for
// diagnostics that are parsed into closed classes and never logged raw.
func runStdout(t *testing.T, limit time.Duration, name string, args ...string) (step, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = cleanEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return step{ran: true, timeout: true, class: "timeout"}, ""
	}
	out := step{ran: true, class: errorClass(stderr.String())}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.exit = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s did not start: %v", filepath.Base(name), err)
	}
	text := stdout.String()
	if len(text) > 64<<10 {
		text = text[:64<<10]
	}
	return out, text
}

// identityClass reports how `security find-identity -p codesigning` sees the
// fixture identity in the fixture keychain: "valid" when the -v listing names
// its selector, "listed-invalid" when only the full listing does, otherwise
// "absent". Raw listing text never leaves the test.
func identityClass(t *testing.T, keychain, selector string) string {
	t.Helper()
	sel := strings.ToUpper(selector)
	valid, validText := runStdout(t, 30*time.Second, "/usr/bin/security", "find-identity", "-v", "-p", "codesigning", keychain)
	all, allText := runStdout(t, 30*time.Second, "/usr/bin/security", "find-identity", "-p", "codesigning", keychain)
	switch {
	case !valid.ok() || !all.ok():
		return "unknown"
	case strings.Contains(strings.ToUpper(validText), sel):
		return "valid"
	case strings.Contains(strings.ToUpper(allText), sel):
		return "listed-invalid"
	}
	return "absent"
}

// userSearchList returns the current user keychain search list.
func userSearchList(t *testing.T) []string {
	t.Helper()
	s, text := runStdout(t, 30*time.Second, "/usr/bin/security", "list-keychains", "-d", "user")
	if !s.ok() {
		t.Fatalf("SETUP: list-keychains: %s", s)
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if p := strings.Trim(strings.TrimSpace(line), `"`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setUserSearchList(t *testing.T, list []string) step {
	t.Helper()
	return run(t, 30*time.Second, "/usr/bin/security", append([]string{"list-keychains", "-d", "user", "-s"}, list...)...)
}

type arm struct {
	identity     string
	sign, verify step
}

func (a arm) String() string {
	return "identity=" + a.identity + " sign=" + a.sign.String() + " verify=" + a.verify.String()
}

// signWithFixture runs one arm on fresh state: a new keychain created by the
// release import path, a fresh copy of the same fixture executable, then
// codesign with the release argv (signArgs) and a strict verification.
// searchList puts ONLY this fixture keychain in front of the user search
// list for the arm and restores the original list on every exit. partition
// widens the imported key's partition list to apple-tool:,apple:.
// timestamp=false swaps only --timestamp for --timestamp=none.
func signWithFixture(t *testing.T, values map[string]string, binary string, searchList, partition, timestamp bool) arm {
	t.Helper()
	// Security reports the keychain's resolved path, and the import refuses a
	// different directory; resolve the temp dir's symlinks (/var -> /private/var).
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(random)
	signer, err := openSigningKeychainWithPassword(filepath.Join(work, "control.keychain-db"), values, password)
	if err != nil {
		t.Fatalf("SETUP: fixture import through the release path failed: %v", err)
	}
	defer func() {
		if err := signer.close(); err != nil {
			t.Errorf("CLEANUP: fixture keychain: %v", err)
		}
	}()
	if searchList {
		original := userSearchList(t)
		if s := setUserSearchList(t, append([]string{signer.path}, original...)); !s.ok() {
			t.Fatalf("SETUP: add the fixture keychain to the user search list: %s", s)
		}
		defer func() {
			if s := setUserSearchList(t, original); !s.ok() {
				t.Errorf("CLEANUP: restore the user search list: %s", s)
			}
		}()
	}
	if partition {
		// Fixture only: the security tool takes this throwaway keychain's
		// password in argv. The password is random per arm and never logged.
		if s := run(t, 60*time.Second, "/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", password, signer.path); !s.ok() {
			t.Fatalf("SETUP: set-key-partition-list on the fixture keychain: %s", s)
		}
	}
	target := filepath.Join(work, "a2a")
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, raw, 0755); err != nil {
		t.Fatal(err)
	}
	out := arm{identity: identityClass(t, signer.path, signer.selector)}
	// The self-signed fixture cannot satisfy the release Apple-anchored
	// requirement, so the arms embed and verify a fixture requirement bound
	// to the fixture leaf. The release requirement is not changed.
	expr := `identifier "works.relux.a2a" and certificate leaf = H"` + signer.selector + `"`
	args := signArgs(signer.selector, signer.path, "a2a", expr, target)
	if !timestamp {
		for i, a := range args {
			if a == "--timestamp" {
				args[i] = "--timestamp=none"
			}
		}
	}
	out.sign = run(t, 90*time.Second, "/usr/bin/codesign", args...)
	if out.sign.ok() {
		out.verify = run(t, 60*time.Second, "/usr/bin/codesign", "--verify", "--strict", "--test-requirement", "="+expr, target)
	}
	return out
}

// Discriminates two hypotheses for the first hosted binary-sign failure on a
// throwaway self-signed identity, with no trust-settings change: H6, the
// release keychain is not in the user search list, so codesign cannot find
// the identity; and H1, the imported key's partition list does not admit
// codesign. Every arm imports through the release import path and signs one
// fixture executable with the release codesign argv; arms differ only in
// search-list presence and partition widening. The present+widened arm is the
// positive control; each component's verdict needs it to sign and verify.
// The timestamped pair is informational and does not qualify the Apple
// timestamp service. Limits: a self-signed, untrusted fixture with a fixture
// requirement, not a Developer ID chain; it says nothing about the release
// certificate, its chain or validity (H2, H3), and alone does not prove the
// original incident had either cause.
func TestSigningKeychainAccessControl(t *testing.T) {
	dir := os.Getenv(signingControlDirEnv)
	if dir == "" {
		t.Skip(signingControlDirEnv + " not set: no fixture identity")
	}
	p12, err := os.ReadFile(filepath.Join(dir, "identity.p12"))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := os.ReadFile(filepath.Join(dir, "identity.pass"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"MACOS_SIGN_P12":          base64.StdEncoding.EncodeToString(p12),
		"MACOS_SIGN_P12_PASSWORD": strings.TrimSpace(string(pass)),
	}
	src := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "fixture")
	if s := run(t, 120*time.Second, "/usr/bin/xcrun", "clang", "-arch", "arm64", "-o", binary, src); !s.ok() {
		t.Fatalf("SETUP: fixture executable: %s", s)
	}
	before := userSearchList(t)
	t.Logf("SIGNING-CONTROL search-list before: %d entries", len(before))

	results := map[string]arm{}
	for _, c := range []struct {
		name                             string
		searchList, partition, timestamp bool
	}{
		{"absent/unchanged", false, false, false},
		{"absent/widened", false, true, false},
		{"present/unchanged", true, false, false},
		{"present/widened", true, true, false},
		{"present/unchanged/timestamp", true, false, true},
		{"present/widened/timestamp", true, true, true},
	} {
		r := signWithFixture(t, values, binary, c.searchList, c.partition, c.timestamp)
		results[c.name] = r
		t.Logf("SIGNING-CONTROL arm=%s %s", c.name, r)
	}
	after := userSearchList(t)
	restored := strings.Join(after, "\n") == strings.Join(before, "\n")
	t.Logf("SIGNING-CONTROL search-list after: %d entries, restored=%v", len(after), restored)
	if !restored {
		t.Errorf("CLEANUP: user search list changed: %d entries before, %d after", len(before), len(after))
	}

	h6, h1, detail := matrixVerdict(results["absent/widened"], results["present/unchanged"], results["present/widened"])
	t.Logf("SIGNING-CONTROL VERDICT H6=%s H1=%s: %s", h6, h1, detail)
	if h6 == "INCONCLUSIVE" || h1 == "INCONCLUSIVE" {
		t.Fatalf("SIGNING-CONTROL INCONCLUSIVE: H6=%s H1=%s", h6, h1)
	}
}

// aclClass reports whether a refusal is of the access-control class: the
// headless key-use refusal that a missing partition entry produces.
func aclClass(class string) bool {
	return class == "internal-component" || class == "interaction-not-allowed"
}

func signedAndVerified(a arm) bool { return a.sign.ok() && a.verify.ok() }

func refusedAs(a arm, admit func(string) bool) bool {
	return a.sign.ran && !a.sign.timeout && a.sign.exit != 0 && admit(a.sign.class)
}

// matrixVerdict decides both components on the untimestamped arms. The
// positive control (present+widened) must sign and verify, or both components
// are INCONCLUSIVE. H6 compares absent+widened with the control: REPRODUCED
// if it is refused as identity-not-found, NOT_REPRODUCED if it signs and
// verifies. H1 compares present+unchanged with the control: REPRODUCED if it
// is refused with an access-control class, NOT_REPRODUCED if it signs and
// verifies. Anything else, including a timeout, an UNRUN step or a sign
// without verification, is INCONCLUSIVE for that component.
func matrixVerdict(absentWidened, presentUnchanged, presentWidened arm) (string, string, string) {
	detail := "absent/widened " + absentWidened.String() + "; present/unchanged " + presentUnchanged.String() + "; present/widened " + presentWidened.String()
	if !signedAndVerified(presentWidened) {
		return "INCONCLUSIVE", "INCONCLUSIVE", "positive control failed; " + detail
	}
	component := func(a arm, admit func(string) bool) string {
		switch {
		case signedAndVerified(a):
			return "NOT_REPRODUCED"
		case refusedAs(a, admit):
			return "REPRODUCED"
		}
		return "INCONCLUSIVE"
	}
	notFound := func(c string) bool { return c == "identity-not-found" }
	return component(absentWidened, notFound), component(presentUnchanged, aclClass), detail
}

// Proves the bounded error vocabulary: known codesign failures map to fixed
// classes, empty stderr is "none", and unrecognised text never passes through.
// Limit: the strings are representative codesign messages, not a complete set.
func TestSigningControlErrorClass(t *testing.T) {
	for in, want := range map[string]string{
		"/x/a2a: errSecInternalComponent\n":                             "internal-component",
		"Warning: unable to build chain to self-signed root":            "chain-build",
		"The timestamp service is not available.":                       "timestamp",
		"error: The specified item could not be found in the keychain.": "identity-not-found",
		"":                 "none",
		"secret-ish words": "other",
	} {
		if got := errorClass(in); got != want {
			t.Fatalf("errorClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// Proves the matrix verdict rule on synthetic arm results, independently of a
// run. Each component needs the positive control (present+widened) to sign and
// verify. H6 is REPRODUCED only for an identity-not-found refusal of
// absent+widened, and H1 only for an access-control refusal of
// present+unchanged; a signed and verified arm is NOT_REPRODUCED. A refusal of
// another class, a timeout, an UNRUN step and a sign without verification are
// INCONCLUSIVE, and a failed positive control makes both INCONCLUSIVE.
func TestSigningControlMatrixVerdict(t *testing.T) {
	good := step{ran: true, class: "none"}
	refused := func(class string) step { return step{ran: true, exit: 1, class: class} }
	pass := arm{sign: good, verify: good}
	notFound := arm{sign: refused("identity-not-found")}
	acl := arm{sign: refused("internal-component")}
	for _, c := range []struct {
		name                                            string
		absentWidened, presentUnchanged, presentWidened arm
		h6, h1                                          string
	}{
		{"H6 only", notFound, pass, pass, "REPRODUCED", "NOT_REPRODUCED"},
		{"H1 only", pass, acl, pass, "NOT_REPRODUCED", "REPRODUCED"},
		{"both", notFound, acl, pass, "REPRODUCED", "REPRODUCED"},
		{"neither", pass, pass, pass, "NOT_REPRODUCED", "NOT_REPRODUCED"},
		{"H1 interaction class", pass, arm{sign: refused("interaction-not-allowed")}, pass, "NOT_REPRODUCED", "REPRODUCED"},
		{"positive control refused", notFound, acl, notFound, "INCONCLUSIVE", "INCONCLUSIVE"},
		{"positive control verify fails", notFound, acl, arm{sign: good, verify: refused("requirement-unsatisfied")}, "INCONCLUSIVE", "INCONCLUSIVE"},
		{"positive control verify UNRUN", notFound, acl, arm{sign: good}, "INCONCLUSIVE", "INCONCLUSIVE"},
		{"positive control UNRUN", notFound, acl, arm{}, "INCONCLUSIVE", "INCONCLUSIVE"},
		{"H6 wrong class (ACL)", acl, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H6 wrong class (chain)", arm{sign: refused("chain-build")}, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H1 wrong class (not-found)", pass, notFound, pass, "NOT_REPRODUCED", "INCONCLUSIVE"},
		{"H1 wrong class (other)", pass, arm{sign: refused("other")}, pass, "NOT_REPRODUCED", "INCONCLUSIVE"},
		{"H6 timeout", arm{sign: step{ran: true, timeout: true, class: "timeout"}}, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H1 UNRUN with ACL class", pass, arm{sign: step{exit: 1, class: "internal-component"}}, pass, "NOT_REPRODUCED", "INCONCLUSIVE"},
		{"H6 signs, verify fails", arm{sign: good, verify: refused("other")}, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H1 signs, verify UNRUN", pass, arm{sign: good}, pass, "NOT_REPRODUCED", "INCONCLUSIVE"},
	} {
		h6, h1, _ := matrixVerdict(c.absentWidened, c.presentUnchanged, c.presentWidened)
		if h6 != c.h6 || h1 != c.h1 {
			t.Fatalf("%s: H6=%s H1=%s, want H6=%s H1=%s", c.name, h6, h1, c.h6, c.h1)
		}
	}
}

// Proves an UNRUN step never reads as success: it is not ok and prints UNRUN,
// beside an executed exit 0 that is ok.
func TestSigningControlUnrunStep(t *testing.T) {
	if (step{}).ok() || (step{}).String() != "UNRUN" {
		t.Fatalf("zero step = ok:%v %q, want not ok and UNRUN", (step{}).ok(), step{})
	}
	if ran := (step{ran: true, class: "none"}); !ran.ok() || ran.String() != "exit=0 class=none" {
		t.Fatalf("executed exit 0 = ok:%v %q", ran.ok(), ran)
	}
}
