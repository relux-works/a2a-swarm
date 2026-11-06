package cli

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func schemaNotice(kind string) config.OwnNoticeObservation {
	yes, no := true, false
	n := config.OwnNoticeObservation{PossiblyOrphaned: &yes, Reason: orphanReason, Since: "2026-10-20T16:00:00Z", ObservedAt: "2026-10-20T16:01:00Z"}
	switch kind {
	case "legacy":
		n = config.OwnNoticeObservation{}
	case "false":
		n.PossiblyOrphaned = &no
		n.Reason = ""
		n.Since = ""
	case "unknown":
		n.PossiblyOrphaned = nil
	case "offset-zero":
		n.Since = "2026-10-20T16:00:00.123+00:00"
		n.ObservedAt = "2026-10-20T16:01:00.456+00:00"
	case "newline-since":
		n.Since += "\nmallory\tworking\tfresh"
	case "newline-observed":
		n.ObservedAt += "\nmallory\tworking\tfresh"
	case "control-observed":
		n.ObservedAt += "\x1b[2J"
	case "reason":
		n.Reason = "forged reason\nmallory"
	case "missing-since":
		n.Since = ""
	case "missing-observed":
		n.ObservedAt = ""
	case "offset":
		n.Since = "2026-10-20T16:00:00+01:00"
	case "before-since":
		n.ObservedAt = "2026-10-20T15:59:00Z"
	case "false-history":
		n.PossiblyOrphaned = &no
	case "unknown-no-reason":
		n.PossiblyOrphaned = nil
		n.Reason = ""
	case "unknown-no-since":
		n.PossiblyOrphaned = nil
		n.Since = ""
	}
	return n
}

// Peer bob uses only bob's synthetic key/credentials. Alice's actual who
// consumes it. Validation, not escaping alone, must prevent malformed facts
// from becoming fresh suspected telemetry or forged output rows.
func TestNoticePeerReadSchema(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"true", "legacy", "false", "unknown", "offset-zero", "newline-since", "newline-observed", "control-observed", "reason", "missing-since", "missing-observed", "offset", "before-since", "false-history", "unknown-no-reason", "unknown-no-since"} {
		t.Run(kind, func(t *testing.T) {
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
			record.OwnNotice = schemaNotice(kind)
			if _, err = s.client.PutState(context.Background(), room, StatusEventType, s.creds.UserID, record); err != nil {
				t.Fatal(err)
			}
			text := e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
			raw := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t).stdout
			var rows []whoRow
			if err = json.Unmarshal([]byte(raw), &rows); err != nil {
				t.Fatal(err)
			}
			if strings.Count(text, "\n") != len(rows) || strings.Contains(text, "mallory") || strings.Contains(raw, "mallory") {
				t.Fatalf("peer notice payload escaped attribution: %q %s", text, raw)
			}
			found := false
			for _, row := range rows {
				if row.Name != "bob" {
					continue
				}
				found = true
				valid := kind == "true" || kind == "legacy" || kind == "false" || kind == "unknown" || kind == "offset-zero"
				if valid {
					if row.Health != "fresh" || !reflect.DeepEqual(row.Record.OwnNotice, record.OwnNotice) {
						t.Fatalf("valid peer notice rejected: %+v", row)
					}
				} else if row.Health != "invalid" || row.NoticeError != "invalid own notice telemetry" || row.Record.OwnNotice.PossiblyOrphaned != nil || strings.Contains(text, "SUSPECTED-DEAF") {
					t.Fatalf("malformed peer notice admitted: %+v %q", row, text)
				}
			}
			if !found {
				t.Fatal("actual peer identity lost")
			}
		})
	}
}

