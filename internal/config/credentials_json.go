package config

import (
	"bytes"
	"encoding/json"
	"slices"
)

type credentialsJSON Credentials

// Legacy JSON accepts repeated/case-folded keys. Keep every optional value so a
// later valid occurrence cannot conceal an earlier malformed predicate.
type waitKeywordValues []json.RawMessage

func (v *waitKeywordValues) UnmarshalJSON(data []byte) error {
	*v = append(*v, slices.Clone(data))
	return nil
}

// UnmarshalJSON leaves optional predicate validation to wait/watch. All other
// fields retain the standard credential decoder's type and case-fold checks.
// Strict callers MUST use DecodeConnectionJSON: DisallowUnknownFields alone
// does not apply inside this custom unmarshaler.
func (c *Credentials) UnmarshalJSON(data []byte) error {
	return c.decodeJSON(data, false)
}

// Named metadata uses the same optional handling without waiving its stricter
// unknown-field checks (a custom UnmarshalJSON otherwise bypasses those checks).
func (c *Credentials) decodeJSON(data []byte, strict bool) error {
	var next credentialsJSON
	wire := struct {
		*credentialsJSON
		Keywords waitKeywordValues `json:"wait_keywords"`
	}{credentialsJSON: &next}
	d := json.NewDecoder(bytes.NewReader(data))
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(&wire); err != nil {
		return err
	}
	next.waitKeywordsRaw = wire.Keywords
	for _, raw := range wire.Keywords {
		// Invalid optional data stays opaque; it never becomes an active filter.
		_ = json.Unmarshal(raw, &next.WaitKeywords)
	}
	*c = Credentials(next)
	return nil
}

// ValidateWaitKeywords must run before wait/watch requests or delivery effects.
// Errors contain only fixed predicate diagnostics, never credential contents.
func (c Credentials) ValidateWaitKeywords() error {
	for _, raw := range c.waitKeywordsRaw {
		var words WaitKeywords
		if err := json.Unmarshal(raw, &words); err != nil {
			return err
		}
	}
	return nil
}

// marshalCredentials preserves the original optional value byte-for-byte on
// unrelated rewrites. Passing it through MarshalIndent would reformat opaque
// objects/arrays and HTML-escape strings. Invalid configuration is never silently
// dropped or replaced by the zero typed value left by its failed decoder.
func marshalCredentials(c Credentials) ([]byte, error) {
	var original WaitKeywords
	for _, raw := range c.waitKeywordsRaw {
		_ = json.Unmarshal(raw, &original)
	}
	// Compare the observable decoded value, including nil versus empty. A
	// nil-to-nil assignment cannot express intent through this callback API;
	// use a nonnil empty slice to clear an opaque value decoded as nil.
	retain := len(c.waitKeywordsRaw) > 0 && slices.Equal(original, c.WaitKeywords) && (original == nil) == (c.WaitKeywords == nil)
	wire := credentialsJSON(c)
	if retain {
		wire.WaitKeywords = nil
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil || !retain {
		return data, err
	}
	data = data[:len(data)-2]
	for _, raw := range c.waitKeywordsRaw {
		data = append(data, []byte(",\n  \"wait_keywords\": ")...)
		data = append(data, raw...)
	}
	return append(data, '\n', '}'), nil
}
