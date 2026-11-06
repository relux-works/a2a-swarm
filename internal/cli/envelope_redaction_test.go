package cli

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves --envelope-input refuses a known-redacted original and an edit whose
// explicit target is known redacted (synced or not), from local tombstones alone (the fake
// server keeps returning the stale content without unsigned redaction flags),
// while a valid neighbouring envelope event in the same room still projects.
// Limit: covers tombstones learned through sync; server unsigned markers are
// covered by the matrix package refusal table.
func TestEnvelopeInputLocalTombstones(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	withEnvelope := func(c map[string]any) map[string]any {
		c[matrix.EnvelopeContentKey] = "e30"
		return c
	}
	target := e.hs.inject(room, "@bob:test.local", withEnvelope(msg(removedFixture, nil)))
	edit := e.hs.inject(room, "@bob:test.local", withEnvelope(editContent(target, removedFixture)))
	neighbour := e.hs.inject(room, "@bob:test.local", withEnvelope(msg("valid-neighbour", nil)))
	e.hs.mu.Lock()
	e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": target})
	e.hs.mu.Unlock()
	syncRedactionFixture(t, e, "alice")
	for _, id := range []string{target, edit} {
		r := e.as("alice", "read", room, "--event", id, "--envelope-input", "--json")
		if r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "ENVELOPE_REDACTED") {
			t.Fatalf("known-redacted envelope escaped: exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
		}
		noRemoved(t, r)
	}
	// An edit the client never synced: only the post-fetch tombstone check,
	// which consults the observed content's explicit target, can refuse it.
	unsynced := e.hs.inject(room, "@bob:test.local", withEnvelope(editContent(target, removedFixture)))
	if r := e.as("alice", "read", room, "--event", unsynced, "--envelope-input", "--json"); r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "ENVELOPE_REDACTED") {
		t.Fatalf("unsynced edit of a redacted target escaped: exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
	r := e.as("alice", "read", room, "--event", neighbour, "--envelope-input", "--json").ok(t)
	rec := readEnvelopeRecord(t, r)
	env, err := base64.RawURLEncoding.DecodeString(rec.EnvelopeBase64)
	if err != nil || string(env) != "{}" || rec.ReturnedEventID != neighbour {
		t.Fatalf("valid neighbour envelope changed: %+v", rec)
	}
	// With the server fetch failing, a known-redacted original is still named
	// redacted: the pre-fetch check decides before any server response.
	e.hs.fail("GET /_matrix/client/v3/rooms/{room}/event/{event}", 500, "M_UNKNOWN", "fetch failed")
	if r := e.as("alice", "read", room, "--event", target, "--envelope-input", "--json"); r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "ENVELOPE_REDACTED") {
		t.Fatalf("pre-fetch refusal lost: exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
}
