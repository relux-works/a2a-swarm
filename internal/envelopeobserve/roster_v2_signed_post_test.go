package envelopeobserve

import (
	"encoding/json"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/signedpost"
)

func rosterPostAttestations(t *testing.T) (string, string) {
	t.Helper()
	var attestations map[string]string
	if err := json.Unmarshal(loadVector(t, "P-BIP-author").Fixture["attestations"], &attestations); err != nil {
		t.Fatal(err)
	}
	intact := attestations[postAttestationID]
	if intact == "" {
		t.Fatal("public author attestation missing")
	}
	if err := json.Unmarshal(loadVector(t, "PA1").Fixture["attestations"], &attestations); err != nil {
		t.Fatal(err)
	}
	for id, encoded := range attestations {
		if id != postAttestationID {
			return intact, encoded
		}
	}
	t.Fatal("public neighboring attestation missing")
	return "", ""
}

// TestRosterV2SignedPostOrder proves verification, carried attestation, metadata and room-recipient gates precede the publisher decision beside valid shared-carrier controls.
func TestRosterV2SignedPostOrder(t *testing.T) {
	rows, facts := rosterV2Fixtures(t)
	o := rosterV2Observer(t, rows, facts)
	intact, other := rosterPostAttestations(t)
	for _, tc := range []struct {
		name    string
		body    func(map[string]any)
		content func(map[string]any)
		want    string
	}{
		{name: "single-control", want: "verified: beta"},
		{name: "listed-room-control", body: roomAddressed("@other:test.invalid", postReader), want: "verified: beta"},
		{name: "broadcast-control", body: roomAddressed(), want: "verified: beta"},
		{name: "attestation-binding", content: func(c map[string]any) { c[signedpost.AttestationContentKey] = other }, want: "invalid: attestation_mismatch"},
		{name: "attestation-encoding", content: func(c map[string]any) { c[signedpost.AttestationContentKey] = intact + "=" }, want: "invalid: attestation_mismatch"},
		{name: "metadata", content: func(c map[string]any) { postHeader(c)["kind"] = "decision" }, want: "invalid: metadata_mismatch"},
		{name: "recipient", body: roomAddressed("@other:test.invalid", "@third:test.invalid"), want: "invalid: recipient_not_addressed"},
		{name: "other-user-no-fallback", body: func(b map[string]any) {
			b["to"] = "@other:test.invalid"
			postArguments(b)["to"] = []any{"@other:test.invalid"}
		}, want: "invalid: message_recipient_mismatch"},
		{name: "other-room-no-fallback", body: func(b map[string]any) { b["to"] = "!other:test.invalid"; postArguments(b)["to"] = []any{} }, want: "invalid: message_recipient_mismatch"},
		{name: "non-post-no-fallback", body: func(b map[string]any) { b["to"] = postRoom; b["action"] = "message.deliver" }, want: "invalid: message_recipient_mismatch"},
		{name: "verification-before-attestation", content: func(c map[string]any) { c["body"] = "changed"; c[signedpost.AttestationContentKey] = other }, want: "invalid: message_text_mismatch"},
		{name: "attestation-before-metadata", content: func(c map[string]any) {
			c[signedpost.AttestationContentKey] = other
			postHeader(c)["kind"] = "decision"
		}, want: "invalid: attestation_mismatch"},
		{name: "metadata-before-recipient", body: roomAddressed("@other:test.invalid", "@third:test.invalid"), content: func(c map[string]any) { postHeader(c)["kind"] = "decision" }, want: "invalid: metadata_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, room, reader, now := fromVector(t, loadVector(t, "P-BIP-author"))
			ev := signedPost(t, "hello", tc.body, func(content map[string]any) {
				content[signedpost.AttestationContentKey] = intact
				if tc.content != nil {
					tc.content(content)
				}
			})
			ev.Sender = sharedRosterPublisher
			if got := o.Observe(ev, room, reader, now); got != tc.want {
				t.Fatalf("ordered shared-carrier label %q, want %q", got, tc.want)
			}
			ev.Sender = "@unlisted:example.invalid"
			want := tc.want
			if want == "verified: beta" {
				want = "invalid: publisher_mismatch"
			}
			if got := o.Observe(ev, room, reader, now); got != want {
				t.Fatalf("publisher decision masked an earlier gate: %q, want %q", got, want)
			}
		})
	}
}

// TestRosterAuthorDecisionAfterContent proves either roster version reports a carried-attestation failure before an otherwise mismatched sender decision.
func TestRosterAuthorDecisionAfterContent(t *testing.T) {
	intact, other := rosterPostAttestations(t)
	rows, facts := rosterV2Fixtures(t)
	v2 := rosterV2Observer(t, rows, facts)
	v1, _, room, reader, now := fromVector(t, loadVector(t, "P-BIP-author"))
	for _, tc := range []struct {
		name         string
		o            *Observer
		sender, want string
	}{
		{"legacy", v1, postSender, "verified: " + postSender},
		{"shared", v2, sharedRosterPublisher, "verified: beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := signedPost(t, "hello", nil, func(c map[string]any) { c[signedpost.AttestationContentKey] = intact })
			ev.Sender = tc.sender
			if got := tc.o.Observe(ev, room, reader, now); got != tc.want {
				t.Fatalf("valid author control %q, want %q", got, tc.want)
			}
			ev = signedPost(t, "hello", nil, func(c map[string]any) { c[signedpost.AttestationContentKey] = other })
			ev.Sender = tc.sender
			if tc.o.byAuthor == nil {
				tc.o.authors[postSender] = rows[0].AuthorKey
			} else {
				ev.Sender = "@unlisted:example.invalid"
			}
			if got := tc.o.Observe(ev, room, reader, now); got != "invalid: attestation_mismatch" {
				t.Fatalf("author decision bypassed carried evidence: %q", got)
			}
		})
	}
}
