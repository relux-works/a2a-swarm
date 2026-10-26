package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The shared delivery wrapper preserves underlying typed-error discovery and
// text for nonterminal callers, while nested terminal rendering stays fixed.
// Both actual guard boundaries refuse writes; the valid control runs callback.
func TestReviewR8bDeliveryErrorChainCompatibility(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			e, _, s := bindDiagnosticEnv(t)
			bindPendingAppend(t, e, config.MailEntry{ID: "pending", TS: e.now().UnixMilli()})
			state := readFile(t, e.paths().StateFile("alice"))
			mail := readFile(t, e.paths().MailboxFile("alice"))
			pathErr := &os.PathError{Op: "fixture", Path: "R8B_SYNTHETIC_PATH", Err: os.ErrPermission}
			original := fmt.Errorf("R8B_SYNTHETIC_CONTEXT: %w", pathErr)
			calls := 0
			s.checkBinding = func() error {
				calls++
				if calls == failAt {
					return original
				}
				return nil
			}
			called := false
			_, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(*config.State, *config.Mailbox) ([]string, error) { called = true; return nil, nil })
			if failAt == 0 {
				if err != nil || !called {
					t.Fatal("valid caller refused", err)
				}
				return
			}
			var gotPath *os.PathError
			if err == nil || called || err.Error() != original.Error() || errors.Unwrap(err) != original || !errors.Is(err, os.ErrPermission) || !errors.As(err, &gotPath) || gotPath != pathErr {
				t.Fatal("delivery error-chain compatibility changed", err)
			}
			rendered := terminalAt("dispatch", terminalAt("mailbox", fmt.Errorf("R8B_SYNTHETIC_OUTER: %w", err)))
			d := terminalErrorDiagnostic("dispatch", rendered, e.now())
			if d.Phase != "connection" || d.Class != "permission" || d.Result != "error" || strings.Contains(rendered.Error(), "R8B_SYNTHETIC") {
				t.Fatal("nested terminal projection lost typed provenance or copied text", d)
			}
			if state != readFile(t, e.paths().StateFile("alice")) || mail != readFile(t, e.paths().MailboxFile("alice")) {
				t.Fatal("refused delivery changed durable authority")
			}
		})
	}
}

// The real named-connection change still refuses terminal work with fixed safe
// metadata and the underlying cause preserved. No callback, State/mailbox write
// or network effect is allowed; unrelated current-profile edits remain valid.
func TestReviewR8bNamedTerminalRefusalSafety(t *testing.T) {
	for _, op := range []string{"dispatch", "update"} {
		t.Run(op, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.profile = "alpha"
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			if err = config.SaveState(s.paths, s.name, config.State{NextBatch: "original"}); err != nil {
				t.Fatal(err)
			}
			f.r.Current = "beta"
			f.save()
			called := false
			_, err = s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(*config.State, *config.Mailbox) ([]string, error) { called = true; return nil, nil })
			if err != nil || !called {
				t.Fatal("unrelated profile edit refused", err)
			}
			before, err := os.ReadFile(s.paths.StateFile(s.name))
			if err != nil {
				t.Fatal(err)
			}
			mailBefore, _ := os.ReadFile(s.paths.MailboxFile(s.name))
			effects := f.effects()
			c := f.r.Profiles["alpha"]
			c.Store.AccessTokenService = "test.changed.token"
			f.r.Profiles["alpha"] = c
			f.save()
			called = false
			if op == "dispatch" {
				err = a.dispatchTerminal(context.Background(), s, "fixture-generation")
			} else {
				err = a.terminalUpdate(s, "fixture-generation", func(*config.TerminalBinding) { called = true })
			}
			var failure *terminalFailure
			if err == nil || !errors.As(err, &failure) || failure.phase != "connection" || err.Error() != "binding connection: unavailable" || !strings.Contains(errors.Unwrap(failure).Error(), "selected connection changed") {
				t.Fatal("safe refusal or underlying cause lost", err)
			}
			after, _ := os.ReadFile(s.paths.StateFile(s.name))
			mailAfter, _ := os.ReadFile(s.paths.MailboxFile(s.name))
			if called || string(before) != string(after) || string(mailBefore) != string(mailAfter) || effects != f.effects() {
				t.Fatal("changed connection allowed effects")
			}
		})
	}
}
