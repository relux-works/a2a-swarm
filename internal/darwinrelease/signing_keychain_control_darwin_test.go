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
	"slices"
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

// stdoutCap bounds captured stdout. Output beyond it is never truncated into a
// "complete" result: the step is refused as output-overflow.
const stdoutCap = 64 << 10

// runStdout is run with a bounded capture of stdout, for diagnostics that are
// parsed into closed classes and never logged raw. Output over stdoutCap
// makes the step not ok (class output-overflow) and returns no text.
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
	if stdout.Len() > stdoutCap {
		return step{ran: true, exit: -1, class: "output-overflow"}, ""
	}
	return out, stdout.String()
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

// listOps reads and replaces the user keychain search list. The native ops
// use the security tool; the restore-contract test uses fakes.
type listOps struct {
	get func() ([]string, error)
	set func([]string) error
}

func nativeListOps(t *testing.T) listOps {
	return listOps{
		get: func() ([]string, error) {
			s, text := runStdout(t, 30*time.Second, "/usr/bin/security", "list-keychains", "-d", "user")
			if !s.ok() {
				return nil, errors.New("list-keychains " + s.String())
			}
			var out []string
			for _, line := range strings.Split(text, "\n") {
				if p := strings.Trim(strings.TrimSpace(line), `"`); p != "" {
					out = append(out, p)
				}
			}
			return out, nil
		},
		set: func(list []string) error {
			if s := run(t, 30*time.Second, "/usr/bin/security", append([]string{"list-keychains", "-d", "user", "-s"}, list...)...); !s.ok() {
				return errors.New("list-keychains -s " + s.String())
			}
			return nil
		},
	}
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// armSearchList puts the search list into the arm's state and returns the
// OBSERVED state plus a restore function that is valid on every path,
// including a setter that fails after it changed the list. The caller defers
// restore immediately. The target is derived from original, the list read
// before any import, minus the fixture: "present" is the fixture first, then
// that list; "absent" is that list alone. The observed state is "present" or
// "absent" only when a readback after the set shows exactly that; otherwise
// it is set-failed, unread or unexpected.
func armSearchList(ops listOps, original []string, fixture string, present bool) (string, func() error) {
	restore := func() error {
		if err := ops.set(original); err != nil {
			return err
		}
		got, err := ops.get()
		if err != nil {
			return err
		}
		if !sameList(got, original) {
			return errors.New("search list not restored to the original")
		}
		return nil
	}
	var base []string
	for _, p := range original {
		if p != fixture {
			base = append(base, p)
		}
	}
	want := base
	if present {
		want = append([]string{fixture}, base...)
	}
	if err := ops.set(want); err != nil {
		return "set-failed", restore
	}
	got, err := ops.get()
	if err != nil {
		return "unread", restore
	}
	if !sameList(got, want) {
		return "unexpected", restore
	}
	if present {
		return "present", restore
	}
	return "absent", restore
}

type arm struct {
	list, identity string
	sign, verify   step
}

func (a arm) String() string {
	list := a.list
	if list == "" {
		list = "UNRUN"
	}
	return "list=" + list + " identity=" + a.identity + " sign=" + a.sign.String() + " verify=" + a.verify.String()
}

// signWithFixture runs one arm on fresh state: a new keychain created by the
// release import path, a fresh copy of the same fixture executable, then
// codesign with the release argv (signArgs) and a strict verification.
// Both search-list states are SET and read back (armSearchList), whatever the
// import did to the list; the original list, read before any import, is
// restored on every exit. partition widens the imported key's partition list
// to apple-tool:,apple:. timestamp=false swaps only --timestamp for
// --timestamp=none.
func signWithFixture(t *testing.T, ops listOps, original []string, values map[string]string, binary string, searchList, partition, timestamp bool) arm {
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
	// Observation only: whether the import itself put the keychain on the list.
	if got, err := ops.get(); err == nil {
		t.Logf("SIGNING-CONTROL import_listed=%v", slices.Contains(got, signer.path))
	} else {
		t.Logf("SIGNING-CONTROL import_listed=unknown")
	}
	state, restore := armSearchList(ops, original, signer.path, searchList)
	defer func() {
		if err := restore(); err != nil {
			t.Errorf("CLEANUP: restore the user search list: %v", err)
		}
	}()
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
	out := arm{list: state, identity: identityClass(t, signer.path, signer.selector)}
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
	ops := nativeListOps(t)
	// The original list is read before any import or mutation. A final
	// t.Cleanup restores and re-reads it even if an arm stops the test.
	before, err := ops.get()
	if err != nil {
		t.Fatalf("SETUP: read the original user search list: %v", err)
	}
	t.Logf("SIGNING-CONTROL search-list before: %d entries", len(before))
	t.Cleanup(func() {
		got, err := ops.get()
		if err == nil && sameList(got, before) {
			t.Logf("SIGNING-CONTROL search-list after: %d entries, restored=true", len(got))
			return
		}
		if err := ops.set(before); err != nil {
			t.Errorf("CLEANUP: final restore of the user search list: %v", err)
			return
		}
		got, err = ops.get()
		restored := err == nil && sameList(got, before)
		t.Logf("SIGNING-CONTROL search-list after: restored=%v (after a final restore)", restored)
		if !restored {
			t.Errorf("CLEANUP: user search list not restored")
		}
	})

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
		r := signWithFixture(t, ops, before, values, binary, c.searchList, c.partition, c.timestamp)
		results[c.name] = r
		t.Logf("SIGNING-CONTROL arm=%s %s", c.name, r)
	}

	h6, h1, detail := matrixVerdict(results["absent/widened"], results["present/unchanged"], results["present/widened"])
	t.Logf("SIGNING-CONTROL VERDICT H6=%s H1=%s: %s", h6, h1, detail)
	if !admittedVerdict(h6) || !admittedVerdict(h1) {
		t.Fatalf("SIGNING-CONTROL not admitted: H6=%s H1=%s", h6, h1)
	}
}

