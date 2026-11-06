package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type scope struct {
	parent   *scope
	decls    map[string][]*binding
	function bool
}
type binding struct {
	name   string
	kind   string
	scope  *scope
	origin *scope
	def    *definition
}
type definition struct {
	kind       string
	async      bool
	nonSimple  bool
	start, end int // complete token interval, including parameters and body
	bind       *binding
	calls      []*call
	methods    map[string]bool
}
type call struct {
	ref       int // identifier token, or -1 for dynamic calls
	member    string
	construct bool
}
type program struct {
	tokens  []token
	defs    []*definition
	root    *scope
	data    []byte
	ctx     context.Context
	written map[*binding]bool
	hashes  map[*definition]string
	work    int
	scopes  []*scope
}
type parser struct {
	program
	lex            lexer
	look           *token
	scope          *scope
	stack          []*definition
	depth          int
	loop, switches int
	callRefs       int
}
type expr struct {
	start, end  int
	ref         int
	member      string
	assignable  bool
	def         *definition
	paramTokens []int
	unary       bool
	logical     int
}

func parse(ctx context.Context, data []byte) (out *program, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(parseError); ok {
				out = nil
				err = e
			} else {
				panic(r)
			}
		}
	}()
	if err = validateInput(ctx, data); err != nil {
		return nil, err
	}
	root := &scope{decls: map[string][]*binding{}, function: true}
	p := &parser{program: program{root: root, data: data, ctx: ctx, written: map[*binding]bool{}, hashes: map[*definition]string{}, scopes: []*scope{root}}, lex: lexer{ctx: ctx, data: string(data)}, scope: root}
	for p.peek(true).kind != "eof" {
		p.statement()
	}
	for _, s := range p.scopes {
		for _, bs := range s.decls {
			if len(bs) > 1 {
				for _, b := range bs {
					if b.kind != "function" && b.kind != "var" {
						return nil, parseError("duplicate_lexical_binding")
					}
				}
			}
		}
	}
	for _, s := range p.scopes {
		for _, bs := range s.decls {
			for _, b := range bs {
				if b.kind == "var" {
					for inner := b.origin; inner != b.scope; inner = inner.parent {
						for _, other := range inner.decls[b.name] {
							if other.kind != "var" && other.kind != "function" {
								return nil, parseError("var_lexical_conflict")
							}
						}
					}
				}
			}
		}
	}
	for _, t := range p.tokens {
		if t.role == "write" {
			if b, ok := resolve(t.scope, t.raw); ok {
				p.written[b] = true
			}
		}
	}
	if ctx.Err() != nil {
		return nil, parseError("deadline")
	}
	return &p.program, nil
}

func (p *parser) peek(operand bool) token {
	if p.look == nil {
		t := p.lex.next(operand)
		p.look = &t
	}
	return *p.look
}
func (p *parser) take(operand bool) int {
	t := p.peek(operand)
	p.look = nil
	if len(p.tokens) >= maxTokens {
		refusal("token_limit")
	}
	t.scope = p.scope
	p.tokens = append(p.tokens, t)
	return len(p.tokens) - 1
}
func (p *parser) is(raw string, operand bool) bool { return p.peek(operand).raw == raw }
func (p *parser) eat(raw string) bool {
	if p.is(raw, false) {
		p.take(false)
		return true
	}
	return false
}
func (p *parser) need(raw string) int {
	if !p.is(raw, false) {
		refusal("syntax")
	}
	return p.take(false)
}
func (p *parser) enter() {
	p.depth++
	if p.depth > maxDepth {
		refusal("depth_limit")
	}
	if p.lex.ctx.Err() != nil {
		refusal("deadline")
	}
}
func (p *parser) leave() { p.depth-- }
func (p *parser) child() *scope {
	s := &scope{parent: p.scope, decls: map[string][]*binding{}}
	p.scopes = append(p.scopes, s)
	p.scope = s
	return s
}
func (p *parser) declare(i int) *binding {
	t := &p.tokens[i]
	if t.kind != "id" {
		refusal("binding_syntax")
	}
	if t.raw == "eval" || t.raw == "arguments" {
		refusal("unsupported_restricted_binding")
	}
	t.role = "identifier"
	t.scope = p.scope
	b := &binding{name: t.raw, scope: p.scope, origin: p.scope, kind: "lexical"}
	p.scope.decls[t.raw] = append(p.scope.decls[t.raw], b)
	return b
}
func resolve(s *scope, name string) (*binding, bool) {
	for ; s != nil; s = s.parent {
		if bs := s.decls[name]; len(bs) > 0 {
			if len(bs) != 1 {
				return nil, false
			}
			return bs[0], true
		}
	}
	return nil, false
}
func (p *parser) identifier() int {
	if p.peek(true).kind != "id" {
		refusal("identifier_syntax")
	}
	i := p.take(true)
	p.tokens[i].role = "identifier"
	return i
}
func (p *parser) property() int {
	t := p.peek(true)
	if t.kind != "id" && t.kind != "keyword" && t.kind != "string" && t.kind != "number" {
		refusal("property_syntax")
	}
	i := p.take(true)
	p.tokens[i].role = "property"
	return i
}
func (p *parser) memberProperty() int {
	t := p.peek(true)
	if t.kind != "id" && t.kind != "keyword" {
		refusal("invalid_member_name")
	}
	return p.property()
}

