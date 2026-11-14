//go:build darwin && arm64 && cgo

package darwinrelease

import (
	"bytes"
	"context"
	"encoding/base64"
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
// identity (identity.p12, identity.pass) that the job trusts for code signing.
// It is never a release credential. Without it the control is skipped.
const signingControlDirEnv = "A2A_SIGNING_KEYCHAIN_CONTROL_DIR"

type signOutcome struct {
	exit    int
	stderr  string
	timeout bool
}

func (o signOutcome) ok() bool { return o.exit == 0 && !o.timeout }

func (o signOutcome) String() string {
	if o.timeout {
		return "timeout"
	}
	first, _, _ := strings.Cut(strings.TrimSpace(o.stderr), "\n")
	if o.exit == 0 {
		return "exit=0"
	}
	return "exit=" + strconv.Itoa(o.exit) + " stderr=" + first
}

// signWithFixture imports the fixture identity through the release import
// path, optionally widens the key partition list with the security tool, and
// runs codesign with the release argv (signArgs). timestamp=false swaps only
// --timestamp for --timestamp=none, to separate a network failure.
func signWithFixture(t *testing.T, dir string, values map[string]string, binary string, partition, timestamp bool) signOutcome {
	t.Helper()
	work := t.TempDir()
	password := "fixture-keychain-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	signer, err := openSigningKeychainWithPassword(filepath.Join(work, "control.keychain-db"), values, password)
	if err != nil {
		t.Fatalf("fixture import through the release path failed: %v", err)
	}
	defer func() {
		if err := signer.close(); err != nil {
			t.Errorf("fixture keychain cleanup: %v", err)
		}
	}()
	if partition {
		// Fixture-only: the security tool takes the keychain password in argv.
		cmd := exec.Command("/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", password, signer.path)
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = nil, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("set-key-partition-list on the fixture keychain: %v: %s", err, stderr.String())
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
	expr, err := Requirement("a2a")
	if err != nil {
		t.Fatal(err)
	}
	args := signArgs(signer.selector, signer.path, "a2a", expr, target)
	if !timestamp {
		for i, a := range args {
			if a == "--timestamp" {
				args[i] = "--timestamp=none"
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/codesign", args...)
	cmd.Env = cleanEnvironment()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	out := signOutcome{stderr: stderr.String()}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.timeout = true
		return out
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.exit = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("codesign did not run: %v", err)
	}
	return out
}

// Discriminates the class of the first hosted binary-sign failure on a
// throwaway self-signed identity, through the release import path
// (openSigningKeychainWithPassword) and the release codesign argv (signArgs).
// Positive control: with the key partition list widened to apple-tool:,apple:
// and no timestamp, codesign must succeed; otherwise the fixture itself is
// unusable and the control is INCONCLUSIVE (fails). Verdicts, all logged:
// H1-REPRODUCED when the unmodified import fails and the widened one signs;
// H1-REFUTED when the unmodified import signs. Limit: a self-signed fixture
// trusted for code signing on the runner, not a Developer ID chain, so this
// says nothing about the release certificate itself (H2/H3).
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
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-arch", "arm64", "-o", binary, src).CombinedOutput(); err != nil {
		t.Fatalf("fixture binary: %v: %s", err, out)
	}

	results := map[string]signOutcome{}
	for _, c := range []struct {
		name                 string
		partition, timestamp bool
	}{
		{"release-import/timestamp", false, true},
		{"release-import/no-timestamp", false, false},
		{"partition-widened/timestamp", true, true},
		{"partition-widened/no-timestamp", true, false},
	} {
		results[c.name] = signWithFixture(t, dir, values, binary, c.partition, c.timestamp)
		t.Logf("SIGNING-CONTROL %s: %s", c.name, results[c.name])
	}

	if !results["partition-widened/no-timestamp"].ok() {
		t.Fatalf("SIGNING-CONTROL VERDICT=INCONCLUSIVE: the positive control did not sign: %s", results["partition-widened/no-timestamp"])
	}
	switch {
	case results["release-import/no-timestamp"].ok():
		t.Logf("SIGNING-CONTROL VERDICT=H1-REFUTED: the release import signs without a partition change")
	default:
		t.Logf("SIGNING-CONTROL VERDICT=H1-REPRODUCED: the release import cannot sign; the widened partition list can")
	}
}