// admittedVerdict is the closed set a component may end in for the control to
// pass. REPRODUCED is the keeper rule's witness. NOT_REPRODUCED is the phase-1
// brief's honest outcome (item 5), for a comparison arm that signs and verifies
// in its observed state. Everything else, INCONCLUSIVE, an empty value or any
// other string, fails the control.
func admittedVerdict(v string) bool { return v == "REPRODUCED" || v == "NOT_REPRODUCED" }

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
// are INCONCLUSIVE. Each arm's search-list state must be the OBSERVED one it
// stands for (absent or present), or that component is INCONCLUSIVE. H6 compares absent+widened with the control: REPRODUCED
// if it is refused as identity-not-found, NOT_REPRODUCED if it signs and
// verifies. H1 compares present+unchanged with the control: REPRODUCED if it
// is refused with an access-control class, NOT_REPRODUCED if it signs and
// verifies. Anything else, including a timeout, an UNRUN step or a sign
// without verification, is INCONCLUSIVE for that component.
func matrixVerdict(absentWidened, presentUnchanged, presentWidened arm) (string, string, string) {
	detail := "absent/widened " + absentWidened.String() + "; present/unchanged " + presentUnchanged.String() + "; present/widened " + presentWidened.String()
	if presentWidened.list != "present" || !signedAndVerified(presentWidened) {
		return "INCONCLUSIVE", "INCONCLUSIVE", "positive control failed; " + detail
	}
	component := func(a arm, list string, admit func(string) bool) string {
		switch {
		case a.list != list:
			return "INCONCLUSIVE"
		case signedAndVerified(a):
			return "NOT_REPRODUCED"
		case refusedAs(a, admit):
			return "REPRODUCED"
		}
		return "INCONCLUSIVE"
	}
	notFound := func(c string) bool { return c == "identity-not-found" }
	return component(absentWidened, "absent", notFound), component(presentUnchanged, "present", aclClass), detail
}

