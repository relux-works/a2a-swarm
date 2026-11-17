package signedpost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/relux-works/swarma-envelope/record"
)

// Identity is what the holder reports about the author key.
type Identity struct {
	Key           string // K: the author key id
	AttestationID string // T: the content id of the attestation
	Attestation   []byte // A: the raw attestation bytes
}

// Signer is the one seam behind which a key-holder client plugs in. The sender
// never holds, reads or derives a key. Sign takes only the exact preimage: the
// request id is derived inside the implementation from that preimage (see
// RequestID), so a caller cannot express a different preimage under the same
// id. The returned key is the key id text the holder signed with, as bytes;
// the signature is the raw 64 bytes.
type Signer interface {
	Identity(ctx context.Context) (Identity, error)
	Sign(ctx context.Context, preimage []byte) (key, signature []byte, err error)
}

// Errors a Signer returns. Anything else is treated as an invalid result.
var (
	ErrUnconfigured = errors.New("signer not configured")
	ErrUnavailable  = errors.New("signer unavailable") // retryable
	ErrUnqualified  = errors.New("signer unqualified")
)

// RefusedError is a holder refusal that must not be retried. HolderCode is the
// holder's code name; anything that is not a plain code name is dropped.
type RefusedError struct{ HolderCode string }

func (e *RefusedError) Error() string { return "signer refused" }

// Refusal is a deliberate refusal of a signed post. Nothing was published.
type Refusal struct {
	Code      string
	Detail    string
	Retryable bool
}

func (r *Refusal) Error() string {
	if r.Detail != "" {
		return "signed post refused: " + r.Code + " (" + r.Detail + ")"
	}
	return "signed post refused: " + r.Code
}

// Refuse builds a Refusal.
func Refuse(code, detail string) error { return &Refusal{Code: code, Detail: detail} }

// Code returns the code of a signed-post refusal, or "".
func Code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func codeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// classify maps a Signer error to the sender's closed vocabulary.
func classify(err error) error {
	var refused *RefusedError
	switch {
	case errors.Is(err, ErrUnconfigured):
		return Refuse(RefuseSignerUnconfigured, "")
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &Refusal{Code: RefuseSignerUnavailable, Retryable: true}
	case errors.Is(err, ErrUnqualified):
		return Refuse(RefuseSignerUnqualified, "")
	case errors.As(err, &refused):
		detail := ""
		if codeName(refused.HolderCode) {
			detail = refused.HolderCode
		}
		return Refuse(RefuseSignerRefused, detail)
	}
	return Refuse(RefuseSignerResultInvalid, "")
}

