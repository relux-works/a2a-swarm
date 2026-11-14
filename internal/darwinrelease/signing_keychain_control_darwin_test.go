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
	work := t.TempDir()
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

	unchanged, widened := results["unchanged/no-timestamp"], results["widened/no-timestamp"]
	signedAndVerified := func(a arm) bool { return a.sign.ok() && a.verify.ok() }
	switch {
	case signedAndVerified(unchanged):
		t.Logf("SIGNING-CONTROL VERDICT=H1_NOT_REPRODUCED: the unchanged import signs and verifies")
	case !unchanged.sign.ok() && !unchanged.sign.timeout && signedAndVerified(widened):
		t.Logf("SIGNING-CONTROL VERDICT=H1-REPRODUCED: unchanged refused (%s); widened partition list signs and verifies", unchanged.sign)
	default:
		t.Fatalf("SIGNING-CONTROL VERDICT=INCONCLUSIVE: unchanged sign=%s verify=%s; widened sign=%s verify=%s", unchanged.sign, unchanged.verify, widened.sign, widened.verify)
	}
}

// Proves the bounded error vocabulary: known codesign failures map to fixed
// classes, empty stderr is "none", and unrecognised text never passes through.
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
