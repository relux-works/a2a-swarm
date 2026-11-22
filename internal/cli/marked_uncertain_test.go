package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestUncertainWriteIsNotACursorTakenElsewhere proves the rule a consumer
// loop decides by when its mailbox write of a marked identity fails. "The
// cursor was taken elsewhere" (stale, or held by a live owner) makes the loop
// reload the State and poll on. A sidecar write whose outcome is uncertain
// does not, alone or joined with the "stale" that the abort after an
// uncertain commit answers: the consumer stops with the failure.
//
// Controls: stale and held are still taken elsewhere; a lock timeout is not.
//
// It proves the classification only. The uncertain write itself is planted and
// followed in the config package, where the writer's seam is.
func TestUncertainWriteIsNotACursorTakenElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"stale", config.ErrBindPartStale, true},
		{"held by a live owner", config.ErrBindPartHeld, true},
		{"lock timeout", config.ErrLockTimeout, false},
		{"uncertain write", fmt.Errorf("%w: the directory was not synced", config.ErrBindPartWrite), false},
		{"uncertain commit, then its abort finds nothing", errors.Join(fmt.Errorf("commit: %w", config.ErrBindPartWrite), config.ErrBindPartStale), false},
	} {
		if got := cursorTakenElsewhere(tc.err); got != tc.want {
			t.Errorf("%s: taken elsewhere = %t, want %t", tc.name, got, tc.want)
		}
	}
}
