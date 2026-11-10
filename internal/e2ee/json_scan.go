package e2ee

import (
	"encoding/json"
	"unicode/utf8"
)

// scanJSON checks syntax, decoded duplicate keys, UTF-8 and the same depth limit
// as strictJSON, without allocating tokens or decoding scalar values. The typed
// decoder remains responsible for field names/types. compact is the exact size
// of this raw JSON after encoding/json's compact+HTML escaping for RawMessage;
// it deliberately preserves number spelling, string escapes and member order.
func scanJSON(data []byte, depth int) (compact int, err error) {
	if len(data) == 0 || !utf8.Valid(data) {
		return 0, ErrCodec
	}
	p := jsonScan{data: data}
	if !p.value(depth) {
		return 0, ErrCodec
	}
	p.space()
	if p.at != len(data) {
		return 0, ErrCodec
	}
	return p.compact, nil
}

type jsonScan struct {
	data        []byte
	at, compact int
}

func (p *jsonScan) space() {
	for p.at < len(p.data) {
		switch p.data[p.at] {
		case ' ', '\t', '\r', '\n':
			p.at++
		default:
			return
		}
	}
}
func (p *jsonScan) take(c byte) bool {
	p.space()
	if p.at == len(p.data) || p.data[p.at] != c {
		return false
	}
	p.at++
	p.compact++
	return true
}
func (p *jsonScan) quoted() (start, end int, escaped, ok bool) {
	start = p.at
	if p.at == len(p.data) || p.data[p.at] != '"' {
		return
	}
	p.at++
	for p.at < len(p.data) {
		c := p.data[p.at]
		p.at++
		switch {
		case c == '"':
			p.compact += p.at - start
			return start, p.at, escaped, true
		case c < 0x20:
			return
		case c == '\\':
			escaped = true
			if p.at == len(p.data) {
				return
			}
			c = p.data[p.at]
			p.at++
			switch c {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for range 4 {
					if p.at == len(p.data) {
						return
					}
					c = p.data[p.at]
					p.at++
					if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
						return
					}
				}
			default:
				return
			}
		case c == '<' || c == '>' || c == '&':
			p.compact += 5
		case c == 0xe2 && p.at+1 < len(p.data) && p.data[p.at] == 0x80 && (p.data[p.at+1] == 0xa8 || p.data[p.at+1] == 0xa9):
			p.compact += 3
		}
	}
	return
}
func (p *jsonScan) value(depth int) bool {
	if depth > 32 {
		return false
	}
	p.space()
	if p.at == len(p.data) {
		return false
	}
	switch p.data[p.at] {
	case '"':
		_, _, _, ok := p.quoted()
		return ok
	case '{':
		p.at++
		p.compact++
		p.space()
		if p.at < len(p.data) && p.data[p.at] == '}' {
			p.at++
			p.compact++
			return true
		}
		seen := map[string]bool{}
		for {
			start, end, escaped, ok := p.quoted()
			if !ok {
				return false
			}
			key := string(p.data[start+1 : end-1])
			if escaped {
				if json.Unmarshal(p.data[start:end], &key) != nil {
					return false
				}
			}
			if seen[key] {
				return false
			}
			seen[key] = true
			if !p.take(':') || !p.value(depth+1) {
				return false
			}
			p.space()
			if p.at < len(p.data) && p.data[p.at] == '}' {
				p.at++
				p.compact++
				return true
			}
			if !p.take(',') {
				return false
			}
			p.space()
		}
	case '[':
		p.at++
		p.compact++
		p.space()
		if p.at < len(p.data) && p.data[p.at] == ']' {
			p.at++
			p.compact++
			return true
		}
		for {
			if !p.value(depth + 1) {
				return false
			}
			p.space()
			if p.at < len(p.data) && p.data[p.at] == ']' {
				p.at++
				p.compact++
				return true
			}
			if !p.take(',') {
				return false
			}
		}
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case 'n':
		return p.literal("null")
	default:
		start := p.at
		if p.data[p.at] == '-' {
			p.at++
		}
		if p.at == len(p.data) {
			return false
		}
		if p.data[p.at] == '0' {
			p.at++
		} else {
			if p.data[p.at] < '1' || p.data[p.at] > '9' {
				return false
			}
			for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
				p.at++
			}
		}
		if p.at < len(p.data) && p.data[p.at] == '.' {
			p.at++
			digits := p.at
			for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
				p.at++
			}
			if digits == p.at {
				return false
			}
		}
		if p.at < len(p.data) && (p.data[p.at] == 'e' || p.data[p.at] == 'E') {
			p.at++
			if p.at < len(p.data) && (p.data[p.at] == '+' || p.data[p.at] == '-') {
				p.at++
			}
			digits := p.at
			for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
				p.at++
			}
			if digits == p.at {
				return false
			}
		}
		p.compact += p.at - start
		return true
	}
}
func (p *jsonScan) literal(value string) bool {
	if len(p.data)-p.at < len(value) || string(p.data[p.at:p.at+len(value)]) != value {
		return false
	}
	p.at += len(value)
	p.compact += len(value)
	return true
}
