//go:build unix

package envelopeobserve

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A public BIP-340 test key from the pinned fixtures under testdata/.
const rosterBIPKey = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"

// newRosterTestKey returns a fresh public key identifier. The private half is
// dropped here and never reaches a log, an argument or a file.
func newRosterTestKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(public)
}

func rosterCode(err error) string {
	if err == nil {
		return ""
	}
	var refusal *RosterRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	var publication *RosterPublicationError
	if errors.As(err, &publication) {
		return publication.Code
	}
	return "not a roster outcome"
}

type rosterInputRow struct {
	MXID      string `json:"mxid"`
	AuthorKey string `json:"author_key"`
	Source    string `json:"source"`
}

func rosterInputJSON(t *testing.T, rows ...rosterInputRow) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Version string           `json:"version"`
		Entries []rosterInputRow `json:"entries"`
	}{"a2a-roster-input/1", rows})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// rosterTestProtection is the test side of the RosterProtection seam. It
// stands in for ownership only: the caller's files count as root-owned, and
// the directories above the fixture are accepted as they are. Modes and file
// types below the fixture are judged on the real file system.
type rosterTestProtection struct {
	root    bool
	noFiles bool
	above   []os.FileInfo
}

func (p rosterTestProtection) CallerMayPublish() bool { return p.root }

func (p rosterTestProtection) Protected(st os.FileInfo, directory bool) bool {
	for _, ancestor := range p.above {
		if os.SameFile(st, ancestor) {
			return directory && st.IsDir()
		}
	}
	if !directory && p.noFiles {
		return false
	}
	return st.Mode().Perm()&0o022 == 0 &&
		(directory && st.IsDir() || !directory && st.Mode().IsRegular())
}

// newRosterFixture returns a 0755 directory whose ancestors the seam accepts.
func newRosterFixture(t *testing.T) (string, rosterTestProtection) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := rosterTestProtection{root: true}
	for current := base; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			t.Fatal(err)
		}
		p.above = append(p.above, st)
		if current == filepath.Dir(current) {
			break
		}
	}
	dir := filepath.Join(base, "host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return dir, p
}

func rosterDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestRosterInputRefusals proves every invalid input returns its exact code and preserves the previous file beside valid controls.
// Duplicate member cases rely on the strict parser, so they share input_malformed.
func TestRosterInputRefusals(t *testing.T) {
	k1, k2 := newRosterTestKey(t), newRosterTestKey(t)
	alpha := rosterInputRow{"@alpha:example.invalid", k1, "console"}
	beta := rosterInputRow{"@beta:example.invalid", k2, "console"}
	entry := `{"mxid":"@alpha:example.invalid","author_key":"` + k1 + `","source":"console"}`
	doc := func(entries string) []byte {
		return []byte(`{"version":"a2a-roster-input/1","entries":[` + entries + `]}`)
	}
	with := func(change func(*rosterInputRow)) []byte {
		row := alpha
		change(&row)
		return rosterInputJSON(t, beta, row)
	}
	tooMany := make([]rosterInputRow, 0, 4097)
	for i := 0; i < 4097; i++ {
		tooMany = append(tooMany, rosterInputRow{fmt.Sprintf("@u%04d:example.invalid", i), k1, "console"})
	}

	controls := map[string][]byte{
		"raw-document":   doc(entry),
		"two-entries":    rosterInputJSON(t, alpha, beta),
		"bip340-key":     with(func(r *rosterInputRow) { r.AuthorKey = rosterBIPKey }),
		"source-empty":   with(func(r *rosterInputRow) { r.Source = "" }),
		"source-64":      with(func(r *rosterInputRow) { r.Source = strings.Repeat("s", 63) + " " }),
		"historical-id":  with(func(r *rosterInputRow) { r.MXID = "@Alpha_1=/+.-:Example.invalid:8448" }),
		"whitespace-doc": []byte(" {\n\"entries\": [" + entry + "],\n\"version\": \"a2a-roster-input/1\"}\n"),
	}
	for name, raw := range controls {
		t.Run("control/"+name, func(t *testing.T) {
			entries, err := ParseRosterInput(raw)
			if err != nil || len(entries) == 0 {
				t.Fatalf("valid input refused: %q", rosterCode(err))
			}
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			if err := os.WriteFile(out, []byte("previous roster bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if n, err := WriteRoster(raw, out, p); err != nil || n != len(entries) {
				t.Fatalf("valid publication refused: %d, %q", n, rosterCode(err))
			}
		})
	}
	entries, err := ParseRosterInput(rosterInputJSON(t, beta, alpha))
	if err != nil || len(entries) != 2 ||
		entries[0] != (RosterEntry{"@beta:example.invalid", k2, "console"}) ||
		entries[1] != (RosterEntry{"@alpha:example.invalid", k1, "console"}) {
		t.Fatalf("valid input not returned in order: %q", rosterCode(err))
	}

	for _, tc := range []struct {
		name string
		raw  []byte
		code string
	}{
		{"not-json", []byte(`{`), "input_malformed"},
		{"top-array", []byte(`[]`), "input_malformed"},
		{"trailing-value", append(doc(entry), []byte(`{}`)...), "input_malformed"},
		{"not-utf8", []byte("{\"version\":\"a2a-roster-input/1\",\"entries\":[],\"x\":\"\xff\"}"), "input_malformed"},
		{"duplicate-top-member", []byte(`{"version":"a2a-roster-input/1","version":"a2a-roster-input/1","entries":[` + entry + `]}`), "input_malformed"},
		{"duplicate-entry-member", doc(`{"mxid":"@alpha:example.invalid","mxid":"@alpha:example.invalid","author_key":"` + k1 + `","source":"console"}`), "input_malformed"},
		{"case-variant-top-member", []byte(`{"version":"a2a-roster-input/1","Entries":[],"entries":[` + entry + `]}`), "input_member_case"},
		{"case-variant-entry-member", doc(`{"MXID":"@alpha:example.invalid","author_key":"` + k1 + `","source":"console"}`), "input_member_case"},
		{"unknown-top-member", []byte(`{"version":"a2a-roster-input/1","facts":{},"entries":[` + entry + `]}`), "input_member_unknown"},
		{"unknown-entry-member", doc(`{"mxid":"@alpha:example.invalid","author_key":"` + k1 + `","source":"console","comment":"x"}`), "input_member_unknown"},
		{"missing-entries", []byte(`{"version":"a2a-roster-input/1"}`), "input_member_missing"},
		{"missing-source", doc(`{"mxid":"@alpha:example.invalid","author_key":"` + k1 + `"}`), "input_member_missing"},
		{"other-version", []byte(`{"version":"a2a-roster-input/2","entries":[` + entry + `]}`), "input_version"},
		{"roster-as-input", []byte(`{"version":"a2a-envelope-roster/1","entries":[` + entry + `]}`), "input_version"},
		{"version-number", []byte(`{"version":1,"entries":[` + entry + `]}`), "input_version"},
		{"entries-object", []byte(`{"version":"a2a-roster-input/1","entries":{}}`), "input_entries"},
		{"entry-string", doc(`"@alpha:example.invalid"`), "input_entries"},
		{"entries-empty", doc(``), "input_entries_count"},
		{"entries-4097", rosterInputJSON(t, tooMany...), "input_entries_count"},
		{"mxid-number", doc(`{"mxid":1,"author_key":"` + k1 + `","source":"console"}`), "input_member_type"},
		{"source-null", doc(`{"mxid":"@alpha:example.invalid","author_key":"` + k1 + `","source":null}`), "input_member_type"},
		{"mxid-no-sigil", with(func(r *rosterInputRow) { r.MXID = "alpha:example.invalid" }), "mxid_invalid"},
		{"mxid-localpart-only", with(func(r *rosterInputRow) { r.MXID = "@alpha" }), "mxid_invalid"},
		{"mxid-empty-local", with(func(r *rosterInputRow) { r.MXID = "@:example.invalid" }), "mxid_invalid"},
		{"mxid-empty-server", with(func(r *rosterInputRow) { r.MXID = "@alpha:" }), "mxid_invalid"},
		{"mxid-space", with(func(r *rosterInputRow) { r.MXID = "@al pha:example.invalid" }), "mxid_invalid"},
		{"mxid-control", with(func(r *rosterInputRow) { r.MXID = "@alpha\n:example.invalid" }), "mxid_invalid"},
		{"mxid-non-ascii", with(func(r *rosterInputRow) { r.MXID = "@alph\u00e4:example.invalid" }), "mxid_invalid"},
		{"mxid-256-bytes", with(func(r *rosterInputRow) { r.MXID = "@" + strings.Repeat("a", 239) + ":example.invalid" }), "mxid_invalid"},
		{"mxid-duplicate", rosterInputJSON(t, alpha, beta, rosterInputRow{"@alpha:example.invalid", k2, "console"}), "mxid_duplicate"},
		{"mxid-duplicate-same-key", rosterInputJSON(t, alpha, alpha), "mxid_duplicate"},
		{"mxid-case-variant", rosterInputJSON(t, alpha, rosterInputRow{"@Alpha:example.invalid", k2, "console"}), "mxid_case_variant"},
		{"key-other-type", with(func(r *rosterInputRow) { r.AuthorKey = "x25519:" + strings.TrimPrefix(k1, "ed25519:") }), "key_type"},
		{"key-type-case", with(func(r *rosterInputRow) { r.AuthorKey = "ED25519:" + strings.TrimPrefix(k1, "ed25519:") }), "key_type"},
		{"key-no-type", with(func(r *rosterInputRow) { r.AuthorKey = strings.TrimPrefix(k1, "ed25519:") }), "key_type"},
		{"key-not-a-key", with(func(r *rosterInputRow) { r.AuthorKey = "ed25519:other" }), "key_invalid"},
		{"source-del", with(func(r *rosterInputRow) { r.Source = "console\x7f" }), "source_invalid"},
		{"source-65", with(func(r *rosterInputRow) { r.Source = strings.Repeat("s", 65) }), "source_invalid"},
		{"source-newline", with(func(r *rosterInputRow) { r.Source = "console\nline" }), "source_invalid"},
		{"source-non-ascii", with(func(r *rosterInputRow) { r.Source = "cons\u00f6le" }), "source_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := ParseRosterInput(tc.raw)
			if got := rosterCode(err); got != tc.code {
				t.Fatalf("code %q, want %q", got, tc.code)
			}
			if entries != nil {
				t.Fatal("a refused input returned entries")
			}
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			const previous = "previous roster bytes\n"
			if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			if n, err := WriteRoster(tc.raw, out, p); n != 0 || rosterCode(err) != tc.code {
				t.Fatalf("refused publication = %d, %q; want 0, %q", n, rosterCode(err), tc.code)
			}
			if got, err := os.ReadFile(out); err != nil || string(got) != previous {
				t.Fatalf("input refusal changed the previous roster: %q, %v", got, err)
			}
			if names := rosterDirNames(t, dir); len(names) != 1 || names[0] != "roster.json" {
				t.Fatalf("directory after input refusal: %v", names)
			}
		})
	}
}

