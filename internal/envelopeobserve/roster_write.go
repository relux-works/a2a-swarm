package envelopeobserve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/swarma-envelope/record"
)

// RosterInputVersion names the default version 1 operator input document.
const RosterInputVersion = "a2a-roster-input/1"

const (
	rosterVersion     = "a2a-envelope-roster/1"
	maxRosterEntries  = 4096
	maxRosterSource   = 64
	rosterFingerprint = 16
)

// RosterRefusal rejects provisioning before publication. Code is a fixed local
// name; it never carries input text, paths or key material.
type RosterRefusal struct{ Code string }

func (e *RosterRefusal) Error() string { return "host roster refused: " + e.Code }

func refuseRoster(code string) error { return &RosterRefusal{Code: code} }

// RosterPublicationError reports a publication that happened but needs operator
// attention. It is not an input refusal, and no rollback is attempted.
type RosterPublicationError struct{ Code string }

func (e *RosterPublicationError) Error() string {
	message := "host roster published: " + e.Code + ": the new roster is at the final path"
	switch e.Code {
	case "published_unsynced":
		return message + "; directory sync failed"
	case "published_unprotected":
		return message + "; the reader will treat it as unavailable until the operator fixes the protection"
	}
	return message
}

func publishedRoster(code string) error { return &RosterPublicationError{Code: code} }

// RosterEntry is one operator assertion: this key speaks for this exact sender.
// Source is an operator label; the frozen roster schema does not carry it.
type RosterEntry struct {
	MXID      string
	AuthorKey string
	Source    string
}

// RosterAuthor is the printable view of one loaded roster row.
type RosterAuthor struct {
	MXID        string
	KeyType     string
	Fingerprint string
}

// RosterProtection is the single seam for the ownership facts of a published
// roster. Production is HostRosterProtection; there is no path around it.
type RosterProtection interface {
	// CallerMayPublish reports whether this process can create a file that
	// the reader accepts (root-owned).
	CallerMayPublish() bool
	// Protected applies the reader's storage rule to one path entry:
	// root-owned, no group/other write, a directory or an ordinary file.
	Protected(st os.FileInfo, directory bool) bool
}

// HostRosterProtection returns the production ownership predicate.
func HostRosterProtection() RosterProtection { return hostRosterProtection{} }

// Authors lists what the reader loaded, sorted by sender. The fingerprint is
// the first 16 hex digits of SHA-256 over the complete public key identifier.
func (o *Observer) Authors() []RosterAuthor {
	if o == nil {
		return nil
	}
	if o.byAuthor != nil {
		return o.publisherAuthors()
	}
	rows := make([]RosterAuthor, 0, len(o.authors))
	for sender, key := range o.authors {
		kind, _, _ := strings.Cut(key, ":")
		sum := sha256.Sum256([]byte(key))
		rows = append(rows, RosterAuthor{MXID: sender, KeyType: kind, Fingerprint: hex.EncodeToString(sum[:])[:rosterFingerprint]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].MXID < rows[j].MXID })
	return rows
}

// closedMembers refuses case variants, unknown members and missing members.
// Duplicate member names are already refused by the strict parser.
func closedMembers(m map[string]any, names ...string) error {
	unknown := false
	for member := range m {
		exact, variant := false, false
		for _, name := range names {
			if member == name {
				exact = true
			} else if strings.EqualFold(member, name) {
				variant = true
			}
		}
		if exact {
			continue
		}
		if variant {
			return refuseRoster("input_member_case")
		}
		unknown = true
	}
	if unknown {
		return refuseRoster("input_member_unknown")
	}
	for _, name := range names {
		if _, present := m[name]; !present {
			return refuseRoster("input_member_missing")
		}
	}
	return nil
}

// validRosterSender is exactly the reader's admission rule for a sender.
func validRosterSender(sender string) bool {
	if !matrix.ValidVerificationID(sender, '@') {
		return false
	}
	for i := 0; i < len(sender); i++ {
		if sender[i] < 0x21 || sender[i] > 0x7e {
			return false
		}
	}
	return true
}

func validRosterSource(source string) bool {
	if len(source) > maxRosterSource {
		return false
	}
	for i := 0; i < len(source); i++ {
		if source[i] < 0x20 || source[i] > 0x7e {
			return false
		}
	}
	return true
}