// Proves the control's pass rule is a closed allowlist, not "anything but
// INCONCLUSIVE": only REPRODUCED and NOT_REPRODUCED pass, and INCONCLUSIVE,
// an empty value and unknown strings fail. Also proves matrixVerdict itself
// emits only the three-value vocabulary, over a grid of arm outcomes.
func TestSigningControlVerdictVocabulary(t *testing.T) {
	for v, want := range map[string]bool{"REPRODUCED": true, "NOT_REPRODUCED": true, "INCONCLUSIVE": false, "": false, "PASS": false, "reproduced": false} {
		if admittedVerdict(v) != want {
			t.Fatalf("admittedVerdict(%q) = %v, want %v", v, !want, want)
		}
	}
	good := step{ran: true, class: "none"}
	var arms []arm
	for _, list := range []string{"absent", "present", "unexpected", ""} {
		for _, sign := range []step{good, {ran: true, exit: 1, class: "identity-not-found"}, {ran: true, exit: 1, class: "internal-component"}, {ran: true, exit: 1, class: "other"}, {ran: true, timeout: true, class: "timeout"}, {}} {
			for _, verify := range []step{good, {ran: true, exit: 3, class: "other"}, {}} {
				arms = append(arms, arm{list: list, sign: sign, verify: verify})
			}
		}
	}
	for _, a := range arms {
		for _, b := range arms {
			for _, c := range []arm{arms[0], arms[len(arms)/2], {list: "present", sign: good, verify: good}} {
				h6, h1, _ := matrixVerdict(a, b, c)
				for _, v := range []string{h6, h1} {
					if v != "REPRODUCED" && v != "NOT_REPRODUCED" && v != "INCONCLUSIVE" {
						t.Fatalf("matrixVerdict emitted %q outside the vocabulary", v)
					}
				}
			}
		}
	}
}

