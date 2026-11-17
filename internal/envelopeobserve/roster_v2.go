package envelopeobserve

import (
	"encoding/json"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/swarma-envelope/record"
)

const (
	rosterVersionV2      = "a2a-envelope-roster/2"
	rosterInputVersionV2 = "a2a-roster-input/2"
	maxRosterPublishers  = 64
	maxRosterAuthorLabel = 255
)

type rosterKeyBinding struct {
	Author     string
	Publishers map[string]bool
}

type rosterKeyEntry struct {
	AuthorKey  string
	Author     string
	Publishers []string
	Source     string
}

// rosterSyntaxBounded limits containers before either JSON decoder allocates.
// Syntax, duplicate names and Unicode scalars are checked by the strict codec.
func rosterSyntaxBounded(raw []byte) bool {
	if len(raw) > maxRosterBytes {
		return false
	}
	depth := 0
	quoted, escaped := false, false
	for _, c := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > 16 {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !quoted
}

// decodeRosterDocument preserves the version 1 codec path. The envelope codec
// caps every array at 64; only version 2 needs a larger outer authors list.
// That list uses the carrier's strict decoder, while facts retain the pinned
// codec's integer, string, array and remaining depth limits.
func decodeRosterDocument(raw []byte) (map[string]any, error) {
	value, err := record.Parse(raw, maxRosterBytes, 16)
	if err == nil {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, ErrRoster
		}
		return m, nil
	}
	if !rosterSyntaxBounded(raw) {
		return nil, ErrRoster
	}
	var fields map[string]json.RawMessage
	var version string
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["version"], &version) != nil || version != rosterVersionV2 {
		return nil, ErrRoster
	}
	value, err = matrix.ParseStrictJSON(raw)
	if err != nil {
		return nil, ErrRoster
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, ErrRoster
	}
	facts, err := record.Parse(fields["facts"], maxRosterBytes, 15)
	if err != nil {
		return nil, ErrRoster
	}
	m["facts"] = facts
	return m, nil
}

func validRosterAuthorLabel(label string) bool {
	if len(label) == 0 || len(label) > maxRosterAuthorLabel {
		return false
	}
	for i := 0; i < len(label); i++ {
		if label[i] < 0x21 || label[i] > 0x7e {
			return false
		}
	}
	return true
}

// parseRosterKeyEntries is shared by the reader list and operator input. Keys
// have exact canonical spelling: no case folding or alternate base64 is used.
func parseRosterKeyEntries(value any, withSource bool) ([]rosterKeyEntry, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, refuseRoster("input_entries")
	}
	if len(list) == 0 || len(list) > maxRosterEntries {
		return nil, refuseRoster("input_entries_count")
	}
	entries := make([]rosterKeyEntry, 0, len(list))
	seenKeys := make(map[string]bool, len(list))
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, refuseRoster("input_entries")
		}
		names := []string{"author_key", "author", "publishers"}
		if withSource {
			names = append(names, "source")
		}
		if err := closedMembers(row, names...); err != nil {
			return nil, err
		}
		key, keyOK := row["author_key"].(string)
		author, authorOK := row["author"].(string)
		if !keyOK || !authorOK {
			return nil, refuseRoster("input_member_type")
		}
		if !strings.HasPrefix(key, "ed25519:") && !strings.HasPrefix(key, "bip340:") {
			return nil, refuseRoster("key_type")
		}
		if record.ValidateKey(key) != nil {
			return nil, refuseRoster("key_invalid")
		}
		if seenKeys[key] {
			return nil, refuseRoster("key_duplicate")
		}
		seenKeys[key] = true
		if !validRosterAuthorLabel(author) {
			return nil, refuseRoster("author_invalid")
		}
		publishers, ok := row["publishers"].([]any)
		if !ok {
			return nil, refuseRoster("publishers_invalid")
		}
		if len(publishers) == 0 || len(publishers) > maxRosterPublishers {
			return nil, refuseRoster("publishers_count")
		}
		entry := rosterKeyEntry{AuthorKey: key, Author: author}
		seenPublishers := make(map[string]bool, len(publishers))
		for _, item := range publishers {
			publisher, ok := item.(string)
			if !ok {
				return nil, refuseRoster("input_member_type")
			}
			if !validRosterSender(publisher) {
				return nil, refuseRoster("mxid_invalid")
			}
			if seenPublishers[publisher] {
				return nil, refuseRoster("publisher_duplicate")
			}
			seenPublishers[publisher] = true
			entry.Publishers = append(entry.Publishers, publisher)
		}
		if withSource {
			source, ok := row["source"].(string)
			if !ok {
				return nil, refuseRoster("input_member_type")
			}
			if !validRosterSource(source) {
				return nil, refuseRoster("source_invalid")
			}
			entry.Source = source
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func rosterKeyBindings(entries []rosterKeyEntry) (map[string]rosterKeyBinding, []any) {
	bindings := make(map[string]rosterKeyBinding, len(entries))
	keys := make([]any, 0, len(entries))
	for _, entry := range entries {
		publishers := make(map[string]bool, len(entry.Publishers))
		for _, publisher := range entry.Publishers {
			publishers[publisher] = true
		}
		bindings[entry.AuthorKey] = rosterKeyBinding{Author: entry.Author, Publishers: publishers}
		keys = append(keys, entry.AuthorKey)
	}
	return bindings, keys
}

func parseRosterV2(m map[string]any) (*Observer, error) {
	entries, err := parseRosterKeyEntries(m["authors"], false)
	if err != nil {
		return nil, ErrRoster
	}
	bindings, keys := rosterKeyBindings(entries)
	return loadRosterFacts(&Observer{byAuthor: bindings}, m["facts"], keys)
}
