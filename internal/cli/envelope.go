package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Envelope carriage, send side. `a2a post --envelope @file` places the exact
// file bytes in the event under matrix.EnvelopeContentKey (unpadded base64url)
// beside a human-readable text body. The command only carries: it never parses,
// canonicalizes or re-serializes the envelope, and it derives no authority from
// the sender, the display name or the envelope's own claims.

// envelopePostSelection refuses ambiguous or unsupported post shapes before any
// file or network access. flag.Parse alone would let the last of several
// --envelope values win silently.
func envelopePostSelection(args []string, operand string, voice bool, positionals int) error {
	count := 0
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "="); strings.HasPrefix(arg, "-") && name == "envelope" {
			count++
		}
	}
	switch {
	case count != 1:
		return usagef("--envelope may be given exactly once")
	case !strings.HasPrefix(operand, "@") || len(operand) < 2:
		return usagef("--envelope takes a file operand: --envelope @file")
	case voice:
		return usagef("--envelope cannot be combined with --voice")
	case positionals != 2:
		return usagef("--envelope needs exactly one text argument after the room (quote it: the text is carried byte for byte)")
	}
	return nil
}

// readEnvelopeFile returns the event-content value for the exact bytes of the
// @file operand. An empty or oversized file is refused, never truncated; the
// read itself is bounded so a huge file is not loaded.
func readEnvelopeFile(operand string) (string, error) {
	f, err := os.Open(operand[1:])
	if err != nil {
		return "", matrix.Refuse(matrix.RefuseEnvelopeUnreadable, "")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, matrix.EnvelopeMaxBytes+1))
	if err != nil {
		return "", matrix.Refuse(matrix.RefuseEnvelopeUnreadable, "")
	}
	return matrix.EncodeEnvelope(data)
}

// readEnvelopeText resolves the human-readable text (literal, @file or -). The
// consumer hashes the exact event text, so unlike an ordinary post body nothing
// is trimmed, normalized or sanitized and no protocol header lines are added:
// the event body is exactly these bytes. Text that the ordinary body path would
// alter (invalid UTF-8, CR, control or bidi characters) is refused, not repaired.
func (a *App) readEnvelopeText(arg string) (string, error) {
	data, err := a.readBodyBytes(arg)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", matrix.Refuse(matrix.RefuseTextInvalid, "text is not valid UTF-8")
	}
	text := string(data)
	if strings.TrimSpace(text) == "" {
		return "", matrix.Refuse(matrix.RefuseTextEmpty, "")
	}
	if sanitizeText(text) != text {
		return "", matrix.Refuse(matrix.RefuseTextInvalid, "text contains CR, control or bidi characters")
	}
	return text, nil
}

// sendThreadEnvelope sends the thread reply with the exact text as its body and
// the encoded envelope under the fixed content key. The event content is checked
// against the published byte budget before anything is sent.
func (s *session) sendThreadEnvelope(ctx context.Context, roomID, label string, h Header, text, encoded string, mentions []string) (string, error) {
	content := threadReply(h, mentions)
	content["msgtype"] = "m.text"
	content["body"] = text
	content[matrix.EnvelopeContentKey] = encoded
	if err := matrix.CheckEventContentBudget(content); err != nil {
		return "", err
	}
	return s.sendThreadContent(ctx, roomID, label, h, content)
}

// EnvelopeInput is the versioned projection record for strict consumers. It
// reports observations only: nothing in it is an identity or authority verdict.
type EnvelopeInput struct {
	Type             string `json:"type"`
	SenderMXID       string `json:"sender_mxid"`
	ReceiverMXID     string `json:"receiver_mxid"`
	RequestedRoomID  string `json:"requested_room_id"`
	RequestedEventID string `json:"requested_event_id"`
	ReturnedRoomID   string `json:"returned_room_id"`
	ReturnedEventID  string `json:"returned_event_id"`
	EnvelopeBase64   string `json:"envelope_base64"`
	BodyTextBase64   string `json:"body_text_base64"`
}

// envelopeSelection requires one explicit room ID, event ID and --json, and
// refuses topic/last selection, repeated selection or mode flags, and the other
// strict mode, before any request.
func envelopeSelection(args []string, room, event string, asJSON bool) error {
	if !asJSON || !matrix.ValidVerificationID(room, '!') || !matrix.ValidVerificationID(event, '$') {
		return usagef("--envelope-input requires one explicit room ID, --event ID and --json")
	}
	seen := map[string]bool{}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "topic", "last", "verification-input":
			return usagef("--envelope-input cannot be combined with --topic, --last or --verification-input")
		case "event", "json", "envelope-input":
			if seen[name] {
				return usagef("--envelope-input refuses repeated selection or mode flags")
			}
			seen[name] = true
		}
	}
	return nil
}

func readEnvelopeInput(ctx context.Context, a *App, room, event string) error {
	s, err := a.session()
	if err != nil {
		return err
	}
	if !matrix.ValidVerificationID(s.creds.UserID, '@') {
		return usagef("invalid selected envelope receiver")
	}
	// Refuse a known redacted original before any server response, as
	// --verification-input does.
	if s.client.FilterEvents != nil {
		events, filterErr := s.client.FilterEvents(room, []matrix.Event{{EventID: event}})
		if filterErr != nil {
			return filterErr
		}
		if len(events) != 1 || events[0].IsRedacted() {
			return errors.New("read: ENVELOPE_REDACTED")
		}
	}
	v, err := s.client.GetEnvelopeEvent(ctx, room, event)
	if err != nil {
		return err
	}
	// Consult local tombstones again with the observed event, so an edit whose
	// explicit target is known redacted is refused too.
	if s.client.FilterEvents != nil {
		filtered, filterErr := s.client.FilterEvents(room, []matrix.Event{{EventID: v.EventID, Sender: v.Sender, Type: "m.room.message", Content: v.Content}})
		if filterErr != nil {
			return filterErr
		}
		if len(filtered) != 1 || filtered[0].IsRedacted() {
			return errors.New("read: ENVELOPE_REDACTED")
		}
	}
	data, err := json.Marshal(EnvelopeInput{
		Type: "a2a.event-envelope-input/1", SenderMXID: v.Sender, ReceiverMXID: s.creds.UserID,
		RequestedRoomID: room, RequestedEventID: event, ReturnedRoomID: v.RoomID, ReturnedEventID: v.EventID,
		EnvelopeBase64: base64.RawURLEncoding.EncodeToString(v.Envelope),
		BodyTextBase64: base64.RawURLEncoding.EncodeToString(v.BodyText),
	})
	if err != nil {
		return err
	}
	if err = s.checkConnection(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Propagate writer failure and short writes. A consumer must also require a
	// successful exit and one complete record.
	data = append(data, '\n')
	n, err := a.Stdout.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
