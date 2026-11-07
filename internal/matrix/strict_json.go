package matrix

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// strictJSON interprets exactly one Unicode-scalar RFC8259 value. It is used
// only for server-generated controls. The returned tree is the consumed value;
// no second semantic JSON decoder is permitted on this partition.
func strictJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errWireNotUTF8
	}
	p := wireParser{b: raw}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(raw) {
		return nil, errWireTrailing
	}
	return v, nil
}

var errWireJSON = errors.New("matrix: invalid control JSON")

// errWireNotUTF8 keeps the original message; callers may tell it apart.
var errWireNotUTF8 = errors.New("matrix: response is not UTF-8")

// wireReason refines errWireJSON for callers that must report a specific
// refusal. It keeps the same text and errors.Is(err, errWireJSON) identity.
type wireReason struct{ reason string }

func (*wireReason) Error() string { return errWireJSON.Error() }
func (*wireReason) Unwrap() error { return errWireJSON }

var (
	errWireDuplicate = &wireReason{"duplicate member name"}
	errWireTrailing  = &wireReason{"data after the JSON value"}
	errWireSurrogate = &wireReason{"unpaired surrogate escape"}
)

const maxJSONDepth = 10000 // encoding/json's existing composite nesting limit

type wireParser struct {
	b          []byte
	i          int
	depthLimit int // zero preserves the control parser's historical limit
}

func (p *wireParser) space() {
	for p.i < len(p.b) && strings.ContainsRune(" \t\r\n", rune(p.b[p.i])) {
		p.i++
	}
}
func (p *wireParser) take(c byte) bool {
	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++
		return true
	}
	return false
}
func (p *wireParser) value(depth int) (any, error) {
	p.space()
	if p.i == len(p.b) {
		return nil, errWireJSON
	}
	switch p.b[p.i] {
	case '{', '[':
		limit := p.depthLimit
		if limit == 0 {
			limit = maxJSONDepth
		}
		if depth >= limit {
			return nil, errWireJSON
		}
		object := p.b[p.i] == '{'
		p.i++
		p.space()
		if object {
			m := map[string]any{}
			if p.take('}') {
				return m, nil
			}
			for {
				k, err := p.str()
				if err != nil {
					return nil, err
				}
				if _, exists := m[k]; exists {
					return nil, errWireDuplicate
				}
				p.space()
				if !p.take(':') {
					return nil, errWireJSON
				}
				v, err := p.value(depth + 1)
				if err != nil {
					return nil, err
				}
				m[k] = v
				p.space()
				if p.take('}') {
					return m, nil
				}
				if !p.take(',') {
					return nil, errWireJSON
				}
				p.space()
			}
		}
		a := []any{}
		if p.take(']') {
			return a, nil
		}
		for {
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
			p.space()
			if p.take(']') {
				return a, nil
			}
			if !p.take(',') {
				return nil, errWireJSON
			}
		}
	case '"':
		return p.str()
	case 't':
		if p.literal("true") {
			return true, nil
		}
	case 'f':
		if p.literal("false") {
			return false, nil
		}
	case 'n':
		if p.literal("null") {
			return nil, nil
		}
	default:
		return p.number()
	}
	return nil, errWireJSON
}
func (p *wireParser) literal(s string) bool {
	if strings.HasPrefix(string(p.b[p.i:]), s) {
		p.i += len(s)
		return true
	}
	return false
}
func (p *wireParser) digits() bool {
	start := p.i
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
	}
	return p.i > start
}
func (p *wireParser) number() (any, error) {
	start := p.i
	p.take('-')
	if !p.take('0') {
		if p.i == len(p.b) || p.b[p.i] < '1' || p.b[p.i] > '9' {
			return nil, errWireJSON
		}
		p.digits()
	}
	if p.take('.') && !p.digits() {
		return nil, errWireJSON
	}
	if p.take('e') || p.take('E') {
		if !p.take('+') {
			p.take('-')
		}
		if !p.digits() {
			return nil, errWireJSON
		}
	}
	return json.Number(string(p.b[start:p.i])), nil
}
func (p *wireParser) hex() (rune, error) {
	if len(p.b)-p.i < 4 {
		return 0, errWireJSON
	}
	v, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 16)
	if err != nil {
		return 0, errWireJSON
	}
	p.i += 4
	return rune(v), nil
}
func (p *wireParser) str() (string, error) {
	if !p.take('"') {
		return "", errWireJSON
	}
	var b strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		p.i++
		if c == '"' {
			return b.String(), nil
		}
		if c < 0x20 {
			return "", errWireJSON
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if p.i == len(p.b) {
			return "", errWireJSON
		}
		c = p.b[p.i]
		p.i++
		switch c {
		case '"', '\\', '/':
			b.WriteByte(c)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, err := p.hex()
			if err != nil {
				return "", err
			}
			if r >= 0xd800 && r <= 0xdbff {
				if !p.take('\\') || !p.take('u') {
					return "", errWireSurrogate
				}
				low, err := p.hex()
				if err != nil {
					return "", err
				}
				if low < 0xdc00 || low > 0xdfff {
					return "", errWireSurrogate
				}
				r = utf16.DecodeRune(r, low)
			} else if r >= 0xdc00 && r <= 0xdfff {
				return "", errWireSurrogate
			}
			b.WriteRune(r)
		default:
			return "", errWireJSON
		}
	}
	return "", errWireJSON
}
