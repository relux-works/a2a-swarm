package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// All credentials and HTTP services in these tests are fresh, synthetic fixtures.
// Raw captured output is deliberately not logged; assertions report booleans.
func reviewRun(e *env, args ...string) result {
	var out, errs bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, HTTP: e.hs.srv.Client(), Now: time.Now}
	returnCode := a.Run(context.Background(), args)
	return result{returnCode, out.String(), errs.String()}
}

func TestReviewBatchUnsupportedEncodingDoesNotLeakKnownCredential(t *testing.T) {
	e, original, data, _ := attachWorld(t)
	fields := attachmentFields(t, e, original)
	fields["encoding"] = map[string]any{"version": "future/" + e.tokens["bob"]}
	id := injectFileAttachment(e, fields)
	dir := filepath.Join(outputTestDir(t), "fresh")
	r := reviewRun(e, "--as", "bob", "fetch", "proj", id, "$unknown", "-o", dir)
	if r.code != ExitError {
		t.Fatalf("mixed-batch exit=%d", r.code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("positive control: verified stored file missing")
	}
	tuitestkit.ContainsStr(t, r.stdout, "\tok\n")
	tuitestkit.ContainsStr(t, r.stdout, "\tevent_not_found\n")
	leaked := strings.Contains(r.stdout+r.stderr, e.tokens["bob"])
	t.Logf("verified_file=true typed_statuses=true known_credential_in_output=%t", leaked)
	if leaked {
		t.Error("batch encoding warning exposes a registered credential through stderr")
	}
}

func TestReviewSavedInboxStructuredSecretKeysAreRedacted(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	synthetic := "review_fixture_value_8731"
	content := toMsg("bob", "notice", `{"password":"`+synthetic+`"}`)
	h := content[ContentKey].(Header)
	enc, _ := json.Marshal(map[string]any{"version": "future/1", "password": synthetic, "nested": map[string]any{"access_token": synthetic}})
	h.Attachment = &Attachment{Name: "ordinary.bin", Size: 1, SHA256: strings.Repeat("0", 64), MXC: "mxc://test.local/synthetic", Encoding: enc}
	content[ContentKey] = h
	id := e.hs.inject(room, "@bob:"+testServer, content)
	path := filepath.Join(outputTestDir(t), "frame")
	r := reviewRun(e, "--as", "alice", "inbox", "--out", path, "--summary")
	if r.code != ExitOK {
		t.Fatalf("save exit=%d", r.code)
	}
	records, err := readSavedInbox(path)
	if err != nil || len(records) != 1 {
		t.Fatal("positive control: complete readable saved frame missing")
	}
	bodyClean := !strings.Contains(records[0].Body, synthetic)
	metadataClean := !strings.Contains(string(records[0].Attachment.Encoding), synthetic)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("body_secret_redacted=%t metadata_secret_redacted=%t receipt_recorded=%t", bodyClean, metadataClean, e.received("alice", id))
	if !bodyClean {
		t.Fatal("positive control: inline JSON secret redaction failed")
	}
	if !metadataClean || bytes.Contains(raw, []byte(synthetic)) {
		t.Error("saved frame retains credential-shaped JSON values inside attachment metadata")
	}
}

func TestBatchEncodingDiagnosticContract(t *testing.T) {
	for _, tc := range []string{"future-registered", "coding-registered", "future-shaped", "coding-shaped", "future-ordinary", "coding-ordinary"} {
		t.Run(tc, func(t *testing.T) {
			e, fields, _, stored := encodingFixture(t)
			label := "ordinary-future"
			if strings.HasSuffix(tc, "registered") {
				label = e.tokens["bob"]
			}
			if strings.HasSuffix(tc, "shaped") {
				label = "syt_synthetic502_unsupported"
			}
			if strings.HasPrefix(tc, "future") {
				fields["encoding"] = map[string]any{"version": "future/" + label, "opaque": []any{nil, true}}
			} else {
				fields["encoding"].(map[string]any)["coding"] = label
			}
			id := injectFileAttachment(e, fields)
			dir := filepath.Join(outputTestDir(t), "batch")
			r := reviewRun(e, "--as", "bob", "fetch", "proj", id, "$unknown", "-o", dir)
			if r.code != ExitError {
				t.Fatalf("mixed batch exit=%d", r.code)
			}
			if strings.Contains(r.stdout+r.stderr, e.tokens["bob"]) || strings.Contains(r.stdout+r.stderr, "syt_") {
				t.Error("credential detected in captured batch output")
			}
			if r.stderr != "a2a: attachment encoding not supported; saved the stored bytes\na2a: batch fetch: 1 attachment(s) failed\n" {
				t.Error("batch diagnostic bytes differ from fixed safe contract")
			}
			want := fmt.Sprintf("%s\treport.json.gz\t%s\t%d\tok\n$unknown\t-\t-\t0\tevent_not_found\n", id, sumHex(stored), len(stored))
			if r.stdout != want {
				t.Error("batch result bytes changed")
			}
			got, err := os.ReadFile(filepath.Join(dir, "report.json.gz"))
			if err != nil || !bytes.Equal(got, stored) || len(dirEntries(t, dir)) != 1 {
				t.Error("fallback did not preserve verified stored bytes")
			}
		})
	}
	// Explicit legacy bytes with harmless metadata, including version/coding,
	// target path and stdout. Raw captured output is never logged.
	for _, future := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy-%t", future), func(t *testing.T) {
			e, fields, _, stored := encodingFixture(t)
			label := attachmentEncodingVersion + "/zstd"
			if future {
				fields["encoding"] = map[string]any{"version": "future/v2"}
				label = "future/v2"
			} else {
				fields["encoding"].(map[string]any)["coding"] = "zstd"
			}
			id := injectFileAttachment(e, fields)
			dir := outputTestDir(t)
			t.Chdir(dir)
			r := reviewRun(e, "--as", "bob", "fetch", "proj", id)
			want := "a2a: attachment encoding " + label + " not supported; saved the stored bytes as report.json.gz\n"
			if r.code != ExitOK || r.stdout != "report.json.gz\n" || r.stderr != want {
				t.Error("legacy warning/stdout bytes changed")
			}
			got, err := os.ReadFile(filepath.Join(dir, "report.json.gz"))
			if err != nil || !bytes.Equal(got, stored) {
				t.Error("legacy stored bytes changed")
			}
		})
	}
}

