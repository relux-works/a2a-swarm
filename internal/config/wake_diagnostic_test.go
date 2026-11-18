package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/wakediag"
)

// Both diagnostic pointers are deep-cloned: mutating a working copy cannot
// change memoized history or a concurrently held State snapshot.
func TestWakeDiagnosticClone(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	src := State{Runtime: &codexwake.Binding{WakeDiagnostic: wakediag.Retain(nil, wakediag.NativeDefiniteRefusal, at)}, Terminal: &TerminalBinding{WakeDiagnostic: wakediag.Retain(nil, wakediag.TerminalLayoutRefused, at)}}
	copy, ok := cloneState(&src)
	if !ok || !reflect.DeepEqual(src, copy) {
		t.Fatal("clone disabled or changed history")
	}
	copy.Runtime.WakeDiagnostic.Kind = wakediag.NativeTransportUncertain
	copy.Terminal.WakeDiagnostic.At = at.Add(time.Hour)
	if src.Runtime.WakeDiagnostic.Kind != wakediag.NativeDefiniteRefusal || !src.Terminal.WakeDiagnostic.At.Equal(at) {
		t.Fatal("cloned history aliases source")
	}
}
