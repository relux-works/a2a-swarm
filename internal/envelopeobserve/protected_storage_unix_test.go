//go:build unix

package envelopeobserve

import (
	"os"
	"testing"
)

// TestProtectedStorageAgreement proves the reader and writer enforce the same storage rule with only the reader's non-root caller clause.
func TestProtectedStorageAgreement(t *testing.T) {
	assertRosterStorageAgreement(t)
}

// assertRosterStorageAgreement checks both callers so unilateral drift is caught
// by the reader and writer contract tests, not just by the changed caller.
func assertRosterStorageAgreement(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name      string
		info      fileInfo
		dir, want bool
	}{
		{"root-file", fileInfo{0o644, 0}, false, true},
		{"root-directory", fileInfo{os.ModeDir | 0o755, 0}, true, true},
		{"other-owner", fileInfo{0o644, 1000}, false, false},
		{"group-write-file", fileInfo{0o664, 0}, false, false},
		{"world-write-file", fileInfo{0o646, 0}, false, false},
		{"group-write-directory", fileInfo{os.ModeDir | 0o775, 0}, true, false},
		{"world-write-directory", fileInfo{os.ModeDir | 0o757, 0}, true, false},
		{"symlink", fileInfo{os.ModeSymlink | 0o444, 0}, false, false},
		{"fifo", fileInfo{os.ModeNamedPipe | 0o444, 0}, false, false},
		{"file-as-directory", fileInfo{0o644, 0}, true, false},
		{"directory-as-file", fileInfo{os.ModeDir | 0o755, 0}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := protectedInfo(tc.info, tc.dir); got != (tc.want && os.Geteuid() != 0) {
				t.Fatalf("reader storage decision %v, want %v", got, tc.want && os.Geteuid() != 0)
			}
			if got := HostRosterProtection().Protected(tc.info, tc.dir); got != tc.want {
				t.Fatalf("writer storage decision %v, want %v", got, tc.want)
			}
		})
	}
}
