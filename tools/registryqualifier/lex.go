package main

import (
	"context"
	"errors"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// This is a deliberately bounded, rejecting lexer, not a JavaScript runtime.
// Slash is scanned in the expression context supplied by the grammar. Template
// substitutions return to the grammar, including nested templates and regexps.
const (
	maxBytes       = 64 << 20
	maxTokens      = 1_000_000
	maxDepth       = 256
	maxDefinitions = 50_000
)

type token struct {
	raw     string
	kind    string
	lo, hi  int
	newline bool
	role    string
	scope   *scope
	mask    bool
}

type lexer struct {
	ctx   context.Context
	data  string
	pos   int
	steps int
}

func refusal(code string) { panic(parseError(code)) }

type parseError string

func (p parseError) Error() string { return string(p) }

func (l *lexer) check() {
	l.steps++
	if l.steps&1023 == 0 && l.ctx.Err() != nil {
		refusal("deadline")
	}
}

func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' }
func digit(c byte) bool  { return c >= '0' && c <= '9' }
func hex(c byte) bool    { return digit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

var keywords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("async await break case catch class const continue debugger default delete do else extends false finally for function if in instanceof let new null of return static super switch this throw true try typeof var void while with yield import export enum implements interface package private protected public") {
		keywords[w] = true
	}
}

func (l *lexer) skip() bool {
	nl := false
	for l.pos < len(l.data) {
		l.check()
		c := l.data[l.pos]
		if c == ' ' || c == '\t' || c == '\v' || c == '\f' || c == '\r' || c == '\n' {
			nl = nl || c == '\r' || c == '\n'
			l.pos++
			continue
		}
		if strings.HasPrefix(l.data[l.pos:], "//") {
			l.pos += 2
			for l.pos < len(l.data) && l.data[l.pos] != '\n' && l.data[l.pos] != '\r' {
				l.check()
				l.pos++
			}
			continue
		}
		if strings.HasPrefix(l.data[l.pos:], "/*") {
			l.pos += 2
			for !strings.HasPrefix(l.data[l.pos:], "*/") {
				if l.pos >= len(l.data) {
					refusal("unterminated_comment")
				}
				l.check()
				nl = nl || l.data[l.pos] == '\r' || l.data[l.pos] == '\n'
				l.pos++
			}
			l.pos += 2
			continue
		}
		break
	}
	return nl
}

func (l *lexer) escape(quote byte) {
	if l.pos >= len(l.data) {
		refusal("unterminated_escape")
	}
	c := l.data[l.pos]
	l.pos++
	switch c {
	case '\r':
		if l.pos < len(l.data) && l.data[l.pos] == '\n' {
			l.pos++
		}
	case '\n':
	case 'x':
		for range 2 {
			if l.pos >= len(l.data) || !hex(l.data[l.pos]) {
				refusal("invalid_escape")
			}
			l.pos++
		}
	case 'u':
		if l.pos < len(l.data) && l.data[l.pos] == '{' {
			l.pos++
			n, value := 0, uint32(0)
			for l.pos < len(l.data) && hex(l.data[l.pos]) {
				c := l.data[l.pos]
				v := uint32(c - '0')
				if c >= 'a' {
					v = uint32(c - 'a' + 10)
				} else if c >= 'A' {
					v = uint32(c - 'A' + 10)
				}
				value = value*16 + v
				n++
				l.pos++
				if n > 6 {
					refusal("invalid_escape")
				}
			}
			if n == 0 || value > utf8.MaxRune || l.pos >= len(l.data) || l.data[l.pos] != '}' {
				refusal("invalid_escape")
			}
			l.pos++
		} else {
			for range 4 {
				if l.pos >= len(l.data) || !hex(l.data[l.pos]) {
					refusal("invalid_escape")
				}
				l.pos++
			}
		}
	default:
		// Legacy octal and non-ASCII escapes are outside this grammar.
		if digit(c) && (c != '0' || l.pos < len(l.data) && digit(l.data[l.pos])) || c >= 128 {
			refusal("unsupported_escape")
		}
		_ = quote
	}
}

func (l *lexer) quoted(quote byte) {
	l.pos++
	for l.pos < len(l.data) {
		l.check()
		c := l.data[l.pos]
		l.pos++
		if c == quote {
			return
		}
		if c == '\r' || c == '\n' || c < 32 {
			refusal("invalid_string")
		}
		if c == '\\' {
			l.escape(quote)
		}
	}
	refusal("unterminated_string")
}

func (l *lexer) number() {
	start := l.pos
	base := byte(10)
	if l.data[l.pos] == '0' && l.pos+1 < len(l.data) {
		switch l.data[l.pos+1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
	}
	valid := func(c byte) bool {
		if base == 16 {
			return hex(c)
		}
		return digit(c) && c-'0' < base
	}
	scanDigits := func() int {
		n := 0
		sep := false
		for l.pos < len(l.data) {
			l.check()
			c := l.data[l.pos]
			if valid(c) {
				n++
				sep = false
				l.pos++
				continue
			}
			if c == '_' && n > 0 && !sep {
				sep = true
				l.pos++
				continue
			}
			break
		}
		if sep {
			refusal("invalid_number")
		}
		return n
	}
	integer := true
	if base != 10 {
		l.pos += 2
		if scanDigits() == 0 {
			refusal("invalid_number")
		}
	} else {
		n := scanDigits()
		if l.pos < len(l.data) && l.data[l.pos] == '.' {
			integer = false
			l.pos++
			m := scanDigits()
			if n+m == 0 {
				refusal("invalid_number")
			}
		}
		if l.pos < len(l.data) && (l.data[l.pos] == 'e' || l.data[l.pos] == 'E') {
			integer = false
			l.pos++
			if l.pos < len(l.data) && (l.data[l.pos] == '+' || l.data[l.pos] == '-') {
				l.pos++
			}
			if scanDigits() == 0 {
				refusal("invalid_number")
			}
		}
		if n > 1 && l.data[start] == '0' && l.data[start+1] != '.' {
			refusal("unsupported_legacy_number")
		}
	}
	if l.pos < len(l.data) && l.data[l.pos] == 'n' {
		if !integer {
			refusal("invalid_number")
		}
		l.pos++
	}
	if l.pos < len(l.data) && (letter(l.data[l.pos]) || digit(l.data[l.pos]) || l.data[l.pos] == '\\') {
		refusal("invalid_number")
	}
}

func (l *lexer) regexp() {
	l.pos++
	start := l.pos
	inClass, escaped := false, false
	parens := 0
	for l.pos < len(l.data) {
		if l.pos-start > 1<<20 {
			refusal("regexp_size_limit")
		}
		l.check()
		c := l.data[l.pos]
		l.pos++
		if c == '\r' || c == '\n' || c < 32 {
			refusal("invalid_regexp")
		}
		if escaped {
			if !strings.ContainsRune("dDsSwWbBfnrtv\\/.+*?()[]{}^$|-x", rune(c)) {
				refusal("unsupported_regexp_escape")
			}
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
		case '(':
			parens++
			if parens > maxDepth {
				refusal("depth_limit")
			}
			if l.pos < len(l.data) && l.data[l.pos] == '?' && !strings.HasPrefix(l.data[l.pos:], "?:") {
				refusal("unsupported_regexp_group")
			}
		case ')':
			parens--
			if parens < 0 {
				refusal("invalid_regexp")
			}
		case '/':
			if parens != 0 {
				refusal("invalid_regexp")
			}
			pattern := strings.ReplaceAll(l.data[start:l.pos-1], "\\/", "/")
			if _, err := syntax.Parse(pattern, syntax.Perl); err != nil {
				refusal("invalid_or_unsupported_regexp")
			}
			flags := map[byte]bool{}
			for l.pos < len(l.data) && letter(l.data[l.pos]) {
				f := l.data[l.pos]
				if !strings.ContainsRune("dgimsy", rune(f)) || flags[f] {
					refusal("unsupported_regexp_flag")
				}
				flags[f] = true
				l.pos++
			}
			return
		}
	}
	refusal("unterminated_regexp")
}

// template is called only immediately after ` or the substitution's closing }.
func (l *lexer) template(first bool) token {
	start := l.pos
	if first {
		l.pos++
	}
	for l.pos < len(l.data) {
		l.check()
		c := l.data[l.pos]
		l.pos++
		if c == '\\' {
			l.escape('`')
			continue
		}
		if c == '`' {
			return token{raw: l.data[start:l.pos], kind: "template_end", lo: start, hi: l.pos}
		}
		if c == '$' && l.pos < len(l.data) && l.data[l.pos] == '{' {
			l.pos++
			return token{raw: l.data[start:l.pos], kind: "template_part", lo: start, hi: l.pos}
		}
	}
	refusal("unterminated_template")
	return token{}
}

func (l *lexer) next(operand bool) token {
	nl := l.skip()
	start := l.pos
	t := token{lo: start, newline: nl}
	if start == len(l.data) {
		t.kind = "eof"
		t.hi = start
		return t
	}
	c := l.data[l.pos]
	switch {
	case letter(c):
		l.pos++
		for l.pos < len(l.data) && (letter(l.data[l.pos]) || digit(l.data[l.pos])) {
			l.check()
			l.pos++
		}
		t.kind = "id"
		if keywords[l.data[start:l.pos]] {
			t.kind = "keyword"
		}
	case digit(c) || c == '.' && l.pos+1 < len(l.data) && digit(l.data[l.pos+1]):
		t.kind = "number"
		l.number()
	case c == '\'' || c == '"':
		t.kind = "string"
		l.quoted(c)
	case c == '`':
		t = l.template(true)
		t.newline = nl
		return t
	case c == '/' && operand:
		t.kind = "regexp"
		l.regexp()
	default:
		t.kind = "punct"
		if c >= 128 || c == '\\' || c == '#' {
			refusal("unsupported_identifier_or_syntax")
		}
		found := false
		for _, op := range []string{">>>=", "===", "!==", "**=", ">>>", "<<=", ">>=", "&&=", "||=", "??=", "...", "=>", "==", "!=", "<=", ">=", "++", "--", "&&", "||", "??", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "**", "?."} {
			if strings.HasPrefix(l.data[l.pos:], op) {
				l.pos += len(op)
				found = true
				break
			}
		}
		if !found {
			if !strings.ContainsRune("{}()[];:,.?~!+-*/%<>=&|^", rune(c)) {
				refusal("invalid_character")
			}
			l.pos++
		}
	}
	t.raw = l.data[start:l.pos]
	t.hi = l.pos
	return t
}

func validateInput(ctx context.Context, data []byte) error {
	if len(data) == 0 || len(data) > maxBytes {
		return errors.New("input_size")
	}
	if ctx.Err() != nil {
		return errors.New("deadline")
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return errors.New("invalid_encoding")
	}
	// U+2028/U+2029 affect both ASI and literal lexing. Reject instead of guessing.
	if strings.ContainsAny(string(data), "\u2028\u2029") {
		return errors.New("unsupported_line_separator")
	}
	return nil
}