func TestSavedInboxNestedMetadataRedactionAndReceipt(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	fixture := "synthetic502_unregistered_value"
	var escaped strings.Builder
	for _, c := range e.tokens["alice"] {
		fmt.Fprintf(&escaped, "\\u%04x", c)
	}
	encoding := fmt.Sprintf(`{"version":"future/1","ordinary":{"name":"visible metadata","number":17,"bool":true,"nil":null,"PasswordHint":"keep hint","access_token_count":3},"list":[{"PaSsWoRd":%q},{"inner":[{"access_token":%q},{"REFRESH_TOKEN":%q},{"ToKeN":%q}]}],"structured":{"password":[%q,{"value":%q}],"token":{"value":%q}},"escaped":{"\u0070assword":%q,"known":"%s"}}`, fixture, fixture, fixture, fixture, fixture, fixture, fixture, fixture, escaped.String())
	content := toMsg("bob", "notice", "ordinary body")
	h := content[ContentKey].(Header)
	h.Attachment = &Attachment{Name: "ordinary.bin", Size: 1, SHA256: strings.Repeat("0", 64), MXC: "mxc://test.local/synthetic", Encoding: json.RawMessage(encoding)}
	content[ContentKey] = h
	id := e.hs.inject(room, "@bob:"+testServer, content)
	later := e.hs.inject(room, "@bob:"+testServer, ping(2))
	path := filepath.Join(outputTestDir(t), "frame")
	r := reviewRun(e, "--as", "alice", "inbox", "--out", path, "--summary", "--limit", "1")
	if r.code != ExitOK {
		t.Fatalf("save exit=%d", r.code)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fixture)) || bytes.Contains(raw, []byte(e.tokens["alice"])) {
		t.Error("saved frame retains synthetic credential")
	}
	records, err := readSavedInbox(path)
	if err != nil || len(records) != 1 {
		t.Fatal("saved frame is not readable")
	}
	var got, want any
	err = json.Unmarshal(records[0].Attachment.Encoding, &got)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"version":"future/1","ordinary":{"name":"visible metadata","number":17,"bool":true,"nil":null,"PasswordHint":"keep hint","access_token_count":3},"list":[{"PaSsWoRd":"<redacted>"},{"inner":[{"access_token":"<redacted>"},{"REFRESH_TOKEN":"<redacted>"},{"ToKeN":"<redacted>"}]}],"structured":{"password":"<redacted>","token":"<redacted>"},"escaped":{"password":"<redacted>","known":"<redacted>"}}`
	if err = json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("nested metadata redaction or ordinary metadata preservation differs")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Error("saved frame permissions changed")
	}
	if !e.received("alice", id) || e.received("alice", later) {
		t.Error("save receipted a different batch")
	}
	before := len(e.hs.urls())
	show := reviewRun(e, "--as", "alice", "inbox", "show", id, "--from", path)
	if show.code != ExitOK || !strings.Contains(show.stdout, "ordinary body") || len(e.hs.urls()) != before || e.received("alice", later) {
		t.Error("local show changed receipt or HTTP behavior")
	}
}

func TestSavedInboxDecodedSecretStringsAndCase(t *testing.T) {
	known := "synthetic502_quoted\"newline\nbackslash\\tail"
	var a App
	a.secrets.Add(known)
	value := map[string]any{"ordinary": []any{known, "unchanged"}}
	// EqualFold matches the existing (?i) JSON-field patterns, including the
	// Unicode long-s simple fold. Similar, non-secret field names stay intact.
	for _, key := range []string{"password", "PASSWORD", "paſſword", "access_token", "ACCESS_TOKEN", "token", "ToKeN", "refresh_token", "REFRESH_TOKEN"} {
		value[key] = "synthetic502_unknown\"newline\nend"
	}
	value["password_hint"] = "ordinary hint"
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	got := scrubInboxValue(decoded, a.secrets.Scrub).(map[string]any)
	for key := range value {
		if key == "ordinary" || key == "password_hint" {
			continue
		}
		if got[key] != "<redacted>" {
			t.Errorf("recognized field %q not redacted", key)
		}
	}
	if !reflect.DeepEqual(got["ordinary"], []any{"<redacted>", "unchanged"}) || got["password_hint"] != "ordinary hint" {
		t.Error("decoded known-string or ordinary metadata handling changed")
	}
}
