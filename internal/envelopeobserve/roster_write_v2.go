package envelopeobserve

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"path/filepath"
	"sort"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func parseRosterInputV2(raw []byte) ([]rosterKeyEntry, error) {
	if !rosterSyntaxBounded(raw) {
		return nil, refuseRoster("input_malformed")
	}
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
	if doc["version"] != rosterInputVersionV2 {
		return nil, refuseRoster("input_version")
	}
	return parseRosterKeyEntries(doc["entries"], true)
}

func encodeRosterV2(entries []rosterKeyEntry) ([]byte, error) {
	ordered := append([]rosterKeyEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].AuthorKey < ordered[j].AuthorKey })
	var b strings.Builder
	b.WriteString(`{"authors":[`)
	for i, entry := range ordered {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"author":`)
		if !quoteRosterString(&b, entry.Author) {
			return nil, refuseRoster("output_unencodable")
		}
		b.WriteString(`,"author_key":`)
		if !quoteRosterString(&b, entry.AuthorKey) {
			return nil, refuseRoster("output_unencodable")
		}
		b.WriteString(`,"publishers":[`)
		publishers := append([]string(nil), entry.Publishers...)
		sort.Strings(publishers)
		for j, publisher := range publishers {
			if j > 0 {
				b.WriteByte(',')
			}
			if !quoteRosterString(&b, publisher) {
				return nil, refuseRoster("output_unencodable")
			}
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`],"facts":{},"version":`)
	quoteRosterString(&b, rosterVersionV2)
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func checkRosterRoundTripV2(data []byte, want map[string]rosterKeyBinding) error {
	parsed, err := ParseRoster(data)
	if err != nil {
		return refuseRoster("selfcheck_failed")
	}
	if parsed.byAuthor == nil || len(parsed.byAuthor) != len(want) {
		return refuseRoster("selfcheck_mismatch")
	}
	for key, expected := range want {
		actual, present := parsed.byAuthor[key]
		if !present || actual.Author != expected.Author || !maps.Equal(actual.Publishers, expected.Publishers) {
			return refuseRoster("selfcheck_mismatch")
		}
	}
	doc, _ := decodeRosterDocument(data) // ParseRoster already validated the document.
	if len(doc["facts"].(map[string]any)) != 0 {
		return refuseRoster("selfcheck_mismatch")
	}
	return nil
}

// WriteRosterV2 builds a key-indexed roster from a2a-roster-input/2. Publication,
// protection, existing-facts refusal and post-rename reports match WriteRoster.
func WriteRosterV2(input []byte, out string, p RosterProtection) (int, error) {
	if p == nil {
		p = HostRosterProtection()
	}
	return newRosterWriter(p).writeV2(input, out)
}

func (w rosterWriter) writeV2(input []byte, out string) (int, error) {
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		return 0, refuseRoster("out_path")
	}
	if !w.protection.CallerMayPublish() {
		return 0, refuseRoster("not_root")
	}
	entries, err := parseRosterInputV2(input)
	if err != nil {
		return 0, err
	}
	// Freeze the expected sets before the encoder sees its independent slices.
	want, _ := rosterKeyBindings(entries)
	data, err := w.encodeV2(entries)
	if err != nil {
		return 0, err
	}
	if err := checkRosterRoundTripV2(data, want); err != nil {
		return 0, err
	}
	if err := w.publish(out, data); err != nil {
		return 0, err
	}
	return len(want), nil
}

// publisherAuthors retains the three-column host view: one row per carrier/key
// pair, including multiple keys carried by the same exact Matrix sender.
func (o *Observer) publisherAuthors() []RosterAuthor {
	rows := []RosterAuthor{}
	for key, binding := range o.byAuthor {
		kind, _, _ := strings.Cut(key, ":")
		sum := sha256.Sum256([]byte(key))
		fingerprint := hex.EncodeToString(sum[:])[:rosterFingerprint]
		for publisher := range binding.Publishers {
			rows = append(rows, RosterAuthor{MXID: publisher, KeyType: kind, Fingerprint: fingerprint})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MXID != rows[j].MXID {
			return rows[i].MXID < rows[j].MXID
		}
		if rows[i].KeyType != rows[j].KeyType {
			return rows[i].KeyType < rows[j].KeyType
		}
		return rows[i].Fingerprint < rows[j].Fingerprint
	})
	return rows
}
