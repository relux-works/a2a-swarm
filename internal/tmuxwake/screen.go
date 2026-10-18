// Package tmuxwake implements the guarded terminal fallback, not an atomic
// input transport. It never handles peer bodies or records captured screens.
package tmuxwake

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)
var rule = regexp.MustCompile(`^─{5,}$`)

type Box struct {
	Text        string
	Row, Column int // expected cursor at the end of the single input line
}

func plain(s string) string { return sgr.ReplaceAllString(s, "") }

// visible drops dim/grey placeholder runs, preserving every plain character
// and space. SGR color payload numbers are not interpreted as separate codes.
func visible(s string) string {
	dim, grey, reverse := false, false, false
	var out strings.Builder
	var cursor string
	emit := func(text string) {
		if cursor != "" {
			if !dim && !grey {
				out.WriteString(cursor)
			}
			cursor = ""
		}
		if dim || grey {
			return
		}
		prefix := strings.TrimLeft(out.String(), " ")
		if reverse && (prefix == "❯ " || prefix == "❯\u00a0" || prefix == "› ") && utf8.RuneCountInString(text) == 1 {
			cursor = text
			return
		}
		out.WriteString(text)
	}
	for len(s) > 0 {
		loc := sgr.FindStringSubmatchIndex(s)
		if loc == nil {
			emit(s)
			break
		}
		if loc[0] > 0 {
			emit(s[:loc[0]])
		}
		var codes []int
		for _, c := range strings.Split(s[loc[2]:loc[3]], ";") {
			n, _ := strconv.Atoi(c)
			codes = append(codes, n)
		}
		for i := 0; i < len(codes); i++ {
			switch c := codes[i]; c {
			case 0:
				dim, grey, reverse = false, false, false
			case 2:
				dim = true
			case 22:
				dim = false
			case 7:
				reverse = true
			case 27:
				reverse = false
			case 39:
				grey = false
			case 90:
				grey = true
			case 38, 48:
				if i+2 < len(codes) && codes[i+1] == 5 {
					if c == 38 {
						v := codes[i+2]
						grey = v == 8 || v == 7 || v >= 240 && v <= 255
					}
					i += 2
				} else if i+4 < len(codes) && codes[i+1] == 2 {
					if c == 38 {
						grey = codes[i+2] >= 80 && codes[i+2] <= 200 && codes[i+2] == codes[i+3] && codes[i+3] == codes[i+4]
					}
					i += 4
				}
			default:
				if c >= 30 && c <= 37 || c >= 91 && c <= 97 {
					grey = false
				}
			}
		}
		s = s[loc[1]:]
	}
	// A lone reverse cell without a following dim/grey run is real input.
	if cursor != "" {
		out.WriteString(cursor)
	}
	return out.String()
}

// Input recognizes only the single-line input layouts qualified by fixtures.
// Multiline/wrapped drafts are deliberately unavailable; no screen is persisted.
func Input(screen, runtime string) (Box, bool) {
	lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
	start := max(0, len(lines)-18)
	bottom := strings.ToLower(plain(strings.Join(lines[start:], "\n")))
	for _, denied := range []string{"esc to interrupt", "escape to interrupt", "do you want to", "would you like to", "implement this plan", "permission required", "permission request", "approval required", "approve this", "allow once", "bypass permissions?", "accept edits?", "enter to confirm"} {
		if strings.Contains(bottom, denied) {
			return Box{}, false
		}
	}
	if runtime == "claude" {
		var rules []int
		for i := start; i < len(lines); i++ {
			if rule.MatchString(strings.TrimSpace(plain(lines[i]))) {
				rules = append(rules, i)
			}
		}
		if len(rules) < 2 {
			return Box{}, false
		}
		a, b := rules[len(rules)-2], rules[len(rules)-1]
		if b != a+2 {
			return Box{}, false
		}
		return inputLine(lines[a+1], "❯", a+1)
	}
	if runtime != "codex" {
		return Box{}, false
	}
	for i := len(lines) - 1; i >= start; i-- {
		if plain(lines[i]) != "›" && !strings.HasPrefix(plain(lines[i]), "› ") {
			continue
		}
		// The footer is separated from the prompt. An unknown intervening line
		// may be a wrapped draft or dialog and cannot count as an empty box.
		if i+1 >= len(lines) || strings.TrimSpace(plain(lines[i+1])) != "" {
			return Box{}, false
		}
		footer := plain(strings.Join(lines[i+1:], "\n"))
		if !strings.Contains(footer, "? for shortcuts") {
			return Box{}, false
		}
		return inputLine(lines[i], "›", i)
	}
	return Box{}, false
}

func inputLine(line, glyph string, row int) (Box, bool) {
	text := visible(line)
	text = strings.TrimLeft(text, " ")
	if text != glyph && !strings.HasPrefix(text, glyph+" ") && !strings.HasPrefix(text, glyph+"\u00a0") {
		return Box{}, false
	}
	if text == glyph {
		text = ""
	} else {
		text = strings.TrimPrefix(text, glyph)
		if strings.HasPrefix(text, "\u00a0") {
			text = strings.TrimPrefix(text, "\u00a0")
		} else {
			text = strings.TrimPrefix(text, " ")
		}
	}
	// tmux pads a capture with spaces. A draft containing only spaces remains
	// uncertain; the cursor check also prevents treating it as empty.
	text = strings.TrimRight(text, " ")
	for _, r := range text {
		if r < 32 || r > 126 {
			return Box{}, false
		}
	}
	return Box{Text: text, Row: row, Column: 2 + len(text)}, true
}
