package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// MaxBodyBytes caps message and card bodies (Matrix events are limited to 64 KiB in total).
const MaxBodyBytes = 32 << 10

// SnippetMax is the longest body excerpt `a2a wait` ever prints.
const SnippetMax = 200

// isUnsafeRune reports control characters (C0, C1, DEL) and bidi/format
// controls that could spoof or garble terminal output.
func isUnsafeRune(r rune) bool {
	if r == utf8.RuneError {
		return true
	}
	if unicode.IsControl(r) {
		return true
	}
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	}
	return false
}

// sanitizeText strips unsafe characters but keeps newlines and tabs.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if isUnsafeRune(r) {
			return -1
		}
		return r
	}, s)
}

// sanitizeLine turns s into one printable line: control characters (newlines
// included) become spaces and runs of whitespace collapse.
func sanitizeLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if isUnsafeRune(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// snippet returns a single sanitized line of at most max runes.
func snippet(s string, max int) string {
	s = sanitizeLine(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}

// readBody resolves a body argument: "-" reads stdin, "@path" reads a file,
// anything else is literal text. Empty bodies and bodies over MaxBodyBytes are refused.
func (a *App) readBody(arg string) (string, error) {
	data, err := a.readBodyBytes(arg)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("body is not valid UTF-8")
	}
	body := strings.TrimRight(sanitizeText(string(data)), " \t\n")
	if strings.TrimSpace(body) == "" {
		return "", usagef("empty message body refused")
	}
	return body, nil
}

// readBodyBytes returns the exact bytes of a body argument (see readBody),
// bounded by MaxBodyBytes, without any validation or transformation.
func (a *App) readBodyBytes(arg string) ([]byte, error) {
	var data []byte
	var err error
	switch {
	case arg == "-":
		data, err = io.ReadAll(io.LimitReader(a.Stdin, MaxBodyBytes+1))
	case strings.HasPrefix(arg, "@") && len(arg) > 1:
		var f *os.File
		f, err = os.Open(arg[1:])
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(f, MaxBodyBytes+1))
			f.Close()
		}
	default:
		data = []byte(arg)
	}
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > MaxBodyBytes {
		return nil, fmt.Errorf("body too large (limit %d bytes)", MaxBodyBytes)
	}
	return data, nil
}

var aliasLocalRE = regexp.MustCompile(`^[A-Za-z0-9._=\-]+$`)

// normalizeRoom turns "#alias", "alias", "#alias:server" into "#alias:server",
// and returns "!roomid" untouched.
func normalizeRoom(arg, server string) (string, error) {
	switch {
	case arg == "":
		return "", usagef("room required")
	case strings.HasPrefix(arg, "!"):
		return arg, nil
	}
	alias := strings.TrimPrefix(arg, "#")
	local, srv, hasServer := strings.Cut(alias, ":")
	if !aliasLocalRE.MatchString(local) {
		return "", usagef("invalid room %q: use #alias, alias, #alias:server, or !roomid", arg)
	}
	if !hasServer || srv == "" {
		srv = server
	}
	return "#" + local + ":" + srv, nil
}

// resolveRoom returns the room id for a room argument and a display label.
func (s *session) resolveRoom(ctx context.Context, arg string) (string, string, error) {
	norm, err := normalizeRoom(arg, s.server)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(norm, "!") {
		return norm, norm, nil
	}
	id, err := s.client.ResolveAlias(ctx, norm)
	if err != nil {
		return "", "", err
	}
	return id, s.shortAlias(norm), nil
}

// shortAlias drops the server part when it is our own server.
func (s *session) shortAlias(alias string) string {
	return strings.TrimSuffix(alias, ":"+s.server)
}

// normalizeUser turns "name", "@name", "@name:server" into a full user id.
func normalizeUser(arg, server string) (string, error) {
	u := strings.TrimPrefix(arg, "@")
	local, srv, hasServer := strings.Cut(u, ":")
	if !aliasLocalRE.MatchString(local) {
		return "", usagef("invalid user %q: use name, @name, or @name:server", arg)
	}
	if !hasServer || srv == "" {
		srv = server
	}
	return "@" + strings.ToLower(local) + ":" + srv, nil
}

// localpart returns "name" for "@name:server".
func localpart(userID string) string {
	u := strings.TrimPrefix(userID, "@")
	if i := strings.Index(u, ":"); i >= 0 {
		return u[:i]
	}
	return u
}

// roomLabel returns the canonical alias (short form) of a room, or its id.
func (s *session) roomLabel(ctx context.Context, roomID string, cache map[string]string) string {
	if l, ok := cache[roomID]; ok {
		return l
	}
	label := roomID
	var ca struct {
		Alias string `json:"alias"`
	}
	if err := s.client.GetState(ctx, roomID, "m.room.canonical_alias", "", &ca); err == nil && ca.Alias != "" {
		label = s.shortAlias(ca.Alias)
	}
	if cache != nil {
		cache[roomID] = label
	}
	return label
}

// isNotFound requires the interpreted Matrix code. A status-only malformed
// error body cannot establish absence for a fallback or mutation decision.
func isNotFound(err error) bool {
	return matrix.IsCode(err, "M_NOT_FOUND")
}