// Local persisted malformed metadata is unavailable in bind text/JSON and
// refused by both own publication routes before Matrix writes. Valid values
// retain their exact UTC spelling and all heartbeat/other fields.
func TestNoticeLocalSchemaBoundaries(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"true", "legacy", "false", "unknown", "offset-zero", "newline-since", "newline-observed", "reason", "false-history", "missing-observed"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			e.as("alice", statusArgs("working")...).ok(t)
			a := eApp(e)
			s, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			n := schemaNotice(kind)
			_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "schema", Interval: 2 * time.Second, RetryMax: 30 * time.Second, OwnNotice: n}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			raw := e.as("alice", "bind", "status", "--json").ok(t).stdout
			text := e.as("alice", "bind", "status").ok(t).stdout
			valid := kind == "true" || kind == "legacy" || kind == "false" || kind == "unknown" || kind == "offset-zero"
			before := *e.state("alice").Liveness
			puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			err = s.publishTerminalNotice(context.Background(), "schema")
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				got := *e.state("alice").Liveness
				got.OwnNotice = before.OwnNotice
				if !reflect.DeepEqual(got, before) {
					t.Fatal("notice changed heartbeat/fields")
				}
				var gotN config.OwnNoticeObservation
				json.Unmarshal([]byte(raw), &gotN)
				if !reflect.DeepEqual(n, gotN) {
					t.Fatalf("valid bind facts rejected: %s", raw)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "invalid own notice telemetry") {
					t.Fatal("malformed local notice published")
				}
				if !strings.Contains(raw, `"own_notice_possibly_orphaned":null`) || !strings.Contains(raw, `"notice_error":"invalid own notice telemetry"`) || strings.Contains(text, "SUSPECTED-DEAF") || strings.Contains(text, "mallory") {
					t.Fatalf("malformed bind facts rendered: %s %q", raw, text)
				}
				if err = s.writeLiveness(context.Background(), before); err == nil {
					t.Fatal("merged malformed notice published")
				}
				if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
					t.Fatal("malformed notice attempted remote write")
				}
			}
		})
	}
}

// Dispatch mailbox-read failure and real consumer sync-only failures propagate
// unknown through the own publisher; remote failure remains unavailable locally.
// Retained evidence is never given a new observation/heartbeat/first-since.
func TestNoticeErrorPublication(t *testing.T) {
	isolateBindTest(t)
	for _, kind := range []string{"dispatch", "consumer", "publication-failed", "generation"} {
		t.Run(kind, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.e.as("alice", "status", "init").ok(t)
			f.e.as("alice", statusArgs("working")...).ok(t)
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.e.as("alice", "inbox").ok(t)
			id := f.mail()
			f.control(map[string]any{})
			f.step()
			old := f.binding().OwnNotice
			heart := f.e.state("alice").Liveness.HeartbeatAt
			if kind == "generation" {
				f.update(func(st *config.State) { st.Terminal.Generation = "new" })
				puts := f.e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
				f.a.unknownTerminalNotice(context.Background(), f.s, "orphan-gen", true)
				if f.e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
					t.Fatal("retired generation published")
				}
				return
			}
			if kind == "dispatch" {
				path := f.s.paths.MailboxFile(f.s.name)
				bytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte("{broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
				err = f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
				if restore := os.WriteFile(path, bytes, 0600); restore != nil {
					t.Fatal(restore)
				}
				if err == nil {
					t.Fatal("mailbox read fault not reached")
				}
			} else {
				f.a.Now = time.Now
				f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "synthetic sync interruption")
				if kind == "publication-failed" {
					f.e.hs.fail("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}", 503, "M_UNKNOWN", "synthetic publish interruption")
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { _, err := f.a.runTerminal(ctx, f.s, "orphan-gen", *f.binding()); done <- err }()
				awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "consumer unavailable; retrying" })
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("private consumer did not stop")
				}
			}
			n := f.binding().OwnNotice
			if n.PossiblyOrphaned != nil || n.Since != old.Since || n.Reason != old.Reason || n.ObservedAt != old.ObservedAt || !f.e.state("alice").Liveness.HeartbeatAt.Equal(heart) {
				t.Fatal("error publication changed evidence freshness")
			}
			who := f.e.as("alice", "who", "--stale-after", "3m").ok(t).stdout
			if kind == "publication-failed" {
				if f.binding().NoticePublication != "unavailable: own liveness publication failed" || !strings.Contains(who, "SUSPECTED-DEAF") {
					t.Fatal("failed unknown publication falsely visible")
				}
			} else if f.binding().NoticePublication != "published" || !strings.Contains(who, "own notice unknown; last suspected since="+old.Since) {
				t.Fatalf("error unknown not published: %q", who)
			}
			if f.e.received("alice", id) {
				t.Fatal("error publication lost mail")
			}
			screen := f.wait(func(s orphanScreen) bool { return true })
			if screen.Erased != 0 || len(screen.Submitted) != 0 {
				t.Fatal("error publication touched draft")
			}
		})
	}
}

// Generated suspicion uses one instant for first-since and observation, even
// with a clock advancing between calls; production dispatch must publish it.
func TestNoticeAdvancingClockPublication(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	f.e.as("alice", "status", "init").ok(t)
	f.e.as("alice", statusArgs("working")...).ok(t)
	f.a.Now = func() time.Time { f.now = f.now.Add(time.Nanosecond); return f.now }
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.e.as("alice", "inbox").ok(t)
	f.control(map[string]any{})
	f.step()
	if f.binding().NoticePublication != "published" || noticeValid(f.binding().OwnNotice) != nil {
		t.Fatal("advancing clock generated invalid notice")
	}
}
