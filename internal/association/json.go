// Package association compares one stored own binding. It never observes a
// native runtime and cannot authenticate the launcher that supplies its input.
package association

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var invalid = errors.New("association unavailable")
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var generation = regexp.MustCompile(`^[0-9a-f]{32}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var pane = regexp.MustCompile(`^%[0-9]{1,10}$`)

// parse preserves numeric spelling, rejects duplicate keys at every depth and
// bounds work before any typed decoding. State permits unrelated fields.
func parse(b []byte, depth, tokens int) (map[string]any, error) {
	if !utf8.Valid(b) {
		return nil, invalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) (any, error)
	next := func() (json.Token, error) {
		tokens--
		if tokens < 0 {
			return nil, invalid
		}
		return d.Token()
	}
	value = func(level int) (any, error) {
		if level > depth {
			return nil, invalid
		}
		t, e := next()
		if e != nil {
			return nil, invalid
		}
		switch t {
		case json.Delim('{'):
			m := map[string]any{}
			keys := map[string]bool{}
			for d.More() {
				k, e := next()
				if e != nil {
					return nil, invalid
				}
				s, ok := k.(string)
				if !ok {
					return nil, invalid
				}
				folded := foldKey(s)
				if keys[folded] {
					return nil, invalid
				}
				keys[folded] = true
				v, e := value(level + 1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			end, e := next()
			if e != nil || end != json.Delim('}') {
				return nil, invalid
			}
			return m, nil
		case json.Delim('['):
			var a []any
			for d.More() {
				v, e := value(level + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := next()
			if e != nil || end != json.Delim(']') {
				return nil, invalid
			}
			return a, nil
		default:
			if _, bad := t.(json.Delim); bad {
				return nil, invalid
			}
			return t, nil
		}
	}
	v, e := value(0)
	if e != nil {
		return nil, e
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, invalid
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, invalid
	}
	return m, nil
}

// encoding/json accepts Unicode case aliases for typed Go fields. Refuse their
// coexistence, too, without quadratic comparisons on the bounded state object.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		low := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < low {
				low = n
			}
		}
		b.WriteRune(low)
	}
	return b.String()
}

func closed(m map[string]any, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if m[k] == nil {
			return false
		}
	}
	return true
}
func text(v any, max int) string {
	s, ok := v.(string)
	if !ok || len(s) == 0 || len(s) > max || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ""
		}
	}
	return s
}
func integer(v any, max int64) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok || !decimal.MatchString(string(n)) {
		return 0, false
	}
	i, e := strconv.ParseInt(string(n), 10, 64)
	return i, e == nil && i <= max
}
func pid(v any) int {
	n, ok := integer(v, 2147483647)
	if !ok {
		return 0
	}
	return int(n)
}
