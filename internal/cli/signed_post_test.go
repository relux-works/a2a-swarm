package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/relux-works/a2a-swarm/internal/envelopeobserve"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
)

// Expected identities are literals fixed here: the public BIP-340 author
// fixture of the pinned envelope module (../envelopeobserve/testdata/P-BIP-author.json).
// The fake signer signs with that fixture's published test-only operand (32
// bytes of 0x03 in the module's vector generator); it is not a secret and
// protects nothing. No other key material exists in these tests.
const (
	signedPostKey           = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"
	signedPostAttestationID = "sha256:f0f08c11a4388a53fc9240302f29b9984a13ce9b9a8b27f74d87f183951d74bc"
	signedPostOtherKey      = "ed25519:b2CdQco2XmEi01K78Tg0l7-24Eu5_wHLHlSvQE3mj1M"
	signedPostAudience      = "service-a"
	signedPostAuthor        = "@author:test.invalid"
)

type signedPostFixture struct {
	Fixture map[string]json.RawMessage `json:"fixture"`
}

func loadSignedPostFixture(t *testing.T) signedPostFixture {
	t.Helper()
	raw, err := os.ReadFile("../envelopeobserve/testdata/P-BIP-author.json")
	if err != nil {
		t.Fatal(err)
	}
	var v signedPostFixture
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func signedPostIdentity(t *testing.T) signedpost.Identity {
	t.Helper()
	var attestations map[string]string
	if err := json.Unmarshal(loadSignedPostFixture(t).Fixture["attestations"], &attestations); err != nil {
		t.Fatal(err)
	}
	a, err := base64.RawURLEncoding.DecodeString(attestations[signedPostAttestationID])
	if err != nil || len(a) == 0 {
		t.Fatalf("fixture attestation missing: %v", err)
	}
	return signedpost.Identity{Key: signedPostKey, AttestationID: signedPostAttestationID, Attestation: a}
}

// postFakeSigner is the in-package fake behind the signer seam. Like a holder
// it admits the preimage with the module's own admission and signs SHA-256 of
// it once.
type postFakeSigner struct {
	identity      signedpost.Identity
	identityErr   error
	signErrs      []error // one per Sign call, consumed in order; nil signs
	wrongKey      bool
	flipSignature bool
	identityCalls int
	signCalls     int
	ids           []string
}

func (s *postFakeSigner) Identity(context.Context) (signedpost.Identity, error) {
	s.identityCalls++
	return s.identity, s.identityErr
}

func (s *postFakeSigner) Sign(_ context.Context, preimage []byte) ([]byte, []byte, error) {
	s.signCalls++
	s.ids = append(s.ids, signedpost.RequestID(preimage))
	if len(s.signErrs) > 0 {
		err := s.signErrs[0]
		s.signErrs = s.signErrs[1:]
		if err != nil {
			return nil, nil, err
		}
	}
	if _, err := record.AdmitSigner(preimage, record.IdentityPolicy{Check: true, CallerKey: signedPostKey, CallerAttestationID: signedPostAttestationID}); err != nil {
		return nil, nil, &signedpost.RefusedError{HolderCode: err.Error()}
	}
	private, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	digest := sha256.Sum256(preimage)
	signature, err := schnorr.Sign(private, digest[:])
	if err != nil {
		return nil, nil, err
	}
	out := signature.Serialize()
	if s.flipSignature {
		out[3] ^= 1
	}
	key := signedPostKey
	if s.wrongKey {
		key = signedPostOtherKey
	}
	return []byte(key), out, nil
}

func signedPostRecipients(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("@u%02d:%s", i, testServer)
	}
	return strings.Join(names, ",")
}

// Claim: a missing signer refuses with zero HTTP and no @file consumption; the unsigned controls send the untouched body.
func TestPostSignUnconfiguredPublishesNothing(t *testing.T) {
	for _, fileBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("file-body-%v", fileBody), func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
			body := "text"
			if fileBody {
				// A pipe exposed as @file is consumable only once. The unsigned
				// control can read it only if the refusal left its bytes untouched.
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { r.Close(); w.Close() })
				if _, err := w.WriteString(body); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				body = fmt.Sprintf("@/dev/fd/%d", r.Fd())
			}
			args := []string{"post", "proj", "--topic", "alpha", "--to", "alice", body}
			before, calls := e.eventCount(), len(e.hs.Calls())
			r := e.as("bob", append(args, "--sign")...)
			r.failsWith(t, ExitError, "a2a: signed post refused: signer_unconfigured")
			if e.eventCount() != before || r.stdout != "" || len(e.hs.Calls()) != calls {
				t.Fatalf("refusal had effects: events %d->%d HTTP %d->%d stdout %q", before, e.eventCount(), calls, len(e.hs.Calls()), r.stdout)
			}

			r = e.as("bob", args...).ok(t)
			id := strings.TrimSpace(r.stdout)
			if e.eventCount() != before+1 || !strings.HasPrefix(id, "$") || len(e.hs.Calls()) == calls {
				t.Fatal("unsigned control did not send")
			}
			if body := storedContent(t, e, id)["body"].(string); !strings.HasSuffix(body, "\ntext") {
				t.Fatal("unsigned control did not read the untouched body")
			}
		})
	}
}

