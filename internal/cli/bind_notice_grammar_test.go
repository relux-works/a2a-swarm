package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type noticeTimestampCase struct {
	family, name, value string
	valid               bool
}

// The generator encodes RFC3339 5.6 widths/separators and 5.7 calendar/ranges,
// independently of Go's parser and the production grammar. Bounded partitions
// are intentional: this is not exhaustive full-RFC or leap-second support.
func noticeTimestampCases() []noticeTimestampCase {
	var cases []noticeTimestampCase
	add := func(family, value string, valid bool) {
		cases = append(cases, noticeTimestampCase{family, fmt.Sprintf("%s-%04d", family, len(cases)), value, valid})
	}
	for _, date := range []string{"1900-02-28", "2000-02-29", "2024-04-30", "2026-12-31"} {
		for _, clock := range []string{"00:00:00", "06:07:08", "23:59:59"} {
			for _, sep := range []string{"T", "t"} {
				for _, frac := range []string{"", ".0", ".123", ".123456789", ".12345678901234567890"} {
					for _, zone := range []string{"Z", "z", "+00:00", "-00:00"} {
						add("valid", date+sep+clock+frac+zone, true)
					}
				}
			}
		}
	}
	const base = "2026-01-02T06:07:08Z"
	for _, span := range [][2]int{{0, 4}, {5, 7}, {8, 10}, {11, 13}, {14, 16}, {17, 19}} {
		for i := span[0]; i < span[1]; i++ {
			add("short-component", base[:i]+base[i+1:], false)
			for _, bad := range []string{"x", " ", "\t", "\x00", "１", "١"} {
				add("non-ascii-digit", base[:i]+bad+base[i+1:], false)
			}
		}
		add("wide-component", base[:span[0]]+"0"+base[span[0]:], false)
	}
	for _, sep := range []string{",", ":", ";", "/", "٫", "．", " ", "\t"} {
		add("decimal-separator", base[:19]+sep+"123Z", false)
	}
	add("empty-fraction", base[:19]+".Z", false)
	for _, at := range []int{4, 7, 10, 13, 16} {
		for _, sep := range []string{"/", "_", " ", "\n", "\x00", "："} {
			add("separator", base[:at]+sep+base[at+1:], false)
		}
	}
	for _, date := range []string{"1900-02-29", "2026-02-29", "2024-02-30", "2026-04-31", "2026-00-02", "2026-13-02", "2026-01-00", "2026-01-32"} {
		add("calendar", date+base[10:], false)
	}
	for _, clock := range []string{"24:00:00", "99:07:08", "06:60:08", "06:99:08", "06:07:61", "06:07:99"} {
		add("clock-range", base[:11]+clock+"Z", false)
	}
	// The existing time.Time-based observation contract cannot represent leap
	// seconds; keep this explicit bound rather than claim all of RFC3339.
	add("leap-second-bound", "2016-12-31T23:59:60Z", false)
	for _, zone := range []string{"+00:01", "-00:01", "+01:00", "-01:00", "+24:00", "+00:60", "+0:00", "+00:0", "+0000", "UTC", "", " Z"} {
		add("offset", base[:19]+zone, false)
	}
	for _, tail := range []string{"\x00", "\n", "\r\n", "\t", " ", "\x1b[2J", "\u00a0", "x", "Z", "+00:00", ".0"} {
		add("trailing", base+tail, false)
		add("leading", tail+base, false)
	}
	return cases
}

func grammarNotice(field, value string) config.OwnNoticeObservation {
	n := schemaNotice("true")
	n.Since = "0000-01-01T00:00:00Z"
	n.ObservedAt = "9999-12-31T23:59:59Z"
	if field == "since" {
		n.Since = value
	} else {
		n.ObservedAt = value
	}
	return n
}