// ParseRosterInput decodes one closed a2a-roster-input/1 document. Any
// refusal rejects the whole document; no partial entry list is returned.
func ParseRosterInput(raw []byte) ([]RosterEntry, error) {
	value, err := matrix.ParseStrictJSON(raw)
	if err != nil {
		return nil, refuseRoster("input_malformed")
	}
	doc, ok := value.(map[string]any)
	if !ok {
		return nil, refuseRoster("input_malformed")
	}
	if err := closedMembers(doc, "version", "entries"); err != nil {
		return nil, err
	}
	if version, ok := doc["version"].(string); !ok || version != RosterInputVersion {
		return nil, refuseRoster("input_version")
	}
	list, ok := doc["entries"].([]any)
	if !ok {
		return nil, refuseRoster("input_entries")
	}
	if len(list) == 0 || len(list) > maxRosterEntries {
		return nil, refuseRoster("input_entries_count")
	}
	entries := make([]RosterEntry, 0, len(list))
	folded := make(map[string]string, len(list))
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, refuseRoster("input_entries")
		}
		if err := closedMembers(row, "mxid", "author_key", "source"); err != nil {
			return nil, err
		}
		sender, senderOK := row["mxid"].(string)
		key, keyOK := row["author_key"].(string)
		source, sourceOK := row["source"].(string)
		if !senderOK || !keyOK || !sourceOK {
			return nil, refuseRoster("input_member_type")
		}
		if !validRosterSender(sender) {
			return nil, refuseRoster("mxid_invalid")
		}
		lower := strings.ToLower(sender)
		if previous, seen := folded[lower]; seen {
			if previous == sender {
				return nil, refuseRoster("mxid_duplicate")
			}
			return nil, refuseRoster("mxid_case_variant")
		}
		folded[lower] = sender
		if !strings.HasPrefix(key, "ed25519:") && !strings.HasPrefix(key, "bip340:") {
			return nil, refuseRoster("key_type")
		}
		if record.ValidateKey(key) != nil {
			return nil, refuseRoster("key_invalid")
		}
		if !validRosterSource(source) {
			return nil, refuseRoster("source_invalid")
		}
		entries = append(entries, RosterEntry{MXID: sender, AuthorKey: key, Source: source})
	}
	return entries, nil
}

func quoteRosterString(b *strings.Builder, s string) bool {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return true
}

// encodeRoster writes the frozen reader schema in one fixed spelling: compact
// JSON, members in byte order, senders in byte order, no trailing newline.
func encodeRoster(authors map[string]string) ([]byte, error) {
	senders := make([]string, 0, len(authors))
	for sender := range authors {
		senders = append(senders, sender)
	}
	sort.Strings(senders)
	var b strings.Builder
	b.WriteString(`{"authors":{`)
	for i, sender := range senders {
		if i > 0 {
			b.WriteByte(',')
		}
		if !quoteRosterString(&b, sender) {
			return nil, refuseRoster("output_unencodable")
		}
		b.WriteByte(':')
		if !quoteRosterString(&b, authors[sender]) {
			return nil, refuseRoster("output_unencodable")
		}
	}
	b.WriteString(`},"facts":{},"version":`)
	quoteRosterString(&b, rosterVersion)
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// checkRosterRoundTrip loads the encoded bytes through the reader's parser
// and requires the parsed sender map to equal the input map exactly.
func checkRosterRoundTrip(data []byte, want map[string]string) error {
	parsed, err := ParseRoster(data)
	if err != nil {
		return refuseRoster("selfcheck_failed")
	}
	if len(parsed.authors) != len(want) {
		return refuseRoster("selfcheck_mismatch")
	}
	for sender, key := range want {
		if got, present := parsed.authors[sender]; !present || got != key {
			return refuseRoster("selfcheck_mismatch")
		}
	}
	return nil
}

func syncRosterDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// rosterWriter holds the seam and the in-package fault points. The encoders,
// beforeRename and syncDir are replaced only by tests of this package.
type rosterWriter struct {
	protection   RosterProtection
	encode       func(map[string]string) ([]byte, error)
	encodeV2     func([]rosterKeyEntry) ([]byte, error)
	beforeRename func() error
	syncDir      func(string) error
}

func newRosterWriter(p RosterProtection) rosterWriter {
	return rosterWriter{protection: p, encode: encodeRoster, encodeV2: encodeRosterV2, syncDir: syncRosterDir}
}

// WriteRoster builds the roster from one input document and publishes it at
// the absolute path out. It returns the number of published senders. It never
// changes ownership or modes of existing entries and never creates ancestors.
func WriteRoster(input []byte, out string, p RosterProtection) (int, error) {
	if p == nil {
		p = HostRosterProtection()
	}
	return newRosterWriter(p).write(input, out)
}

func (w rosterWriter) write(input []byte, out string) (int, error) {
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		return 0, refuseRoster("out_path")
	}
	if !w.protection.CallerMayPublish() {
		return 0, refuseRoster("not_root")
	}
	entries, err := ParseRosterInput(input)
	if err != nil {
		return 0, err
	}
	want := make(map[string]string, len(entries))
	for _, entry := range entries {
		want[entry.MXID] = entry.AuthorKey
	}
	data, err := w.encode(want)
	if err != nil {
		return 0, err
	}
	if err := checkRosterRoundTrip(data, want); err != nil {
		return 0, err
	}
	if err := w.publish(out, data); err != nil {
		return 0, err
	}
	return len(want), nil
}

