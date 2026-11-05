package cli

import (
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// mirrorChunks preserves every byte and keeps fences of at most 4000 runes
// atomic. Longer fences use the historical 4000-rune boundaries instead.
// Unclosed fences run to end of source. If fence preservation exceeds eight
// chunks, use historical boundaries for the entire message. The caller bounds
// source to 32000 runes (or a shorter truncation notice).
func mirrorChunks(source string) []string {
	data := []byte(source)
	positions := &mirrorFenceParser{BlockParser: parser.NewFencedCodeBlockParser(), source: source}
	markdown := goldmark.New(goldmark.WithParserOptions(parser.WithBlockParsers(util.Prioritized(positions, 699))))
	markdown.Parser().Parse(text.NewReader(data))
	fences := positions.spans

	var chunks []string
	for start := 0; start < len(source); {
		end := start
		for count := 0; end < len(source) && count < 4000; count++ {
			_, size := utf8.DecodeRuneInString(source[end:])
			end += size
		}
		for _, span := range fences {
			if end > span[0] && end < span[1] && utf8.RuneCountInString(source[span[0]:span[1]]) <= 4000 {
				if span[0] > start {
					end = span[0]
				} else {
					end = span[1]
				}
				break
			}
		}
		chunks = append(chunks, source[start:end])
		start = end
	}
	if len(chunks) > 8 {
		chunks = nil
		chars := []rune(source)
		for len(chars) > 0 {
			count := min(4000, len(chars))
			chunks = append(chunks, string(chars[:count]))
			chars = chars[count:]
		}
	}
	return chunks
}

// Wrap goldmark's fence parser so source ranges include opening/closing lines,
// even for empty blocks and nested list/quote fences. No second fence grammar.
type mirrorFenceParser struct {
	parser.BlockParser
	source string
	spans  [][2]int
	nodes  map[ast.Node]int
}

func (p *mirrorFenceParser) Open(parent ast.Node, r text.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := r.PeekLine()
	n, state := p.BlockParser.Open(parent, r, pc)
	if n != nil {
		start := strings.LastIndex(p.source[:segment.Start], "\n") + 1
		if p.nodes == nil {
			p.nodes = make(map[ast.Node]int)
		}
		p.nodes[n] = len(p.spans)
		p.spans = append(p.spans, [2]int{start, len(p.source)})
	}
	return n, state
}
func (p *mirrorFenceParser) Continue(n ast.Node, r text.Reader, pc parser.Context) parser.State {
	_, segment := r.PeekLine()
	state := p.BlockParser.Continue(n, r, pc)
	if state&parser.Close != 0 {
		p.spans[p.nodes[n]][1] = segment.Stop
	}
	return state
}
