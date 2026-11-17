package envelopeobserve

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/swarma-envelope/verifier"
)

// TestRosterV1VectorGoldens proves the existing public vectors retain byte-exact version 1 labels.
func TestRosterV1VectorGoldens(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"PA1", "verified: @author:test.invalid"},
		{"PC2", "verified: @author:test.invalid"},
		{"P-BIP-author", "verified: @author:test.invalid"},
		{"P-BIP-high-s", "verified: @author:test.invalid"},
		{"C01", "invalid: message_room_mismatch"},
		{"C02", "invalid: message_recipient_mismatch"},
		{"C04", "invalid: message_text_mismatch"},
		{"C05", "invalid: message_text_mismatch"},
		{"C11", "invalid: message_text_mismatch"},
		{"E12", "invalid: envelope_signature_invalid"},
		{"N-BIP-author-key", "invalid: signer_unknown"},
		{"N-BIP-bad-signature", "invalid: envelope_signature_invalid"},
		{"N-BIP-body-binding", "invalid: message_text_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, ev, room, to, now := fromVector(t, loadVector(t, tc.name))
			if got := o.Observe(ev, room, to, now); !bytes.Equal([]byte(got), []byte(tc.want)) {
				t.Fatalf("label bytes %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRosterV1NeighborGoldens proves version 1 preserves its exact valid, unsigned, unavailable and refusal labels beside the original valid control.
func TestRosterV1NeighborGoldens(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"control", "verified: @author:test.invalid"},
		{"carrier-numbers", "verified: @author:test.invalid"},
		{"tampered-body", "invalid: message_text_mismatch"},
		{"wrong-author", "invalid: author_mismatch"},
		{"unknown-author", "unknown-author"},
		{"missing-envelope", "unsigned (no envelope)"},
		{"missing-roster", "unknown-author"},
		{"oversize", "invalid: content_too_large"},
		{"malformed", "invalid: envelope_malformed"},
		{"duplicate-content", "invalid: content_malformed"},
		{"alias-body", "invalid: content_key_alias"},
		{"expired", "invalid: expired"},
		{"unavailable-time", "invalid: time_unavailable"},
		{"missing-journal", "invalid: id_journal_unavailable"},
		{"redacted", "invalid: event_redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, ev, room, to, now := fromVector(t, loadVector(t, "PA1"))
			var content map[string]any
			if err := json.Unmarshal(ev.Content, &content); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "carrier-numbers":
				content["unrelated"] = map[string]any{"fraction": 1.5, "exponent": 1e100}
			case "tampered-body":
				content["body"] = "HELLO"
			case "wrong-author":
				o.authors[ev.Sender] = "ed25519:other"
			case "unknown-author":
				ev.Sender = "@other:test.invalid"
			case "missing-envelope":
				delete(content, matrix.EnvelopeContentKey)
				content["unrelated"] = 1.5
			case "missing-roster":
				o = nil
			case "oversize":
				content[matrix.EnvelopeContentKey] = strings.Repeat("A", 90000)
			case "malformed":
				content[matrix.EnvelopeContentKey] = "%%%"
			case "alias-body":
				content["Body"] = "evil"
			case "expired":
				now = now.Add(2 * time.Minute)
			case "unavailable-time":
				o.context.Time = verifier.Evidence{}
			case "missing-journal":
				o.context.IDJournalAvailable = verifier.Evidence{}
			case "redacted":
				ev.Unsigned = json.RawMessage(`{"redacted_because":{}}`)
			}
			var err error
			ev.Content, err = json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "duplicate-content" {
				ev.Content = []byte(strings.TrimSuffix(string(ev.Content), "}") + `,"body":"evil"}`)
			}
			if got := o.Observe(ev, room, to, now); !bytes.Equal([]byte(got), []byte(tc.want)) {
				t.Fatalf("label bytes %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRosterV1EncodingGolden proves the version 1 writer retains its exact compact member order and sender spelling.
func TestRosterV1EncodingGolden(t *testing.T) {
	a, _, _, _, _ := fromVector(t, loadVector(t, "PA1"))
	b, _, _, _, _ := fromVector(t, loadVector(t, "P-BIP-author"))
	k1, k2 := a.authors["@author:test.invalid"], b.authors["@author:test.invalid"]
	got, err := encodeRoster(map[string]string{"@beta:example.invalid": k2, "@alpha:example.invalid": k1})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"authors":{"@alpha:example.invalid":"` + k1 + `","@beta:example.invalid":"` + k2 + `"},"facts":{},"version":"a2a-envelope-roster/1"}`
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("encoding bytes %q, want %q", got, want)
	}
}
