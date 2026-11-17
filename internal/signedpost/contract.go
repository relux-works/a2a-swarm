// Package signedpost is the sender side of a signed post: the contract rows,
// the one signer seam, the checks that run before anything is signed, and the
// build, sign, verify and assemble steps on the pinned envelope module.
//
// This package dials nothing. The CLI configures a native signer only when
// message_author is present; absent configuration means no signer is configured.
// No production purpose-5 pin exists, so every signed post is refused before
// publication; see docs/signed-post-contract.md and the "Signed posts" section
// of docs/cli.md for what is and is not implemented.
package signedpost

import "time"

// Contract rows of the signed object (docs/signed-post-contract.md, section 2).
// Each row is stated once, here; no other file repeats a literal.
const (
	// KeyPrefix is the only admitted author key type: K = KeyPrefix + base64url(x-only 32).
	KeyPrefix = "bip340:"
	// ContentIDPrefix starts T, the content id of the attestation.
	ContentIDPrefix = "sha256:"
	// EnvelopeVersion and SignatureRole are the `version` and `role` members of the record.
	EnvelopeVersion = "swarm-envelope/2"
	SignatureRole   = "author"
	// DomainTag is followed by one LF and the canonical record in the preimage.
	DomainTag = "swarma-msg/2"
	// MaxPreimageBytes bounds the preimage.
	MaxPreimageBytes = 24576
	// MessageType and Action are the `type` and `action` body members.
	MessageType = "message/1"
	Action      = "a2a.post/1"
	// TargetKind and TargetID are the fixed `target` body member. The pinned
	// module requires a well-formed target in every message/1 body; the contract
	// names none, so a post always carries this one constant value.
	TargetKind = "station"
	TargetID   = "a2a/post"
	// AttestationType is the record type of the attestation A.
	AttestationType = "attestation/1"
	// AttestationContentKey carries A beside the envelope, outside the signed bytes.
	AttestationContentKey = "works.relux.swarm.author_attestation"
	// MaxLifetime bounds exp - iat.
	MaxLifetime = 120 * time.Second
	// MaxRecipients and MaxRecipientBytes bound the signed recipient list.
	MaxRecipients     = 64
	MaxRecipientBytes = 255
	// MaxAttestationBytes bounds the raw attestation A the signer returns.
	MaxAttestationBytes = 8192
	// MaxSignAttempts bounds one signing request: the first try and at most two
	// retries of the same preimage after a retryable signer failure.
	MaxSignAttempts = 3
	// MaxPublishAttempts bounds the sends of one post: the first and one retry.
	MaxPublishAttempts = 2
)

// BodyMembers is the closed member set of a `message/1` body.
var BodyMembers = []string{"type", "aud", "nonce", "iat", "exp", "room", "to", "body_sha256", "author", "author_attestation", "action", "target", "arguments"}

// ArgumentMembers is the closed member set of the signed `arguments`.
var ArgumentMembers = []string{"kind", "topic", "re", "to"}

// The sender's closed exit vocabulary for a signed post (contract section 4).
const (
	RefuseSignerUnconfigured  = "signer_unconfigured"
	RefuseSignerUnavailable   = "signer_unavailable"
	RefuseSignerUnqualified   = "signer_unqualified"
	RefuseSignerRefused       = "signer_refused"
	RefuseSignerResultInvalid = "signer_result_invalid"
	RefuseRecipientsTooMany   = "recipients_too_many"
)

// Fixed local details. A detail is never received text, a key or attestation bytes.
const (
	DetailRecipientCount       = "more than 64 recipients"
	DetailRecipientDuplicate   = "duplicate recipient"
	DetailRecipientLength      = "recipient over 255 bytes"
	DetailRecipientForm        = "recipient is not a printable ASCII id"
	DetailRecipientMXID        = "recipient is not a full MXID"
	DetailPreimage             = "signing input over 24576 bytes"
	DetailAudience             = "audience not configured"
	DetailKeyType              = "author key type not admitted"
	DetailAttestationID        = "attestation id malformed"
	DetailAttestationEmpty     = "attestation empty"
	DetailAttestationLarge     = "attestation over 8192 bytes"
	DetailAttestationMalformed = "attestation malformed"
	DetailAttestationMismatch  = "attestation content id differs from the attestation id"
	DetailNoPin                = "purpose-5 pin not configured"
	DetailNonce                = "no randomness for the nonce"
	DetailKeyDiffers           = "returned key differs from the identity key"
	DetailSignature            = "signature does not verify"
)