// Both fields traverse the shared production validator; retained-unknown uses
// exactly the same grammar. Expected verdicts come from the generator families.
func TestNoticeTimestampGeneratedGrammar(t *testing.T) {
	cases := noticeTimestampCases()
	families := map[string]int{}
	for _, c := range cases {
		families[c.family]++
	}
	expected := map[string]int{"valid": 480, "short-component": 14, "wide-component": 6, "non-ascii-digit": 84, "decimal-separator": 8, "empty-fraction": 1, "separator": 30, "calendar": 8, "clock-range": 6, "leap-second-bound": 1, "offset": 12, "trailing": 11, "leading": 11}
	if len(cases) != 672 || !reflect.DeepEqual(families, expected) {
		t.Fatalf("timestamp generator partitions changed: %d %v", len(cases), families)
	}
	t.Logf("generated %d timestamps, partitions=%v; two fields x true/retained-unknown", len(cases), families)
	for _, field := range []string{"since", "observed"} {
		t.Run(field, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					n := grammarNotice(field, c.value)
					for _, unknown := range []bool{false, true} {
						if unknown {
							n.PossiblyOrphaned = nil
						}
						if got := noticeValid(n) == nil; got != c.valid {
							t.Errorf("timestamp grammar class=%s value=%q unknown=%t accepted=%t want=%t", c.family, c.value, unknown, got, c.valid)
						}
					}
				})
			}
		})
	}
}