// Decode only for syntax restrictions and local method lookup. Normalization
// always retains the original spelling, including quoted/escaped keys.
func propertyKey(raw string) string {
	if len(raw) < 2 || raw[0] != '\'' && raw[0] != '"' {
		return raw
	}
	var out strings.Builder
	for i := 1; i < len(raw)-1; i++ {
		c := raw[i]
		if c != '\\' {
			out.WriteByte(c)
			continue
		}
		i++
		c = raw[i]
		switch c {
		case 'x', 'u':
			n := 2
			if c == 'u' {
				n = 4
			}
			start := i + 1
			end := start + n
			if c == 'u' && raw[start] == '{' {
				start++
				end = start
				for raw[end] != '}' {
					end++
				}
				i = end
			} else {
				i = end - 1
			}
			value, _ := strconv.ParseUint(raw[start:end], 16, 32)
			out.WriteRune(rune(value))
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'v':
			out.WriteByte('\v')
		case 'f':
			out.WriteByte('\f')
		case 'b':
			out.WriteByte('\b')
		case '0':
			out.WriteByte(0)
		case '\r':
			if i+1 < len(raw)-1 && raw[i+1] == '\n' {
				i++
			}
		case '\n':
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
func (p *parser) semicolon() {
	if p.eat(";") {
		return
	}
	t := p.peek(true)
	if t.kind == "eof" || t.raw == "}" || t.newline {
		return
	}
	refusal("missing_semicolon")
}
func (p *parser) newDef(kind string, start int) *definition {
	if len(p.defs) >= maxDefinitions {
		refusal("definition_limit")
	}
	d := &definition{kind: kind, start: start, methods: map[string]bool{}}
	p.defs = append(p.defs, d)
	return d
}
func (p *parser) block(newScope bool) {
	p.enter()
	defer p.leave()
	p.need("{")
	old := p.scope
	if newScope {
		p.child()
	}
	defer func() { p.scope = old }()
	directives := !newScope && len(p.stack) > 0 && p.stack[len(p.stack)-1].kind != "class"
	for !p.is("}", true) {
		if p.peek(true).kind == "eof" {
			refusal("unterminated_block")
		}
		if directives {
			t := p.peek(true)
			if t.kind != "string" {
				directives = false
			} else if propertyKey(t.raw) == "use strict" && p.stack[len(p.stack)-1].nonSimple {
				refusal("strict_nonsimple_parameters")
			}
		}
		p.statement()
	}
	p.need("}")
}

func (p *parser) statement() {
	p.enter()
	defer p.leave()
	t := p.peek(true)
	switch t.raw {
	case ";":
		p.take(true)
	case "{":
		p.block(true)
	case "function":
		p.function(true, false)
	case "async":
		// An async declaration is recognized without treating a slash as regex.
		start := p.take(true)
		if !p.is("function", true) || p.peek(true).newline {
			refusal("unsupported_async_statement")
		}
		p.function(true, true).start = start
	case "class":
		p.class(true)
	case "let", "const", "var":
		p.variables(false)
		p.semicolon()
	case "return", "throw":
		if len(p.stack) == 0 {
			refusal("return_outside_function")
		}
		p.take(true)
		n := p.peek(true)
		if n.newline {
			if t.raw == "throw" {
				refusal("throw_newline")
			}
			p.semicolon()
			return
		}
		if n.raw != ";" && n.raw != "}" && n.kind != "eof" {
			p.expression(0)
		} else if t.raw == "throw" {
			refusal("syntax")
		}
		p.semicolon()
	case "if":
		p.take(true)
		p.need("(")
		p.expression(0)
		p.need(")")
		p.bodyStatement()
		if p.eat("else") {
			p.bodyStatement()
		}
	case "while":
		p.take(true)
		p.need("(")
		p.expression(0)
		p.need(")")
		p.loop++
		p.bodyStatement()
		p.loop--
	case "do":
		p.take(true)
		p.loop++
		p.bodyStatement()
		p.loop--
		p.need("while")
		p.need("(")
		p.expression(0)
		p.need(")")
		p.semicolon()
	case "for":
		p.forStatement()
	case "switch":
		p.take(true)
		p.need("(")
		p.expression(0)
		p.need(")")
		p.need("{")
		old := p.scope
		p.child()
		p.switches++
		defaults := 0
		caseSeen := false
		for !p.is("}", true) {
			if p.eat("case") {
				caseSeen = true
				p.expression(1)
				p.need(":")
			} else if p.eat("default") {
				caseSeen = true
				defaults++
				if defaults > 1 {
					refusal("duplicate_default")
				}
				p.need(":")
			} else {
				if !caseSeen {
					refusal("statement_before_switch_case")
				}
				p.statement()
			}
		}
		p.need("}")
		p.switches--
		p.scope = old
	case "try":
		p.take(true)
		p.block(true)
		caught := false
		if p.eat("catch") {
			caught = true
			old := p.scope
			p.child()
			if p.eat("(") {
				p.pattern()
				p.need(")")
			}
			p.block(false)
			p.scope = old
		}
		if p.eat("finally") {
			caught = true
			p.block(true)
		}
		if !caught {
			refusal("syntax")
		}
	case "break", "continue":
		if p.loop == 0 && (t.raw == "continue" || p.switches == 0) {
			refusal("invalid_jump")
		}
		p.take(true)
		if p.peek(true).kind == "id" && !p.peek(true).newline {
			refusal("unsupported_label")
		}
		p.semicolon()
	case "debugger":
		p.take(true)
		p.semicolon()
	case "with", "yield", "import", "export", "enum", "implements", "interface", "package", "private", "protected", "public", "static", "super":
		refusal("unsupported_statement")
	case "else", "case", "default", "catch", "finally", "}":
		refusal("syntax")
	default:
		p.expression(0)
		p.semicolon()
	}
}

func (p *parser) bodyStatement() {
	switch p.peek(true).raw {
	case "let", "const", "class", "function", "async":
		refusal("unsupported_single_statement_declaration")
	}
	p.statement()
}

func metadata(name string) bool {
	return name == "VERSION" || name == "BUILD_TIME" || name == "GIT_SHA" || name == "\"VERSION\"" || name == "'VERSION'" || name == "\"BUILD_TIME\"" || name == "'BUILD_TIME'" || name == "\"GIT_SHA\"" || name == "'GIT_SHA'"
}
func (p *parser) maskValue(name string, e expr) {
	if !metadata(name) {
		return
	}
	// Mask only an immediate primitive value associated with this exact name.
	// Expressions, templates with substitutions and similarly named keys stay.
	if e.end-e.start == 1 {
		t := &p.tokens[e.start]
		if t.kind == "string" || t.kind == "number" || t.raw == "true" || t.raw == "false" || t.raw == "null" || t.kind == "template_end" {
			t.mask = true
		}
	}
}

type declarations struct {
	count, initialized, patterns int
	constant                     bool
}

func (p *parser) variables(inFor bool) declarations {
	kind := p.peek(true).raw
	p.take(true)
	info := declarations{constant: kind == "const"}
	for {
		start := len(p.tokens)
		bs := p.pattern()
		info.count++
		pattern := p.tokens[start].kind != "id"
		if pattern {
			info.patterns++
		}
		for _, b := range bs {
			b.kind = kind
		}
		if kind == "var" {
			for _, b := range bs {
				owner := b.scope
				for !owner.function {
					owner = owner.parent
				}
				if owner != b.scope {
					entries := b.scope.decls[b.name]
					for j, entry := range entries {
						if entry == b {
							b.scope.decls[b.name] = append(entries[:j], entries[j+1:]...)
							break
						}
					}
					b.scope = owner
					owner.decls[b.name] = append(owner.decls[b.name], b)
				}
			}
		}
		if p.eat("=") {
			info.initialized++
			e := p.expression(2)
			if len(bs) == 1 && len(p.tokens) > start && p.tokens[start].kind == "id" {
				b := bs[0]
				p.maskValue(b.name, e)
				if e.def != nil {
					e.def.bind = b
					b.def = e.def
				}
			}
		} else if (kind == "const" || pattern) && !inFor {
			refusal("uninitialized_const")
		}
		if !p.eat(",") {
			break
		}
	}
	return info
}

func (p *parser) pattern() []*binding {
	p.enter()
	defer p.leave()
	var bs []*binding
	if p.eat("[") {
		for !p.is("]", true) {
			if p.eat(",") {
				continue
			}
			rest := p.eat("...")
			bs = append(bs, p.pattern()...)
			if p.eat("=") {
				if rest {
					refusal("invalid_rest")
				}
				p.expression(2)
			}
			if rest {
				if !p.is("]", false) {
					refusal("invalid_rest")
				}
				break
			}
			if !p.eat(",") {
				break
			}
		}
		p.need("]")
		return bs
	}
	if p.eat("{") {
		for !p.is("}", true) {
			if p.eat("...") {
				bs = append(bs, p.declare(p.identifier()))
				if !p.is("}", false) {
					refusal("invalid_rest")
				}
				break
			}
			if p.eat("[") {
				p.expression(0)
				p.need("]")
				p.need(":")
				bs = append(bs, p.pattern()...)
			} else {
				i := p.property()
				if p.eat(":") {
					bs = append(bs, p.pattern()...)
				} else {
					if p.tokens[i].kind != "id" {
						refusal("invalid_shorthand")
					}
					b := p.declare(i)
					p.tokens[i].role = "shorthand"
					bs = append(bs, b)
				}
			}
			if p.eat("=") {
				p.expression(2)
			}
			if !p.eat(",") {
				break
			}
		}
		p.need("}")
		return bs
	}
	i := p.identifier()
	bs = append(bs, p.declare(i))
	return bs
}

func (p *parser) forStatement() {
	p.need("for")
	if p.eat("await") {
		refusal("unsupported_for_await")
	}
	p.need("(")
	old := p.scope
	p.child()
	defer func() { p.scope = old }()
	if !p.is(";", true) {
		info := declarations{}
		if t := p.peek(true).raw; t == "let" || t == "const" || t == "var" {
			info = p.variables(true)
		} else {
			p.expression(0)
		}
		// 'in' is intentionally refused in expressions; accepting it here needs
		// a separate NoIn grammar. The refusal avoids a misparsed loop header.
		if p.eat("of") {
			if info.count != 1 || info.initialized != 0 {
				refusal("unsupported_or_invalid_for_of_binding")
			}
			p.expression(0)
			p.need(")")
			p.loop++
			p.bodyStatement()
			p.loop--
			return
		}
		if (info.constant || info.patterns > 0) && info.initialized < info.count {
			refusal("uninitialized_for_binding")
		}
	}
	p.need(";")
	if !p.is(";", true) {
		p.expression(0)
	}
	p.need(";")
	if !p.is(")", true) {
		p.expression(0)
	}
	p.need(")")
	p.loop++
	p.bodyStatement()
	p.loop--
}

func (p *parser) parameters() bool {
	complex := false
	p.need("(")
	for !p.is(")", true) {
		rest := p.eat("...")
		if rest || p.peek(true).kind != "id" {
			complex = true
		}
		p.pattern()
		if p.eat("=") {
			complex = true
			if rest {
				refusal("invalid_rest")
			}
			p.expression(2)
		}
		if rest {
			if !p.is(")", false) {
				refusal("invalid_rest")
			}
			break
		}
		if !p.eat(",") {
			break
		}
	}
	p.need(")")
	return complex
}
func (p *parser) function(decl, async bool) *definition {
	p.enter()
	defer p.leave()
	start := p.need("function")
	if p.eat("*") {
		refusal("unsupported_generator")
	}
	var b *binding
	name := -1
	if p.peek(true).kind == "id" {
		name = p.identifier()
		if decl {
			b = p.declare(name)
		}
	} else if decl {
		refusal("missing_function_name")
	}
	d := p.newDef("function", start)
	d.async = async
	d.bind = b
	if b != nil {
		b.kind = "function"
		b.def = d
	}
	old := p.scope
	p.child()
	p.scope.function = true
	if !decl && name >= 0 {
		b = p.declare(name)
		b.kind = "function"
		b.def = d
		d.bind = b
	}
	oldLoop, oldSwitch := p.loop, p.switches
	p.loop = 0
	p.switches = 0
	p.stack = append(p.stack, d)
	d.nonSimple = p.parameters()
	p.block(false)
	p.stack = p.stack[:len(p.stack)-1]
	p.loop = oldLoop
	p.switches = oldSwitch
	p.scope = old
	d.end = len(p.tokens)
	_ = async
	return d
}
func (p *parser) method(start int, async bool) {
	d := p.newDef("method", start)
	d.async = async
	old := p.scope
	p.child()
	p.scope.function = true
	oldLoop, oldSwitch := p.loop, p.switches
	p.loop = 0
	p.switches = 0
	p.stack = append(p.stack, d)
	d.nonSimple = p.parameters()
	p.block(false)
	p.stack = p.stack[:len(p.stack)-1]
	p.loop = oldLoop
	p.switches = oldSwitch
	p.scope = old
	d.end = len(p.tokens)
}
func (p *parser) class(decl bool) *definition {
	p.enter()
	defer p.leave()
	start := p.need("class")
	var b *binding
	name := -1
	if p.peek(true).kind == "id" {
		name = p.identifier()
		if decl {
			b = p.declare(name)
		}
	} else if decl {
		refusal("missing_class_name")
	}
	d := p.newDef("class", start)
	d.bind = b
	if b != nil {
		b.kind = "class"
		b.def = d
	}
	old := p.scope
	p.child()
	if !decl && name >= 0 {
		b = p.declare(name)
		b.def = d
		d.bind = b
	}
	p.stack = append(p.stack, d)
	// Inheritance can invoke arbitrary constructor behavior outside this level.
	if p.eat("extends") {
		refusal("unsupported_inheritance")
	}
	p.need("{")
	constructorSeen := false
	for !p.is("}", true) {
		if p.eat(";") {
			continue
		}
		memberStart := len(p.tokens)
		static := p.eat("static")
		if p.is("{", true) {
			refusal("unsupported_static_block")
		}
		i := p.property()
		key := p.tokens[i].raw
		semanticKey := propertyKey(key)
		async := false
		if key == "get" || key == "set" {
			if !p.is("(", false) {
				refusal("unsupported_accessor")
			}
		}
		if key == "async" {
			if !p.is("(", false) {
				if p.peek(true).newline {
					refusal("async_newline")
				}
				async = true
				i = p.property()
				key = p.tokens[i].raw
				semanticKey = propertyKey(key)
			}
		}
		if p.is("(", false) {
			if static && semanticKey == "prototype" {
				refusal("invalid_static_prototype")
			}
			if !static && semanticKey == "constructor" {
				if async || constructorSeen {
					refusal("invalid_constructor")
				}
				constructorSeen = true
			}
			if semanticKey == "constructor" && async {
				refusal("invalid_constructor")
			}
			p.method(memberStart, async)
			if static {
				d.methods[semanticKey] = true
			}
		} else {
			if semanticKey == "constructor" || static && semanticKey == "prototype" {
				refusal("invalid_class_field")
			}
			if p.eat("=") {
				e := p.expression(2)
				p.maskValue(key, e)
			}
			p.semicolon()
		}
	}
	p.need("}")
	p.stack = p.stack[:len(p.stack)-1]
	p.scope = old
	d.end = len(p.tokens)
	return d
}

var precedence = map[string]int{",": 1, "=": 2, "+=": 2, "-=": 2, "*=": 2, "/=": 2, "%=": 2, "**=": 2, "<<=": 2, ">>=": 2, ">>>=": 2, "&=": 2, "|=": 2, "^=": 2, "&&=": 2, "||=": 2, "??=": 2, "?": 3, "??": 4, "||": 4, "&&": 5, "|": 6, "^": 7, "&": 8, "==": 9, "!=": 9, "===": 9, "!==": 9, "<": 10, ">": 10, "<=": 10, ">=": 10, "instanceof": 10, "<<": 11, ">>": 11, ">>>": 11, "+": 12, "-": 12, "*": 13, "/": 13, "%": 13, "**": 14}

func (p *parser) expression(min int) expr {
	p.enter()
	defer p.leave()
	e := p.prefix()
	for {
		t := p.peek(false)
		if t.raw == "in" {
			refusal("unsupported_in_operator")
		}
		if t.raw == "=>" {
			refusal("invalid_arrow")
		}
		prec, ok := precedence[t.raw]
		if !ok || prec < min {
			break
		}
		p.take(false)
		if t.raw == "?" {
			p.expression(2)
			p.need(":")
			p.expression(2)
			e = expr{start: e.start, end: len(p.tokens), ref: -1}
			continue
		}
		if t.raw == "**" && e.unary {
			refusal("invalid_unary_exponent")
		}
		if prec == 2 {
			if !e.assignable {
				refusal("invalid_assignment")
			}
			if e.ref >= 0 {
				p.write(e.ref)
			}
		}
		next := prec + 1
		if prec == 2 || t.raw == "**" {
			next = prec
		}
		r := p.expression(next)
		logical := e.logical | r.logical
		if t.raw == "??" {
			logical |= 1
		}
		if t.raw == "&&" || t.raw == "||" {
			logical |= 2
		}
		if logical == 3 {
			refusal("invalid_nullish_mix")
		}
		if prec == 2 {
			if e.member != "" {
				p.maskValue(e.member, r)
			}
			if e.ref >= 0 && r.def != nil {
				refusal("unsupported_reassigned_function")
			}
		}
		e = expr{start: e.start, end: len(p.tokens), ref: -1, logical: logical}
	}
	return e
}
func (p *parser) write(i int) {
	// Resolve after the complete scope is parsed (including hoisted declarations).
	if p.tokens[i].raw == "eval" || p.tokens[i].raw == "arguments" {
		refusal("unsupported_restricted_write")
	}
	p.tokens[i].role = "write"
}
func (p *parser) call(e expr, construct bool) {
	c := &call{ref: e.ref, member: e.member, construct: construct}
	for _, d := range p.stack {
		p.callRefs++
		if p.callRefs > 100_000 {
			refusal("call_limit")
		}
		d.calls = append(d.calls, c)
	}
}
func (p *parser) arguments() {
	p.need("(")
	for !p.is(")", true) {
		p.eat("...")
		p.expression(2)
		if !p.eat(",") {
			break
		}
	}
	p.need(")")
}

func (p *parser) prefix() expr {
	start := len(p.tokens)
	t := p.peek(true)
	e := expr{start: start, ref: -1}
	switch {
	case t.kind == "id":
		i := p.identifier()
		if p.eat("=>") {
			return p.arrow(start, []int{i})
		}
		e.ref = i
		e.assignable = true
		e.paramTokens = []int{i}
	case t.kind == "number" || t.kind == "string" || t.kind == "regexp":
		p.take(true)
	case t.kind == "template_part" || t.kind == "template_end":
		p.template()
	case t.raw == "true" || t.raw == "false" || t.raw == "null" || t.raw == "this":
		p.take(true)
	case t.raw == "function":
		e.def = p.function(false, false)
	case t.raw == "class":
		e.def = p.class(false)
	case t.raw == "async":
		p.take(true)
		if p.peek(true).newline {
			refusal("unsupported_async_expression")
		}
		if p.is("function", true) {
			e.def = p.function(false, true)
			e.def.start = start
		} else if p.peek(true).kind == "id" {
			i := p.identifier()
			p.need("=>")
			return p.arrowFunction(start, []int{i}, true)
		} else {
			refusal("unsupported_async_arrow")
		}
	case t.raw == "(":
		p.take(true)
		var params []int
		validParams := true
		trailing := false
		if !p.is(")", true) {
			for {
				a := p.expression(2)
				if a.end-a.start != 1 || a.ref < 0 {
					validParams = false
				}
				params = append(params, a.ref)
				if !p.eat(",") {
					break
				}
				if p.is(")", true) {
					trailing = true
					break
				}
			}
		}
		p.need(")")
		if p.eat("=>") {
			if !validParams {
				refusal("unsupported_arrow_parameters")
			}
			return p.arrow(start, params)
		}
		if trailing {
			refusal("invalid_trailing_comma")
		}
		if len(params) == 0 {
			refusal("empty_expression")
		}
		// Parenthesized direct calls are dynamic here: retaining an imprecise
		// target would turn (a,b)() into a false uniquely resolved call.
	case t.raw == "[":
		p.take(true)
		for !p.is("]", true) {
			if p.eat(",") {
				continue
			}
			p.eat("...")
			p.expression(2)
			if !p.eat(",") {
				break
			}
		}
		p.need("]")
	case t.raw == "{":
		p.object()
	case t.raw == "new":
		p.take(true)
		if p.peek(true).kind != "id" {
			refusal("unsupported_new_target")
		}
		i := p.identifier()
		target := expr{ref: i}
		for p.eat(".") {
			k := p.memberProperty()
			if target.member != "" {
				target.ref = -1
			}
			target.member = p.tokens[k].raw
		}
		p.call(target, true)
		if p.is("(", false) {
			p.arguments()
		}
	case t.raw == "delete":
		refusal("unsupported_delete")
	case t.raw == "!" || t.raw == "~" || t.raw == "+" || t.raw == "-" || t.raw == "typeof" || t.raw == "void" || t.raw == "await" || t.raw == "++" || t.raw == "--":
		if t.raw == "await" && (len(p.stack) == 0 || !p.stack[len(p.stack)-1].async) {
			refusal("await_outside_async")
		}
		p.take(true)
		a := p.expression(15)
		e.unary = t.raw != "++" && t.raw != "--"
		if t.raw == "++" || t.raw == "--" {
			if !a.assignable {
				refusal("invalid_update")
			}
			if a.ref >= 0 && a.member == "" {
				p.write(a.ref)
			}
		}
	default:
		refusal("unsupported_expression")
	}
	for {
		t = p.peek(false)
		switch t.raw {
		case ".":
			p.take(false)
			i := p.memberProperty()
			if e.member != "" {
				e.ref = -1
			}
			e.member = p.tokens[i].raw
			e.assignable = true
			e.paramTokens = nil
			e.def = nil
		case "[":
			if e.ref >= 0 {
				p.write(e.ref)
			}
			p.take(false)
			p.expression(0)
			p.need("]")
			e.ref = -1
			e.member = ""
			e.assignable = true
			e.paramTokens = nil
			e.def = nil
		case "(":
			p.call(e, false)
			p.arguments()
			e.ref = -1
			e.member = ""
			e.assignable = false
			e.paramTokens = nil
			e.def = nil
		case "++", "--":
			if t.newline {
				e.end = len(p.tokens)
				return e
			}
			if !e.assignable {
				refusal("invalid_update")
			}
			p.take(false)
			if e.ref >= 0 && e.member == "" {
				p.write(e.ref)
			}
			e.assignable = false
			e.ref = -1
		case "?.":
			refusal("unsupported_optional_chain")
		default:
			if t.kind == "template_part" || t.kind == "template_end" {
				refusal("unsupported_tagged_template")
			}
			e.end = len(p.tokens)
			return e
		}
	}
}

func (p *parser) arrow(start int, params []int) expr { return p.arrowFunction(start, params, false) }
func (p *parser) arrowFunction(start int, params []int, async bool) expr {
	d := p.newDef("arrow", start)
	d.async = async
	old := p.scope
	p.child()
	p.scope.function = true
	for _, i := range params {
		if i < 0 {
			refusal("unsupported_arrow_parameters")
		}
		p.tokens[i].scope = p.scope
		p.declare(i)
	}
	oldLoop, oldSwitch := p.loop, p.switches
	p.loop = 0
	p.switches = 0
	p.stack = append(p.stack, d)
	if p.is("{", true) {
		p.block(false)
	} else {
		p.expression(2)
	}
	p.stack = p.stack[:len(p.stack)-1]
	p.loop = oldLoop
	p.switches = oldSwitch
	p.scope = old
	d.end = len(p.tokens)
	return expr{start: start, end: len(p.tokens), ref: -1, def: d}
}
func (p *parser) template() {
	i := p.take(true)
	for p.tokens[i].kind == "template_part" {
		p.expression(0)
		p.need("}")
		if p.look != nil {
			refusal("template_state")
		}
		t := p.lex.template(false)
		t.scope = p.scope
		if len(p.tokens) >= maxTokens {
			refusal("token_limit")
		}
		p.tokens = append(p.tokens, t)
		i = len(p.tokens) - 1
	}
}
func (p *parser) object() {
	p.need("{")
	prototypeSeen := false
	for !p.is("}", true) {
		if p.eat("...") {
			p.expression(2)
		} else if p.eat("[") {
			p.expression(0)
			p.need("]")
			if p.eat(":") {
				p.expression(2)
			} else if p.is("(", false) {
				p.method(len(p.tokens), false)
			} else {
				refusal("syntax")
			}
		} else {
			start := len(p.tokens)
			i := p.property()
			key := p.tokens[i].raw
			if p.eat(":") {
				if propertyKey(key) == "__proto__" {
					if prototypeSeen {
						refusal("duplicate_object_prototype")
					}
					prototypeSeen = true
				}
				e := p.expression(2)
				p.maskValue(key, e)
			} else if p.is("(", false) {
				p.method(start, false)
			} else if key == "async" && (p.peek(true).kind == "id" || p.peek(true).kind == "string") {
				if p.peek(true).newline {
					refusal("async_newline")
				}
				p.property()
				p.method(start, true)
			} else {
				if p.tokens[i].kind != "id" {
					refusal("invalid_shorthand")
				}
				p.tokens[i].role = "shorthand"
				if p.eat("=") {
					refusal("unsupported_object_default")
				}
			}
		}
		if !p.eat(",") {
			break
		}
	}
	p.need("}")
}

func (p *program) find(name string) (*definition, string) {
	var matches []*definition
	for _, d := range p.defs {
		if d.bind != nil && d.bind.name == name {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return nil, "missing_anchor"
	}
	if len(matches) != 1 {
		return nil, "ambiguous_anchor"
	}
	d := matches[0]
	b, ok := resolve(d.bind.scope, name)
	if !ok || b != d.bind {
		return nil, "ambiguous_anchor"
	}
	if p.mutable(b) {
		return nil, "mutable_anchor"
	}
	return d, ""
}
func (p *program) mutable(b *binding) bool {
	return p.written[b]
}
func (p *program) target(c *call) (*definition, string) {
	if c.ref < 0 {
		return nil, "dynamic_callee"
	}
	t := p.tokens[c.ref]
	b, ok := resolve(t.scope, t.raw)
	if !ok {
		return nil, "unresolved_or_ambiguous_callee"
	}
	if b.def == nil {
		return nil, "nonfunction_callee"
	}
	if p.mutable(b) {
		return nil, "mutable_callee"
	}
	d := b.def
	if c.member != "" {
		if d.kind != "class" || !d.methods[c.member] || c.construct {
			return nil, "unresolved_member_callee"
		}
	} else if c.construct && d.kind != "class" && d.kind != "function" {
		return nil, "unsupported_constructor_callee"
	}
	return d, ""
}

func (p *program) normalized(d *definition) string {
	if value, ok := p.hashes[d]; ok {
		return value
	}
	ids := map[string]int{}
	h := sha256.New()
	bindings := map[*binding]int{}
	next := 0
	for _, t := range p.tokens[d.start:d.end] {
		p.work++
		if p.work > 8_000_000 {
			refusal("comparison_work_limit")
		}
		if p.work&1023 == 0 && p.ctx.Err() != nil {
			refusal("deadline")
		}
		raw := t.raw
		if t.mask {
			raw = "<metadata-value>"
		} else if t.role == "identifier" || t.role == "write" || t.role == "shorthand" {
			key := ""
			b, ok := resolve(t.scope, t.raw)
			var id int
			if ok {
				var seen bool
				id, seen = bindings[b]
				if !seen {
					id = next
					next++
					bindings[b] = id
				}
			} else {
				// Ambiguous identifiers are never folded together. Calls through
				// them separately refuse; free data names retain identity here.
				key = t.raw
				var seen bool
				id, seen = ids[key]
				if !seen {
					id = next
					next++
					ids[key] = id
				}
			}
			raw = fmt.Sprintf("$%d", id)
			if t.role == "shorthand" {
				raw = t.raw + ":" + raw
			}
		}
		// Newline presence is retained: ASI, return and postfix operators use it.
		if t.newline {
			io.WriteString(h, "N")
		}
		fmt.Fprintf(h, "%d:%s", len(raw), raw)
	}
	out := fmt.Sprintf("%x", h.Sum(nil))
	p.hashes[d] = out
	return out
}