// Proves the release import refuses an unusable P12 before it creates any
// keychain, through both entry points, beside a valid-shape control that is
// refused later. Order note: crypto/rand.Read never returns an error (Go
// 1.24+), so generating the keychain password before the P12 decode leaves
// no observable error-order change; p12-decode stays the first refusal.
func TestSigningKeychainOpenRefusesBadP12First(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "!!not-base64!!"} {
		values := map[string]string{"MACOS_SIGN_P12": bad, "MACOS_SIGN_P12_PASSWORD": "x"}
		path := filepath.Join(dir, "never.keychain-db")
		if _, err := openSigningKeychain(path, values); err == nil || err.Error() != "p12-decode" {
			t.Fatalf("openSigningKeychain(%q) = %v, want p12-decode", bad, err)
		}
		if _, err := openSigningKeychainWithPassword(path, values, "fixture"); err == nil || err.Error() != "p12-decode" {
			t.Fatalf("openSigningKeychainWithPassword(%q) = %v, want p12-decode", bad, err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("a keychain was created for an unusable P12 %q", bad)
		}
	}
	// Control: decodable bytes that are not a PKCS#12 pass the decode and
	// are refused at import, so the test above is not refusing everything.
	// The import creates (and deletes) a scratch keychain, so it runs only in
	// the fixture job, keeping release-native-controls free of keychain work.
	if os.Getenv(signingControlDirEnv) == "" {
		t.Log("decode-passes control skipped: " + signingControlDirEnv + " not set")
		return
	}
	values := map[string]string{"MACOS_SIGN_P12": base64.StdEncoding.EncodeToString([]byte("not a p12")), "MACOS_SIGN_P12_PASSWORD": "x"}
	if _, err := openSigningKeychain(filepath.Join(dir, "control.keychain-db"), values); err == nil || err.Error() == "p12-decode" {
		t.Fatalf("decodable non-P12 = %v, want a refusal after the decode", err)
	}
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
// INCONCLUSIVE, and a failed positive control makes both INCONCLUSIVE. An arm
// whose observed search-list state is not the one it stands for is
// INCONCLUSIVE for its component.
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
		{"H6 absent state unobserved", arm{list: "unexpected", sign: refused("identity-not-found")}, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H6 absent arm actually present", arm{list: "present", sign: refused("identity-not-found")}, pass, pass, "INCONCLUSIVE", "NOT_REPRODUCED"},
		{"H1 present state unread", pass, arm{list: "unread", sign: refused("internal-component")}, pass, "NOT_REPRODUCED", "INCONCLUSIVE"},
		{"positive control set failed", notFound, acl, arm{list: "set-failed", sign: good, verify: good}, "INCONCLUSIVE", "INCONCLUSIVE"},
	} {
		// Rows that do not name a search-list state get the one their arm
		// stands for; rows that name one test a wrong or unobserved state.
		if c.absentWidened.list == "" {
			c.absentWidened.list = "absent"
		}
		if c.presentUnchanged.list == "" {
			c.presentUnchanged.list = "present"
		}
		if c.presentWidened.list == "" {
			c.presentWidened.list = "present"
		}
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

// fakeList is an in-memory search list whose setter can fail after it has
// already changed the list, and whose getter can fail.
type fakeList struct {
	list           []string
	setFailAfter   int // the Nth set call (1-based) mutates and then fails
	setFailBefore  int // the Nth set call fails without mutating
	getFail, calls int
	getFailAlways  bool
}

func (f *fakeList) ops() listOps {
	return listOps{
		get: func() ([]string, error) {
			if f.getFailAlways || f.getFail > 0 {
				if f.getFail > 0 {
					f.getFail--
				}
				return nil, errors.New("synthetic read failure")
			}
			return append([]string(nil), f.list...), nil
		},
		set: func(l []string) error {
			f.calls++
			if f.calls == f.setFailBefore {
				return errors.New("synthetic set failure, unchanged")
			}
			f.list = append([]string(nil), l...)
			if f.calls == f.setFailAfter {
				return errors.New("synthetic set failure after mutation")
			}
			return nil
		},
	}
}

// Proves the search-list restore contract without touching a real keychain:
// armSearchList reports only an OBSERVED absent/present state, and its
// restore returns the list to the pre-import original on every path,
// including a setter that fails after mutating it and an import that had
// already listed the fixture. A restore whose set or readback fails, or whose
// readback differs, reports an error rather than success.
func TestSearchListRestoreContract(t *testing.T) {
	original := []string{"/k/login.keychain-db"}
	const fx = "/w/control.keychain-db"
	for _, c := range []struct {
		name       string
		start      []string
		fake       fakeList
		present    bool
		wantState  string
		restoreErr bool
	}{
		{"present observed", original, fakeList{}, true, "present", false},
		{"absent observed", original, fakeList{}, false, "absent", false},
		{"absent although the import listed the fixture", []string{fx, "/k/login.keychain-db"}, fakeList{}, false, "absent", false},
		{"setter fails after mutating", original, fakeList{setFailAfter: 1}, true, "set-failed", false},
		{"setter fails without mutating", original, fakeList{setFailBefore: 1}, true, "set-failed", false},
		{"readback fails", original, fakeList{getFail: 1}, true, "unread", false},
		{"restore set fails", original, fakeList{setFailBefore: 2}, true, "present", true},
		{"restore readback fails", original, fakeList{getFailAlways: true}, true, "unread", true},
	} {
		f := c.fake
		f.list = append([]string(nil), c.start...)
		state, restore := armSearchList(f.ops(), original, fx, c.present)
		if state != c.wantState {
			t.Fatalf("%s: state %q, want %q", c.name, state, c.wantState)
		}
		err := restore()
		if (err != nil) != c.restoreErr {
			t.Fatalf("%s: restore error %v, want error=%v", c.name, err, c.restoreErr)
		}
		if !c.restoreErr && !sameList(f.list, original) {
			t.Fatalf("%s: list %q after restore, want the original %q", c.name, f.list, original)
		}
	}
	// A restore whose readback differs from the original is an error.
	f := fakeList{list: original}
	ops := f.ops()
	ops.set = func([]string) error { f.list = []string{"/other"}; return nil }
	if _, restore := armSearchList(ops, original, fx, true); restore() == nil {
		t.Fatal("a restore that leaves a different list reported success")
	}
}

// Proves runStdout refuses output over its cap instead of returning a
// truncated listing, beside a small output that is returned whole.
func TestRunStdoutRefusesOverflow(t *testing.T) {
	if s, text := runStdout(t, 30*time.Second, "/bin/echo", "small"); !s.ok() || text != "small\n" {
		t.Fatalf("small output: %s %q", s, text)
	}
	big := strings.Repeat("x", stdoutCap)
	if s, text := runStdout(t, 30*time.Second, "/bin/echo", big); s.ok() || s.class != "output-overflow" || text != "" {
		t.Fatalf("overflow: %s, %d bytes returned", s, len(text))
	}
}
