package matrixtransport

import (
	"context"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	tr "github.com/relux-works/a2a-swarm/transport"
)

type addressing struct{ transport *Transport }

func canonical(s string, sigil byte) bool {
	if len(s) < 4 || len(s) > 255 || s[0] != sigil || !utf8.ValidString(s) {
		return false
	}
	local, server, ok := strings.Cut(s[1:], ":")
	if !ok || local == "" {
		return false
	}
	for _, r := range local {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return serverName(server)
}

// Server grammar matches the wire client's historical hostname/IPv6 forms.
func serverName(s string) bool {
	host, tail := s, ""
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return false
		}
		host, tail = s[1:end], s[end+1:]
		a, e := netip.ParseAddr(host)
		if e != nil || !a.Is6() || a.Zone() != "" {
			return false
		}
	} else {
		if i := strings.IndexByte(s, ':'); i >= 0 {
			host, tail = s[:i], s[i:]
		}
		if host == "" || len(host) > 255 {
			return false
		}
		for _, b := range []byte(host) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.') {
				return false
			}
		}
	}
	if tail == "" {
		return true
	}
	if tail[0] != ':' || len(tail) < 2 || len(tail) > 6 {
		return false
	}
	for _, b := range []byte(tail[1:]) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
func (a addressing) Principal(s string) (tr.PrincipalID, error) {
	if a.transport.closed.Load() {
		return "", tr.NewError(tr.Unavailable)
	}
	if !canonical(s, '@') {
		return "", tr.NewError(tr.InvalidReference)
	}
	return tr.PrincipalID(s), nil
}
func (a addressing) Conversation(ctx context.Context, s string) (tr.ConversationID, error) {
	if a.transport.closed.Load() || ctx.Err() != nil {
		return "", tr.NewError(tr.Unavailable)
	}
	if canonical(s, '!') {
		return tr.ConversationID(s), nil
	}
	if !canonical(s, '#') {
		return "", tr.NewError(tr.InvalidReference)
	}
	client, _ := readClient(a.transport.client)
	id, e := client.ResolveAlias(ctx, s)
	if e != nil {
		return "", safe(e)
	}
	if !canonical(id, '!') {
		return "", tr.NewError(tr.InvalidReference)
	}
	return tr.ConversationID(id), nil
}
func (addressing) Format(p tr.PrincipalID) string { return string(p) }

// Received principals retain Matrix's historical localparts (including an empty
// localpart and non-NUL controls). Server names still use the closed grammar.
// Addressing deliberately requires a nonempty, printable canonical reference.
func receivedPrincipal(s string) bool {
	if len(s) > 255 || !utf8.ValidString(s) || !strings.HasPrefix(s, "@") {
		return false
	}
	local, server, ok := strings.Cut(s[1:], ":")
	return ok && !strings.ContainsRune(local, 0) && serverName(server)
}

// Match the existing receipt contract: event IDs are opaque after the sigil,
// with a 255-byte maximum. Do not borrow registration/verification restrictions.
func eventID(s string) bool {
	return len(s) >= 2 && len(s) <= 255 && s[0] == '$' && utf8.ValidString(s)
}
