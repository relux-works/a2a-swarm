//go:build unix

package envelopeobserve

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type rosterV2InputRow struct {
	rosterV2TestRow
	Source string `json:"source"`
}

func rosterV2Input(t *testing.T, rows []rosterV2TestRow) []byte {
	t.Helper()
	entries := make([]rosterV2InputRow, len(rows))
	for i, row := range rows {
		entries[i] = rosterV2InputRow{rosterV2TestRow: row, Source: "public record"}
	}
	return rosterV2Marshal(t, map[string]any{"version": rosterInputVersionV2, "entries": entries})
}

func rosterV2Preserved(t *testing.T, dir, out string, before []byte, inode os.FileInfo) {
	t.Helper()
	after, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("previous roster bytes changed: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil || !os.SameFile(inode, st) {
		t.Fatalf("previous roster inode changed: %v", err)
	}
	if names := rosterDirNames(t, dir); !reflect.DeepEqual(names, []string{"roster.json"}) {
		t.Fatalf("temporary or extra entries remain: %v", names)
	}
}

// TestRosterV2WriterRoundTrip proves canonical version 2 output preserves both key bindings and all publishers, omits source, and requires explicit version selection.
func TestRosterV2WriterRoundTrip(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	rows[0].Publishers = append(rows[0].Publishers, "@z:example.invalid")
	dir, p := newRosterFixture(t)
	out := filepath.Join(dir, "roster.json")
	input := rosterV2Input(t, rows)
	if n, err := WriteRosterV2(input, out, p); err != nil || n != 2 {
		t.Fatalf("valid version 2 write: count=%d code=%q", n, rosterCode(err))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"authors":[{"author":"beta","author_key":"` + rows[1].AuthorKey + `","publishers":["@bridge:example.invalid"]},{"author":"alpha","author_key":"` + rows[0].AuthorKey + `","publishers":["@bridge:example.invalid","@z:example.invalid"]}],"facts":{},"version":"a2a-envelope-roster/2"}`
	if !bytes.Equal(data, []byte(want)) || bytes.Contains(data, []byte("public record")) || bytes.Contains(data, []byte(`"source"`)) {
		t.Fatal("version 2 output differs from canonical public-only bytes")
	}
	o, err := ParseRoster(data)
	if err != nil || len(o.byAuthor) != 2 || o.byAuthor[rows[0].AuthorKey].Author != "alpha" || len(o.byAuthor[rows[0].AuthorKey].Publishers) != 2 {
		t.Fatalf("version 2 reader round trip failed: %v", err)
	}
	view := o.Authors()
	if len(view) != 3 || view[0].MXID != sharedRosterPublisher || view[0].KeyType != "bip340" || view[1].MXID != sharedRosterPublisher || view[1].KeyType != "ed25519" || view[2].MXID != "@z:example.invalid" {
		t.Fatalf("publisher/key view lost or reordered rows: %v", view)
	}
	inode, err := os.Stat(out)
	if err != nil || inode.Mode().Perm() != 0o644 {
		t.Fatalf("published mode: %v", err)
	}
	if n, err := WriteRoster(input, out, p); n != 0 || rosterCode(err) != "input_version" {
		t.Fatalf("default writer accepted version 2 input: count=%d code=%q", n, rosterCode(err))
	}
	rosterV2Preserved(t, dir, out, data, inode)
	legacy := rosterInputJSON(t, rosterInputRow{MXID: sharedRosterPublisher, AuthorKey: rows[0].AuthorKey, Source: "console"})
	if n, err := WriteRosterV2(legacy, out, p); n != 0 || rosterCode(err) != "input_version" {
		t.Fatalf("version 2 writer accepted version 1 input: count=%d code=%q", n, rosterCode(err))
	}
	rosterV2Preserved(t, dir, out, data, inode)
}

// TestRosterV2InputRefusals proves invalid operator rows return fixed refusal codes and preserve the previous file beside successful source and shared-carrier controls.
func TestRosterV2InputRefusals(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	valid := rosterV2Input(t, rows)
	change := func(f func(map[string]any, map[string]any)) []byte {
		var doc map[string]any
		if err := json.Unmarshal(valid, &doc); err != nil {
			t.Fatal(err)
		}
		f(doc, doc["entries"].([]any)[0].(map[string]any))
		return rosterV2Marshal(t, doc)
	}
	for _, source := range []string{"", strings.Repeat("s", 64)} {
		dir, p := newRosterFixture(t)
		if n, err := WriteRosterV2(change(func(_ map[string]any, row map[string]any) { row["source"] = source }), filepath.Join(dir, "roster.json"), p); err != nil || n != 2 {
			t.Fatalf("valid source/shared-carrier control: count=%d code=%q", n, rosterCode(err))
		}
	}
	for _, tc := range []struct {
		name, code string
		change     func(map[string]any, map[string]any)
	}{
		{"duplicate-key", "key_duplicate", func(doc, row map[string]any) {
			doc["entries"].([]any)[1].(map[string]any)["author_key"] = row["author_key"]
		}},
		{"case-member", "input_member_case", func(_ map[string]any, row map[string]any) { row["AUTHOR"] = row["author"]; delete(row, "author") }},
		{"unknown-member", "input_member_unknown", func(_ map[string]any, row map[string]any) { row["extra"] = true }},
		{"missing-member", "input_member_missing", func(_ map[string]any, row map[string]any) { delete(row, "source") }},
		{"member-type", "input_member_type", func(_ map[string]any, row map[string]any) { row["source"] = 1 }},
		{"empty-label", "author_invalid", func(_ map[string]any, row map[string]any) { row["author"] = "" }},
		{"label-space", "author_invalid", func(_ map[string]any, row map[string]any) { row["author"] = "alpha beta" }},
		{"publishers-object", "publishers_invalid", func(_ map[string]any, row map[string]any) { row["publishers"] = map[string]any{} }},
		{"publishers-empty", "publishers_count", func(_ map[string]any, row map[string]any) { row["publishers"] = []any{} }},
		{"publishers-65", "publishers_count", func(_ map[string]any, row map[string]any) { row["publishers"] = make([]string, 65) }},
		{"publisher-duplicate", "publisher_duplicate", func(_ map[string]any, row map[string]any) {
			row["publishers"] = []string{sharedRosterPublisher, sharedRosterPublisher}
		}},
		{"publisher-type", "input_member_type", func(_ map[string]any, row map[string]any) { row["publishers"] = []any{true} }},
		{"mxid-invalid", "mxid_invalid", func(_ map[string]any, row map[string]any) { row["publishers"] = []string{"bridge:example.invalid"} }},
		{"key-type", "key_type", func(_ map[string]any, row map[string]any) { row["author_key"] = "p256:AAAA" }},
		{"key-padding", "key_invalid", func(_ map[string]any, row map[string]any) { row["author_key"] = rows[0].AuthorKey + "=" }},
		{"source-65", "source_invalid", func(_ map[string]any, row map[string]any) { row["source"] = strings.Repeat("s", 65) }},
		{"source-control", "source_invalid", func(_ map[string]any, row map[string]any) { row["source"] = "console\n" }},
		{"version", "input_version", func(doc, _ map[string]any) { doc["version"] = "a2a-roster-input/1" }},
		{"empty-entries", "input_entries_count", func(doc, _ map[string]any) { doc["entries"] = []any{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			if _, err := WriteRosterV2(valid, out, p); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(out)
			inode, _ := os.Stat(out)
			if n, err := WriteRosterV2(change(tc.change), out, p); n != 0 || rosterCode(err) != tc.code {
				t.Fatalf("invalid operator row: count=%d code=%q, want %q", n, rosterCode(err), tc.code)
			}
			rosterV2Preserved(t, dir, out, before, inode)
		})
	}
}

// TestRosterV2WriterSelfCheck proves reader rejection, key loss, label/publisher mutation and invented facts are refused before publication beside the real encoder control.
func TestRosterV2WriterSelfCheck(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	rows[0].Publishers = append(rows[0].Publishers, "@backup:example.invalid")
	input := rosterV2Input(t, rows)
	for _, tc := range []struct {
		name, code string
		encode     func([]rosterKeyEntry) ([]byte, error)
	}{
		{"drop-entry", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) { return encodeRosterV2(entries[1:]) }},
		{"mutate-label", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) {
			entries[0].Author = "changed"
			return encodeRosterV2(entries)
		}},
		{"mutate-publisher", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) {
			entries[0].Publishers[0] = "@other:example.invalid"
			return encodeRosterV2(entries)
		}},
		{"drop-publisher", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) {
			entries[0].Publishers = entries[0].Publishers[:1]
			return encodeRosterV2(entries)
		}},
		{"duplicate-key", "selfcheck_failed", func(entries []rosterKeyEntry) ([]byte, error) { return encodeRosterV2(append(entries, entries[0])) }},
		{"wrong-version", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) {
			return encodeRoster(map[string]string{sharedRosterPublisher: entries[0].AuthorKey})
		}},
		{"malformed", "selfcheck_failed", func([]rosterKeyEntry) ([]byte, error) { return []byte(`{`), nil }},
		{"invent-facts", "selfcheck_mismatch", func(entries []rosterKeyEntry) ([]byte, error) {
			data, err := encodeRosterV2(entries)
			return bytes.Replace(data, []byte(`"facts":{}`), []byte(`"facts":{"audience":"invented"}`), 1), err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			if _, err := WriteRosterV2(input, out, p); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(out)
			inode, _ := os.Stat(out)
			w := newRosterWriter(p)
			w.encodeV2 = tc.encode
			if n, err := w.writeV2(input, out); n != 0 || rosterCode(err) != tc.code {
				t.Fatalf("self-check: count=%d code=%q, want %q", n, rosterCode(err), tc.code)
			}
			rosterV2Preserved(t, dir, out, before, inode)
		})
	}
}

// TestRosterV2ExistingFacts proves both writer versions preserve nonempty facts in a reader-valid 65-author version 2 roster beside replaceable empty-facts controls.
func TestRosterV2ExistingFacts(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	large := append([]rosterV2TestRow{}, rows...)
	for len(large) < 65 {
		large = append(large, rosterV2TestRow{AuthorKey: rosterV2PublicKey(t), Author: "gamma", Publishers: []string{sharedRosterPublisher}})
	}
	previous := rosterV2Document(t, large, map[string]any{"audience": "installation"})
	if _, err := ParseRoster(previous); err != nil {
		t.Fatalf("existing facts control is not reader-valid: %v", err)
	}
	legacy := rosterInputJSON(t, rosterInputRow{MXID: sharedRosterPublisher, AuthorKey: rows[0].AuthorKey, Source: "console"})
	for _, tc := range []struct {
		name  string
		input []byte
		write func([]byte, string, RosterProtection) (int, error)
	}{{"default", legacy, WriteRoster}, {"version-two", rosterV2Input(t, rows), WriteRosterV2}} {
		t.Run(tc.name, func(t *testing.T) {
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			if err := os.WriteFile(out, rosterV2Document(t, large, map[string]any{}), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := tc.write(tc.input, out, p); err != nil {
				t.Fatalf("empty-facts replacement control: %v", err)
			}
			if err := os.WriteFile(out, previous, 0o644); err != nil {
				t.Fatal(err)
			}
			inode, _ := os.Stat(out)
			if n, err := tc.write(tc.input, out, p); n != 0 || rosterCode(err) != "existing_facts" {
				t.Fatalf("existing facts: count=%d code=%q", n, rosterCode(err))
			}
			rosterV2Preserved(t, dir, out, previous, inode)
		})
	}
}

// TestRosterV2Publication proves version 2 shares pre-rename preservation and distinct post-rename publication reports with the existing publisher.
func TestRosterV2Publication(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	input := rosterV2Input(t, rows)
	for _, code := range []string{"not_root", "publish_interrupted", "published_unsynced", "published_unprotected"} {
		t.Run(code, func(t *testing.T) {
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			if _, err := WriteRosterV2(input, out, p); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(out)
			inode, _ := os.Stat(out)
			w := newRosterWriter(p)
			switch code {
			case "not_root":
				p.root = false
				w.protection = p
			case "publish_interrupted":
				w.beforeRename = func() error { return errors.New("interruption") }
			case "published_unsynced":
				w.syncDir = func(string) error { return errors.New("sync failure") }
			case "published_unprotected":
				w.syncDir = func(string) error { return os.Chmod(out, 0o666) }
			}
			changed := append([]rosterV2TestRow{}, rows...)
			changed[0].Author = "changed"
			n, err := w.writeV2(rosterV2Input(t, changed), out)
			if n != 0 || rosterCode(err) != code {
				t.Fatalf("publication outcome: count=%d code=%q, want %q", n, rosterCode(err), code)
			}
			if !strings.HasPrefix(code, "published_") {
				rosterV2Preserved(t, dir, out, before, inode)
				return
			}
			var publication *RosterPublicationError
			var refusal *RosterRefusal
			if !errors.As(err, &publication) || errors.As(err, &refusal) || !strings.Contains(err.Error(), "the new roster is at the final path") {
				t.Fatalf("post-rename outcome misreported: %v", err)
			}
			data, readErr := os.ReadFile(out)
			if readErr != nil || bytes.Equal(data, before) {
				t.Fatalf("publication was rolled back or unreadable: %v", readErr)
			}
			o, parseErr := ParseRoster(data)
			if parseErr != nil || o.byAuthor[changed[0].AuthorKey].Author != "changed" {
				t.Fatalf("new version 2 bytes are not at the final path: %v", parseErr)
			}
		})
	}
}

// TestRosterV2WriterBounds proves 4096 authors publish through the version 2 reader while 4097 authors, duplicate members and oversized or over-depth input preserve that publication.
func TestRosterV2WriterBounds(t *testing.T) {
	rows := make([]rosterV2TestRow, maxRosterEntries+1)
	for i := range rows {
		rows[i] = rosterV2TestRow{AuthorKey: rosterV2PublicKey(t), Author: "alpha", Publishers: []string{sharedRosterPublisher}}
	}
	dir, p := newRosterFixture(t)
	out := filepath.Join(dir, "roster.json")
	if n, err := WriteRosterV2(rosterV2Input(t, rows[:maxRosterEntries]), out, p); err != nil || n != maxRosterEntries {
		t.Fatalf("4096-author writer control: count=%d code=%q", n, rosterCode(err))
	}
	before, _ := os.ReadFile(out)
	inode, _ := os.Stat(out)
	small := rosterV2Input(t, rows[:1])
	for _, tc := range []struct {
		name, code string
		raw        []byte
	}{
		{"4097-authors", "input_entries_count", rosterV2Input(t, rows)},
		{"duplicate-member", "input_malformed", bytes.Replace(small, []byte(`"version":`), []byte(`"version":"a2a-roster-input/2","version":`), 1)},
		{"oversized", "input_malformed", append(append([]byte{}, small...), bytes.Repeat([]byte(" "), maxRosterBytes)...)},
		{"over-depth", "input_malformed", []byte(strings.Repeat("[", 17) + strings.Repeat("]", 17))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n, err := WriteRosterV2(tc.raw, out, p); n != 0 || rosterCode(err) != tc.code {
				t.Fatalf("bounded input: count=%d code=%q, want %q", n, rosterCode(err), tc.code)
			}
			rosterV2Preserved(t, dir, out, before, inode)
		})
	}
}
