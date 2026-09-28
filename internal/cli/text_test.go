package cli

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Proves flags parse anywhere among positionals, "--" ends flag parsing, and
// unknown flags are usage errors.
func TestParseInterspersed(t *testing.T) {
	fs := newFlagSet("t")
	topic := fs.String("topic", "", "")
	verbose := fs.Bool("v", false, "")
	pos, err := parseInterspersed(fs, []string{"room", "--topic", "x", "-v", "hello", "--", "--not-a-flag", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if *topic != "x" || !*verbose || strings.Join(pos, "|") != "room|hello|--not-a-flag|-" {
		t.Fatalf("topic=%q v=%v pos=%q", *topic, *verbose, pos)
	}
	fs2 := newFlagSet("t")
	fs2.String("topic", "", "")
	pos, err = parseInterspersed(fs2, []string{"--topic=y", "-"})
	if err != nil || strings.Join(pos, "|") != "-" {
		t.Fatalf("pos=%q err=%v", pos, err)
	}
	if _, err := parseInterspersed(newFlagSet("t"), []string{"--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	_, err = parseInterspersed(newFlagSet("t"), []string{"--nope"})
	var ue usageError
	if !errors.As(err, &ue) {
		t.Fatalf("err %T is not a usage error", err)
	}
}

// Proves every accepted room form normalizes to #alias:server or stays a room id,
// and malformed rooms are refused.
func TestNormalizeRoom(t *testing.T) {
	for in, want := range map[string]string{
		"lobby": "#lobby:s", "#lobby": "#lobby:s", "#lobby:other": "#lobby:other", "!abc": "!abc", "a2a-swarm": "#a2a-swarm:s",
	} {
		if got, err := normalizeRoom(in, "s"); err != nil || got != want {
			t.Errorf("normalizeRoom(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "#", "a b", "#x/y"} {
		if _, err := normalizeRoom(bad, "s"); err == nil {
			t.Errorf("normalizeRoom(%q) accepted", bad)
		}
	}
}

// Proves every accepted user form normalizes to @name:server and malformed users are refused.
func TestNormalizeUser(t *testing.T) {
	for in, want := range map[string]string{"bob": "@bob:s", "@bob": "@bob:s", "@bob:x": "@bob:x", "Bob": "@bob:s"} {
		if got, err := normalizeUser(in, "s"); err != nil || got != want {
			t.Errorf("normalizeUser(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "@", "a b", "x/y"} {
		if _, err := normalizeUser(bad, "s"); err == nil {
			t.Errorf("normalizeUser(%q) accepted", bad)
		}
	}
}

// Proves sanitizers remove C0/C1 controls and bidi overrides, keep printable
// Unicode, and that snippet never exceeds its limit.
func TestSanitize(t *testing.T) {
	in := "ok\x1b[31m red‮\u0085 \x00ümlaut\n next\tTab"
	if got := sanitizeLine(in); got != "ok[31m red ümlaut next Tab" {
		t.Fatalf("sanitizeLine = %q", got)
	}
	if got := sanitizeText(in); got != "ok[31m red ümlaut\n next\tTab" {
		t.Fatalf("sanitizeText = %q", got)
	}
	long := strings.Repeat("é", 500)
	s := snippet(long, SnippetMax)
	if utf8.RuneCountInString(s) != SnippetMax || !strings.HasSuffix(s, "...") {
		t.Fatalf("snippet len %d", utf8.RuneCountInString(s))
	}
	if snippet("short", SnippetMax) != "short" {
		t.Fatal("short snippet changed")
	}
}

// Proves topic bodies parse into slug and title and that non-topic bodies do not.
func TestParseTopicBody(t *testing.T) {
	slug, title, ok := parseTopicBody("[topic] channel-live: the swarm channel\n\nbody")
	if !ok || slug != "channel-live" || title != "the swarm channel" {
		t.Fatalf("got %q %q %v", slug, title, ok)
	}
	for _, bad := range []string{"topic x: y", "[topic] Bad Slug: y", "[topic] noseparator", ""} {
		if _, _, ok := parseTopicBody(bad); ok {
			t.Errorf("parseTopicBody(%q) accepted", bad)
		}
	}
}

// Proves the header block round-trips: formatHeaderBody output is stripped back
// to the text, and bodies without a header are untouched.
func TestHeaderBody(t *testing.T) {
	h := Header{From: "a", To: []string{"b", "c"}, At: "2026-09-29T00:00:00Z", Kind: "ack"}
	body := formatHeaderBody(h, "text\n\nmore")
	if !strings.HasPrefix(body, "- from: a\n- to: b, c\n- at: 2026-09-29T00:00:00Z\n- re: -\n- kind: ack\n\n") {
		t.Fatalf("body = %q", body)
	}
	if stripHeaderBody(body) != "text\n\nmore" || stripHeaderBody("plain\n\ntext") != "plain\n\ntext" {
		t.Fatal("stripHeaderBody broken")
	}
}
