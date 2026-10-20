package cli

import (
	"context"
	"encoding/json"
	"github.com/relux-works/a2a-swarm/internal/config"
	"strings"
	"testing"
)

type timestampProbe struct {
	name, value string
	valid       bool
}

var timestampProbes = []timestampProbe{
	{"valid-z", "2026-10-20T06:00:00Z", true},
	{"valid-fraction-offset", "2026-10-20T06:00:00.123+00:00", true},
	{"one-digit-hour", "2026-10-20T6:00:00Z", false},
	{"comma-fraction", "2026-10-20T06:00:00,123Z", false},
}

func probeNotice(field, value string) config.OwnNoticeObservation {
	n := schemaNotice("true")
	n.Since = "2026-10-20T05:00:00Z"
	n.ObservedAt = "2026-10-20T07:00:00Z"
	if field == "since" {
		n.Since = value
	} else {
		n.ObservedAt = value
	}
	return n
}

// Peer bob writes bob's real synthetic state key; alice reads who. Normative
// RFC3339 section 5.6 requires two-digit hours and dot fractions. This probes
// both fields through the production read path with valid UTC neighbors.
func TestReviewCR4TimestampGrammar(t *testing.T) {
	for _, field := range []string{"since", "observed"} {
		t.Run(field, func(t *testing.T) {
			for _, p := range timestampProbes {
				t.Run(p.name, func(t *testing.T) {
					e, _ := world(t)
					e.as("alice", "status", "init").ok(t)
					e.as("bob", "join", "status").ok(t)
					e.as("bob", statusArgs("working")...).ok(t)
					s, err := eApp(e).sessionAs("bob")
					if err != nil {
						t.Fatal(err)
					}
					room, _, err := s.resolveRoom(context.Background(), StatusRoom)
					if err != nil {
						t.Fatal(err)
					}
					record := *e.state("bob").Liveness
					record.OwnNotice = probeNotice(field, p.value)
					if _, err = s.client.PutState(context.Background(), room, StatusEventType, s.creds.UserID, record); err != nil {
						t.Fatal(err)
					}
					raw := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
					text := e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
					var rows []whoRow
					if err = json.Unmarshal([]byte(raw), &rows); err != nil {
						t.Fatal(err)
					}
					found := false
					for _, row := range rows {
						if row.Name != "bob" {
							continue
						}
						found = true
						t.Logf("field=%s value=%q raw=%s text=%q", field, p.value, raw, text)
						if !p.valid {
							if row.Health != "invalid" || row.Record.OwnNotice.PossiblyOrphaned != nil || strings.Contains(text, "SUSPECTED-DEAF") {
								t.Errorf("non-RFC3339 timestamp admitted as fresh factual suspicion")
							}
						} else if row.Health != "fresh" || row.Record.OwnNotice.PossiblyOrphaned == nil {
							t.Error("valid RFC3339 UTC refused")
						}
					}
					if !found {
						t.Fatal("actual writer missing")
					}
				})
			}
		})
	}
}

// Persist the same bytes locally and drive actual bind text/JSON and own-key
// publication. Invalid facts must be unavailable before any Matrix PUT; nearby
// valid UTC values preserve successful publication and the selected schema.
func TestReviewCR4LocalTimestampGrammar(t *testing.T) {
	for _, field := range []string{"since", "observed"} {
		t.Run(field, func(t *testing.T) {
			for _, p := range timestampProbes {
				t.Run(p.name, func(t *testing.T) {
					e, _ := world(t)
					e.as("alice", "status", "init").ok(t)
					e.as("alice", statusArgs("working")...).ok(t)
					a := eApp(e)
					s, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					n := probeNotice(field, p.value)
					_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.Terminal = &config.TerminalBinding{Generation: "review-cr4", OwnNotice: n}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					raw := e.as("alice", "bind", "status", "--json").ok(t).stdout
					text := e.as("alice", "bind", "status").ok(t).stdout
					puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
					err = s.publishTerminalNotice(context.Background(), "review-cr4")
					t.Logf("field=%s value=%q raw=%s text=%q publish=%v", field, p.value, raw, text, err)
					if p.valid {
						if err != nil || !strings.Contains(text, "SUSPECTED-DEAF") {
							t.Errorf("valid UTC refused: %v", err)
						}
					} else {
						if !strings.Contains(raw, `"own_notice_possibly_orphaned":null`) || strings.Contains(text, "SUSPECTED-DEAF") {
							t.Error("non-RFC3339 local timestamp rendered as factual suspicion")
						}
						if err == nil || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
							t.Error("non-RFC3339 local timestamp published")
						}
					}
				})
			}
		})
	}
}
