package cli

import (
	"math"
	"testing"
)

// The native length guards admit exactly the values the Security API and
// C.GoBytes carry without truncation or a negative length, checked at the
// boundaries without allocating large buffers (#138 review, finding 4).
func TestKeychainNativeLengthBounds(t *testing.T) {
	for _, c := range []struct {
		n    int
		want bool
	}{{-1, false}, {0, true}, {1, true}, {math.MaxUint32, true}, {math.MaxUint32 + 1, false}, {math.MaxInt64, false}} {
		if got := nativeInputLen(c.n); got != c.want {
			t.Errorf("nativeInputLen(%d) = %v, want %v", c.n, got, c.want)
		}
	}
	for _, c := range []struct {
		n    uint64
		want bool
	}{{0, true}, {math.MaxInt32, true}, {math.MaxInt32 + 1, false}, {math.MaxUint32, false}} {
		if got := nativeOutputLen(c.n); got != c.want {
			t.Errorf("nativeOutputLen(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
