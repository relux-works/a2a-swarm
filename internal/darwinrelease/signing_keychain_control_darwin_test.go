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

type step struct {
	exit    int
	class   string
	timeout bool
}

func (s step) ok() bool { return s.exit == 0 && !s.timeout }

func (s step) String() string {
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
		return step{timeout: true, class: "timeout"}
	}
	out := step{class: errorClass(stderr.String())}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.exit = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s did not start: %v", filepath.Base(name), err)
	}
	return out
}

type arm struct{ sign, verify step }

// signWithFixture runs one arm on fresh state: a new keychain created by the
// release import path, a fresh copy of the same fixture executable, then
// codesign with the release argv (signArgs) and a strict verification.
// partition widens the imported key's partition list to apple-tool:,apple:
// and is the only difference between the arms. timestamp=false swaps only
// --timestamp for --timestamp=none.
func signWithFixture(t *testing.T, values map[string]string, binary string, partition, timestamp bool) arm {
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
	out := arm{sign: run(t, 90*time.Second, "/usr/bin/codesign", args...)}
	if out.sign.ok() {
		out.verify = run(t, 60*time.Second, "/usr/bin/codesign", "--verify", "--strict", "--test-requirement", "="+expr, target)
	}
	return out
}

// Discriminates the access-control hypothesis for the first hosted
// binary-sign failure. Both arms import one throwaway self-signed identity
// through the release import path and sign the same fixture executable with
// the release codesign argv. They differ only in whether the imported key's
// partition list is widened to apple-tool:,apple:. A verdict needs both
// untimestamped arms to execute: H1-REPRODUCED when the unchanged arm is
// refused and the widened arm signs and verifies; H1_NOT_REPRODUCED when the
// unchanged arm signs and verifies. Anything else is INCONCLUSIVE and fails.
// The timestamped pair is informational and does not qualify the Apple
// timestamp service. Limits: a self-signed fixture with a fixture
// requirement, not a Developer ID chain, so this says nothing about the
// release certificate, its chain or validity (H2, H3), and alone does not
// prove the original incident had this cause.
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

	results := map[string]arm{}
	for _, c := range []struct {
		name                 string
		partition, timestamp bool
	}{
		{"unchanged/no-timestamp", false, false},
		{"widened/no-timestamp", true, false},
		{"unchanged/timestamp", false, true},
		{"widened/timestamp", true, true},
	} {
		r := signWithFixture(t, values, binary, c.partition, c.timestamp)
		results[c.name] = r
		t.Logf("SIGNING-CONTROL arm=%s sign=%s verify=%s", c.name, r.sign, r.verify)
	}

	verdict, detail := controlVerdict(results["unchanged/no-timestamp"], results["widened/no-timestamp"])
	if verdict == "INCONCLUSIVE" {
		t.Fatalf("SIGNING-CONTROL VERDICT=INCONCLUSIVE: %s", detail)
	}
	t.Logf("SIGNING-CONTROL VERDICT=%s: %s", verdict, detail)
}

// aclClass reports whether a refusal is of the access-control class: the
// headless key-use refusal that a missing partition entry produces.
func aclClass(class string) bool {
	return class == "internal-component" || class == "interaction-not-allowed"
}

// controlVerdict decides on the untimestamped pair. The widened arm is the
// positive control and is checked first: unless it signs and verifies,
// nothing else is admitted. Then the unchanged arm either signs and verifies
// (H1_NOT_REPRODUCED), or is refused at signing with an access-control class
// (H1-REPRODUCED). Any other refusal, a timeout, or a sign without a passing
// verification is INCONCLUSIVE.
func controlVerdict(unchanged, widened arm) (string, string) {
	pair := "unchanged sign=" + unchanged.sign.String() + " verify=" + unchanged.verify.String() +
		"; widened sign=" + widened.sign.String() + " verify=" + widened.verify.String()
	if !widened.sign.ok() || !widened.verify.ok() {
		return "INCONCLUSIVE", "positive control failed; " + pair
	}
	if unchanged.sign.ok() {
		if unchanged.verify.ok() {
			return "H1_NOT_REPRODUCED", pair
		}
		return "INCONCLUSIVE", "unchanged signed but did not verify; " + pair
	}
	if !unchanged.sign.timeout && aclClass(unchanged.sign.class) {
		return "H1-REPRODUCED", pair
	}
	return "INCONCLUSIVE", "unchanged refusal is not of the access-control class; " + pair
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

// Proves the verdict rule on synthetic arm results, independently of a run:
// H1-REPRODUCED needs a healthy positive control AND an access-control-class
// refusal of the unchanged arm; H1_NOT_REPRODUCED needs the positive control
// AND a signed, verified unchanged arm. Every other combination, including a
// non-ACL refusal, a timeout, a sign without verification and any failure of
// the positive control, is INCONCLUSIVE.
func TestSigningControlVerdict(t *testing.T) {
	good := step{class: "none"}
	acl := step{exit: 1, class: "internal-component"}
	pass := arm{sign: good, verify: good}
	for _, c := range []struct {
		name               string
		unchanged, widened arm
		want               string
	}{
		{"valid H1 pair", arm{sign: acl}, pass, "H1-REPRODUCED"},
		{"valid H1 pair, interaction class", arm{sign: step{exit: 1, class: "interaction-not-allowed"}}, pass, "H1-REPRODUCED"},
		{"valid no-repro pair", pass, pass, "H1_NOT_REPRODUCED"},
		{"chain refusal is not ACL", arm{sign: step{exit: 1, class: "chain-build"}}, pass, "INCONCLUSIVE"},
		{"identity-not-found is not ACL", arm{sign: step{exit: 1, class: "identity-not-found"}}, pass, "INCONCLUSIVE"},
		{"requirement refusal is not ACL", arm{sign: step{exit: 1, class: "requirement-unsatisfied"}}, pass, "INCONCLUSIVE"},
		{"other refusal is not ACL", arm{sign: step{exit: 1, class: "other"}}, pass, "INCONCLUSIVE"},
		{"refusal without stderr is not ACL", arm{sign: step{exit: 1, class: "none"}}, pass, "INCONCLUSIVE"},
		{"timeout is not ACL", arm{sign: step{timeout: true, class: "timeout"}}, pass, "INCONCLUSIVE"},
		{"unchanged signs, verify fails", arm{sign: good, verify: step{exit: 3, class: "requirement-unsatisfied"}}, pass, "INCONCLUSIVE"},
		{"positive control sign fails, unchanged ACL", arm{sign: acl}, arm{sign: acl}, "INCONCLUSIVE"},
		{"positive control verify fails, unchanged ACL", arm{sign: acl}, arm{sign: good, verify: step{exit: 3, class: "other"}}, "INCONCLUSIVE"},
		{"positive control fails, unchanged passes", pass, arm{sign: acl}, "INCONCLUSIVE"},
	} {
		if got, _ := controlVerdict(c.unchanged, c.widened); got != c.want {
			t.Fatalf("%s: verdict %s, want %s", c.name, got, c.want)
		}
	}
}