// Proves that every refusal of a signed post ends in its exact code, sends no
// event and never falls back to an unsigned post, each beside the same post
// without --sign being sent (and never reaching the signer). Rows without the
// test-only qualification are the production state: they stop at the missing
// pin and never reach Sign. Rows with it reach Sign and are refused on its
// result: a returned key other than the identity key, a signature with one
// flipped bit, a holder refusal (one Sign call) and a retryable failure (three
// Sign calls, then signer_unavailable). The recipient rows record ZERO signer
// calls. The published positive is TestPostSignRoundTrip.
// Claim: every signed-post refusal publishes nothing and the corresponding unsigned control bypasses the signer.
func TestPostSignNeverPublishes(t *testing.T) {
	valid := signedPostIdentity(t)
	cases := []struct {
		name          string
		to            string
		qualified     bool
		edit          func(*postFakeSigner)
		want          string
		identityCalls int
		signCalls     int
	}{
		{name: "valid-identity-no-pin", to: "alice", want: "signed post refused: signer_unqualified (purpose-5 pin not configured)", identityCalls: 1},
		{name: "broadcast-no-pin", to: "all", want: "signed post refused: signer_unqualified (purpose-5 pin not configured)", identityCalls: 1},
		{name: "cap-64-no-pin", to: signedPostRecipients(64), want: "signed post refused: signer_unqualified (purpose-5 pin not configured)", identityCalls: 1},
		{name: "over-cap-65", to: signedPostRecipients(65), qualified: true, want: "signed post refused: recipients_too_many (more than 64 recipients)"},
		{name: "duplicate-recipient", to: "alice,alice", qualified: true, want: "signed post refused: recipients_too_many (duplicate recipient)"},
		{name: "signer-unavailable", to: "alice", edit: func(s *postFakeSigner) { s.identityErr = signedpost.ErrUnavailable }, want: "signed post refused: signer_unavailable", identityCalls: 1},
		{name: "signer-refused", to: "alice", edit: func(s *postFakeSigner) { s.identityErr = &signedpost.RefusedError{HolderCode: "request_conflict"} }, want: "signed post refused: signer_refused (request_conflict)", identityCalls: 1},
		{name: "signer-garbled", to: "alice", edit: func(s *postFakeSigner) { s.identityErr = errors.New("garbled") }, want: "signed post refused: signer_result_invalid", identityCalls: 1},
		{name: "attestation-empty", to: "alice", qualified: true, edit: func(s *postFakeSigner) { s.identity.Attestation = nil }, want: "signed post refused: signer_unqualified (attestation empty)", identityCalls: 1},
		{name: "attestation-8193", to: "alice", qualified: true, edit: func(s *postFakeSigner) { s.identity.Attestation = make([]byte, 8193) }, want: "signed post refused: signer_unqualified (attestation over 8192 bytes)", identityCalls: 1},
		{name: "attestation-id-differs", to: "alice", qualified: true, edit: func(s *postFakeSigner) {
			s.identity.AttestationID = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}, want: "signed post refused: signer_unqualified (attestation content id differs from the attestation id)", identityCalls: 1},
		{name: "returned-key-differs", to: "alice", qualified: true, edit: func(s *postFakeSigner) { s.wrongKey = true }, want: "signed post refused: signer_result_invalid (returned key differs from the identity key)", identityCalls: 1, signCalls: 1},
		{name: "bad-signature", to: "alice", qualified: true, edit: func(s *postFakeSigner) { s.flipSignature = true }, want: "signed post refused: signer_result_invalid (signature does not verify)", identityCalls: 1, signCalls: 1},
		{name: "sign-refused", to: "alice", qualified: true, edit: func(s *postFakeSigner) {
			s.signErrs = []error{&signedpost.RefusedError{HolderCode: "signer_identity_mismatch"}}
		}, want: "signed post refused: signer_refused (signer_identity_mismatch)", identityCalls: 1, signCalls: 1},
		{name: "sign-unavailable", to: "alice", qualified: true, edit: func(s *postFakeSigner) {
			s.signErrs = []error{signedpost.ErrUnavailable, signedpost.ErrUnavailable, signedpost.ErrUnavailable, signedpost.ErrUnavailable}
		}, want: "signed post refused: signer_unavailable", identityCalls: 1, signCalls: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
			signer := &postFakeSigner{identity: valid}
			if tc.edit != nil {
				tc.edit(signer)
			}
			e.configure = func(a *App) {
				a.postSigner, a.postAudience = signer, signedPostAudience
				if tc.qualified {
					a.postQualified = func(signedpost.Identity) bool { return true }
				}
			}
			args := []string{"post", "proj", "--topic", "alpha", "--to", tc.to, "text"}

			before := e.eventCount()
			r := e.as("bob", append(args, "--sign")...)
			r.failsWith(t, ExitError, "a2a: "+tc.want+"\n")
			if e.eventCount() != before || r.stdout != "" {
				t.Fatalf("refused signed post had an effect: events %d->%d stdout %q", before, e.eventCount(), r.stdout)
			}
			if signer.identityCalls != tc.identityCalls || signer.signCalls != tc.signCalls {
				t.Fatalf("signer calls: identity %d (want %d), sign %d (want %d)", signer.identityCalls, tc.identityCalls, signer.signCalls, tc.signCalls)
			}
			if strings.Contains(r.stderr, signedPostKey) || strings.Contains(r.stderr, signedPostOtherKey) {
				t.Fatalf("refusal output carries a key: %s", r.stderr)
			}

			r = e.as("bob", args...).ok(t)
			if e.eventCount() != before+1 || !strings.HasPrefix(strings.TrimSpace(r.stdout), "$") {
				t.Fatalf("unsigned control not sent: events %d->%d out %q", before, e.eventCount(), r.stdout)
			}
			if signer.identityCalls != tc.identityCalls || signer.signCalls != tc.signCalls {
				t.Fatal("an unsigned post reached the signer")
			}
		})
	}
}

