package registryqual

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const marker = "PRIVATE-MARKER-7f3a"

// A captured-shape record whose identity values carry a private marker.
const capturedRecord = `{"pid":91983,"sessionId":"` + marker + `-sid","cwd":"/Users/` + marker + `","startedAt":1793000000000,` +
	`"procStart":"Thu Oct 29 01:00:00 2026","version":"2.1.312","peerProtocol":1,` +
	`"peerFeatures":["notify_idle","reply_across_default_dirs","artifact_yield"],"kind":"interactive","entrypoint":"cli",` +
	`"pidDomain":"darwin","tmux":"/private/tmp/` + marker + `:@3.%7","messagingSocketPath":"/tmp/` + marker + `.sock",` +
	`"name":"` + marker + `","nameSource":"derived","nameSince":1793000000001,"status":"shell","updatedAt":1793000000002,` +
	`"statusUpdatedAt":1793000000003}`

// The manifest lists every top-level key in order with its JSON type and
// never a value.
func TestManifestKeysAndTypesOnly(t *testing.T) {
	fields, err := Manifest([]byte(capturedRecord))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fields {
		got = append(got, f.Key+":"+f.Type)
	}
	want := "pid:number sessionId:string cwd:string startedAt:number procStart:string version:string peerProtocol:number " +
		"peerFeatures:array<string> kind:string entrypoint:string pidDomain:string tmux:string messagingSocketPath:string " +
		"name:string nameSource:string nameSince:number status:string updatedAt:number statusUpdatedAt:number"
	if strings.Join(got, " ") != want {
		t.Fatalf("manifest:\n%s\nwant:\n%s", strings.Join(got, " "), want)
	}
}

// Input that is not exactly one JSON object with unique keys is refused,
// while the valid record above is accepted.
func TestManifestRefusesMalformedRecords(t *testing.T) {
	for name, data := range map[string]string{
		"duplicate key": `{"pid":1,"pid":2}`,
		"trailing data": `{"pid":1} {}`,
		"array":         `[1]`,
		"truncated":     `{"pid":`,
	} {
		if _, err := Manifest([]byte(data)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The fixture keeps key order, types and protocol vocabulary, and replaces
// every identity value with the fixed synthetic one: the result equals the
// reviewed PR #127 fixture shape and holds no captured private value.
func TestFixtureSynthesisesIdentity(t *testing.T) {
	out, err := Fixture([]byte(capturedRecord))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "pid": 1234,
  "sessionId": "11111111-1111-4111-8111-111111111111",
  "cwd": "/synthetic",
  "startedAt": 1793016001000,
  "procStart": "Mon Oct 26 12:00:00 2026",
  "version": "2.1.312",
  "peerProtocol": 1,
  "peerFeatures": [
    "notify_idle",
    "reply_across_default_dirs",
    "artifact_yield"
  ],
  "kind": "interactive",
  "entrypoint": "cli",
  "pidDomain": "darwin",
  "tmux": "synthetic:@0.%1",
  "messagingSocketPath": "/synthetic/not-opened",
  "name": "synthetic",
  "nameSource": "derived",
  "nameSince": 1793016001000,
  "status": "shell",
  "updatedAt": 1793016002000,
  "statusUpdatedAt": 1793016002000
}
`
	if string(out) != want {
		t.Fatalf("fixture:\n%s", out)
	}
	if strings.Contains(string(out), marker) || strings.Contains(string(out), "91983") {
		t.Fatal("fixture leaked a captured value")
	}
	empty := strings.Replace(capturedRecord, `"status":"shell"`, `"status":"shell","formerNames":[]`, 1)
	if _, err := Fixture([]byte(empty)); err != nil {
		t.Fatalf("empty formerNames refused: %v", err)
	}
}

// Any field the fixture cannot vouch for refuses the whole fixture: an unknown
// key, non-empty name history, a changed identity type, or a protocol value
// outside the reviewed vocabulary, including a short token that would pass a
// character check. The refusal names the field, never its value. The
// captured record above is the paired positive with reviewed values.
func TestFixtureRefusesUnreviewedFields(t *testing.T) {
	cases := map[string][2]string{
		"unknown key":           {`"status":"shell"`, `"status":"shell","hostSessionId":"` + marker + `"`},
		"name history":          {`"status":"shell"`, `"status":"shell","formerNames":[{"name":"` + marker + `"}]`},
		"identity type":         {`"pid":91983`, `"pid":"` + marker + `"`},
		"protocol free text":    {`"version":"2.1.312"`, `"version":"2.1.312 /Users/` + marker + `"`},
		"feature free text":     {`"artifact_yield"`, `"artifact yield ` + marker + `"`},
		"short private status":  {`"status":"shell"`, `"status":"PRIVATEx"`},
		"short private version": {`"version":"2.1.312"`, `"version":"alexis"`},
		"unreviewed feature":    {`"artifact_yield"`, `"PRIVATEx"`},
		"unreviewed kind":       {`"kind":"interactive"`, `"kind":"alexis"`},
		"null features":         {`"peerFeatures":["notify_idle","reply_across_default_dirs","artifact_yield"]`, `"peerFeatures":null`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if strings.Count(capturedRecord, c[0]) != 1 {
				t.Fatalf("anchor %q not unique", c[0])
			}
			out, err := Fixture([]byte(strings.Replace(capturedRecord, c[0], c[1], 1)))
			if err == nil || out != nil {
				t.Fatalf("fixture accepted: %s", out)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "PRIVATEx") || strings.Contains(err.Error(), "alexis") {
				t.Fatalf("refusal leaked a value: %v", err)
			}
		})
	}
}

// ReadRecord admits only a small regular file: a symlink, a directory, an
// oversized file and a FIFO (without blocking on it) are refused, a normal
// record is read; the read itself refuses data beyond the bound even when
// the size check passed.
func TestReadRecordBounds(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "1234.json")
	if err := os.WriteFile(ok, []byte(capturedRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(ok); err != nil {
		t.Fatalf("regular record refused: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxRecord+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, dir, big} {
		if _, err := ReadRecord(p); err == nil {
			t.Errorf("%s accepted", filepath.Base(p))
		}
	}
	if err := mkfifo(filepath.Join(dir, "fifo.json")); err == nil {
		if _, err := ReadRecord(filepath.Join(dir, "fifo.json")); err == nil {
			t.Error("fifo accepted")
		}
	}
	if _, err := readLimited(strings.NewReader("12345"), "grown", 4); err == nil {
		t.Error("over-limit read accepted")
	}
	if b, err := readLimited(strings.NewReader("1234"), "exact", 4); err != nil || string(b) != "1234" {
		t.Errorf("at-limit read: %q %v", b, err)
	}
}
