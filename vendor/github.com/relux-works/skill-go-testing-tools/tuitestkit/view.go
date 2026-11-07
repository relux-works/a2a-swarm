package tuitestkit

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"
)

// StripANSI removes all ANSI escape sequences from a string.
// Delegates to github.com/charmbracelet/x/ansi.Strip — the canonical
// ANSI stripping implementation from the lipgloss team.
func StripANSI(s string) string {
	return ansi.Strip(s)
}

// --- Model-based assertions ---

// ViewContains asserts that model.View() contains the given text after
// stripping ANSI escape codes. Fails the test if text is not found.
func ViewContains(t testing.TB, model tea.Model, text string) {
	t.Helper()
	ContainsStr(t, model.View(), text)
}

// ViewNotContains asserts that model.View() does NOT contain the given text
// after stripping ANSI escape codes. Fails the test if text is found.
func ViewNotContains(t testing.TB, model tea.Model, text string) {
	t.Helper()
	NotContainsStr(t, model.View(), text)
}

// ViewLines splits model.View() into lines, strips ANSI escape codes from
// each line, and removes trailing empty lines.
func ViewLines(model tea.Model) []string {
	return LinesFromStr(model.View())
}

// ViewLineContains asserts that the line at lineIdx in model.View() contains
// the given text after ANSI stripping. Fails gracefully if lineIdx is out of bounds.
func ViewLineContains(t testing.TB, model tea.Model, lineIdx int, text string) {
	t.Helper()
	lines := ViewLines(model)
	if lineIdx < 0 || lineIdx >= len(lines) {
		t.Errorf("ViewLineContains: line index %d out of range (view has %d lines)", lineIdx, len(lines))
		return
	}
	if !strings.Contains(lines[lineIdx], text) {
		t.Errorf("ViewLineContains: line %d = %q, want it to contain %q", lineIdx, lines[lineIdx], text)
	}
}

// ViewLineEquals asserts that the line at lineIdx in model.View() equals
// the given text exactly after ANSI stripping. Fails gracefully if lineIdx is out of bounds.
func ViewLineEquals(t testing.TB, model tea.Model, lineIdx int, text string) {
	t.Helper()
	lines := ViewLines(model)
	if lineIdx < 0 || lineIdx >= len(lines) {
		t.Errorf("ViewLineEquals: line index %d out of range (view has %d lines)", lineIdx, len(lines))
		return
	}
	if lines[lineIdx] != text {
		t.Errorf("ViewLineEquals: line %d = %q, want %q", lineIdx, lines[lineIdx], text)
	}
}

// ViewMatchesRegex asserts that model.View() (after ANSI stripping) matches
// the given regular expression pattern. Fails if the pattern is invalid or
// does not match.
func ViewMatchesRegex(t testing.TB, model tea.Model, pattern string) {
	t.Helper()
	MatchesRegexStr(t, model.View(), pattern)
}

// --- String-based variants ---

// ContainsStr asserts that the given view string contains text after
// stripping ANSI escape codes. Useful when you already called View() yourself.
func ContainsStr(t testing.TB, view string, text string) {
	t.Helper()
	stripped := StripANSI(view)
	if !strings.Contains(stripped, text) {
		t.Errorf("ContainsStr: view does not contain %q\n  stripped view: %q", text, stripped)
	}
}

// NotContainsStr asserts that the given view string does NOT contain text
// after stripping ANSI escape codes.
func NotContainsStr(t testing.TB, view string, text string) {
	t.Helper()
	stripped := StripANSI(view)
	if strings.Contains(stripped, text) {
		t.Errorf("NotContainsStr: view unexpectedly contains %q\n  stripped view: %q", text, stripped)
	}
}

// LinesFromStr splits a view string into lines, strips ANSI escape codes from
// each line, and removes trailing empty lines.
func LinesFromStr(view string) []string {
	raw := strings.Split(view, "\n")
	lines := make([]string, len(raw))
	for i, line := range raw {
		lines[i] = StripANSI(line)
	}
	// Trim trailing empty lines
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// MatchesRegexStr asserts that the given view string (after ANSI stripping)
// matches the given regular expression pattern. Fails if the pattern is
// invalid or does not match.
func MatchesRegexStr(t testing.TB, view string, pattern string) {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Errorf("MatchesRegexStr: invalid regex %q: %v", pattern, err)
		return
	}
	stripped := StripANSI(view)
	if !re.MatchString(stripped) {
		t.Errorf("MatchesRegexStr: view does not match pattern %q\n  stripped view: %q", pattern, stripped)
	}
}
