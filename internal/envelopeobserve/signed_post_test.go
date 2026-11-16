package envelopeobserve

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/swarma-envelope/record"
)

// Expected identities are literals fixed here. The author key and attestation
// id are the public BIP-340 author fixture (testdata/P-BIP-author.json), whose
// roster facts make the pinned verifier accept the author. The test signs with
// that fixture's published test-only operand (32 bytes of 0x03 in the pinned
// module's vector generator); it is not a secret and protects nothing.
const (
	postKey           = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"
	postAttestationID = "sha256:f0f08c11a4388a53fc9240302f29b9984a13ce9b9a8b27f74d87f183951d74bc"
	postRoom          = "!room:test.invalid"
	postSender        = "@author:test.invalid"
	postReader        = "@recipient:test.invalid"
)

// signedPost builds one signed post event: a message/1 body with the post
// action, signed once over the module's signing preimage, carried beside the
// header and the mentions a sender would put in the event.
func signedPost(t *testing.T, text string, editBody, editContent func(map[string]any)) matrix.Event {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	body := map[string]any{
		"type":               "message/1",
		"aud":                "service-a",
		"nonce":              base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16)),
		"iat":                "2026-01-01T00:00:00Z",
		"exp":                "2026-01-01T00:02:00Z",
		"room":               postRoom,
		"to":                 postReader,
		"body_sha256":        hex.EncodeToString(sum[:]),
		"author":             postKey,
		"author_attestation": postAttestationID,
		"action":             "a2a.post/1",
		"target":             map[string]any{"kind": "station", "id": "a2a/post"},
		"arguments":          map[string]any{"kind": "notice", "topic": "$root", "re": "", "to": []any{postReader}},
	}
	if editBody != nil {
		editBody(body)
	}
	preimage, err := record.SigningPreimage("message/1", postKey, "author", body)
	if err != nil {
		t.Fatal(err)
	}
	private, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	digest := sha256.Sum256(preimage)
	signature, err := schnorr.Sign(private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	id, err := record.ContentID("message/1", body)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := record.Canonical(map[string]any{
		"version": "swarm-envelope/2", "body": body, "id": id,
		"signatures": []any{map[string]any{"role": "author", "key": postKey, "sig": base64.RawURLEncoding.EncodeToString(signature.Serialize())}},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"msgtype": "m.text", "body": text, matrix.EnvelopeContentKey: base64.RawURLEncoding.EncodeToString(envelope),
		"ai.relux.a2a": map[string]any{"v": 1, "type": "message", "from": "author", "at": "2026-01-01T00:00:00Z", "kind": "notice", "topic": "$root"},
	}
	if arguments, ok := body["arguments"].(map[string]any); ok {
		if list, _ := arguments["to"].([]any); len(list) > 0 {
			content["m.mentions"] = map[string]any{"user_ids": list}
		}
	}
	if editContent != nil {
		editContent(content)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return matrix.Event{Type: "m.room.message", Sender: postSender, Content: raw}
}

func postArguments(body map[string]any) map[string]any { return body["arguments"].(map[string]any) }
func postHeader(content map[string]any) map[string]any {
	return content["ai.relux.a2a"].(map[string]any)
}

// roomAddressed turns the body into a post addressed to the room with the
// given signed recipient list.
func roomAddressed(list ...any) func(map[string]any) {
	return func(body map[string]any) {
		body["to"] = postRoom
		postArguments(body)["to"] = append([]any{}, list...)
	}
}

// Proves the receiver rule for signed posts, each refusal beside a verified
// neighbour that differs in one thing:
//   - a post to this reader alone, a room-addressed post that lists the reader
//     and a room broadcast with an empty list are verified;
//   - a room-addressed post that does not list the reader is
//     recipient_not_addressed;
//   - the fallback is taken only when the signed `to` is the selected room id:
//     another user's id, another room's id and a non-post action addressed to
//     the room stay message_recipient_mismatch;
//   - signed kind, topic, re, recipients, argument set and target that differ
//     from what the event carries are metadata_mismatch, as are a missing
//     header and a case variant of the header member;
//   - a changed text still fails the verifier first (message_text_mismatch).
//
// The expired row records the named limit: verification runs at read time, so
// a post read after its 120 s lifetime is labelled expired. Limit of the whole
// test: the events are built here, not sent by the sender (internal/cli does
// that), and the reader's clock and roster come from the fixture.
// Claim: only selected-room fallback with matching signed metadata and addressed recipients verifies.
func TestObserveSignedPost(t *testing.T) {
	other, third := "@other:test.invalid", "@third:test.invalid"
	cases := []struct {
		name        string
		editBody    func(map[string]any)
		editContent func(map[string]any)
		text        string // delivered text when it differs from the signed text
		late        time.Duration
		want        string
	}{
		{name: "single-recipient", want: "verified: " + postSender},
		{name: "room-listed", editBody: roomAddressed(other, postReader), want: "verified: " + postSender},
		{name: "room-broadcast", editBody: roomAddressed(), want: "verified: " + postSender},
		{name: "room-not-listed", editBody: roomAddressed(other, third), want: "invalid: recipient_not_addressed"},
		{name: "other-user", editBody: func(b map[string]any) { b["to"] = other; postArguments(b)["to"] = []any{other} }, want: "invalid: message_recipient_mismatch"},
		{name: "other-room", editBody: func(b map[string]any) { b["to"] = "!other:test.invalid"; postArguments(b)["to"] = []any{} }, want: "invalid: message_recipient_mismatch"},
		{name: "other-action-to-room", editBody: func(b map[string]any) { b["to"] = postRoom; b["action"] = "message.deliver" }, want: "invalid: message_recipient_mismatch"},
		{name: "kind-differs", editContent: func(c map[string]any) { postHeader(c)["kind"] = "decision" }, want: "invalid: metadata_mismatch"},
		{name: "topic-differs", editContent: func(c map[string]any) { postHeader(c)["topic"] = "$elsewhere" }, want: "invalid: metadata_mismatch"},
		{name: "topic-removed", editContent: func(c map[string]any) { delete(postHeader(c), "topic") }, want: "invalid: metadata_mismatch"},
		{name: "re-added", editContent: func(c map[string]any) { postHeader(c)["re"] = "$question" }, want: "invalid: metadata_mismatch"},
		{name: "re-signed-and-carried", editBody: func(b map[string]any) { postArguments(b)["re"] = "$question" }, editContent: func(c map[string]any) { postHeader(c)["re"] = "$question" }, want: "verified: " + postSender},
		{name: "mention-removed", editBody: roomAddressed(other, postReader), editContent: func(c map[string]any) { c["m.mentions"] = map[string]any{"user_ids": []any{postReader}} }, want: "invalid: metadata_mismatch"},
		{name: "mentions-reordered", editBody: roomAddressed(other, postReader), editContent: func(c map[string]any) { c["m.mentions"] = map[string]any{"user_ids": []any{postReader, other}} }, want: "invalid: metadata_mismatch"},
		{name: "mention-added-to-broadcast", editBody: roomAddressed(), editContent: func(c map[string]any) { c["m.mentions"] = map[string]any{"user_ids": []any{postReader}} }, want: "invalid: metadata_mismatch"},
		{name: "mentions-absent", editContent: func(c map[string]any) { delete(c, "m.mentions") }, want: "invalid: metadata_mismatch"},
		{name: "extra-argument", editBody: func(b map[string]any) { postArguments(b)["extra"] = "x" }, want: "invalid: metadata_mismatch"},
		{name: "argument-missing", editBody: func(b map[string]any) { delete(postArguments(b), "re") }, want: "invalid: metadata_mismatch"},
		{name: "duplicate-in-signed-list", editBody: roomAddressed(postReader, postReader), want: "invalid: metadata_mismatch"},
		{name: "room-with-one-listed", editBody: roomAddressed(postReader), want: "invalid: metadata_mismatch"},
		{name: "single-list-differs-from-to", editBody: func(b map[string]any) { postArguments(b)["to"] = []any{postReader, other} }, want: "invalid: metadata_mismatch"},
		{name: "other-target", editBody: func(b map[string]any) { b["target"] = map[string]any{"kind": "station", "id": "team/member"} }, want: "invalid: metadata_mismatch"},
		{name: "header-missing", editContent: func(c map[string]any) { delete(c, "ai.relux.a2a") }, want: "invalid: metadata_mismatch"},
		{name: "header-case-variant", editContent: func(c map[string]any) { c["AI.relux.a2a"] = map[string]any{"kind": "decision"} }, want: "invalid: metadata_mismatch"},
		{name: "text-changed", text: "HELLO", want: "invalid: message_text_mismatch"},
		{name: "room-listed-text-changed", editBody: roomAddressed(other, postReader), text: "HELLO", want: "invalid: message_text_mismatch"},
		{name: "expired", late: 2 * time.Minute, want: "invalid: expired"},
		{name: "room-listed-expired", editBody: roomAddressed(other, postReader), late: 2 * time.Minute, want: "invalid: expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _, room, reader, now := fromVector(t, loadVector(t, "P-BIP-author"))
			if room != postRoom || reader != postReader || o.authors[postSender] != postKey {
				t.Fatalf("fixture context changed: %s %s %s", room, reader, o.authors[postSender])
			}
			editContent := tc.editContent
			if tc.text != "" {
				editContent = func(c map[string]any) { c["body"] = tc.text }
			}
			ev := signedPost(t, "hello", tc.editBody, editContent)
			if got := o.Observe(ev, room, reader, now.Add(tc.late)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Proves the room fallback uses the SELECTED room, not the signed one alone:
// the same room-addressed post is verified when read in its room and is
// message_room_mismatch when the reader's selected room is another one.
// Claim: the same room-addressed envelope verifies only in the selected signed room.
func TestObserveSignedPostOtherSelectedRoom(t *testing.T) {
	o, _, room, reader, now := fromVector(t, loadVector(t, "P-BIP-author"))
	ev := signedPost(t, "hello", roomAddressed(), nil)
	if got := o.Observe(ev, room, reader, now); got != "verified: "+postSender {
		t.Fatalf("control: %q", got)
	}
	if got := o.Observe(ev, "!elsewhere:test.invalid", reader, now); got != "invalid: message_room_mismatch" {
		t.Fatalf("got %q", got)
	}
}
