package signedpost

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/relux-works/swarma-envelope/record"
)

// Verification is the closed, content-free refusal vocabulary shared by the
// author-side sender and the station carrier. It conveys no qualification.
type Verification string

func (v Verification) Error() string { return string(v) }

const (
	InvalidEnvelope     Verification = "signed_envelope_invalid"
	AuthorMismatch      Verification = "signed_author_mismatch"
	InvalidSignature    Verification = "signed_signature_invalid"
	RoomMismatch        Verification = "signed_room_mismatch"
	RecipientsMismatch  Verification = "signed_recipients_mismatch"
	TextMismatch        Verification = "signed_text_mismatch"
	MetadataMismatch    Verification = "signed_metadata_mismatch"
	Expired             Verification = "signed_post_expired"
	InvalidLifetime     Verification = "signed_lifetime_invalid"
	MissingAttestation  Verification = "signed_attestation_missing"
	InvalidAttestation  Verification = "signed_attestation_invalid"
	LargeAttestation    Verification = "signed_attestation_too_large"
	AttestationMismatch Verification = "signed_attestation_mismatch"
	BindingUnavailable  Verification = "signed_binding_unavailable"
)

func exactMembers(m map[string]any, members ...string) bool {
	if len(m) != len(members) {
		return false
	}
	for _, name := range members {
		if _, ok := m[name]; !ok {
			return false
		}
	}
	return true
}

func postStamp(value any) (time.Time, bool) {
	s, ok := value.(string)
	t, err := time.Parse(stampLayout, s)
	return t, ok && err == nil && t.Year() >= 1970 && t.Format(stampLayout) == s
}

// Verify checks one completed post using the pinned parser, content-id,
// preimage and signature functions. expectedKey is independently selected host
// policy (or the sender's checked identity), never an envelope claim. Exactly
// one author signature is admitted and verified once, without double hashing.
// The original envelope bytes are only read, never canonicalized for carriage.
// No purpose-5 issuer, qualification, revocation or replay claim is made here.
func Verify(raw []byte, attestation, expectedKey string, d Draft, now time.Time) error {
	v, err := record.Parse(raw, 65536, 16)
	if err != nil {
		return InvalidEnvelope
	}
	e, ok := v.(map[string]any)
	if !ok || !exactMembers(e, "version", "body", "id", "signatures") || e["version"] != EnvelopeVersion {
		return InvalidEnvelope
	}
	body, ok := e["body"].(map[string]any)
	if !ok || !exactMembers(body, BodyMembers...) || body["type"] != MessageType {
		return InvalidEnvelope
	}
	aud, _ := body["aud"].(string)
	nonce, nonceOK := body["nonce"].(string)
	_, nonceErr := record.DecodeBase64(nonce, 16)
	attID, attOK := body["author_attestation"].(string)
	if !printable(aud) || !nonceOK || nonceErr != nil || !attOK || !record.IsContentID(attID) {
		return InvalidEnvelope
	}
	issued, iatOK := postStamp(body["iat"])
	expires, expOK := postStamp(body["exp"])
	if !iatOK || !expOK || !issued.Before(expires) || expires.Sub(issued) > MaxLifetime {
		return InvalidLifetime
	}
	if !now.Before(expires) {
		return Expired
	}
	if now.Before(issued) {
		return InvalidLifetime
	}
	if body["author"] != expectedKey || len(expectedKey) < len(KeyPrefix) || expectedKey[:len(KeyPrefix)] != KeyPrefix || record.ValidateKey(expectedKey) != nil {
		return AuthorMismatch
	}
	id, err := record.ContentID(MessageType, body)
	if err != nil || e["id"] != id {
		return InvalidEnvelope
	}
	if body["room"] != d.Room {
		return RoomMismatch
	}
	to, recipients, err := Recipients(d.Room, d.Recipients)
	if err != nil || body["to"] != to {
		return RecipientsMismatch
	}
	digest := sha256.Sum256(d.Text)
	if body["body_sha256"] != hex.EncodeToString(digest[:]) {
		return TextMismatch
	}
	target, _ := body["target"].(map[string]any)
	arguments, _ := body["arguments"].(map[string]any)
	if !exactMembers(target, "kind", "id") || target["kind"] != TargetKind || target["id"] != TargetID || body["action"] != Action || !exactMembers(arguments, ArgumentMembers...) || arguments["kind"] != d.Kind || arguments["topic"] != d.Topic || arguments["re"] != d.Re {
		return MetadataMismatch
	}
	list, ok := arguments["to"].([]any)
	if !ok || len(list) != len(recipients) {
		return RecipientsMismatch
	}
	for i, value := range list {
		if value != recipients[i] {
			return RecipientsMismatch
		}
	}
	if attestation == "" {
		return MissingAttestation
	}
	if len(attestation) > base64.RawURLEncoding.EncodedLen(MaxAttestationBytes) {
		return LargeAttestation
	}
	att, err := DecodeAttestation(attestation)
	if err != nil {
		return InvalidAttestation
	}
	got, err := AttestationID(att)
	if err != nil {
		return InvalidAttestation
	}
	if got != attID {
		return AttestationMismatch
	}
	sigs, ok := e["signatures"].([]any)
	if !ok || len(sigs) != 1 {
		return InvalidEnvelope
	}
	sig, _ := sigs[0].(map[string]any)
	if !exactMembers(sig, "role", "key", "sig") || sig["role"] != SignatureRole {
		return InvalidEnvelope
	}
	if sig["key"] != expectedKey {
		return AuthorMismatch
	}
	encoded, ok := sig["sig"].(string)
	if !ok {
		return InvalidSignature
	}
	signature, err := record.DecodeBase64(encoded, 64)
	if err != nil {
		return InvalidSignature
	}
	preimage, err := record.SigningPreimage(MessageType, expectedKey, SignatureRole, body)
	if err != nil {
		return InvalidEnvelope
	}
	if _, err := record.AdmitSigner(preimage, record.IdentityPolicy{Check: true, CallerKey: expectedKey, CallerAttestationID: attID}); err != nil {
		return InvalidEnvelope
	}
	if record.VerifySignature(expectedKey, signature, preimage) != nil {
		return InvalidSignature
	}
	return nil
}
