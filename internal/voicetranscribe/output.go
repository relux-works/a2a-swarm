package voicetranscribe

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

// The official single-file offline CLI emits one JSON object. Reject duplicate
// and case-aliased top-level keys rather than selecting an ambiguous value.
// Other engine fields (tokens/timings/log-probs) never become confidence.
func parseOutput(b []byte) (string, string, Code) {
	if len(b) > MaxStdoutBytes {
		return "", "", OutputLimit
	}
	if !utf8.Valid(b) {
		return "", "", InvalidOutput
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return "", "", InvalidOutput
	}
	seen := make(map[string]bool)
	var text, language string
	var hasText bool
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		folded := strings.ToLower(key)
		if err != nil || !ok || seen[folded] {
			return "", "", InvalidOutput
		}
		seen[folded] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return "", "", InvalidOutput
		}
		if folded == "text" || folded == "lang" {
			if folded != key || len(raw) == 0 || raw[0] != '"' {
				return "", "", InvalidOutput
			}
			var s string
			if json.Unmarshal(raw, &s) != nil || strings.ContainsRune(s, utf8.RuneError) {
				return "", "", InvalidOutput
			}
			if key == "text" {
				text, hasText = s, true
			} else {
				language = s
			}
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return "", "", InvalidOutput
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !hasText || strings.TrimSpace(text) == "" {
		return "", "", InvalidOutput
	}
	if len(text) > MaxTranscriptBytes {
		return "", "", OutputLimit
	}
	if language != "en" && language != "ru" {
		language = "unknown"
	}
	return text, language, ""
}
