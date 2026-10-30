package voiceworker

import (
	"strings"
	"testing"
)

// Bad invocation shapes and oversized rune/byte input refuse before native
// model creation; successful native inference is the relocated-release witness.
func TestWorkerRejectsInvalidInput(t *testing.T) {
	for _, tt := range []struct {
		args []string
		text string
	}{
		{nil, "text"},
		{[]string{"only-base"}, "text"},
		{[]string{"base", "out", "extra"}, "text"},
		{[]string{"base", "out"}, strings.Repeat("Я", 6001)},
		{[]string{"base", "out"}, strings.Repeat("x", 24001)},
	} {
		if code := Worker(tt.args, strings.NewReader(tt.text)); code == 0 {
			t.Fatal("invalid native invocation succeeded")
		}
	}
}
