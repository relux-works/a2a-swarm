// Package envelopeobserve labels received messages without admitting or refusing delivery.
package envelopeobserve

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
	"github.com/relux-works/swarma-envelope/verifier"
)

const Unsigned = "unsigned (no envelope)"
const UnknownAuthor = "unknown-author"

// Observer contains a snapshot supplied by the host, never by the event.
// A verdict is observation only. No returned verifier plan is persisted or used.
type Observer struct {
	authors map[string]string // exact Matrix sender -> exact author key
	context verifier.Context
}

func invalid(reason string) string { return "invalid: " + reason }
func errorCode(err error) string {
	var e *record.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "envelope_malformed"
}

// Observe checks the original content and body before any display sanitization.
// It performs no network, signing, mutation, receipts or authority decisions.
// A nil observer explicitly represents an unavailable host roster.
func (o *Observer) Observe(ev matrix.Event, room, recipient string, now time.Time) string {
	// First inspect presence; unsigned ordinary content keeps its existing semantics.
	// Strict parsing below prevents duplicate/alias/invalid-UTF8 content from verifying.
	var presence map[string]json.RawMessage
	if json.Unmarshal(ev.Content, &presence) != nil {
		return invalid("content_malformed")
	}
	if _, present := presence[matrix.EnvelopeContentKey]; !present {
		return Unsigned
	}
	if len(ev.Content) > matrix.EnvelopeEventContentBudget {
		return invalid("content_too_large")
	}
	content, err := matrix.ParseStrictJSON(ev.Content)
	if err != nil {
		return invalid("content_malformed")
	}
	m, ok := content.(map[string]any)
	if !ok {
		return invalid("content_malformed")
	}
	value := m[matrix.EnvelopeContentKey]
	encoded, ok := value.(string)
	if !ok {
		return invalid("envelope_malformed")
	}
	raw, err := matrix.DecodeEnvelope(encoded)
	if err != nil {
		return invalid(matrix.RefusalCode(err))
	}
	parsed, err := record.Parse(raw, matrix.EnvelopeMaxBytes, 16)
	if err != nil {
		return invalid(errorCode(err))
	}
	envelope, ok := parsed.(map[string]any)
	if !ok {
		return invalid("envelope_malformed")
	}
	body, ok := envelope["body"].(map[string]any)
	if !ok {
		return invalid("envelope_malformed")
	}
	author, ok := body["author"].(string)
	if !ok || author == "" {
		return invalid("envelope_malformed")
	}
	if o == nil {
		return UnknownAuthor
	}
	expected, known := o.authors[ev.Sender]
	if !known {
		return UnknownAuthor
	}
	if expected != author {
		return invalid("author_mismatch")
	}
	text, ok := m["body"].(string)
	if !ok {
		return invalid("message_text_unavailable")
	}
	// Only exact spellings are interpreted; aliases cannot disagree with display decoding.
	for key := range m {
		if key != "body" && strings.EqualFold(key, "body") || key != matrix.EnvelopeContentKey && strings.EqualFold(key, matrix.EnvelopeContentKey) || key != signedpost.AttestationContentKey && strings.EqualFold(key, signedpost.AttestationContentKey) {
			return invalid("content_key_alias")
		}
	}
	if ev.IsRedacted() {
		return invalid("event_redacted")
	}
	c := o.context
	c.Now, err = verifier.Present(now.UTC().Format("2006-01-02T15:04:05Z"))
	if err != nil {
		return invalid("time_unavailable")
	}
	verify := func(to string) string {
		delivery, err := verifier.Present(map[string]any{"room": room, "to": to, "text": []byte(text)})
		if err != nil {
			return "message_context_unavailable"
		}
		c.Delivery = delivery
		return verifier.New(c).VerifyMessage(raw).Code
	}
	// First as a message addressed to this receiver alone.
	code := verify(recipient)
	post := body["action"] == signedpost.Action
	viaRoom := false
	if post && code == "message_recipient_mismatch" {
		// The one fallback: a signed post addressed to the selected room itself.
		// Its recipients are the signed list, checked below. Nothing else falls back.
		if to, _ := body["to"].(string); strings.HasPrefix(to, "!") && to == room {
			code, viaRoom = verify(to), true
		}
	}
	if code != "accept" {
		return invalid(code)
	}
	// Carried evidence binds to the signed content id, never to an event-supplied issuer.
	// An absent member leaves the host roster's decision unchanged.
	if value, present := m[signedpost.AttestationContentKey]; present {
		encoded, ok := value.(string)
		if !ok {
			return invalid("attestation_mismatch")
		}
		attestation, err := signedpost.DecodeAttestation(encoded)
		if err != nil {
			return invalid("attestation_mismatch")
		}
		id, err := signedpost.AttestationID(attestation)
		if err != nil || id != body["author_attestation"] {
			return invalid("attestation_mismatch")
		}
	}
	if post {
		list, ok := postMetadata(m, body)
		if !ok {
			return invalid("metadata_mismatch")
		}
		if viaRoom && len(list) > 0 && !listed(list, recipient) {
			return invalid("recipient_not_addressed")
		}
	}
	return "verified: " + ev.Sender
}