// printable reports the envelope's carrier-id form: 1..255 bytes of ASCII 0x21-0x7e.
func printable(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Recipients applies the `to` row to the ordered full recipient ids of a post.
// Exactly one recipient is the signed `to`; zero or several put the room id
// there and the ordered list goes into the signed arguments. A list over the
// cap, a duplicate, an over-long id and an id the envelope cannot carry are
// refused, never truncated, split or rewritten.
func Recipients(room string, recipients []string) (to string, list []string, err error) {
	if len(recipients) > MaxRecipients {
		return "", nil, Refuse(RefuseRecipientsTooMany, DetailRecipientCount)
	}
	seen := make(map[string]bool, len(recipients))
	for _, r := range recipients {
		if len(r) > MaxRecipientBytes {
			return "", nil, Refuse(RefuseRecipientsTooMany, DetailRecipientLength)
		}
		if !printable(r) {
			return "", nil, Refuse(RefuseRecipientsTooMany, DetailRecipientForm)
		}
		local, server, full := strings.Cut(r, ":")
		if !full || !strings.HasPrefix(local, "@") || len(local) < 2 || server == "" {
			return "", nil, Refuse(RefuseRecipientsTooMany, DetailRecipientMXID)
		}
		if seen[r] {
			return "", nil, Refuse(RefuseRecipientsTooMany, DetailRecipientDuplicate)
		}
		seen[r] = true
	}
	list = append([]string{}, recipients...)
	if len(list) == 1 {
		return list[0], list, nil
	}
	return room, list, nil
}

// CheckIdentityShape makes the identity checks that need no parsing of the
// attestation: K is well formed and of the admitted type, T is a well-formed
// content id, A is present and bounded.
func CheckIdentityShape(id Identity) error {
	switch {
	case !strings.HasPrefix(id.Key, KeyPrefix) || record.ValidateKey(id.Key) != nil:
		return Refuse(RefuseSignerUnqualified, DetailKeyType)
	case !record.IsContentID(id.AttestationID):
		return Refuse(RefuseSignerUnqualified, DetailAttestationID)
	case len(id.Attestation) == 0:
		return Refuse(RefuseSignerUnqualified, DetailAttestationEmpty)
	case len(id.Attestation) > MaxAttestationBytes:
		return Refuse(RefuseSignerUnqualified, DetailAttestationLarge)
	}
	return nil
}

var errAttestation = errors.New("attestation malformed")

// AttestationID returns the content id of the raw attestation record A, with
// the pinned module's content-id function: the id of its `attestation/1` body,
// which is the value a verifier compares with `author_attestation`. A record
// that does not parse, has another type or states an `id` member other than
// that content id has no content id here. It verifies no signature and no
// issuer: that is the next unit, under the per-host purpose-5 pin.
func AttestationID(attestation []byte) (string, error) {
	parsed, err := record.Parse(attestation, MaxAttestationBytes, 16)
	if err != nil {
		return "", errAttestation
	}
	envelope, ok := parsed.(map[string]any)
	if !ok {
		return "", errAttestation
	}
	body, ok := envelope["body"].(map[string]any)
	if !ok || body["type"] != AttestationType {
		return "", errAttestation
	}
	id, err := record.ContentID(AttestationType, body)
	if err != nil || envelope["id"] != id {
		return "", errAttestation
	}
	return id, nil
}

// CheckAttestation requires that the content id of A equals T.
func CheckAttestation(id Identity) error {
	got, err := AttestationID(id.Attestation)
	if err != nil {
		return Refuse(RefuseSignerUnqualified, DetailAttestationMalformed)
	}
	if got != id.AttestationID {
		return Refuse(RefuseSignerUnqualified, DetailAttestationMismatch)
	}
	return nil
}

// EncodeAttestation returns the carried form of A: unpadded base64url of the
// raw bytes, carried outside the envelope in AttestationContentKey.
func EncodeAttestation(attestation []byte) (string, error) {
	switch {
	case len(attestation) == 0:
		return "", Refuse(RefuseSignerUnqualified, DetailAttestationEmpty)
	case len(attestation) > MaxAttestationBytes:
		return "", Refuse(RefuseSignerUnqualified, DetailAttestationLarge)
	}
	return base64.RawURLEncoding.EncodeToString(attestation), nil
}

// DecodeAttestation is the strict inverse of EncodeAttestation: canonical
// unpadded base64url of 1..8192 bytes. The encoded length is bounded first.
func DecodeAttestation(value string) ([]byte, error) {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(MaxAttestationBytes) {
		return nil, errAttestation
	}
	raw, err := record.DecodeBase64(value, -1)
	if err != nil || len(raw) == 0 || len(raw) > MaxAttestationBytes {
		return nil, errAttestation
	}
	return raw, nil
}

// requestIDLabel separates request ids from every other digest of a preimage.
const requestIDLabel = "a2a-signed-post-request/1\n"

// RequestID is the request id of a signing request, derived from the exact
// preimage and from nothing else. A Signer implementation derives its id with
// this function inside Sign: the same preimage always has the same id, and a
// different preimage cannot be sent under it. Mapping the value to the
// holder's wire format belongs to the key-holder client.
func RequestID(preimage []byte) string {
	h := sha256.New()
	h.Write([]byte(requestIDLabel))
	h.Write(preimage)
	return hex.EncodeToString(h.Sum(nil))
}

// Sender runs the sender's steps through the one Signer seam.
type Sender struct {
	Signer Signer
	// Audience is the installation's `aud` value. It is part of the signer
	// configuration; production configures none in this unit.
	Audience string
	// Qualified stands for the verification of the attestation under the
	// per-host purpose-5 pin, which is the next unit. No pin exists yet:
	// production leaves it nil and every signed post ends in
	// signer_unqualified. Only tests set it.
	Qualified func(Identity) bool
	// CheckSize, when set, checks the exact completed envelope and raw
	// attestation byte lengths before any signature is requested. The carrier
	// can refuse an event that would exceed its budget without wasting a
	// signature. It must still check the assembled event before publication.
	CheckSize func(envelopeBytes, attestationBytes int) error
	// Now and Rand default to the wall clock and the system random source.
	Now  func() time.Time
	Rand io.Reader
}

func (s Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Prepared is the checked input of the signing step.
type Prepared struct {
	Identity   Identity
	To         string   // the signed `to`
	Recipients []string // the ordered signed recipient list
}

// Prepare runs every step before the signing input is built. The recipient
// rule is applied before any signer call. It never calls Sign and publishes
// nothing; any error means the post must not be sent in any form.
func (s Sender) Prepare(ctx context.Context, room string, recipients []string) (Prepared, error) {
	if s.Signer == nil {
		return Prepared{}, Refuse(RefuseSignerUnconfigured, "")
	}
	if !printable(s.Audience) {
		return Prepared{}, Refuse(RefuseSignerUnconfigured, DetailAudience)
	}
	to, list, err := Recipients(room, recipients)
	if err != nil {
		return Prepared{}, err
	}
	id, err := s.Signer.Identity(ctx)
	if err != nil {
		return Prepared{}, classify(err)
	}
	// Own the returned bytes before any check or qualification callback uses them.
	id.Attestation = append([]byte(nil), id.Attestation...)
	if err := CheckIdentityShape(id); err != nil {
		return Prepared{}, err
	}
	if err := CheckAttestation(id); err != nil {
		return Prepared{}, err
	}
	if s.Qualified == nil || !s.Qualified(id) {
		return Prepared{}, Refuse(RefuseSignerUnqualified, DetailNoPin)
	}
	return Prepared{Identity: id, To: to, Recipients: list}, nil
}

// Draft is one post to sign. Text is the exact `content.body` bytes of the
// Matrix event; Kind, Topic and Re are the values its header carries;
// Recipients are the ordered full ids its mentions carry.
type Draft struct {
	Room       string
	Recipients []string
	Kind       string
	Topic      string
	Re         string
	Text       []byte
}

// Signed is a completed envelope the sender verified itself.
type Signed struct {
	Envelope    []byte    // the completed swarm-envelope/2 bytes, assembled once
	ID          string    // its content id
	Attestation []byte    // A, exactly as the signer returned it
	Expires     time.Time // `exp`: the same bytes may be published again before it
}

const stampLayout = "2006-01-02T15:04:05Z"

var errSigningInput = errors.New("signed post: the signing input cannot be built")

// Sign prepares, builds the signing input, asks for one signature, checks the
// result once and assembles the envelope once. A retryable signer failure is
// retried with the same preimage, at most MaxSignAttempts in total and only
// inside the message lifetime. Any error means nothing may be published.
func (s Sender) Sign(ctx context.Context, d Draft) (Signed, error) {
	p, err := s.Prepare(ctx, d.Room, d.Recipients)
	if err != nil {
		return Signed{}, err
	}
	source := s.Rand
	if source == nil {
		source = rand.Reader
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(source, nonce); err != nil {
		return Signed{}, &Refusal{Code: RefuseSignerUnavailable, Detail: DetailNonce, Retryable: true}
	}
	issued := s.now().UTC().Truncate(time.Second)
	expires := issued.Add(MaxLifetime)
	to := make([]any, len(p.Recipients))
	for i, r := range p.Recipients {
		to[i] = r
	}
	digest := sha256.Sum256(d.Text)
	body := map[string]any{
		"type":               MessageType,
		"aud":                s.Audience,
		"nonce":              base64.RawURLEncoding.EncodeToString(nonce),
		"iat":                issued.Format(stampLayout),
		"exp":                expires.Format(stampLayout),
		"room":               d.Room,
		"to":                 p.To,
		"body_sha256":        hex.EncodeToString(digest[:]),
		"author":             p.Identity.Key,
		"author_attestation": p.Identity.AttestationID,
		"action":             Action,
		"target":             map[string]any{"kind": TargetKind, "id": TargetID},
		"arguments":          map[string]any{"kind": d.Kind, "topic": d.Topic, "re": d.Re, "to": to},
	}
	preimage, err := record.SigningPreimage(MessageType, p.Identity.Key, SignatureRole, body)
	if err != nil {
		return Signed{}, errSigningInput
	}
	if len(preimage) > MaxPreimageBytes {
		return Signed{}, Refuse(RefuseRecipientsTooMany, DetailPreimage)
	}
	id, err := record.ContentID(MessageType, body)
	if err != nil {
		return Signed{}, errSigningInput
	}
	if s.CheckSize != nil {
		// A raw 64-byte signature always has the same unpadded base64url
		// length. Its alphabet needs no JSON escaping, just like this stand-in.
		sizing, err := encodeEnvelope(body, id, p.Identity.Key, strings.Repeat("A", base64.RawURLEncoding.EncodedLen(64)))
		if err != nil {
			return Signed{}, errSigningInput
		}
		if err := s.CheckSize(len(sizing), len(p.Identity.Attestation)); err != nil {
			return Signed{}, err
		}
	}
	var key, signature []byte
	for attempt := 1; ; attempt++ {
		// The signer gets its own copy: the check below is over the bytes built here.
		key, signature, err = s.Signer.Sign(ctx, append([]byte(nil), preimage...))
		if err == nil {
			break
		}
		refused := classify(err)
		var r *Refusal
		if errors.As(refused, &r) && r.Retryable && attempt < MaxSignAttempts && ctx.Err() == nil && s.now().Before(expires) {
			continue
		}
		return Signed{}, refused
	}
	if string(key) != p.Identity.Key {
		return Signed{}, Refuse(RefuseSignerResultInvalid, DetailKeyDiffers)
	}
	if len(signature) != 64 || record.VerifySignature(p.Identity.Key, signature, preimage) != nil {
		return Signed{}, Refuse(RefuseSignerResultInvalid, DetailSignature)
	}
	envelope, err := encodeEnvelope(body, id, p.Identity.Key, base64.RawURLEncoding.EncodeToString(signature))
	if err != nil {
		return Signed{}, errSigningInput
	}
	return Signed{Envelope: envelope, ID: id, Attestation: p.Identity.Attestation, Expires: expires}, nil
}

func encodeEnvelope(body map[string]any, id, key, signature string) ([]byte, error) {
	return record.Canonical(map[string]any{
		"version": EnvelopeVersion,
		"body":    body,
		"id":      id,
		"signatures": []any{map[string]any{
			"role": SignatureRole,
			"key":  key,
			"sig":  signature,
		}},
	})
}

type finalError struct{ err error }

func (e *finalError) Error() string { return e.err.Error() }
func (e *finalError) Unwrap() error { return e.err }

// Final marks a send error after which Publish must not send again.
func Final(err error) error { return &finalError{err: err} }

// Publish signs d and hands the completed envelope to send. When send fails,
// it sends again, at most MaxPublishAttempts in total: before `exp` the very
// same Signed value (identical bytes, no new signature), after `exp` a newly
// signed one with a new nonce, iat and exp. A refusal or a send error is
// returned as it is; nothing here ever sends an unsigned post.
func (s Sender) Publish(ctx context.Context, d Draft, send func(Signed) error) error {
	var signed Signed
	for attempt := 1; ; attempt++ {
		if attempt == 1 || !s.now().Before(signed.Expires) {
			var err error
			if signed, err = s.Sign(ctx, d); err != nil {
				return err
			}
		}
		err := send(signed)
		if err == nil {
			return nil
		}
		var final *finalError
		if errors.As(err, &final) {
			return final.err
		}
		if attempt >= MaxPublishAttempts || ctx.Err() != nil {
			return err
		}
	}
}
