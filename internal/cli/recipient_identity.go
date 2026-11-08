package cli

import (
	"net/netip"
	"strings"
)

// messageRecipient keeps explicit message targets separate from the legacy
// selected-server shorthand. It never discovers accounts or repairs an ID.
func messageRecipient(target, selectedServer string) (mxid string, explicit bool, err error) {
	if !strings.ContainsRune(target, ':') {
		mxid, err = normalizeUser(target, selectedServer)
		return mxid, false, err
	}
	if !strings.HasPrefix(target, "@") {
		return "", true, usagef("invalid user %q: explicit targets require @name:server", target)
	}
	if lower, ascii := recipientASCIILower(target); ascii && lower != target && recipientExactTarget(lower) {
		return "", true, usagef("invalid user %q: explicit targets must be lowercase; use %q", target, lower)
	}
	if !recipientExactTarget(target) {
		return "", true, usagef("invalid user %q: unsupported exact target; expected @name:server with a nonempty supported localpart and server", target)
	}
	return target, true, nil
}

// recipientASCIILower offers a spelling hint only for an ASCII input. Unicode
// case folding can name a different account and must never suggest a target.
func recipientASCIILower(target string) (string, bool) {
	b := []byte(target)
	for i, c := range b {
		if c >= 0x80 {
			return "", false
		}
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

func recipientExactTarget(target string) bool {
	if !strings.HasPrefix(target, "@") || len(target) > 255 {
		return false
	}
	local, server, ok := strings.Cut(target[1:], ":")
	if !ok || local == "" {
		return false
	}
	for _, c := range []byte(local) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("._=-/+", rune(c))) {
			return false
		}
	}
	lower, ascii := recipientASCIILower(server)
	return ascii && lower == server && recipientServerName(server)
}

// Keep the same server-name semantics as matrix.memberServerName, scoped to
// explicit outgoing recipients. Received historical IDs retain their grammar.
// No DNS lookup, spelling normalization or numeric port-range restriction.
func recipientServerName(server string) bool {
	host, tail := server, ""
	if strings.HasPrefix(server, "[") {
		end := strings.IndexByte(server, ']')
		if end < 0 {
			return false
		}
		host, tail = server[1:end], server[end+1:]
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return false
		}
	} else {
		if pos := strings.IndexByte(server, ':'); pos >= 0 {
			host, tail = server[:pos], server[pos:]
		}
		if len(host) == 0 || len(host) > 255 {
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