// TestRosterExistingFacts proves non-empty existing facts refuse replacement before publication beside an empty-facts replacement control.
func TestRosterExistingFacts(t *testing.T) {
	key := newRosterTestKey(t)
	input := rosterInputJSON(t, rosterInputRow{"@alpha:example.invalid", key, "console"})
	for _, tc := range []struct {
		name, facts, code string
	}{
		{"empty-control", `{}`, ""},
		{"audience", `{"audience":"installation"}`, "existing_facts"},
		{"empty-journal", `{"seen":{}}`, "existing_facts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, p := newRosterFixture(t)
			out := filepath.Join(dir, "roster.json")
			previous := `{"authors":{"@previous:example.invalid":"` + key + `"},"facts":` + tc.facts + `,"version":"a2a-envelope-roster/1"}`
			if _, err := ParseRoster([]byte(previous)); err != nil {
				t.Fatal("existing-roster fixture is not reader-valid")
			}
			if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			n, err := WriteRoster(input, out, p)
			if got := rosterCode(err); got != tc.code {
				t.Fatalf("code %q, want %q", got, tc.code)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code != "" {
				if n != 0 || string(got) != previous || !os.SameFile(before, after) {
					t.Fatal("existing_facts changed the previous file")
				}
			} else {
				want := `{"authors":{"@alpha:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
				if n != 1 || string(got) != want || os.SameFile(before, after) {
					t.Fatal("empty-facts control was not replaced by rename")
				}
			}
			if names := rosterDirNames(t, dir); len(names) != 1 || names[0] != "roster.json" {
				t.Fatalf("directory after existing-facts check: %v", names)
			}
		})
	}
}

// TestRosterWritePublishes proves publication uses the exact reader schema and canonical bytes with no leftover temporary.
// Ownership is supplied by the seam; mode and layout are real.
func TestRosterWritePublishes(t *testing.T) {
	k1, k2 := newRosterTestKey(t), newRosterTestKey(t)
	dir, p := newRosterFixture(t)
	out := filepath.Join(dir, "roster.json")
	input := rosterInputJSON(t,
		rosterInputRow{"@beta:example.invalid", k2, "console"},
		rosterInputRow{"@alpha:example.invalid", k1, "paper ledger, page 3"})
	n, err := WriteRoster(input, out, p)
	if err != nil || n != 2 {
		t.Fatalf("WriteRoster = %d, %q", n, rosterCode(err))
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"authors":{"@alpha:example.invalid":"` + k1 + `","@beta:example.invalid":"` + k2 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	if string(got) != want {
		t.Fatalf("published bytes:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(got), "console") || strings.Contains(string(got), "ledger") {
		t.Fatal("the source label reached the roster")
	}
	st, err := os.Lstat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != 0o644 {
		t.Fatalf("published mode %v", st.Mode())
	}
	if names := rosterDirNames(t, dir); len(names) != 1 || names[0] != "roster.json" {
		t.Fatalf("directory after publication: %v", names)
	}
	parsed, err := ParseRoster(got)
	if err != nil {
		t.Fatal("the reader's parser refuses the published roster")
	}
	if len(parsed.authors) != 2 || parsed.authors["@alpha:example.invalid"] != k1 || parsed.authors["@beta:example.invalid"] != k2 {
		t.Fatal("the reader loads a different sender map")
	}
}

// TestRosterWriteRefusalsKeepPrevious proves pre-rename refusals preserve the previous bytes and directory beside a replacing control.
// Ownership cases (not-root, result-unprotected and ancestors above the fixture)
// use the seam; path, symlink, mode and directory cases use the real file system.
func TestRosterWriteRefusalsKeepPrevious(t *testing.T) {
	k1, k2 := newRosterTestKey(t), newRosterTestKey(t)
	alpha := rosterInputRow{"@alpha:example.invalid", k1, "console"}
	beta := rosterInputRow{"@beta:example.invalid", k2, "console"}
	const previous = "previous roster bytes\n"
	published := `{"authors":{"@alpha:example.invalid":"` + k1 + `","@beta:example.invalid":"` + k2 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`

	type rosterCase struct {
		dir, out     string
		input        []byte
		p            rosterTestProtection
		mode         os.FileMode
		encode       func(map[string]string) ([]byte, error)
		beforeRename func() error
	}
	for _, tc := range []struct {
		name     string
		code     string
		userOnly bool
		prep     func(t *testing.T, c *rosterCase)
	}{
		{"control", "", false, func(t *testing.T, c *rosterCase) {}},
		{"relative-out", "out_path", false, func(t *testing.T, c *rosterCase) { c.out = "roster.json" }},
		{"unclean-out", "out_path", false, func(t *testing.T, c *rosterCase) { c.out = c.dir + "/../host/roster.json" }},
		{"symlink-out", "out_symlink", false, func(t *testing.T, c *rosterCase) {
			c.out = filepath.Join(c.dir, "link.json")
			if err := os.Symlink(filepath.Join(c.dir, "roster.json"), c.out); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling-symlink-out", "out_symlink", false, func(t *testing.T, c *rosterCase) {
			c.out = filepath.Join(c.dir, "link.json")
			if err := os.Symlink(filepath.Join(c.dir, "absent.json"), c.out); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory-out", "out_not_regular", false, func(t *testing.T, c *rosterCase) {
			c.out = filepath.Join(c.dir, "sub")
			if err := os.Mkdir(c.out, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-writable-directory", "out_unprotected", false, func(t *testing.T, c *rosterCase) { c.mode = 0o775 }},
		{"world-writable-directory", "out_unprotected", false, func(t *testing.T, c *rosterCase) { c.mode = 0o757 }},
		{"symlinked-directory", "out_unprotected", false, func(t *testing.T, c *rosterCase) {
			alias := filepath.Join(filepath.Dir(c.dir), "alias")
			if err := os.Symlink(c.dir, alias); err != nil {
				t.Fatal(err)
			}
			c.out = filepath.Join(alias, "roster.json")
		}},
		{"missing-directory", "out_unprotected", false, func(t *testing.T, c *rosterCase) {
			c.out = filepath.Join(c.dir, "missing", "roster.json")
		}},
		{"not-root", "not_root", false, func(t *testing.T, c *rosterCase) { c.p.root = false }},
		{"result-unprotected", "result_unprotected", false, func(t *testing.T, c *rosterCase) { c.p.noFiles = true }},
		{"duplicate-mxid", "mxid_duplicate", false, func(t *testing.T, c *rosterCase) {
			c.input = rosterInputJSON(t, alpha, beta, rosterInputRow{"@alpha:example.invalid", k2, "console"})
		}},
		{"duplicate-mxid-same-key", "mxid_duplicate", false, func(t *testing.T, c *rosterCase) {
			c.input = rosterInputJSON(t, alpha, beta, alpha)
		}},
		{"key-other-type", "key_type", false, func(t *testing.T, c *rosterCase) {
			c.input = rosterInputJSON(t, alpha, rosterInputRow{"@beta:example.invalid", "x25519:" + strings.TrimPrefix(k2, "ed25519:"), "console"})
		}},
		{"unknown-member", "input_member_unknown", false, func(t *testing.T, c *rosterCase) {
			c.input = []byte(`{"version":"a2a-roster-input/1","entries":[],"facts":{}}`)
		}},
		{"writer-drops-entry", "selfcheck_mismatch", false, func(t *testing.T, c *rosterCase) {
			c.encode = func(authors map[string]string) ([]byte, error) {
				kept := map[string]string{}
				for sender, key := range authors {
					if sender != "@beta:example.invalid" {
						kept[sender] = key
					}
				}
				return encodeRoster(kept)
			}
		}},
		{"writer-swaps-key", "selfcheck_mismatch", false, func(t *testing.T, c *rosterCase) {
			c.encode = func(authors map[string]string) ([]byte, error) {
				return encodeRoster(map[string]string{"@alpha:example.invalid": k2, "@beta:example.invalid": k1})
			}
		}},
		{"writer-emits-unreadable", "selfcheck_failed", false, func(t *testing.T, c *rosterCase) {
			c.encode = func(map[string]string) ([]byte, error) { return []byte(`{}`), nil }
		}},
		{"interrupted-before-rename", "publish_interrupted", false, func(t *testing.T, c *rosterCase) {
			c.beforeRename = func() error { return errors.New("injected fault") }
		}},
		{"directory-loosened-before-rename", "result_unprotected", false, func(t *testing.T, c *rosterCase) {
			c.beforeRename = func() error { return os.Chmod(c.dir, 0o775) }
		}},
		{"file-loosened-before-rename", "result_unprotected", false, func(t *testing.T, c *rosterCase) {
			c.beforeRename = func() error {
				for _, name := range rosterDirNames(t, c.dir) {
					if strings.HasPrefix(name, ".roster.json.tmp-") {
						return os.Chmod(filepath.Join(c.dir, name), 0o664)
					}
				}
				t.Fatal("temporary file absent at fault point")
				return nil
			}
		}},
		{"directory-not-writable", "publish_failed", true, func(t *testing.T, c *rosterCase) { c.mode = 0o555 }},
		{"directory-not-searchable", "out_unreadable", true, func(t *testing.T, c *rosterCase) { c.mode = 0o600 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.userOnly && os.Geteuid() == 0 {
				t.Skip("root ignores directory modes; run as a non-root user")
			}
			dir, p := newRosterFixture(t)
			final := filepath.Join(dir, "roster.json")
			if err := os.WriteFile(final, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			c := &rosterCase{dir: dir, out: final, input: rosterInputJSON(t, beta, alpha), p: p}
			tc.prep(t, c)
			before := rosterDirNames(t, dir)
			if c.mode != 0 {
				if err := os.Chmod(dir, c.mode); err != nil {
					t.Fatal(err)
				}
			}
			w := newRosterWriter(c.p)
			if c.encode != nil {
				w.encode = c.encode
			}
			w.beforeRename = c.beforeRename
			n, err := w.write(c.input, c.out)
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if got := rosterCode(err); got != tc.code {
				t.Fatalf("code %q, want %q", got, tc.code)
			}
			after := rosterDirNames(t, dir)
			if strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("directory changed: %v -> %v", before, after)
			}
			got, err := os.ReadFile(final)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code == "" {
				if n != 2 || string(got) != published {
					t.Fatalf("control did not replace the roster: %d %s", n, got)
				}
				return
			}
			if n != 0 || string(got) != previous {
				t.Fatalf("a refusal changed the previous roster: %d %q", n, got)
			}
		})
	}
}

// TestRosterPublishReplacesByRename proves publication replaces the inode and an open reader retains the previous bytes.
func TestRosterPublishReplacesByRename(t *testing.T) {
	k1 := newRosterTestKey(t)
	dir, p := newRosterFixture(t)
	out := filepath.Join(dir, "roster.json")
	const previous = "previous roster bytes\n"
	if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	old, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRoster(rosterInputJSON(t, rosterInputRow{"@alpha:example.invalid", k1, "console"}), out, p); err != nil {
		t.Fatalf("WriteRoster: %q", rosterCode(err))
	}
	current, err := os.Lstat(out)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(old, current) {
		t.Fatal("the previous file was rewritten in place")
	}
	stale, err := io.ReadAll(held)
	if err != nil || string(stale) != previous {
		t.Fatalf("an open reader of the previous roster saw %q, %v", stale, err)
	}
	fresh, err := os.ReadFile(out)
	want := `{"authors":{"@alpha:example.invalid":"` + k1 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	if err != nil || string(fresh) != want {
		t.Fatalf("published bytes %q, %v", fresh, err)
	}
	if names := rosterDirNames(t, dir); len(names) != 1 || names[0] != "roster.json" {
		t.Fatalf("directory after publication: %v", names)
	}
}

func assertRosterPublication(t *testing.T, dir, out, wantBytes string, err error, code string) {
	t.Helper()
	if code != "" {
		var publication *RosterPublicationError
		var refusal *RosterRefusal
		if !errors.As(err, &publication) || errors.As(err, &refusal) {
			t.Fatal("post-rename outcome was not a publication report")
		}
		wantMessage := "host roster published: " + code + ": the new roster is at the final path"
		if code == "published_unsynced" {
			wantMessage += "; directory sync failed"
		} else {
			wantMessage += "; the reader will treat it as unavailable until the operator fixes the protection"
		}
		if err.Error() != wantMessage {
			t.Fatalf("publication message %q, want %q", err.Error(), wantMessage)
		}
	}
	got, readErr := os.ReadFile(out)
	if readErr != nil || string(got) != wantBytes {
		t.Fatalf("published bytes %q, %v", got, readErr)
	}
	if _, parseErr := ParseRoster(got); parseErr != nil {
		t.Fatal("new bytes do not satisfy the reader schema")
	}
	if names := rosterDirNames(t, dir); len(names) != 1 || names[0] != "roster.json" {
		t.Fatalf("directory after publication: %v", names)
	}
}

// TestRosterPublishedUnprotected proves post-rename protection failures retain new bytes without a temporary and the reader refuses the unprotected result.
func TestRosterPublishedUnprotected(t *testing.T) {
	key := newRosterTestKey(t)
	input := rosterInputJSON(t, rosterInputRow{"@alpha:example.invalid", key, "console"})
	previous := `{"authors":{"@previous:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	wantBytes := `{"authors":{"@alpha:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	for _, target := range []string{"file", "ancestor"} {
		t.Run(target, func(t *testing.T) {
			for _, loosen := range []bool{false, true} {
				dir, p := newRosterFixture(t)
				out := filepath.Join(dir, "roster.json")
				if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
					t.Fatal(err)
				}
				changed, mode, directory := out, os.FileMode(0o664), false
				if target == "ancestor" {
					changed, mode, directory = dir, 0o775, true
				}
				w := newRosterWriter(p)
				w.syncDir = func(path string) error {
					if err := syncRosterDir(path); err != nil {
						return err
					}
					if loosen {
						return os.Chmod(changed, mode)
					}
					return nil
				}
				_, err := w.write(input, out)
				want := ""
				if loosen {
					want = "published_unprotected"
				}
				if got := rosterCode(err); got != want {
					t.Fatalf("loosen=%v: code %q, want %q", loosen, got, want)
				}
				assertRosterPublication(t, dir, out, wantBytes, err, want)
				st, statErr := os.Lstat(changed)
				if statErr != nil {
					t.Fatal(statErr)
				}
				// Only ownership is synthetic; modes come from the published paths.
				if got := protectedInfo(fileInfo{st.Mode(), 0}, directory); got != (!loosen && os.Geteuid() != 0) {
					t.Fatalf("reader protection decision %v for loosen=%v", got, loosen)
				}
				if loosen {
					if _, loadErr := Load(out); loadErr != ErrRoster {
						t.Fatal("reader accepted the unprotected publication")
					}
				}
			}
		})
	}
}

// TestRosterPublishedUnsynced proves a failed post-rename directory sync reports publication with the new bytes and no temporary beside a synced control.
func TestRosterPublishedUnsynced(t *testing.T) {
	key := newRosterTestKey(t)
	input := rosterInputJSON(t, rosterInputRow{"@alpha:example.invalid", key, "console"})
	previous := `{"authors":{"@previous:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	wantBytes := `{"authors":{"@alpha:example.invalid":"` + key + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	for _, fail := range []bool{false, true} {
		dir, p := newRosterFixture(t)
		out := filepath.Join(dir, "roster.json")
		if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
			t.Fatal(err)
		}
		w := newRosterWriter(p)
		synced := ""
		if fail {
			w.syncDir = func(path string) error { synced = path; return errors.New("injected fault") }
		}
		_, err := w.write(input, out)
		want := ""
		if fail {
			want = "published_unsynced"
		}
		if got := rosterCode(err); got != want {
			t.Fatalf("fail=%v: code %q, want %q", fail, got, want)
		}
		if fail && synced != dir {
			t.Fatalf("synced %q, want the roster directory", synced)
		}
		assertRosterPublication(t, dir, out, wantBytes, err, want)
	}
}

// TestHostRosterStorageAgreement proves unilateral reader or writer predicate drift fails the writer's shared-storage contract.
func TestHostRosterStorageAgreement(t *testing.T) {
	assertRosterStorageAgreement(t)
}

// TestHostRosterProtection proves only root-owned, non-group/other-writable files and directories pass the storage predicate.
// Synthetic file facts; no root-owned file is created.
func TestHostRosterProtection(t *testing.T) {
	p := HostRosterProtection()
	if got := p.CallerMayPublish(); got != (os.Geteuid() == 0) {
		t.Fatalf("CallerMayPublish = %v for euid %d", got, os.Geteuid())
	}
	for _, tc := range []struct {
		name      string
		info      fileInfo
		dir, want bool
	}{
		{"root-file", fileInfo{0o644, 0}, false, true},
		{"root-readonly-file", fileInfo{0o444, 0}, false, true},
		{"user-file", fileInfo{0o644, 1000}, false, false},
		{"user-readonly-file", fileInfo{0o444, 1000}, false, false},
		{"root-group-write", fileInfo{0o664, 0}, false, false},
		{"root-world-write", fileInfo{0o646, 0}, false, false},
		{"root-symlink", fileInfo{os.ModeSymlink | 0o444, 0}, false, false},
		{"root-fifo", fileInfo{os.ModeNamedPipe | 0o444, 0}, false, false},
		{"root-dir", fileInfo{os.ModeDir | 0o755, 0}, true, true},
		{"user-dir", fileInfo{os.ModeDir | 0o755, 1000}, true, false},
		{"group-writable-dir", fileInfo{os.ModeDir | 0o775, 0}, true, false},
		{"sticky-world-writable-dir", fileInfo{os.ModeDir | os.ModeSticky | 0o777, 0}, true, false},
		{"file-as-dir", fileInfo{0o644, 0}, true, false},
		{"dir-as-file", fileInfo{os.ModeDir | 0o755, 0}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Protected(tc.info, tc.dir); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// TestObserverAuthors proves the listing exposes only sorted sender, key type and fingerprint, with no rows when unavailable.
func TestObserverAuthors(t *testing.T) {
	k1 := newRosterTestKey(t)
	roster := `{"authors":{"@beta:example.invalid":"` + rosterBIPKey + `","@alpha:example.invalid":"` + k1 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	o, err := ParseRoster([]byte(roster))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := func(key string) string {
		sum := sha256.Sum256([]byte(key))
		return hex.EncodeToString(sum[:8])
	}
	rows := o.Authors()
	if len(rows) != 2 ||
		rows[0] != (RosterAuthor{"@alpha:example.invalid", "ed25519", fingerprint(k1)}) ||
		rows[1] != (RosterAuthor{"@beta:example.invalid", "bip340", fingerprint(rosterBIPKey)}) {
		t.Fatalf("rows: %+v", rows)
	}
	var unavailable *Observer
	if unavailable.Authors() != nil {
		t.Fatal("an unavailable roster listed authors")
	}
}
