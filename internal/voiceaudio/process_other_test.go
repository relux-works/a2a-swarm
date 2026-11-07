//go:build !linux || !amd64

package voiceaudio

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsupportedAdapter(t *testing.T) {
	// A valid synthetic stream must still refuse on an unqualified platform,
	// regardless of whether a caller supplies a decoder profile. No native
	// executable or platform qualification is required for this control.
	for _, tc := range []struct {
		name    string
		profile Profile
	}{
		{"missing-profile", Profile{}},
		{"populated-profile", Profile{ProfileID, "/not-provisioned/decoder", strings.Repeat("0", 64), "darwin/arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The test runner's temp parent may contain a symlink. Supply the
			// physical path of our new private directory to reach the adapter.
			tmp, err := filepath.EvalSymlinks(privateTemp(t))
			if err != nil {
				t.Fatal(err)
			}
			r := Request{Bytes: fixture(t, "mono-1s"), TMPDIR: tmp, Profile: tc.profile}
			refuse(t, r, PlatformUnqualified)
			assertEmpty(t, r.TMPDIR)
		})
	}
}