// Representative of EVERY generated family at both real trust boundaries:
// synthetic bob self-key -> alice who; local persisted bind -> both own PUT
// routes. Malformed values must disappear, never become fresh facts or writes.
func TestNoticeTimestampProductionFamilies(t *testing.T) {
	var reps []noticeTimestampCase
	seen := map[string]bool{}
	for _, c := range noticeTimestampCases() {
		if !seen[c.family] {
			reps = append(reps, c)
			seen[c.family] = true
		}
	}
	// Cosmetic accepted spellings must also reach public boundaries.
	for _, value := range []string{"2026-01-02t06:07:08z", "2026-01-02T06:07:08.123456789012+00:00", "2026-01-02T06:07:08-00:00"} {
		reps = append(reps, noticeTimestampCase{"valid", fmt.Sprintf("valid-form-%d", len(reps)), value, true})
	}
	for _, field := range []string{"since", "observed"} {
		t.Run(field, func(t *testing.T) {
			for _, c := range reps {
				t.Run(c.name, func(t *testing.T) {
					e, _ := world(t)
					e.as("alice", "status", "init").ok(t)
					e.as("bob", "join", "status").ok(t)
					e.as("bob", statusArgs("working")...).ok(t)
					e.as("alice", statusArgs("working")...).ok(t)
					a := eApp(e)
					peer, err := a.sessionAs("bob")
					if err != nil {
						t.Fatal(err)
					}
					room, _, err := peer.resolveRoom(context.Background(), StatusRoom)
					if err != nil {
						t.Fatal(err)
					}
					n := grammarNotice(field, c.value)
					record := *e.state("bob").Liveness
					record.OwnNotice = n
					if _, err = peer.client.PutState(context.Background(), room, StatusEventType, peer.creds.UserID, record); err != nil {
						t.Fatal(err)
					}
					text := e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
					raw := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
					var rows []whoRow
					if err = json.Unmarshal([]byte(raw), &rows); err != nil {
						t.Fatal(err)
					}
					if strings.Count(text, "\n") != len(rows) {
						t.Fatal("timestamp forged an output row")
					}
					found := false
					for _, row := range rows {
						if row.Name != "bob" {
							continue
						}
						found = true
						if c.valid {
							if row.Health != "fresh" || !reflect.DeepEqual(row.Record.OwnNotice, n) {
								t.Fatalf("valid peer timestamp rejected: %+v", row)
							}
						} else if row.Health != "invalid" || row.NoticeError != "invalid own notice telemetry" || row.Record.OwnNotice.PossiblyOrphaned != nil || strings.Contains(text, "SUSPECTED-DEAF") {
							t.Fatalf("malformed timestamp family admitted by who: %s", c.family)
						}
					}
					if !found {
						t.Fatal("actual writer lost")
					}
					own, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					_, err = config.UpdateState(own.paths, own.name, func(st *config.State) error {
						st.Terminal = &config.TerminalBinding{Generation: "grammar", Interval: 2 * time.Second, RetryMax: 30 * time.Second, OwnNotice: n}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					bindText := e.as("alice", "bind", "status").ok(t).stdout
					bindJSON := e.as("alice", "bind", "status", "--json").ok(t).stdout
					before := *e.state("alice").Liveness
					puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
					err = own.publishTerminalNotice(context.Background(), "grammar")
					if c.valid {
						if err != nil {
							t.Fatal(err)
						}
						var got config.OwnNoticeObservation
						if err = json.Unmarshal([]byte(bindJSON), &got); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(n, got) || !strings.Contains(bindText, "SUSPECTED-DEAF") {
							t.Fatal("valid local timestamp rejected")
						}
						after := *e.state("alice").Liveness
						after.OwnNotice = before.OwnNotice
						if !reflect.DeepEqual(before, after) {
							t.Fatal("timestamp publication changed heartbeat/fields")
						}
						if err = own.writeLiveness(context.Background(), before); err != nil {
							t.Fatal(err)
						}
					} else {
						if err == nil {
							t.Fatalf("malformed timestamp family published: %s", c.family)
						}
						if !strings.Contains(bindJSON, `"own_notice_possibly_orphaned":null`) || !strings.Contains(bindJSON, `"notice_error":"invalid own notice telemetry"`) || strings.Contains(bindText, "SUSPECTED-DEAF") {
							t.Fatal("malformed timestamp family rendered by bind")
						}
						if err = own.writeLiveness(context.Background(), before); err == nil {
							t.Fatal("malformed timestamp family merged into liveness")
						}
						if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts || !reflect.DeepEqual(*e.state("alice").Liveness, before) {
							t.Fatal("invalid timestamp changed state or attempted PUT")
						}
					}
				})
			}
		})
	}
}

// Arbitrary legal fraction precision stays verbatim, but must not lose order
// below time.Time's nanoseconds. This drives publication and the public reader.
func TestNoticeTimestampFractionOrder(t *testing.T) {
	for _, kind := range []string{"forward", "equal", "reversed"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			e.as("bob", "join", "status").ok(t)
			e.as("bob", statusArgs("working")...).ok(t)
			a := eApp(e)
			s, err := a.sessionAs("bob")
			if err != nil {
				t.Fatal(err)
			}
			n := schemaNotice("true")
			n.Since = "2026-10-20T06:00:00.1234567891Z"
			n.ObservedAt = "2026-10-20t06:00:00.1234567892+00:00"
			if kind == "equal" {
				n.ObservedAt = "2026-10-20t06:00:00.12345678910z"
			}
			if kind == "reversed" {
				n.Since, n.ObservedAt = n.ObservedAt, n.Since
			}
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "fractions", Interval: 2 * time.Second, RetryMax: 30 * time.Second, OwnNotice: n}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			room, _, err := s.resolveRoom(context.Background(), StatusRoom)
			if err != nil {
				t.Fatal(err)
			}
			record := *e.state("bob").Liveness
			record.OwnNotice = n
			if _, err = s.client.PutState(context.Background(), room, StatusEventType, s.creds.UserID, record); err != nil {
				t.Fatal(err)
			}
			raw := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
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
				if kind == "reversed" {
					if row.Health != "invalid" || row.Record.OwnNotice.PossiblyOrphaned != nil {
						t.Error("fraction ordering admitted reversed evidence")
					}
				} else if row.Health != "fresh" || !reflect.DeepEqual(row.Record.OwnNotice, n) {
					t.Error("valid exact fractional order rejected")
				}
			}
			if !found {
				t.Fatal("actual writer lost")
			}
			puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			err = s.publishTerminalNotice(context.Background(), "fractions")
			if kind == "reversed" {
				if err == nil || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
					t.Error("fraction ordering admitted reversed evidence")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
