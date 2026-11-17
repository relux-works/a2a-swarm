package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
)

// Signed posts, send side. `a2a post --sign` asks the one signer seam
// (signedpost.Signer) and publishes only what the sender verified itself. This
// version links no signer implementation and has no purpose-5 pin, so in
// production every signed post is refused before any event is sent. A refusal
// or a send error never becomes an unsigned post.

// signedPostSelection refuses unsupported shapes and a missing signer before
// any file or network access.
func signedPostSelection(a *App, envelope, voice bool, positionals int) error {
	switch {
	case envelope:
		return usagef("--sign cannot be combined with --envelope")
	case voice:
		return usagef("--sign cannot be combined with --voice")
	case positionals != 2:
		return usagef("--sign needs exactly one text argument after the room (quote it: the text is signed byte for byte)")
	case a.postSigner == nil:
		return signedpost.Refuse(signedpost.RefuseSignerUnconfigured, "")
	}
	return nil
}

// sendSignedPost sends the same event an unsigned post would send, plus the
// completed envelope under the fixed content key. The envelope signs the hash
// of the exact event body, the room, the recipients (the mentioned full ids)
// and the header's kind, topic and re. The recipient rule runs before any
// signer call; the envelope is assembled once and a repeated send before its
// expiry carries identical bytes under the same transaction id. The raw
// attestation travels beside the envelope, bound by its signed content id.
func (s *session) sendSignedPost(ctx context.Context, roomID, label string, h Header, text string, mentions []string) (string, error) {
	content, err := framedTextContent(formatHeaderBody(h, text), threadReply(h, mentions), formatHeaderBody(h, ""), text, "")
	if err != nil {
		return "", err
	}
	body, ok := content["body"].(string)
	if !ok {
		return "", errors.New("signed post: event body unavailable")
	}
	a := s.app
	sender := signedpost.Sender{Signer: a.postSigner, Audience: a.postAudience, Qualified: a.postQualified, Now: a.Now}
	sender.CheckSize = func(envelopeBytes, attestationBytes int) error {
		// Both carried strings use the base64url alphabet, which JSON never
		// escapes. Equal-length stand-ins therefore measure the exact event.
		content[matrix.EnvelopeContentKey] = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(envelopeBytes))
		content[signedpost.AttestationContentKey] = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(attestationBytes))
		return matrix.CheckEventContentBudget(content)
	}
	draft := signedpost.Draft{Room: roomID, Recipients: mentions, Kind: h.Kind, Topic: h.Topic, Re: h.Re, Text: []byte(body)}
	var id, txn string
	var sent []byte
	err = sender.Publish(ctx, draft, func(signed signedpost.Signed) error {
		encoded, err := matrix.EncodeEnvelope(signed.Envelope)
		if err != nil {
			return signedpost.Final(err)
		}
		attestation, err := signedpost.EncodeAttestation(signed.Attestation)
		if err != nil {
			return signedpost.Final(err)
		}
		content[matrix.EnvelopeContentKey] = encoded
		content[signedpost.AttestationContentKey] = attestation
		if err := matrix.CheckEventContentBudget(content); err != nil {
			return signedpost.Final(err)
		}
		// One transaction id per distinct envelope: the same bytes repeat it.
		if txn == "" || !bytes.Equal(sent, signed.Envelope) {
			txn, sent = matrix.NewTxnID(), signed.Envelope
		}
		receipt, err := s.client.SendMessageTxnReceipt(ctx, roomID, txn, content)
		if err != nil {
			if isAdmissionRefusal(err) || errors.Is(err, matrix.ErrAuth) {
				return signedpost.Final(err)
			}
			return fmt.Errorf("post to %s: %w", label, err)
		}
		id = receipt.EventID()
		return nil
	})
	if err != nil {
		return "", err
	}
	if h.Topic != "" {
		if err := s.rememberThread(h.Topic); err != nil {
			fmt.Fprintf(s.app.Stderr, "a2a: warning: could not record thread in state: %v\n", err)
		}
	}
	return id, nil
}
