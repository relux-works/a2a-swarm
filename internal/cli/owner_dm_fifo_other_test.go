//go:build !unix

package cli

import (
	"path/filepath"
	"testing"
)

func ownerTranscriptSentinel(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "must-not-open")
}
