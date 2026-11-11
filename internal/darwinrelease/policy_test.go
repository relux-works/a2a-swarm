package darwinrelease

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every missing/empty field refuses; all five succeeds. This is presence and
// leak-boundary evidence, not validation of an Apple identity or credential.
func TestSigningConfiguration(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		values := map[string]string{}
		for i, n := range SigningFields {
			if mask&(1<<i) != 0 {
				values[n] = "fixture-value-" + n
			}
		}
		e := SigningConfiguration(func(n string) string { return values[n] })
		if (e == nil) != (mask == 31) {
			t.Fatalf("presence mask %d: %v", mask, e)
		}
		cmd := exec.Command("/bin/sh", filepath.Join("..", "..", ".scripts", "release-config.sh"))
		cmd.Env = cleanEnvironment()
		for k, v := range values {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, e := cmd.CombinedOutput()
		if (e == nil) != (mask == 31) {
			t.Fatalf("shell presence mask %d: %v", mask, e)
		}
		for _, v := range values {
			if strings.Contains(string(out), v) {
				t.Fatal("configuration gate printed a value")
			}
		}
	}
	values := map[string]string{}
	for _, n := range SigningFields {
		values[n] = "present"
	}
	values[SigningFields[0]] = " \t\n"
	if SigningConfiguration(func(n string) string { return values[n] }) == nil {
		t.Fatal("whitespace configuration admitted")
	}
}

// Strict semver rejects option/ref injection, pre-releases and numeric aliases;
// it is not a claim about signed Git objects (covered by TestReleaseTagObjects).
func TestReleaseVersion(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.2.3", "v10.20.30"} {
		if CheckVersion(tag, strings.TrimPrefix(tag, "v")) != nil {
			t.Fatal(tag)
		}
	}
	for _, tag := range []string{"v01.2.3", "1.2.3", "v1.2.3-rc1", "v1.2.3+meta", "-v1.2.3", "v1.2.3\n", "refs/tags/v1.2.3", "v1.2.3/other", "v1.2.4"} {
		if CheckVersion(tag, "1.2.3") == nil {
			t.Fatal("invalid version admitted", tag)
		}
	}
}

// A subprocess cannot inherit any Apple field. Its failure output/argv is not
// echoed by execute. These are synthetic bytes; no real credential is used.
func TestReleaseEnvironmentBoundary(t *testing.T) {
	for _, n := range SigningFields {
		t.Setenv(n, "private-fixture-"+n)
	}
	for _, entry := range cleanEnvironment() {
		for _, n := range SigningFields {
			if strings.HasPrefix(entry, n+"=") {
				t.Fatal("private environment reached child")
			}
		}
	}
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "private-oidc-fixture")
	for _, entry := range cleanEnvironment() {
		if strings.HasPrefix(entry, "ACTIONS_ID_TOKEN_") {
			t.Fatal("OIDC reached ordinary child")
		}
	}
	t.Setenv("RELEASE_CONTROL_CHILD", "1")
	_, e := execute(context.Background(), "", os.Args[0], "-test.run=^TestReleaseErrorChild$", "--", "private-argv-fixture")
	if e == nil || e.Error() != "release subprocess refused" || strings.Contains(e.Error(), "private-") {
		t.Fatal("subprocess failure was not closed", e)
	}
}
func TestReleaseErrorChild(t *testing.T) {
	if os.Getenv("RELEASE_CONTROL_CHILD") != "1" {
		return
	}
	for _, n := range SigningFields {
		if os.Getenv(n) != "" {
			os.Exit(7)
		}
	}
	os.Stderr.WriteString("private-subprocess-output-fixture")
	os.Exit(9)
}

