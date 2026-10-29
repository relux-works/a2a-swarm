package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// LegacyPendingIDs reads the pre-mailbox batch's exact event IDs. The old
// next_batch is evidence only, never a cursor to adopt. Unsupported shapes,
// including duplicate keys, fail closed: opaque bytes alone cannot recover mail.
func LegacyPendingIDs(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("legacy pending recovery: batch object required")
	}
	seen := map[string]bool{}
	var events []string
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, fmt.Errorf("legacy pending recovery: invalid batch field")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return nil, fmt.Errorf("legacy pending recovery: duplicate or invalid batch field")
		}
		seen[key] = true
		switch key {
		case "next_batch":
			var ignored string
			if err = d.Decode(&ignored); err != nil {
				return nil, fmt.Errorf("legacy pending recovery: invalid next_batch")
			}
		case "events":
			if err = d.Decode(&events); err != nil {
				return nil, fmt.Errorf("legacy pending recovery: invalid events array")
			}
		default:
			return nil, fmt.Errorf("legacy pending recovery: unsupported batch field")
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, fmt.Errorf("legacy pending recovery: invalid batch end")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("legacy pending recovery: trailing batch data")
	}
	if !seen["events"] || events == nil {
		return nil, fmt.Errorf("legacy pending recovery: events array required")
	}
	ids := []string{}
	unique := map[string]bool{}
	for _, id := range events {
		if !strings.HasPrefix(id, "$") || len(id) < 2 || strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return nil, fmt.Errorf("legacy pending recovery: invalid event ID")
		}
		if !unique[id] {
			ids = append(ids, id)
			unique[id] = true
		}
	}
	return ids, nil
}

// HasLegacyPendingEvent is a narrow recovery admission, never follow intent.
// A receipt always wins. Malformed batches grant no eligibility and are refused
// by migration/ingest before any cursor is advanced.
func (s State) HasLegacyPendingEvent(id string) bool {
	ids, err := LegacyPendingIDs(s.LegacyPending)
	if err != nil {
		return false
	}
	for _, acked := range s.LegacyAcked {
		if acked == id {
			return false
		}
	}
	for _, pending := range ids {
		if pending == id {
			return true
		}
	}
	return false
}
