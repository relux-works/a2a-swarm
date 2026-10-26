package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Both public formats retain fixed current/legacy publication values. Missing
// and prefix-lookalike values become exactly unknown, without state/mail writes
// or sync. This checks local projection, not remote publication truth.
func TestBindStatusPublicationValues(t *testing.T) {
	known := []string{
		"published", "pending: own liveness publication",
		"unavailable: unbound", "unavailable: invalid own notice telemetry",
		"unavailable: no liveness record (a2a status set)",
		"unavailable: own notice changed", "unavailable: invalid notice retry policy",
		"unavailable: own liveness publication retry pending",
		"unavailable: own liveness publication failed",
	}
	for _, format := range []string{"text", "json"} {
		for i, value := range append(known, "", "published "+bindDiagnosticMarker, "unavailable: "+bindDiagnosticMarker, "pending: "+bindDiagnosticMarker) {
			t.Run(format+"/"+value, func(t *testing.T) {
				e, _, _ := bindDiagnosticEnv(t)
				if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal.NoticePublication = value
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				bindPendingAppend(t, e, config.MailEntry{ID: "pending", TS: e.now().UnixMilli()})
				state := readFile(t, e.paths().StateFile("alice"))
				mail := readFile(t, e.paths().MailboxFile("alice"))
				syncs := e.hs.CallCount("GET /_matrix/client/v3/sync")
				args := []string{"bind", "status"}
				if format == "json" {
					args = append(args, "--json")
				}
				out := e.as("alice", args...).ok(t).stdout
				want := value
				if i >= len(known) {
					want = "unknown"
				}
				if format == "json" {
					var status struct{ Publication string }
					if err := json.Unmarshal([]byte(out), &status); err != nil {
						t.Fatal(err)
					}
					if status.Publication != want {
						t.Fatalf("publication=%q; want %q", status.Publication, want)
					}
				} else if !strings.HasSuffix(out, "publication="+want+"\n") {
					t.Fatalf("wrong publication projection: %q", out)
				}
				if strings.Contains(out, bindDiagnosticMarker) || state != readFile(t, e.paths().StateFile("alice")) || mail != readFile(t, e.paths().MailboxFile("alice")) || syncs != e.hs.CallCount("GET /_matrix/client/v3/sync") {
					t.Fatal("projection leaked arbitrary text or changed authority")
				}
			})
		}
	}
}

// Both actual delivery guards preserve ordinary error identity/text and refuse
// callbacks/writes. Terminal projection alone emits fixed connection codes,
// directly or through an outer mailbox wrapper. No live registry race is claimed.
func TestBindDeliveryConnectionProvenance(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(map[int]string{0: "valid", 1: "entry", 2: "locked"}[failAt], func(t *testing.T) {
			e, _, s := bindDiagnosticEnv(t)
			bindPendingAppend(t, e, config.MailEntry{ID: "pending", TS: e.now().UnixMilli()})
			state := readFile(t, e.paths().StateFile("alice"))
			mail := readFile(t, e.paths().MailboxFile("alice"))
			original := errors.New(bindDiagnosticMarker)
			calls := 0
			s.checkBinding = func() error {
				calls++
				if calls == failAt {
					return original
				}
				return nil
			}
			called := false
			_, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(*config.State, *config.Mailbox) ([]string, error) {
				called = true
				return nil, nil
			})
			if failAt == 0 {
				if err != nil || !called {
					t.Fatalf("valid delivery refused: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, original) || err.Error() != original.Error() || called {
				t.Fatalf("delivery guard changed error contract or ran callback: %v", err)
			}
			for _, projected := range []error{err, terminalAt("mailbox", err)} {
				d := terminalErrorDiagnostic("mailbox", projected, e.now())
				if d.Phase != "connection" || d.Class != "unavailable" {
					t.Fatal(d)
				}
			}
			if strings.Contains(terminalAt("mailbox", err).Error(), bindDiagnosticMarker) || state != readFile(t, e.paths().StateFile("alice")) || mail != readFile(t, e.paths().MailboxFile("alice")) {
				t.Fatal("terminal error leaked arbitrary text or guard changed authority")
			}
		})
	}
}