// Proves --sign refuses the shapes it cannot bind before any request, as usage
// errors, with a signer present; the valid shape beside them reaches the signer
// (TestPostSignNeverPublishes).
// Claim: incompatible signed-post options refuse before any signer call or event send.
func TestPostSignUsageRefusals(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	signer := &postFakeSigner{}
	e.configure = func(a *App) { a.postSigner, a.postAudience = signer, signedPostAudience }
	file := envelopeFile(t, []byte("{}"))
	cases := []struct {
		name string
		args []string
		frag string
	}{
		{"with-envelope", []string{"--sign", "--envelope", "@" + file, "text"}, "--sign cannot be combined with --envelope"},
		{"with-voice", []string{"--sign", "--voice", "text"}, "--sign cannot be combined with --voice"},
		{"two-text-arguments", []string{"--sign", "two", "words"}, "--sign needs exactly one text argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := e.eventCount()
			r := e.as("bob", append([]string{"post", "proj", "--topic", "alpha"}, tc.args...)...)
			r.failsWith(t, ExitUsage, tc.frag)
			if e.eventCount() != before || r.stdout != "" || signer.identityCalls != 0 || signer.signCalls != 0 {
				t.Fatalf("usage refusal had an effect: events %d->%d stdout %q", before, e.eventCount(), r.stdout)
			}
		})
	}
}

