package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	xhtml "golang.org/x/net/html"
)

// HTML is optional: reserve 16 KiB of Matrix's 64 KiB event limit for the
// server envelope. Plain content retains the historical sender/server rules.
const maxTextContentBytes = 48 << 10

var messageMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(literalMessageRenderer{}, 100))),
)

// Override before the default HTML renderer (priority 1000). Source HTML is
// always text, including comments and closing lines; images never emit img.
type literalMessageRenderer struct{}

func (literalMessageRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	for _, kind := range []ast.NodeKind{ast.KindRawHTML, ast.KindHTMLBlock, ast.KindImage} {
		r.Register(kind, func(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			_, err := w.WriteString(html.EscapeString(messageNodeText(n, source)))
			return ast.WalkSkipChildren, err
		})
	}
}

// Semantic text excludes Markdown delimiters, link destinations/definitions,
// and fence info. Raw HTML remains literal; images use alt or source URL.
func messageNodeText(n ast.Node, source []byte) string {
	switch n := n.(type) {
	case *ast.Image:
		alt := string(n.Text(source))
		if strings.TrimSpace(alt) == "" {
			return string(n.Destination)
		}
		return html.UnescapeString(string(util.UnescapePunctuations([]byte(alt))))
	case *ast.RawHTML:
		return string(n.Segments.Value(source))
	case *ast.HTMLBlock:
		value := string(n.Lines().Value(source))
		if n.HasClosure() {
			value += string(n.ClosureLine.Value(source))
		}
		return value
	case *ast.Text:
		value := string(n.Value(source))
		if !n.IsRaw() {
			value = html.UnescapeString(string(util.UnescapePunctuations([]byte(value))))
		}
		if n.SoftLineBreak() || n.HardLineBreak() {
			value += "\n"
		}
		return value
	case *ast.String:
		return html.UnescapeString(string(util.UnescapePunctuations(n.Value)))
	case *ast.AutoLink:
		return string(n.Label(source))
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		return string(n.Lines().Value(source))
	}
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		b.WriteString(messageNodeText(c, source))
	}
	if n.Type() == ast.TypeBlock {
		b.WriteByte('\n')
	}
	return b.String()
}

func visibleMessageText(source string) string {
	var b strings.Builder
	z := xhtml.NewTokenizer(strings.NewReader(source))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return b.String()
		case xhtml.TextToken:
			b.Write(z.Text())
		case xhtml.StartTagToken, xhtml.EndTagToken:
			name, _ := z.TagName()
			if strings.Contains("|p|br|hr|pre|blockquote|li|h1|h2|h3|h4|h5|h6|tr|td|th|", "|"+string(name)+"|") {
				b.WriteByte('\n')
			}
		}
	}
}

// Compare case-sensitive Unicode letter/digit/mark runs with multiplicity.
// Empty visible output or a missing semantic word fails closed to plaintext.
// Punctuation is not a word; AST extraction excludes normal Markdown syntax.
func retainsMessageWords(expected, rendered string) bool {
	visible := visibleMessageText(rendered)
	if strings.TrimSpace(visible) == "" {
		return false
	}
	words := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) })
	}
	counts := map[string]int{}
	for _, word := range words(visible) {
		counts[word]++
	}
	for _, word := range words(expected) {
		counts[word]--
		if counts[word] < 0 {
			return false
		}
	}
	return true
}

// textContent accepts the final plaintext, after any caller-specific redaction
// and framing. It never edits body or the supplied protocol fields.
func textContent(body string, fields map[string]any) (map[string]any, error) {
	return framedTextContent(body, fields, "", body, "")
}

// Framing is escaped separately so user Markdown and reference definitions can
// never style it. body remains the caller's byte-identical protocol text.
func framedTextContent(body string, fields map[string]any, prefix, markdown, suffix string) (map[string]any, error) {
	content := make(map[string]any, len(fields)+4)
	for k, v := range fields {
		content[k] = v
	}
	content["msgtype"], content["body"] = "m.text", body
	delete(content, "format")
	delete(content, "formatted_body")
	// Validate the original plain event, without adding a new local size limit.
	if _, err := json.Marshal(content); err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	var rendered bytes.Buffer
	source := []byte(markdown)
	doc := messageMarkdown.Parser().Parse(text.NewReader(source))
	if err := messageMarkdown.Renderer().Render(&rendered, source, doc); err != nil {
		return content, nil
	}
	frame := func(s string) string {
		if s == "" {
			return ""
		}
		return "<p>" + strings.ReplaceAll(html.EscapeString(s), "\n", "<br>") + "</p>\n"
	}
	safe := sanitizeMessageHTML(frame(prefix) + rendered.String() + frame(suffix))
	expected := prefix + "\n" + messageNodeText(doc, source) + "\n" + suffix
	formatted := prefix != "" || suffix != ""
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() != ast.KindDocument && n.Kind() != ast.KindParagraph && n.Kind() != ast.KindText {
			formatted = true
		}
		return ast.WalkContinue, nil
	})
	if formatted && retainsMessageWords(expected, safe) {
		content["format"], content["formatted_body"] = "org.matrix.custom.html", safe
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	if len(raw) > maxTextContentBytes {
		delete(content, "format")
		delete(content, "formatted_body")
	}
	return content, nil
}

// Matrix's m.room.message HTML subset. Deliberately emit only the subset used
// by our renderer; raw HTML is rendered literally, and this second allowlist
// strips comments, images, foreign namespaces and every unapproved attribute.
func sanitizeMessageHTML(source string) string {
	doc, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(html.EscapeString(n.Data))
			return
		}
		if n.Type != xhtml.ElementNode && n.Type != xhtml.DocumentNode {
			return
		}
		tag := n.Data
		allowed := n.Namespace == "" && strings.Contains("|p|br|hr|blockquote|pre|code|em|strong|del|ul|ol|li|h1|h2|h3|h4|h5|h6|table|thead|tbody|tr|th|td|", "|"+tag+"|")
		href := ""
		if tag == "a" && n.Namespace == "" {
			for _, a := range n.Attr {
				if a.Namespace == "" && a.Key == "href" && allowedMessageLink(a.Val) {
					href = a.Val
				}
			}
			allowed = href != ""
		}
		if tag == "script" || tag == "style" || tag == "iframe" || tag == "svg" || tag == "math" {
			return
		}
		if allowed {
			b.WriteString("<" + tag)
			if href != "" {
				b.WriteString(` href="` + html.EscapeString(href) + `"`)
			}

			if tag == "ol" {
				for _, a := range n.Attr {
					if a.Namespace == "" && a.Key == "start" {
						if value, err := strconv.ParseUint(a.Val, 10, 32); err == nil {
							b.WriteString(` start="` + strconv.FormatUint(value, 10) + `"`)
						}
					}
				}
			}
			// Matrix permits language-* on code, but no arbitrary CSS classes.
			if tag == "code" {
				for _, a := range n.Attr {
					if a.Namespace == "" && a.Key == "class" && strings.HasPrefix(a.Val, "language-") && !strings.ContainsAny(a.Val, " \t\r\n") {
						b.WriteString(` class="` + html.EscapeString(a.Val) + `"`)
					}
				}
			}
			b.WriteString(">")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if allowed && tag != "br" && tag != "hr" {
			b.WriteString("</" + tag + ">")
		}
	}
	walk(doc)
	return b.String()
}

func allowedMessageLink(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "mxc":
		return true
	default:
		return false
	}
}
