package signedpost

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
	"github.com/relux-works/swarma-envelope/record"
)

// Expected values are literals fixed here. The key, the attestation id and the
// attestation record are the public BIP-340 author fixture of the pinned
// envelope module (internal/envelopeobserve/testdata/P-BIP-author.json). The
// fake signer signs with that fixture's published test-only operand (32 bytes
// of 0x03 in the module's vector generator); it is not a secret and protects
// nothing. No other key material exists in this package.
const (
	testKey           = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"
	testAttestationID = "sha256:f0f08c11a4388a53fc9240302f29b9984a13ce9b9a8b27f74d87f183951d74bc"
	otherContentID    = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	otherKey          = "ed25519:b2CdQco2XmEi01K78Tg0l7-24Eu5_wHLHlSvQE3mj1M"
	testAudience      = "service-a"
	testRoom          = "!room:test.invalid"
	helloSHA256       = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
)

var testNow = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

func fixtureAttestation(t testing.TB) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "envelopeobserve", "testdata", "P-BIP-author.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Fixture struct {
			Attestations map[string]string `json:"attestations"`
		} `json:"fixture"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	a, err := base64.RawURLEncoding.DecodeString(v.Fixture.Attestations[testAttestationID])
	if err != nil || len(a) == 0 {
		t.Fatalf("fixture attestation missing: %v", err)
	}
	return a
}

// fakeSigner is the in-package fake behind the seam. Like a holder it admits
// the preimage with the module's own admission, derives the request id from
// the preimage and signs SHA-256 of it once.
type fakeSigner struct {
	identity      Identity
	identityErr   error
	signErrs      []error // one per Sign call, consumed in order; nil signs
	wrongKey      bool
	flipSignature bool
	identityCalls int
	signCalls     int
	ids           []string
	preimages     [][]byte
}

func (f *fakeSigner) Identity(context.Context) (Identity, error) {
	f.identityCalls++
	return f.identity, f.identityErr
}

func (f *fakeSigner) Sign(_ context.Context, preimage []byte) ([]byte, []byte, error) {
	f.signCalls++
	f.ids = append(f.ids, RequestID(preimage))
	f.preimages = append(f.preimages, append([]byte(nil), preimage...))
	if len(f.signErrs) > 0 {
		err := f.signErrs[0]
		f.signErrs = f.signErrs[1:]
		if err != nil {
			return nil, nil, err
		}
	}
	if _, err := record.AdmitSigner(preimage, record.IdentityPolicy{Check: true, CallerKey: testKey, CallerAttestationID: testAttestationID}); err != nil {
		return nil, nil, &RefusedError{HolderCode: err.Error()}
	}
	private, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	digest := sha256.Sum256(preimage)
	signature, err := schnorr.Sign(private, digest[:])
	if err != nil {
		return nil, nil, err
	}
	out := signature.Serialize()
	if f.flipSignature {
		out[3] ^= 1
	}
	key := testKey
	if f.wrongKey {
		key = otherKey
	}
	return []byte(key), out, nil
}

// countingRand yields a different nonce on every read, deterministically.
type countingRand struct{ n byte }

func (r *countingRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
	}
	r.n++
	return len(p), nil
}

func validIdentity(t testing.TB) Identity {
	return Identity{Key: testKey, AttestationID: testAttestationID, Attestation: fixtureAttestation(t)}
}

func qualified(Identity) bool { return true }

func newSender(f *fakeSigner, now *time.Time) Sender {
	return Sender{Signer: f, Audience: testAudience, Qualified: qualified, Now: func() time.Time { return *now }, Rand: &countingRand{}}
}

func draft(text string, to ...string) Draft {
	return Draft{Room: testRoom, Recipients: to, Kind: "notice", Topic: "$root", Re: "", Text: []byte(text)}
}

func recipients(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("@u%02d:test.invalid", i)
	}
	return out
}

func parseEnvelope(t *testing.T, raw []byte) (envelope, body map[string]any) {
	t.Helper()
	v, err := record.Parse(raw, 65536, 16)
	if err != nil {
		t.Fatalf("envelope does not parse: %v", err)
	}
	envelope, _ = v.(map[string]any)
	body, _ = envelope["body"].(map[string]any)
	if envelope == nil || body == nil {
		t.Fatalf("envelope shape: %s", raw)
	}
	return envelope, body
}

func stringList(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func wantRefusal(t *testing.T, err error, code, detail string, retry bool) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != code || r.Detail != detail || r.Retryable != retry {
		t.Fatalf("got %v, want %s (%s) retry=%v", err, code, detail, retry)
	}
}

// Proves the fake's signing operand is the fixture key named by the literal:
// a wrong operand fails here, not as an unexplained signature refusal.
// Claim: the test-generated signing key matches the literal public fixture identity.
func TestFixtureKey(t *testing.T) {
	_, public := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	if got := "bip340:" + base64.RawURLEncoding.EncodeToString(schnorr.SerializePubKey(public)); got != testKey {
		t.Fatalf("fixture operand yields %s", got)
	}
}

// Proves each contract row constant equals the literal of the contract table,
// and that the tag row equals the pinned module's own tag for the message
// type. A silent edit of a row fails here. It does not prove the rows are used.
// Claim: all signed-message constants equal their literal contract values.
func TestContractRows(t *testing.T) {
	if KeyPrefix != "bip340:" || ContentIDPrefix != "sha256:" || EnvelopeVersion != "swarm-envelope/2" || SignatureRole != "author" ||
		DomainTag != "swarma-msg/2" || MaxPreimageBytes != 24576 || MessageType != "message/1" || Action != "a2a.post/1" ||
		MaxLifetime != 120*time.Second || MaxRecipients != 64 || MaxRecipientBytes != 255 || MaxAttestationBytes != 8192 ||
		TargetKind != "station" || TargetID != "a2a/post" || AttestationType != "attestation/1" || AttestationContentKey != "works.relux.swarm.author_attestation" || MaxSignAttempts != 3 || MaxPublishAttempts != 2 {
		t.Fatal("a contract row changed")
	}
	if record.Tag(MessageType) != DomainTag {
		t.Fatalf("module tag %q", record.Tag(MessageType))
	}
	if got := strings.Join(BodyMembers, " "); got != "type aud nonce iat exp room to body_sha256 author author_attestation action target arguments" {
		t.Fatalf("body members: %s", got)
	}
	if got := strings.Join(ArgumentMembers, " "); got != "kind topic re to" {
		t.Fatalf("argument members: %s", got)
	}
	codes := []string{RefuseSignerUnconfigured, RefuseSignerUnavailable, RefuseSignerUnqualified, RefuseSignerRefused, RefuseSignerResultInvalid, RefuseRecipientsTooMany}
	if got := strings.Join(codes, " "); got != "signer_unconfigured signer_unavailable signer_unqualified signer_refused signer_result_invalid recipients_too_many" {
		t.Fatalf("exit vocabulary: %s", got)
	}
}

// Proves the `to` row: one recipient is the signed `to`, zero or several put the
// room id there with the ordered list kept whole; 65 recipients, a duplicate,
// a 256-byte id and an id with a space are refused with recipients_too_many
// beside the 64 and 255 controls.
// Claim: recipient encoding preserves order and rejects invalid full MXIDs, duplicates and size violations.
func TestRecipients(t *testing.T) {
	long := "@" + strings.Repeat("a", 240) + ":test.invalid" // 254 bytes
	cases := []struct {
		name     string
		in       []string
		wantTo   string
		wantCode string
		detail   string
	}{
		{name: "broadcast", in: nil, wantTo: testRoom},
		{name: "single", in: []string{"@one:test.invalid"}, wantTo: "@one:test.invalid"},
		{name: "three", in: []string{"@c:test.invalid", "@a:test.invalid", "@b:test.invalid"}, wantTo: testRoom},
		{name: "cap-64", in: recipients(64), wantTo: testRoom},
		{name: "length-255", in: []string{long + "x"}, wantTo: long + "x"},
		{name: "over-cap-65", in: recipients(65), wantCode: "recipients_too_many", detail: "more than 64 recipients"},
		{name: "duplicate", in: []string{"@a:test.invalid", "@b:test.invalid", "@a:test.invalid"}, wantCode: "recipients_too_many", detail: "duplicate recipient"},
		{name: "length-256", in: []string{long + "xy"}, wantCode: "recipients_too_many", detail: "recipient over 255 bytes"},
		{name: "not-printable", in: []string{"@a b:test.invalid"}, wantCode: "recipients_too_many", detail: "recipient is not a printable ASCII id"},
		{name: "empty-id", in: []string{""}, wantCode: "recipients_too_many", detail: "recipient is not a printable ASCII id"},
		{name: "local-name", in: []string{"recipient"}, wantCode: "recipients_too_many", detail: "recipient is not a full MXID"},
		{name: "room-id", in: []string{"!room:test.invalid"}, wantCode: "recipients_too_many", detail: "recipient is not a full MXID"},
		{name: "missing-server", in: []string{"@recipient:"}, wantCode: "recipients_too_many", detail: "recipient is not a full MXID"},
		{name: "missing-local", in: []string{"@:test.invalid"}, wantCode: "recipients_too_many", detail: "recipient is not a full MXID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			to, list, err := Recipients(testRoom, tc.in)
			if tc.wantCode != "" {
				var r *Refusal
				if !errors.As(err, &r) || r.Code != tc.wantCode || r.Detail != tc.detail || to != "" || list != nil {
					t.Fatalf("got to=%q list=%v err=%v, want refusal %s (%s)", to, list, err, tc.wantCode, tc.detail)
				}
				return
			}
			if err != nil || to != tc.wantTo || len(list) != len(tc.in) {
				t.Fatalf("got to=%q list=%v err=%v", to, list, err)
			}
			for i := range tc.in {
				if list[i] != tc.in[i] {
					t.Fatalf("recipient order changed at %d: %v", i, list)
				}
			}
		})
	}
}

// Proves the identity shape checks: the valid identity and an 8192-byte
// attestation pass; another key type, a malformed key, a malformed content id,
// an empty and an 8193-byte attestation are signer_unqualified with their own
// detail. The content id of A is a separate check (TestAttestationID).
// Claim: only admitted key and content-id shapes with 1..8192 attestation bytes pass.
func TestCheckIdentityShape(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*Identity)
		detail string
	}{
		{name: "valid", edit: func(*Identity) {}},
		{name: "attestation-8192", edit: func(id *Identity) { id.Attestation = make([]byte, 8192) }},
		{name: "other-key-type", edit: func(id *Identity) { id.Key = otherKey }, detail: "author key type not admitted"},
		{name: "malformed-key", edit: func(id *Identity) { id.Key = "bip340:other" }, detail: "author key type not admitted"},
		{name: "empty-key", edit: func(id *Identity) { id.Key = "" }, detail: "author key type not admitted"},
		{name: "id-without-prefix", edit: func(id *Identity) { id.AttestationID = strings.TrimPrefix(testAttestationID, "sha256:") }, detail: "attestation id malformed"},
		{name: "id-short", edit: func(id *Identity) { id.AttestationID = testAttestationID[:len(testAttestationID)-1] }, detail: "attestation id malformed"},
		{name: "id-uppercase", edit: func(id *Identity) { id.AttestationID = "sha256:" + strings.ToUpper(testAttestationID[7:]) }, detail: "attestation id malformed"},
		{name: "attestation-empty", edit: func(id *Identity) { id.Attestation = nil }, detail: "attestation empty"},
		{name: "attestation-8193", edit: func(id *Identity) { id.Attestation = make([]byte, 8193) }, detail: "attestation over 8192 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := validIdentity(t)
			tc.edit(&id)
			err := CheckIdentityShape(id)
			if tc.detail == "" {
				if err != nil {
					t.Fatalf("valid identity refused: %v", err)
				}
				return
			}
			wantRefusal(t, err, "signer_unqualified", tc.detail, false)
		})
	}
}

// Proves the content-id check of A against T with the module's function: the
// fixture record has exactly the literal id and passes; the same record under
// another well-formed T is a mismatch; a record with one changed body byte, a
// record whose own `id` member was changed, a truncated record and a record of
// another shape have no content id (malformed). It proves nothing about A's
// signature or issuer: neither is verified in this unit.
// Claim: only a parsed attestation whose stated content id equals both its body id and T passes.
func TestAttestationID(t *testing.T) {
	a := fixtureAttestation(t)
	if got, err := AttestationID(a); err != nil || got != testAttestationID {
		t.Fatalf("content id of the fixture attestation: %q %v", got, err)
	}
	if !bytes.Contains(a, []byte("generation-0")) || !bytes.Contains(a, []byte(testAttestationID)) {
		t.Fatal("fixture attestation lacks the members the negatives edit")
	}
	cases := []struct {
		name   string
		id     Identity
		detail string
	}{
		{name: "valid", id: Identity{Key: testKey, AttestationID: testAttestationID, Attestation: a}},
		{name: "other-id", id: Identity{Key: testKey, AttestationID: otherContentID, Attestation: a}, detail: "attestation content id differs from the attestation id"},
		{name: "body-byte-changed", id: Identity{Key: testKey, AttestationID: testAttestationID, Attestation: bytes.Replace(a, []byte("generation-0"), []byte("generation-1"), 1)}, detail: "attestation malformed"},
		{name: "id-member-changed", id: Identity{Key: testKey, AttestationID: otherContentID, Attestation: bytes.Replace(a, []byte(testAttestationID), []byte(otherContentID), 1)}, detail: "attestation malformed"},
		{name: "truncated", id: Identity{Key: testKey, AttestationID: testAttestationID, Attestation: a[:len(a)-1]}, detail: "attestation malformed"},
		{name: "other-shape", id: Identity{Key: testKey, AttestationID: testAttestationID, Attestation: []byte(`{"body":{"type":"message/1"},"id":"x"}`)}, detail: "attestation malformed"},
		{name: "not-json", id: Identity{Key: testKey, AttestationID: testAttestationID, Attestation: []byte("opaque")}, detail: "attestation malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAttestation(tc.id)
			if tc.detail == "" {
				if err != nil {
					t.Fatalf("valid attestation refused: %v", err)
				}
				return
			}
			wantRefusal(t, err, "signer_unqualified", tc.detail, false)
		})
	}
}

// Proves canonical attestation encoding round-trips exact bytes and rejects empty, oversize and malformed values.
func TestAttestationCarriedForm(t *testing.T) {
	for _, raw := range [][]byte{fixtureAttestation(t), bytes.Repeat([]byte{0xff}, 8192), {0}} {
		text, err := EncodeAttestation(raw)
		if err != nil || strings.ContainsAny(text, "=+/") {
			t.Fatalf("encode: %q %v", text, err)
		}
		back, err := DecodeAttestation(text)
		if err != nil || !bytes.Equal(back, raw) {
			t.Fatalf("round trip changed %d bytes: %v", len(raw), err)
		}
	}
	if _, err := EncodeAttestation(nil); Code(err) != "signer_unqualified" {
		t.Fatalf("empty attestation encoded: %v", err)
	}
	if _, err := EncodeAttestation(make([]byte, 8193)); Code(err) != "signer_unqualified" {
		t.Fatalf("8193-byte attestation encoded: %v", err)
	}
	for _, text := range []string{"", "AA==", "AB", "A A", strings.Repeat("A", base64.RawURLEncoding.EncodedLen(8192)+1)} {
		if raw, err := DecodeAttestation(text); err == nil {
			t.Fatalf("decoded %q to %d bytes", text, len(raw))
		}
	}
}

// Proves Prepare through the seam: with the test-only qualification the valid
// identity returns exactly the fixed key, content id, attestation bytes and
// recipients after one Identity call; every refusal row returns its exact code,
// the zero result and never calls Sign. The recipient rows record ZERO signer
// calls of any kind.
// Claim: preparation refuses invalid identities or absent qualification, with zero signer calls for invalid recipients.
func TestPrepare(t *testing.T) {
	three := []string{"@c:test.invalid", "@a:test.invalid", "@b:test.invalid"}
	attestation := fixtureAttestation(t)
	cases := []struct {
		name          string
		noSigner      bool
		noAudience    bool
		unqualified   bool
		recipients    []string
		identityErr   error
		edit          func(*Identity)
		wantCode      string
		wantDetail    string
		wantRetry     bool
		identityCalls int
	}{
		{name: "valid", recipients: three, identityCalls: 1},
		{name: "valid-single", recipients: []string{"@one:test.invalid"}, identityCalls: 1},
		{name: "no-signer", noSigner: true, recipients: three, wantCode: "signer_unconfigured"},
		{name: "no-audience", noAudience: true, recipients: three, wantCode: "signer_unconfigured", wantDetail: "audience not configured"},
		{name: "over-cap-before-signer", recipients: recipients(65), wantCode: "recipients_too_many", wantDetail: "more than 64 recipients"},
		{name: "duplicate-before-signer", recipients: []string{"@a:test.invalid", "@a:test.invalid"}, wantCode: "recipients_too_many", wantDetail: "duplicate recipient"},
		{name: "local-name-before-signer", recipients: []string{"recipient"}, wantCode: "recipients_too_many", wantDetail: "recipient is not a full MXID"},
		{name: "signer-unconfigured", recipients: three, identityErr: ErrUnconfigured, wantCode: "signer_unconfigured", identityCalls: 1},
		{name: "signer-unavailable", recipients: three, identityErr: fmt.Errorf("dial: %w", ErrUnavailable), wantCode: "signer_unavailable", wantRetry: true, identityCalls: 1},
		{name: "signer-deadline", recipients: three, identityErr: context.DeadlineExceeded, wantCode: "signer_unavailable", wantRetry: true, identityCalls: 1},
		{name: "signer-refused", recipients: three, identityErr: &RefusedError{HolderCode: "request_conflict"}, wantCode: "signer_refused", wantDetail: "request_conflict", identityCalls: 1},
		{name: "signer-refused-free-text", recipients: three, identityErr: &RefusedError{HolderCode: "Not A Code: " + testKey}, wantCode: "signer_refused", identityCalls: 1},
		{name: "signer-unqualified", recipients: three, identityErr: ErrUnqualified, wantCode: "signer_unqualified", identityCalls: 1},
		{name: "signer-unknown-error", recipients: three, identityErr: errors.New("garbled reply"), wantCode: "signer_result_invalid", identityCalls: 1},
		{name: "attestation-empty", recipients: three, edit: func(id *Identity) { id.Attestation = nil }, wantCode: "signer_unqualified", wantDetail: "attestation empty", identityCalls: 1},
		{name: "attestation-8193", recipients: three, edit: func(id *Identity) { id.Attestation = make([]byte, 8193) }, wantCode: "signer_unqualified", wantDetail: "attestation over 8192 bytes", identityCalls: 1},
		{name: "attestation-id-differs", recipients: three, edit: func(id *Identity) { id.AttestationID = otherContentID }, wantCode: "signer_unqualified", wantDetail: "attestation content id differs from the attestation id", identityCalls: 1},
		{name: "no-pin", unqualified: true, recipients: three, wantCode: "signer_unqualified", wantDetail: "purpose-5 pin not configured", identityCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSigner{identity: validIdentity(t), identityErr: tc.identityErr}
			if tc.edit != nil {
				tc.edit(&f.identity)
			}
			now := testNow
			s := newSender(f, &now)
			if tc.noSigner {
				s.Signer = nil
			}
			if tc.noAudience {
				s.Audience = ""
			}
			if tc.unqualified {
				s.Qualified = nil
			}
			got, err := s.Prepare(context.Background(), testRoom, tc.recipients)
			if f.signCalls != 0 || f.identityCalls != tc.identityCalls {
				t.Fatalf("signer calls: identity %d (want %d), sign %d (want 0)", f.identityCalls, tc.identityCalls, f.signCalls)
			}
			if tc.wantCode == "" {
				wantTo := testRoom
				if len(tc.recipients) == 1 {
					wantTo = "@one:test.invalid"
				}
				if err != nil || got.Identity.Key != testKey || got.Identity.AttestationID != testAttestationID ||
					!bytes.Equal(got.Identity.Attestation, attestation) || got.To != wantTo || !reflect.DeepEqual(got.Recipients, tc.recipients) {
					t.Fatalf("prepared %+v err %v", got, err)
				}
				return
			}
			wantRefusal(t, err, tc.wantCode, tc.wantDetail, tc.wantRetry)
			if Code(err) != tc.wantCode || !reflect.DeepEqual(got, Prepared{}) {
				t.Fatalf("refusal returned a result: %+v", got)
			}
			// A refusal text carries only fixed local words or a holder code name.
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), string(attestation)) {
				t.Fatalf("refusal text carries identity material: %s", err)
			}
		})
	}
}

// Proves the build, sign, verify and assemble path for each body shape (one
// recipient, three recipients, room broadcast): the body has exactly the 13
// members with the fixed literal values, a 120 s lifetime and the hash of the
// exact text; the preimage handed to the signer is the module's signing
// preimage of that body and passes the module's signer admission (inside the
// fake) and domain check; the envelope id is the module's content id; the one
// signature verifies; A is returned byte for byte; the signer saw one Identity
// and one Sign call. Verification by the receiver is proved in
// internal/envelopeobserve and internal/cli.
// Claim: each recipient shape produces the exact canonical signed body, valid signature and intact attestation.
func TestSignRoundTrip(t *testing.T) {
	attestation := fixtureAttestation(t)
	cases := []struct {
		name   string
		to     []string
		wantTo string
	}{
		{name: "single", to: []string{"@one:test.invalid"}, wantTo: "@one:test.invalid"},
		{name: "three", to: []string{"@c:test.invalid", "@a:test.invalid", "@b:test.invalid"}, wantTo: testRoom},
		{name: "broadcast", to: nil, wantTo: testRoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSigner{identity: validIdentity(t)}
			now := testNow
			signed, err := newSender(f, &now).Sign(context.Background(), draft("hello", tc.to...))
			if err != nil {
				t.Fatal(err)
			}
			if f.identityCalls != 1 || f.signCalls != 1 {
				t.Fatalf("signer calls: identity %d sign %d", f.identityCalls, f.signCalls)
			}
			envelope, body := parseEnvelope(t, signed.Envelope)
			if len(body) != len(BodyMembers) {
				t.Fatalf("body has %d members", len(body))
			}
			for _, name := range BodyMembers {
				if _, ok := body[name]; !ok {
					t.Fatalf("body lacks %s", name)
				}
			}
			for name, want := range map[string]string{
				"type": "message/1", "aud": "service-a", "iat": "2026-01-01T00:01:00Z", "exp": "2026-01-01T00:03:00Z",
				"room": "!room:test.invalid", "to": tc.wantTo, "body_sha256": helloSHA256, "author": testKey,
				"author_attestation": testAttestationID, "action": "a2a.post/1",
			} {
				if body[name] != want {
					t.Fatalf("body %s = %v, want %s", name, body[name], want)
				}
			}
			if _, err := record.DecodeBase64(fmt.Sprint(body["nonce"]), 16); err != nil {
				t.Fatalf("nonce %v", body["nonce"])
			}
			if !reflect.DeepEqual(body["target"], map[string]any{"kind": "station", "id": "a2a/post"}) {
				t.Fatalf("target %v", body["target"])
			}
			arguments, _ := body["arguments"].(map[string]any)
			if len(arguments) != 4 || arguments["kind"] != "notice" || arguments["topic"] != "$root" || arguments["re"] != "" {
				t.Fatalf("arguments %v", arguments)
			}
			if !reflect.DeepEqual(stringList(arguments["to"]), append([]string{}, tc.to...)) {
				t.Fatalf("arguments.to %v, want %v", arguments["to"], tc.to)
			}
			if len(envelope) != 4 || envelope["version"] != "swarm-envelope/2" {
				t.Fatalf("envelope members %v", envelope)
			}
			id, err := record.ContentID("message/1", body)
			if err != nil || envelope["id"] != id || signed.ID != id {
				t.Fatalf("envelope id %v, content id %s (%v)", envelope["id"], id, err)
			}
			signatures, _ := envelope["signatures"].([]any)
			if len(signatures) != 1 {
				t.Fatalf("signatures %v", envelope["signatures"])
			}
			entry, _ := signatures[0].(map[string]any)
			if len(entry) != 3 || entry["role"] != "author" || entry["key"] != testKey {
				t.Fatalf("signature entry %v", entry)
			}
			signature, err := record.DecodeBase64(fmt.Sprint(entry["sig"]), 64)
			if err != nil {
				t.Fatal(err)
			}
			preimage, err := record.SigningPreimage("message/1", testKey, "author", body)
			if err != nil || !bytes.Equal(preimage, f.preimages[0]) || !bytes.HasPrefix(preimage, []byte("swarma-msg/2\n")) || len(preimage) > 24576 {
				t.Fatalf("the signer did not get the module's preimage of the published body (%v)", err)
			}
			if err := record.CheckDomain(preimage, "swarma-msg/2", testKey, signature); err != nil {
				t.Fatalf("module domain check: %v", err)
			}
			if !bytes.Equal(signed.Attestation, attestation) {
				t.Fatal("attestation bytes changed")
			}
			if !signed.Expires.Equal(testNow.Add(120 * time.Second)) {
				t.Fatalf("expires %s", signed.Expires)
			}
		})
	}
}

// Proves every refusal of the signing step with its exact code beside the
// valid control: a returned key other than the identity key and a signature
// with one flipped bit are signer_result_invalid; a holder refusal is
// signer_refused after ONE Sign call; a retryable failure is retried with the
// same request id and gives signer_unavailable after exactly three calls, or
// succeeds on the second; the recipient cap is refused with ZERO signer calls;
// a missing pin stops before Sign. Every refusal returns the zero value.
// Claim: key, signature and signer failures never yield an envelope, and retryable errors make at most three identical requests.
func TestSignRefusals(t *testing.T) {
	one := []string{"@one:test.invalid"}
	cases := []struct {
		name       string
		to         []string
		edit       func(*fakeSigner, *Sender)
		wantCode   string
		wantDetail string
		wantRetry  bool
		identity   int
		sign       int
	}{
		{name: "valid", to: one, edit: func(*fakeSigner, *Sender) {}, identity: 1, sign: 1},
		{name: "retry-then-signed", to: one, edit: func(f *fakeSigner, _ *Sender) { f.signErrs = []error{ErrUnavailable} }, identity: 1, sign: 2},
		{name: "returned-key-differs", to: one, edit: func(f *fakeSigner, _ *Sender) { f.wrongKey = true }, wantCode: "signer_result_invalid", wantDetail: "returned key differs from the identity key", identity: 1, sign: 1},
		{name: "bad-signature", to: one, edit: func(f *fakeSigner, _ *Sender) { f.flipSignature = true }, wantCode: "signer_result_invalid", wantDetail: "signature does not verify", identity: 1, sign: 1},
		{name: "sign-refused", to: one, edit: func(f *fakeSigner, _ *Sender) {
			f.signErrs = []error{&RefusedError{HolderCode: "signer_identity_mismatch"}}
		}, wantCode: "signer_refused", wantDetail: "signer_identity_mismatch", identity: 1, sign: 1},
		{name: "sign-unavailable", to: one, edit: func(f *fakeSigner, _ *Sender) {
			f.signErrs = []error{ErrUnavailable, ErrUnavailable, ErrUnavailable, ErrUnavailable}
		}, wantCode: "signer_unavailable", wantRetry: true, identity: 1, sign: 3},
		{name: "sign-garbled", to: one, edit: func(f *fakeSigner, _ *Sender) { f.signErrs = []error{errors.New("garbled")} }, wantCode: "signer_result_invalid", identity: 1, sign: 1},
		{name: "over-cap", to: recipients(65), edit: func(*fakeSigner, *Sender) {}, wantCode: "recipients_too_many", wantDetail: "more than 64 recipients"},
		{name: "no-pin", to: one, edit: func(_ *fakeSigner, s *Sender) { s.Qualified = nil }, wantCode: "signer_unqualified", wantDetail: "purpose-5 pin not configured", identity: 1},
		{name: "attestation-id-differs", to: one, edit: func(f *fakeSigner, _ *Sender) { f.identity.AttestationID = otherContentID }, wantCode: "signer_unqualified", wantDetail: "attestation content id differs from the attestation id", identity: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSigner{identity: validIdentity(t)}
			now := testNow
			s := newSender(f, &now)
			tc.edit(f, &s)
			signed, err := s.Sign(context.Background(), draft("hello", tc.to...))
			if f.identityCalls != tc.identity || f.signCalls != tc.sign {
				t.Fatalf("signer calls: identity %d (want %d), sign %d (want %d)", f.identityCalls, tc.identity, f.signCalls, tc.sign)
			}
			for _, id := range f.ids {
				if id != f.ids[0] {
					t.Fatalf("a retry changed the request id: %v", f.ids)
				}
			}
			if tc.wantCode == "" {
				if err != nil || len(signed.Envelope) == 0 {
					t.Fatalf("valid control refused: %v", err)
				}
				return
			}
			wantRefusal(t, err, tc.wantCode, tc.wantDetail, tc.wantRetry)
			if signed.Envelope != nil || signed.ID != "" || signed.Attestation != nil || !signed.Expires.IsZero() {
				t.Fatalf("refusal returned a result: %+v", signed)
			}
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), otherKey) {
				t.Fatalf("refusal text carries a key: %s", err)
			}
		})
	}
}

// Proves the request-id rule: the id is a function of the exact preimage only.
// Two posts with different text never share an id; neither do two posts that
// differ only in the nonce; the same preimage has the same id. The Signer
// interface gives a caller no way to pass an id.
// Claim: identical preimages share an id and changed text or nonce changes it.
func TestRequestIDs(t *testing.T) {
	sign := func(text string, nonce byte) string {
		f := &fakeSigner{identity: validIdentity(t)}
		now := testNow
		s := newSender(f, &now)
		s.Rand = &countingRand{n: nonce}
		if _, err := s.Sign(context.Background(), draft(text, "@one:test.invalid")); err != nil {
			t.Fatal(err)
		}
		if len(f.ids) != 1 || len(f.ids[0]) != 64 || f.ids[0] != RequestID(f.preimages[0]) {
			t.Fatalf("request ids %v", f.ids)
		}
		return f.ids[0]
	}
	first, same, otherText, otherNonce := sign("first text", 1), sign("first text", 1), sign("second text", 1), sign("first text", 2)
	if first != same {
		t.Fatal("the same preimage has two request ids")
	}
	if first == otherText || first == otherNonce || otherText == otherNonce {
		t.Fatal("different preimages share a request id")
	}
}

// Proves the retry rule of publication. A send that fails before `exp` is
// repeated with byte-identical envelope bytes and no second signature. A send
// that fails when `exp` has passed is repeated with a newly signed envelope:
// another nonce, iat, exp and request id. Sending stops after two attempts and
// returns the send error; a final send error stops at once; a refusal never
// reaches send. Limit: the send function is a test double, not a homeserver.
// Claim: publication retries reuse bytes before expiry, renew them after expiry, and never send after refusal.
func TestPublishRetry(t *testing.T) {
	failure := errors.New("send failed")
	run := func(t *testing.T, edit func(*fakeSigner, *Sender), send func(now *time.Time, attempt int) error) (*fakeSigner, [][]byte, error) {
		t.Helper()
		f := &fakeSigner{identity: validIdentity(t)}
		now := testNow
		s := newSender(f, &now)
		if edit != nil {
			edit(f, &s)
		}
		var sent [][]byte
		err := s.Publish(context.Background(), draft("hello", "@one:test.invalid"), func(signed Signed) error {
			sent = append(sent, signed.Envelope)
			return send(&now, len(sent))
		})
		return f, sent, err
	}
	t.Run("first-send-succeeds", func(t *testing.T) {
		f, sent, err := run(t, nil, func(*time.Time, int) error { return nil })
		if err != nil || len(sent) != 1 || f.signCalls != 1 {
			t.Fatalf("err %v sends %d sign calls %d", err, len(sent), f.signCalls)
		}
	})
	t.Run("retry-before-exp-identical-bytes", func(t *testing.T) {
		f, sent, err := run(t, nil, func(now *time.Time, attempt int) error {
			if attempt == 1 {
				*now = now.Add(119 * time.Second)
				return failure
			}
			return nil
		})
		if err != nil || len(sent) != 2 || !bytes.Equal(sent[0], sent[1]) || f.signCalls != 1 || f.identityCalls != 1 {
			t.Fatalf("err %v sends %d sign calls %d", err, len(sent), f.signCalls)
		}
	})
	t.Run("retry-after-exp-new-nonce", func(t *testing.T) {
		f, sent, err := run(t, nil, func(now *time.Time, attempt int) error {
			if attempt == 1 {
				*now = now.Add(120 * time.Second)
				return failure
			}
			return nil
		})
		if err != nil || len(sent) != 2 || f.signCalls != 2 || f.ids[0] == f.ids[1] {
			t.Fatalf("err %v sends %d sign calls %d ids %v", err, len(sent), f.signCalls, f.ids)
		}
		_, old := parseEnvelope(t, sent[0])
		_, fresh := parseEnvelope(t, sent[1])
		if old["nonce"] == fresh["nonce"] || old["iat"] != "2026-01-01T00:01:00Z" || old["exp"] != "2026-01-01T00:03:00Z" ||
			fresh["iat"] != "2026-01-01T00:03:00Z" || fresh["exp"] != "2026-01-01T00:05:00Z" {
			t.Fatalf("old %v %v %v, new %v %v %v", old["nonce"], old["iat"], old["exp"], fresh["nonce"], fresh["iat"], fresh["exp"])
		}
	})
	t.Run("stops-after-two-sends", func(t *testing.T) {
		_, sent, err := run(t, nil, func(*time.Time, int) error { return failure })
		if !errors.Is(err, failure) || len(sent) != 2 {
			t.Fatalf("err %v sends %d", err, len(sent))
		}
	})
	t.Run("final-error-stops", func(t *testing.T) {
		_, sent, err := run(t, nil, func(*time.Time, int) error { return Final(failure) })
		if err != failure || len(sent) != 1 {
			t.Fatalf("err %v sends %d", err, len(sent))
		}
	})
	t.Run("refusal-never-sends", func(t *testing.T) {
		_, sent, err := run(t, func(f *fakeSigner, _ *Sender) { f.flipSignature = true }, func(*time.Time, int) error { return nil })
		if Code(err) != "signer_result_invalid" || len(sent) != 0 {
			t.Fatalf("err %v sends %d", err, len(sent))
		}
	})
	t.Run("refusal-on-resign-never-sends-again", func(t *testing.T) {
		var signer *fakeSigner
		_, sent, err := run(t, func(f *fakeSigner, _ *Sender) { signer = f }, func(now *time.Time, attempt int) error {
			*now = now.Add(120 * time.Second)
			signer.signErrs = []error{&RefusedError{HolderCode: "request_conflict"}}
			return failure
		})
		if Code(err) != "signer_refused" || len(sent) != 1 {
			t.Fatalf("err %v sends %d", err, len(sent))
		}
	})
}
