package cli

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// stampEvent sets the fake homeserver's origin_server_ts of event id to ts.
func stampEvent(t *testing.T, hs *fakeHS, id string, ts int64) {
	t.Helper()
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for i := range hs.events {
		if hs.events[i].EventID == id {
			hs.events[i].OriginServerTS = ts
			return
		}
	}
	t.Fatalf("setup: event %s not found", id)
}

// jsonLines decodes one JSON object per non-empty stdout line.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var records []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON object per line: %q: %v", line, err)
		}
		records = append(records, m)
	}
	return records
}

// Proves: at_ms is the event's raw origin_server_ts, exact to the
// millisecond. Two events inside the same second keep the same RFC3339 `at`
// but different exact at_ms, and a whole-second neighbour keeps its own value;
// no rounding, no parsing of `at` and no local clock is involved.
func TestEventRecordAtMSIsRawServerMilliseconds(t *testing.T) {
	base := time.Date(2026, 11, 14, 3, 0, 7, 0, time.UTC).UnixMilli()
	for _, ts := range []int64{base, base + 123, base + 987} {
		ev := matrix.Event{EventID: "$e", Type: "m.room.message", Sender: "@bob:test.local", RoomID: "!r:test.local", OriginServerTS: ts}
		rec := eventRecord("message", "#proj", "!r:test.local", ev, "test.local", false)
		if rec.AtMS != ts {
			t.Fatalf("at_ms=%d, want the raw %d", rec.AtMS, ts)
		}
		if rec.At != "2026-11-14T03:00:07Z" {
			t.Fatalf("at changed: %q", rec.At)
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if got, ok := m["at_ms"].(float64); !ok || int64(got) != ts {
			t.Fatalf("JSON at_ms=%v, want %d", m["at_ms"], ts)
		}
	}
}

// Proves: an event without a server timestamp (0, or a negative value) has
// NO at_ms in its JSON: absence stays unknown and is never replaced by the
// current time or a derived value. Records that are not events (invite, gap)
// carry no clock either. Limit: the existing `at` behaviour for such events
// is unchanged and not asserted here.
func TestEventRecordAtMSAbsentWhenUnknown(t *testing.T) {
	for _, ts := range []int64{0, -1} {
		ev := matrix.Event{EventID: "$e", Type: "m.room.message", Sender: "@bob:test.local", RoomID: "!r:test.local", OriginServerTS: ts}
		data, err := json.Marshal(eventRecord("message", "#proj", "!r:test.local", ev, "test.local", false))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"at_ms"`) {
			t.Fatalf("ts=%d produced an at_ms: %s", ts, data)
		}
	}
	for _, rec := range []EventRecord{
		{Type: "invite", RoomID: "!r:test.local", Sender: "@bob:test.local"},
		{Type: "gap", RoomID: "!r:test.local", EventID: "gap-1", Excerpt: "gap: x"},
	} {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"at_ms"`) {
			t.Fatalf("%s record carries a clock: %s", rec.Type, data)
		}
	}
}

// Proves: the user-facing JSON paths carry the exact at_ms of a sub-second
// server timestamp end to end: read --json, and the mailbox-backed inbox
// --json (the stored event keeps its millisecond value). The text output of
// read is unchanged and shows no at_ms.
func TestEventJSONPathsCarryAtMS(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	ts := time.Date(2026, 11, 14, 3, 0, 7, 0, time.UTC).UnixMilli() + 987
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local exact-ms", nil))
	stampEvent(t, e.hs, id, ts)

	find := func(path string, records []map[string]any) map[string]any {
		t.Helper()
		for _, r := range records {
			if r["event_id"] == id {
				return r
			}
		}
		t.Fatalf("%s: event %s not in output", path, id)
		return nil
	}
	for _, c := range []struct {
		path string
		args []string
	}{
		{"read --json", []string{"read", room, "--event", id, "--json"}},
		{"inbox --json", []string{"inbox", "--json"}},
	} {
		r := find(c.path, jsonLines(t, e.as("alice", c.args...).ok(t).stdout))
		if got, ok := r["at_ms"].(float64); !ok || int64(got) != ts {
			t.Fatalf("%s: at_ms=%v, want %d", c.path, r["at_ms"], ts)
		}
		if r["at"] != "2026-11-14T03:00:07Z" {
			t.Fatalf("%s: at=%v changed", c.path, r["at"])
		}
	}
	if text := e.as("alice", "read", room, "--event", id).ok(t).stdout; strings.Contains(text, "at_ms") || strings.Contains(text, strconv.FormatInt(ts, 10)) {
		t.Fatalf("text output changed: %q", text)
	}
}