// A known private-output plant is detected specifically, with a clean positive;
// compressed output safety also depends on the fixed source/member construction.
func TestReleasePrivateOutputPlant(t *testing.T) {
	dir := t.TempDir()
	values := map[string]string{"MACOS_SIGN_P12": base64.StdEncoding.EncodeToString([]byte("fixture-private-der")), "ASC_KEY_P8": "fixture-private-p8", "MACOS_SIGN_P12_PASSWORD": "fixture-private-password"}
	path := filepath.Join(dir, "evidence.json")
	if e := os.WriteFile(path, []byte("public fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := rejectCredentialBytes(dir, values); e != nil {
		t.Fatal(e)
	}
	for _, plant := range []string{"fixture-private-der", "fixture-private-p8", "fixture-private-password", values["MACOS_SIGN_P12"]} {
		if e := os.WriteFile(path, []byte(plant), 0600); e != nil {
			t.Fatal(e)
		}
		if e := rejectCredentialBytes(dir, values); e == nil || e.Error() != "private-material-in-output" {
			t.Fatal("plant not detected specifically", e)
		}
	}
	if e := os.WriteFile(path, []byte("public fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := rejectCredentialBytes(dir, values); e != nil {
		t.Fatal(e)
	}
}

// This is the actual production publication-order helper, with bounded callback
// plants. It proves no publisher call after a failed prerequisite, not Apple/GitHub
// transaction atomicity or native signing success.
func TestNoPublicationAfterFailedGate(t *testing.T) {
	for stop := 0; stop < 6; stop++ {
		called, published := 0, 0
		var gates []func() error
		for i := 0; i < 5; i++ {
			i := i
			gates = append(gates, func() error {
				called++
				if i == stop {
					return errors.New("intended-stage-refusal")
				}
				return nil
			})
		}
		e := PublishAfter(gates, func() error { published++; return nil })
		if stop < 5 {
			if e == nil || e.Error() != "intended-stage-refusal" || published != 0 || called != stop+1 {
				t.Fatal("failure crossed publication boundary", stop, e, called, published)
			}
		} else if e != nil || published != 1 || called != 5 {
			t.Fatal("valid publication control not exercised")
		}
	}
}

// Notary acceptance is a closed status+submission-ID gate; parser fixtures do
// not prove any real submission was notarized.
func TestNotaryAcceptance(t *testing.T) {
	valid := `{"id":"11111111-2222-3333-4444-555555555555","status":"Accepted"}`
	if _, e := acceptedNotary([]byte(valid)); e != nil {
		t.Fatal(e)
	}
	for _, b := range []string{`{}`, `bad`, strings.Replace(valid, "Accepted", "In Progress", 1), strings.Replace(valid, "Accepted", "Invalid", 1), strings.Replace(valid, "11111111-2222-3333-4444-555555555555", "bad", 1)} {
		if _, e := acceptedNotary([]byte(b)); e == nil || e.Error() != "notary-not-accepted" {
			t.Fatal("unknown notary result admitted", e)
		}
	}
}

// The reviewed policy admits the existing source keys without changing the
// history anchor or granting them tag authority; unknown/missing keys refuse.
func TestReleaseSourceTrustPolicy(t *testing.T) {
	root := filepath.Join("..", "..")
	p, e := LoadPolicy(root)
	if e != nil {
		t.Fatal(e)
	}
	if p.HistoryBase != "df69538ab33513cd70c76d67cd4ddde968181ab4" || len(p.SourceSignerFingerprints) != 3 || p.SignerFingerprint != SignerFingerprint {
		t.Fatal("reviewed source range or tag authority changed")
	}
	if e := checkSignerEntries(filepath.Join(root, ".github/source-signers"), "source-maintainer", p.SourceSignerFingerprints); e != nil {
		t.Fatal(e)
	}
	if e := checkSignerFile(filepath.Join(root, ".github/release-signers"), p.SignerFingerprint); e != nil {
		t.Fatal(e)
	}
	fixture := t.TempDir()
	if e := os.Mkdir(filepath.Join(fixture, "release"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, keys := range [][]string{
		append(append([]string{}, p.SourceSignerFingerprints...), "SHA256:unknown"),
		p.SourceSignerFingerprints[:2],
		{SignerFingerprint},
	} {
		plant := p
		plant.SourceSignerFingerprints = keys
		b, e := json.Marshal(plant)
		if e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(fixture, "release/policy.json"), b, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadPolicy(fixture); !errors.Is(e, ErrPolicy) {
			t.Fatal("changed source policy admitted", e)
		}
	}
}
