package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WaitKeywords optionally narrows wait/watch to whole, case-sensitive tokens.
// An omitted field disables this predicate; a present field must be nonempty.
type WaitKeywords []string

func (k *WaitKeywords) UnmarshalJSON(data []byte) error {
	var words []string
	if err := json.Unmarshal(data, &words); err != nil || len(words) == 0 {
		return fmt.Errorf("wait_keywords must be a nonempty list of tokens")
	}
	for _, word := range words {
		if len(word) == 0 || len(word) > 128 {
			return fmt.Errorf("wait_keywords tokens must contain 1-128 ASCII identifier characters")
		}
		for _, b := range []byte(word) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("#._/:@-", rune(b))) {
				return fmt.Errorf("wait_keywords tokens must contain 1-128 ASCII identifier characters")
			}
		}
	}
	*k = words
	return nil
}
