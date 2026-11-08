package cli

import (
	"strconv"
	"strings"
)

// senderText renders the full decoded event sender without clipping or lossy
// sanitization. Bare MXIDs start with @; all other values use reversible ASCII
// quoting so they cannot introduce fields, lines or terminal controls.
func senderText(sender string) string {
	if strings.HasPrefix(sender, "@") {
		bare := true
		for i := 0; i < len(sender); i++ {
			ch := sender[i]
			if ch < 0x21 || ch > 0x7e || ch == '"' || ch == '\\' || ch == '=' {
				bare = false
				break
			}
		}
		if bare {
			return sender
		}
	}
	return strconv.QuoteToASCII(sender)
}