// headerContentKey and mentionsContentKey are the event-content members that
// carry the post header and the full recipient ids.
const (
	headerContentKey   = "ai.relux.a2a"
	mentionsContentKey = "m.mentions"
)

func listed(list []string, id string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

// postMetadata compares the signed metadata of a verified signed post with
// what the event itself carries and returns the signed recipient list. The
// signed arguments must be exactly {kind, topic, re, to}; kind, topic and re
// must equal the event header's values (an absent header member is the empty
// text); `to` must equal the event's mentioned user ids in order; the signed
// target is the one fixed value; a single-recipient post lists exactly its
// `to`. Case variants of the two member names are a mismatch, never a second
// reading. It runs only after the signature verified.
func postMetadata(content, body map[string]any) ([]string, bool) {
	target, _ := body["target"].(map[string]any)
	if len(target) != 2 || target["kind"] != signedpost.TargetKind || target["id"] != signedpost.TargetID {
		return nil, false
	}
	arguments, ok := body["arguments"].(map[string]any)
	if !ok || len(arguments) != len(signedpost.ArgumentMembers) {
		return nil, false
	}
	kind, kindOK := arguments["kind"].(string)
	topic, topicOK := arguments["topic"].(string)
	re, reOK := arguments["re"].(string)
	raw, toOK := arguments["to"].([]any)
	if !kindOK || !topicOK || !reOK || !toOK || len(raw) > signedpost.MaxRecipients {
		return nil, false
	}
	list := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" || listed(list, s) {
			return nil, false
		}
		list = append(list, s)
	}
	if to, _ := body["to"].(string); strings.HasPrefix(to, "!") {
		if len(list) == 1 {
			return nil, false
		}
	} else if len(list) != 1 || list[0] != to {
		return nil, false
	}
	for key := range content {
		if key != headerContentKey && strings.EqualFold(key, headerContentKey) || key != mentionsContentKey && strings.EqualFold(key, mentionsContentKey) {
			return nil, false
		}
	}
	header, ok := content[headerContentKey].(map[string]any)
	if !ok {
		return nil, false
	}
	for name, signed := range map[string]string{"kind": kind, "topic": topic, "re": re} {
		carried, present := header[name]
		if !present {
			carried = ""
		}
		if carried != signed {
			return nil, false
		}
	}
	var mentioned []any
	if v, present := content[mentionsContentKey]; present {
		mentions, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if ids, present := mentions["user_ids"]; present {
			if mentioned, ok = ids.([]any); !ok {
				return nil, false
			}
		}
	}
	if len(mentioned) != len(list) {
		return nil, false
	}
	for i, v := range mentioned {
		if v != list[i] {
			return nil, false
		}
	}
	return list, true
}