func (w rosterWriter) protectedAncestors(dir string) bool {
	for current := dir; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil || !w.protection.Protected(st, true) {
			return false
		}
		if current == filepath.Dir(current) {
			return true
		}
	}
}

// checkExistingRosterFacts protects facts in a bounded reader-format document.
// Malformed bytes cannot be a usable reader snapshot and may be replaced.
func checkExistingRosterFacts(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|rosterOpenNoFollow, 0)
	if err != nil {
		return refuseRoster("out_unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRosterBytes+1))
	if err != nil {
		return refuseRoster("out_unreadable")
	}
	doc, err := decodeRosterDocument(raw)
	if err != nil {
		return nil
	}
	facts, _ := doc["facts"].(map[string]any)
	if len(facts) != 0 {
		return refuseRoster("existing_facts")
	}
	return nil
}

// publish replaces out through a same-directory temporary file. Until the
// rename nothing at out changes; every earlier refusal removes the temporary.
func (w rosterWriter) publish(out string, data []byte) error {
	dir := filepath.Dir(out)
	if !w.protectedAncestors(dir) {
		return refuseRoster("out_unprotected")
	}
	if st, err := os.Lstat(out); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return refuseRoster("out_symlink")
		}
		if !st.Mode().IsRegular() {
			return refuseRoster("out_not_regular")
		}
		if err := checkExistingRosterFacts(out); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return refuseRoster("out_unreadable")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return refuseRoster("publish_failed")
	}
	tmp := filepath.Join(dir, "."+filepath.Base(out)+".tmp-"+hex.EncodeToString(nonce[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|rosterOpenNoFollow, 0o644)
	if err != nil {
		return refuseRoster("publish_failed")
	}
	published := false
	defer func() {
		if !published {
			os.Remove(tmp)
		}
	}()
	// The mode is set on the new file only, so the result does not depend on
	// the caller's umask and stays readable by the non-root reader.
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	var st os.FileInfo
	if err == nil {
		st, err = f.Stat()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return refuseRoster("publish_failed")
	}
	if !w.protection.Protected(st, false) {
		return refuseRoster("result_unprotected")
	}
	if w.beforeRename != nil {
		if err := w.beforeRename(); err != nil {
			return refuseRoster("publish_interrupted")
		}
	}
	// Refuse changes discovered after writing while the previous file is still intact.
	st, err = os.Lstat(tmp)
	if err != nil || !w.protection.Protected(st, false) || !w.protectedAncestors(dir) {
		return refuseRoster("result_unprotected")
	}
	if err := os.Rename(tmp, out); err != nil {
		return refuseRoster("publish_failed")
	}
	published = true
	if err := w.syncDir(dir); err != nil {
		return publishedRoster("published_unsynced")
	}
	// The reader's rule is evaluated again on what is now at the final name.
	st, err = os.Lstat(out)
	if err != nil || !w.protection.Protected(st, false) || !w.protectedAncestors(dir) {
		return publishedRoster("published_unprotected")
	}
	return nil
}
