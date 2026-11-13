//go:build linux || darwin

package hostnotification

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// The actual Adapter.Notify path persists UNKNOWN but admits no STREAM trigger
// while muted. Undo leaves the retained UNKNOWN and all prior evidence intact.
// A nearby eligible adapter still uses the original wire/acceptance transaction.
func TestHostScopeMuteTriggerAndUnknown(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		scope := fixture.Scope(f.paths.Base)
		fixture.Enroll(t, scope)
		op := fixture.Mute(t, scope, "host")
		key, _, err := f.a.Notify(context.Background(), []byte("fixed notice"), limit(), time.Time{})
		if !errors.Is(err, wakemute.ErrMuted) || key.Token != "" {
			t.Fatalf("Host mute: %v", err)
		}
		noRequest(t, f.peer)
		before := f.load(t).HostEnrollment.Attempts.Entries
		if len(before) != 0 {
			t.Fatal("ordinary muted poll manufactured UNKNOWN")
		}
		fixture.Unmute(t, op)
		if !reflect.DeepEqual(before, f.load(t).HostEnrollment.Attempts.Entries) {
			t.Fatal("unmute erased UNKNOWN")
		}
	})
	withAdapter(t, func(f *adapterFixture) {
		fixture.Enroll(t, fixture.Scope(f.paths.Base))
		done := f.serve(func(r Request) (Response, error) {
			if r.Method != "trigger" {
				return Response{}, ErrProtocol
			}
			return acceptedResponse(r), nil
		})
		_, out, err := f.a.Notify(context.Background(), []byte("eligible"), limit(), time.Time{})
		joined(t, done)
		if err != nil || out.Result != "accepted" {
			t.Fatalf("Host positive: %v", err)
		}
	})
}

// Interleave mute after the durable UNKNOWN write but before the real trigger.
// The late gate refuses without erasing the retained ambiguity or sending I/O.
func TestHostScopeMuteLateTriggerFence(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		scope := fixture.Scope(f.paths.Base)
		fixture.Enroll(t, scope)
		approved := fixture.Confirm(t, scope, "host-race", "mute")
		var operation wakemute.Operation
		hit := false
		f.clockHook = func() {
			if hit {
				return
			}
			state, err := config.LoadState(f.paths, "member")
			if err != nil {
				t.Fatal(err)
			}
			if len(state.HostEnrollment.Attempts.Entries) == 1 && state.HostEnrollment.Attempts.Entries[0].State == "unknown" {
				hit = true
				out, e := wakemute.Mute(fixture.Context(t), approved)
				if e != nil {
					t.Fatal(e)
				}
				operation = *out.Operation
			}
		}
		key, _, err := f.a.Notify(context.Background(), []byte("late"), limit(), time.Time{})
		if !hit || !errors.Is(err, wakemute.ErrMuted) || key.Token == "" {
			t.Fatalf("late Host admission: %v", err)
		}
		noRequest(t, f.peer)
		f.clockHook = nil
		before := f.load(t).HostEnrollment.Attempts.Entries
		if len(before) != 1 || before[0].State != "unknown" {
			t.Fatal("late UNKNOWN not retained")
		}
		fixture.Unmute(t, operation)
		if !reflect.DeepEqual(before, f.load(t).HostEnrollment.Attempts.Entries) {
			t.Fatal("cleanup erased late UNKNOWN")
		}
	})
}