// signedPostWorld is a room with an author who can sign (the fake signer,
// qualified through the test-only seam) and three readers whose host roster is
// the fixture's: it binds the author's Matrix id to the fixture key and holds
// the attestation, revocation and time facts the pinned verifier requires at
// the fixed clock. Only the fake transport namespace is adapted to the fixture.
func signedPostWorld(t *testing.T) (*env, string, *postFakeSigner) {
	t.Helper()
	v := loadSignedPostFixture(t)
	for _, k := range []string{"root", "known_keys", "now", "delivery", "required_roles", "assurance", "binding", "request", "agent_attestation_ids", "replay_available", "used_actions"} {
		delete(v.Fixture, k)
	}
	roster, err := json.Marshal(map[string]any{"version": "a2a-envelope-roster/1", "authors": map[string]string{signedPostAuthor: signedPostKey}, "facts": v.Fixture})
	if err != nil {
		t.Fatal(err)
	}
	observer, err := envelopeobserve.ParseRoster(roster)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.hs.server = "test.invalid"
	recipient := e.agent("recipient")
	author := e.agent("author")
	other := e.agent("other")
	third := e.agent("third")
	if author != signedPostAuthor {
		t.Fatalf("author id %q", author)
	}
	old := e.hs.addRoom("fixture", recipient, author, other, third)
	room := "!room:test.invalid"
	e.hs.mu.Lock()
	r := e.hs.rooms[old]
	delete(e.hs.rooms, old)
	r.id = room
	e.hs.rooms[room] = r
	e.hs.aliases[r.alias] = room
	e.hs.mu.Unlock()
	e.now = func() time.Time { return time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC) }
	e.sequentialSessions()
	if err := os.WriteFile(filepath.Join(e.dir, "envelope-observe.json"), []byte(`{"roster_path":"/protected/roster.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signer := &postFakeSigner{identity: signedPostIdentity(t)}
	e.configure = func(a *App) {
		a.loadEnvelopeRoster = func(string) (*envelopeobserve.Observer, error) { return observer, nil }
		a.postSigner, a.postAudience = signer, signedPostAudience
		a.postQualified = func(signedpost.Identity) bool { return true }
	}
	e.as("author", "topic", "new", room, "alpha", "A").ok(t)
	return e, room, signer
}

func (e *env) verificationOf(t *testing.T, reader, room, id string) string {
	t.Helper()
	recs := decodeRecords(t, e.as(reader, "read", room, "--event", id, "--json").ok(t).stdout)
	if len(recs) != 1 || recs[0].EventID != id {
		t.Fatalf("records: %+v", recs)
	}
	return recs[0].Verification
}

func storedContent(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	ev, ok := e.hs.eventByID(id)
	if !ok {
		t.Fatalf("event %q not stored", id)
	}
	var content map[string]any
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		t.Fatal(err)
	}
	return content
}

// Proves the round trip through the real commands: `post --sign` sends one
// event after one Identity and one Sign call, and `read` labels it with the
// pinned verifier and the receiver rule:
//   - one recipient: verified for that reader, message_recipient_mismatch for
//     another member;
//   - two recipients: verified for both, recipient_not_addressed for a member
//     outside the signed list;
//   - no recipient (room broadcast): verified for every member.
//
// Limits: the fake signer is qualified only through the test seam; the roster,
// audience and clock are the fixture's; the carried issuer is not verified.
// Claim: actual signed posts verify only for their addressed readers, including room broadcasts.
func TestPostSignRoundTrip(t *testing.T) {
	const verified = "verified: " + signedPostAuthor
	cases := []struct {
		name string
		to   []string
		want map[string]string // reader -> label
	}{
		{name: "single-recipient", to: []string{"--to", "recipient"}, want: map[string]string{"recipient": verified, "other": "invalid: message_recipient_mismatch"}},
		{name: "two-recipients", to: []string{"--to", "recipient,other"}, want: map[string]string{"recipient": verified, "other": verified, "third": "invalid: recipient_not_addressed"}},
		{name: "broadcast", want: map[string]string{"recipient": verified, "third": verified}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, room, signer := signedPostWorld(t)
			before := e.eventCount()
			args := append([]string{"post", room, "--topic", "alpha", "--sign"}, tc.to...)
			id := strings.TrimSpace(e.as("author", append(args, "hello signed")...).ok(t).stdout)
			if e.eventCount() != before+1 || !strings.HasPrefix(id, "$") {
				t.Fatalf("signed post not sent once: events %d->%d out %q", before, e.eventCount(), id)
			}
			if signer.identityCalls != 1 || signer.signCalls != 1 {
				t.Fatalf("signer calls: identity %d sign %d", signer.identityCalls, signer.signCalls)
			}
			for reader, want := range tc.want {
				if got := e.verificationOf(t, reader, room, id); got != want {
					t.Fatalf("%s reads %q, want %q", reader, got, want)
				}
			}
		})
	}
}

// Proves signed content adds only the envelope and byte-exact attestation; unsigned content is unchanged.
func TestPostSignAddsEnvelopeAndAttestation(t *testing.T) {
	e, room, _ := signedPostWorld(t)
	args := []string{"post", room, "--topic", "alpha", "--to", "recipient", "hello signed"}
	signedID := strings.TrimSpace(e.as("author", append(args, "--sign")...).ok(t).stdout)
	plainID := strings.TrimSpace(e.as("author", args...).ok(t).stdout)
	signed, plain := storedContent(t, e, signedID), storedContent(t, e, plainID)
	encoded, ok := signed[matrix.EnvelopeContentKey].(string)
	if !ok || encoded == "" {
		t.Fatalf("signed event carries no envelope: %v", signed)
	}
	if _, present := plain[matrix.EnvelopeContentKey]; present {
		t.Fatal("unsigned event carries an envelope")
	}
	attestation, ok := signed[signedpost.AttestationContentKey].(string)
	if !ok {
		t.Fatal("signed event carries no attestation")
	}
	raw, err := signedpost.DecodeAttestation(attestation)
	if err != nil || !bytes.Equal(raw, signedPostIdentity(t).Attestation) {
		t.Fatalf("carried attestation changed: %v", err)
	}
	if _, present := plain[signedpost.AttestationContentKey]; present {
		t.Fatal("unsigned event carries an attestation")
	}
	delete(signed, matrix.EnvelopeContentKey)
	delete(signed, signedpost.AttestationContentKey)
	if !reflect.DeepEqual(signed, plain) {
		t.Fatal("signed content differs beyond the envelope and attestation members")
	}
	if got := e.verificationOf(t, "recipient", room, plainID); got != "unsigned (no envelope)" {
		t.Fatalf("unsigned control labelled %q", got)
	}
}

// Proves the metadata binding end to end: a copy of a verified signed post
// whose header kind was changed, or whose mentions lost the recipient, is
// labelled metadata_mismatch; a copy with a changed text is
// message_text_mismatch; the unchanged copy stays verified.
// Claim: altered header or mentions fail metadata binding and altered text fails body binding beside a verified copy.
func TestPostSignMetadataTamper(t *testing.T) {
	e, room, _ := signedPostWorld(t)
	id := strings.TrimSpace(e.as("author", "post", room, "--topic", "alpha", "--sign", "--to", "recipient", "hello signed").ok(t).stdout)
	if got := e.verificationOf(t, "recipient", room, id); got != "verified: "+signedPostAuthor {
		t.Fatalf("control: %q", got)
	}
	cases := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"unchanged-copy", func(map[string]any) {}, "verified: " + signedPostAuthor},
		{"kind", func(c map[string]any) { c[ContentKey].(map[string]any)["kind"] = "decision" }, "invalid: metadata_mismatch"},
		{"re", func(c map[string]any) { c[ContentKey].(map[string]any)["re"] = "$question" }, "invalid: metadata_mismatch"},
		{"mentions", func(c map[string]any) { delete(c, "m.mentions") }, "invalid: metadata_mismatch"},
		{"text", func(c map[string]any) { c["body"] = c["body"].(string) + "!" }, "invalid: message_text_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := storedContent(t, e, id)
			tc.edit(content)
			copyID := e.hs.inject(room, signedPostAuthor, content)
			if got := e.verificationOf(t, "recipient", room, copyID); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Records the named limit: the receiver verifies at read time, so the same
// signed post that is verified inside its 120 s lifetime is labelled expired
// when it is read after it. Observe mode still prints the message.
// Claim: a post verified within its lifetime is labelled expired after 120 seconds.
func TestPostSignReadAfterLifetime(t *testing.T) {
	e, room, _ := signedPostWorld(t)
	id := strings.TrimSpace(e.as("author", "post", room, "--topic", "alpha", "--sign", "--to", "recipient", "hello signed").ok(t).stdout)
	if got := e.verificationOf(t, "recipient", room, id); got != "verified: "+signedPostAuthor {
		t.Fatalf("inside the lifetime: %q", got)
	}
	e.now = func() time.Time { return time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC) }
	if got := e.verificationOf(t, "recipient", room, id); got != "invalid: expired" {
		t.Fatalf("after the lifetime: %q", got)
	}
}

// Proves two signed posts with different text never share a request id, and
// that each id is the one derived from the preimage inside the seam call.
// Claim: posts with different text use different derived signing request ids.
func TestPostSignRequestIDsDiffer(t *testing.T) {
	e, room, signer := signedPostWorld(t)
	for _, text := range []string{"first text", "second text"} {
		e.as("author", "post", room, "--topic", "alpha", "--sign", "--to", "recipient", text).ok(t)
	}
	if len(signer.ids) != 2 || len(signer.ids[0]) != 64 || signer.ids[0] == signer.ids[1] {
		t.Fatalf("request ids %v", signer.ids)
	}
}
