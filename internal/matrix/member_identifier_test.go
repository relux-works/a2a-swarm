package matrix

import (
	"strings"
	"testing"
)

// Proves received-event grammar boundaries, not registration or DNS reachability.
// Historical localparts and exact byte limits are checked beside invalid IDs.
func TestMemberUserIDReceivedGrammar(t *testing.T) {
	for _, id := range []string{
		"@:example.org", "@A b\t\n\r\x1b\x7f界:EXAMPLE.Org", "@a/b+c:example.org:00000",
		"@x:127.000.0.1", "@x:[2001:db8::1]", "@x:[::ffff:192.0.2.1]:99999",
		"@x:a..b", "@" + strings.Repeat("x", 252) + ":s", "@" + strings.Repeat("é", 126) + ":s",
	} {
		if !memberUserID(id) {
			t.Errorf("valid received ID rejected: %q", id)
		}
	}
	for _, id := range []string{
		"", "@", "x:s", "@x", "@x:", "@x\x00:s", "@\xff:s", "@\xed\xa0\x80:s",
		"@x:example.org/path", "@x:example.org?x=y", "@x:example.org#x", "@x:example.org@evil",
		"@x::8448", "@x:example.org:", "@x:example.org:wrong", "@x:example.org:+1", "@x:example.org:123456",
		"@x:example.org:1:2", "@x:example.org\t", "@x:é.org", "@x:exa_mple.org",
		"@x:2001:db8::1", "@x:[::1", "@x:[::1]junk", "@x:[::1]:", "@x:[::1%en0]", "@x:[127.0.0.1]", "@x:[2001:::1]",
		"@" + strings.Repeat("x", 253) + ":s", "@" + strings.Repeat("é", 126) + "x:s",
	} {
		if memberUserID(id) {
			t.Errorf("invalid received ID accepted: %q", id)
		}
	}
}
