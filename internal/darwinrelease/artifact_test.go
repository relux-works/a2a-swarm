package darwinrelease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixturePayload(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range payloadNames {
		mode := os.FileMode(0644)
		if n == "a2a" || n == "a2a-keychain-status" {
			mode = 0755
		}
		if e := os.WriteFile(filepath.Join(dir, n), []byte("synthetic payload "+n), mode); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}

// Actual archive construction/readback requires exact siblings and documents;
// these inert fixture bytes are not native or signature qualification.
func TestCompanionPackageControls(t *testing.T) {
	dir := fixturePayload(t)
	out := filepath.Join(t.TempDir(), "payload.tar.gz")
	if e := makeArchive(dir, out); e != nil {
		t.Fatal(e)
	}
	if e := verifyArchive(out, dir); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(dir, "a2a-keychain-status")); e != nil {
		t.Fatal(e)
	}
	if e := payload(dir); e == nil || e.Error() != "package-members" {
		t.Fatal("missing companion not detected", e)
	}
	if e := os.WriteFile(filepath.Join(dir, "a2a-keychain-status"), []byte("restored"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := payload(dir); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "private.p8"), []byte("plant"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := payload(dir); e == nil || e.Error() != "package-members" {
		t.Fatal("extra private member admitted", e)
	}
}

// Full compiled-requirement identities must match, while both signed images
// differ. These struct fixtures prove comparison only, not native trust or ACLs.
func TestFullDRTransitionComparison(t *testing.T) {
	old := []Identity{{Name: "a2a", SHA256: "old-cli", DR: "full-cli", CompiledDR: "compiled-cli"}, {Name: "a2a-keychain-status", SHA256: "old-helper", DR: "full-helper", CompiledDR: "compiled-helper"}}
	next := append([]Identity{}, old...)
	next[0].SHA256 = "new-cli"
	next[1].SHA256 = "new-helper"
	if e := CompareDR(old, next); e != nil {
		t.Fatal(e)
	}
	for i := range next {
		planted := append([]Identity{}, next...)
		planted[i].CompiledDR = "omitted-term"
		if e := CompareDR(old, planted); e == nil || e.Error() != "dr-transition-refused" {
			t.Fatal("weakened DR admitted", e)
		}
	}
}

// Distributed-byte validation catches a changed archive before the production
// publisher boundary. Fixture certificate/signature bytes are not cosign evidence.
func TestDistributedAssetControls(t *testing.T) {
	dir := t.TempDir()
	archive := "a2a_0.1.0_darwin_arm64.tar.gz"
	for _, name := range []string{archive, "evidence.tar.gz", "release-evidence.json"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("public-fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := checksums(dir); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"checksums.txt.sig", "checksums.txt.pem"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("inert-fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := verifyAssetChecksums(dir); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, archive), []byte("changed-archive"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := verifyAssetChecksums(dir); e == nil || e.Error() != "distributed-bytes-changed" {
		t.Fatal("changed distributed bytes admitted", e)
	}
	if e := os.WriteFile(filepath.Join(dir, archive), []byte("public-fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := verifyAssetChecksums(dir); e != nil {
		t.Fatal(e)
	}
}

// Accepted status alone is insufficient: the uploaded ZIP hash and BOTH arm64
// code-directory tickets must match. Fixtures validate the parser/binding only.
func TestNotaryArchiveAndCompanionControls(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	cli, helper := strings.Repeat("a", 40), strings.Repeat("b", 40)
	raw := `{"jobId":"` + id + `","status":"Accepted","statusCode":0,"sha256":"zip-hash","ticketContents":[{"path":"notarize.zip/a2a","arch":"arm64","cdhash":"` + cli + `"},{"path":"notarize.zip/a2a-keychain-status","arch":"arm64","cdhash":"` + helper + `"}]}`
	hashes := map[string]string{"a2a": cli, "a2a-keychain-status": helper}
	result := NotaryResult{ID: id, Status: "Accepted"}
	if _, e := validateNotaryLog([]byte(raw), result, "zip-hash", hashes); e != nil {
		t.Fatal(e)
	}
	cases := []struct{ raw, want string }{{strings.Replace(raw, "zip-hash", "wrong", 1), "notary-log-not-accepted"}, {strings.Replace(raw, "Accepted", "Invalid", 1), "notary-log-not-accepted"}, {strings.Replace(raw, "a2a-keychain-status", "missing-companion", 1), "notary-companion-ticket-missing"}, {strings.Replace(raw, helper, strings.Repeat("c", 40), 1), "notary-companion-ticket-missing"}, {strings.ReplaceAll(raw, "arm64", "x86_64"), "notary-companion-ticket-missing"}}
	for _, c := range cases {
		if _, e := validateNotaryLog([]byte(c.raw), result, "zip-hash", hashes); e == nil || e.Error() != c.want {
			t.Fatal("notary plant not detected specifically", c.want, e)
		}
	}
}

// The production full-compiled-requirement comparator must reject an added,
// omitted or substituted AST, and accept the complete matching positive.
func TestCompiledRequirementComparator(t *testing.T) {
	expected := []byte("complete compiled requirement fixture")
	if e := matchCompiledRequirement(expected, append([]byte{}, expected...)); e != nil {
		t.Fatal(e)
	}
	for _, actual := range [][]byte{nil, []byte("omitted term"), append(append([]byte{}, expected...), 0)} {
		if e := matchCompiledRequirement(expected, actual); e == nil || e.Error() != "full-designated-requirement-mismatch" {
			t.Fatal("compiled requirement plant admitted", e)
		}
	}
}

// UUID case represents the same submission in either direction, with raw
// evidence retained. Changed/malformed IDs still fail the production log gate;
// these are parser controls, not an observed Apple submission.
func TestNotarySubmissionUUIDIdentity(t *testing.T) {
	lower := "ab12cd34-ef56-4789-9abc-def012345678"
	upper := strings.ToUpper(lower)
	cli, helper := strings.Repeat("a", 40), strings.Repeat("b", 40)
	hashes := map[string]string{"a2a": cli, "a2a-keychain-status": helper}
	for _, c := range []struct {
		name, submit, job string
		valid             bool
	}{
		{"lower-submit-upper-log", lower, upper, true},
		{"upper-submit-lower-log", upper, lower, true},
		{"changed-letter", lower, strings.Replace(upper, "A", "B", 1), false},
		{"malformed-log", lower, upper + "-", false},
		{"malformed-submit", lower + "-", upper, false},
		{"same-malformed", "not-a-uuid", "not-a-uuid", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			submit := []byte(`{"id":"` + c.submit + `","status":"Accepted"}`)
			n, submitErr := acceptedNotary(submit)
			if uuid.MatchString(c.submit) {
				if submitErr != nil || n.ID != c.submit {
					t.Fatal("valid submit ID was changed or refused", submitErr)
				}
			} else if submitErr == nil || submitErr.Error() != "notary-not-accepted" {
				t.Fatal("malformed submission admitted", submitErr)
			}
			// Exercise the log guard even when the earlier submit guard refuses.
			raw := []byte(`{"jobId":"` + c.job + `","status":"Accepted","statusCode":0,"sha256":"zip-hash","ticketContents":[{"path":"notarize.zip/a2a","arch":"arm64","cdhash":"` + cli + `"},{"path":"notarize.zip/a2a-keychain-status","arch":"arm64","cdhash":"` + helper + `"}]}`)
			record, e := validateNotaryLog(raw, NotaryResult{ID: c.submit, Status: "Accepted"}, "zip-hash", hashes)
			if c.valid {
				if e != nil || record.JobID != c.job {
					t.Fatal("same UUID identity refused or raw log field changed", e)
				}
			} else if e == nil || e.Error() != "notary-log-not-accepted" {
				t.Fatal("different/malformed UUID admitted", e)
			}
		})
	}
	// Casing compatibility cannot weaken exact status-code, ZIP or ticket checks.
	raw := `{"jobId":"` + upper + `","status":"Accepted","statusCode":0,"sha256":"zip-hash","ticketContents":[{"path":"a2a","arch":"arm64","cdhash":"` + cli + `"},{"path":"a2a-keychain-status","arch":"arm64","cdhash":"` + helper + `"}]}`
	for _, c := range []struct{ name, old, next, want string }{
		{"status-code", `"statusCode":0`, `"statusCode":1`, "notary-log-not-accepted"},
		{"missing-status-code", `"statusCode":0,`, "", "notary-log-not-accepted"},
		{"zip", "zip-hash", "changed-zip", "notary-log-not-accepted"},
		{"cli-ticket", cli, strings.Repeat("c", 40), "notary-companion-ticket-missing"},
		{"helper-ticket", helper, strings.Repeat("d", 40), "notary-companion-ticket-missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			plant := strings.Replace(raw, c.old, c.next, 1)
			if !json.Valid([]byte(plant)) {
				t.Fatal("invalid control setup JSON")
			}
			if _, e := validateNotaryLog([]byte(plant), NotaryResult{ID: lower, Status: "Accepted"}, "zip-hash", hashes); e == nil || e.Error() != c.want {
				t.Fatal("UUID compatibility weakened notary binding", e)
			}
		})
	}
}
