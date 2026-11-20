package tmuxwake

import (
	"context"
	"errors"
	"testing"
)

// The bound adapter forwards the caller's final audience fence after exact
// owned-notice assessment. Refusal retains the typed notice and sends no Enter;
// this fixture proves no provider UI behavior or atomic human-input guarantee.
func TestBoundSendAudienceFence(t *testing.T) {
	for _, runtime := range []string{"codex", "claude-named"} {
		for _, allow := range []bool{true, false} {
			name := runtime + "/refuse"
			if allow {
				name = runtime + "/allow"
			}
			t.Run(name, func(t *testing.T) {
				f := newBoundFixture(t, runtime, "2.1.310", "idle")
				sample := f.b.AssessReadiness(context.Background())
				if !sample.Eligible() {
					t.Fatal("fixture readiness unavailable")
				}
				calls := 0
				refused := errors.New("fixture audience exclusion")
				got, err := f.b.Send(context.Background(), 1, sample.Key, nil, nil, func() error {
					calls++
					if !allow {
						return refused
					}
					return nil
				})
				state := f.f.wait(func(s fixtureState) bool { return s.PID > 1 })
				if allow {
					if err != nil || got.Disposition != SendSubmitted {
						t.Fatal("positive rejected", got, err)
					}
					state = f.f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
				} else if !errors.Is(err, refused) || got.Disposition != SendRefused || len(state.Submitted) != 0 || state.Text != Notice(1) {
					t.Fatal("audience refusal submitted or altered owned draft", got, err, state)
				}
				if calls != 1 {
					t.Fatal("final audience fence not invoked exactly once")
				}
			})
		}
	}
}
