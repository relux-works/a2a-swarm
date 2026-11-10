//go:build linux || darwin

package cli

import (
	"context"
	"io"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/daemon"
)

// This public admission witness compiles unchanged against the base. The
// adjacent raw watch is an intentionally green-on-both preserved control.
func TestCMNoticeAdmissionWitness(t *testing.T) {
	s := newC5Station(t, func(_ context.Context, inv daemon.Invocation) int {
		_, _ = io.WriteString(inv.Stdout, "notice-control\n")
		return 0
	})
	r := s.run(context.Background(), "", "watch", "--notices", "--json")
	if r.code != 0 || r.stdout != "notice-control\n" || r.stderr != "" {
		t.Fatal("counts notice invocation was not admitted")
	}
}
func TestCMRawWatchControl(t *testing.T) {
	s := newC5Station(t, func(_ context.Context, inv daemon.Invocation) int {
		_, _ = io.WriteString(inv.Stdout, "raw-control\n")
		return 0
	})
	r := s.run(context.Background(), "", "watch", "--json")
	if r.code != 0 || r.stdout != "raw-control\n" || r.stderr != "" {
		t.Fatal("raw watch changed")
	}
}
