package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// VerificationInput is the versioned observation boundary for strict consumers.
// Declarations inside ContentBase64 do not become identity or grant authority.
type VerificationInput struct {
	Type                   string `json:"type"`
	ObservationStatus      string `json:"observation_status"`
	RequestedRoomID        string `json:"requested_room_id"`
	RequestedEventID       string `json:"requested_event_id"`
	ReturnedEventID        string `json:"returned_event_id"`
	SenderMXID             string `json:"sender_mxid"`
	ReceiverMXID           string `json:"receiver_mxid"`
	ReceiverIdentitySource string `json:"receiver_identity_source"`
	EventType              string `json:"event_type"`
	ReturnedRoomIDPresent  bool   `json:"returned_room_id_present"`
	ReturnedRoomID         string `json:"returned_room_id,omitempty"`
	RoomContextSource      string `json:"room_context_source"`
	ContentBase64          string `json:"content_base64"`
}

// Global selectors are removed before command parsing. Refuse repetitions
// only for the new opt-in mode, preserving ordinary commands' compatibility.
func verificationGlobalSelection(args []string) error {
	seen := map[string]int{}
	mode := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "verification-input":
			mode = true
		case "as", "profile", "connection-fingerprint":
			seen[name]++
		}
	}
	if mode {
		for _, count := range seen {
			if count > 1 {
				return usagef("--verification-input refuses repeated identity or connection selectors")
			}
		}
	}
	return nil
}

func verificationSelection(args []string, room, event string, asJSON bool) error {
	if !asJSON || !matrix.ValidVerificationID(room, '!') || !matrix.ValidVerificationID(event, '$') {
		return usagef("--verification-input requires one explicit room ID, --event ID and --json")
	}
	seen := map[string]bool{}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "topic", "last":
			return usagef("--verification-input cannot be combined with --topic or --last")
		case "event", "json", "verification-input":
			if seen[name] {
				return usagef("--verification-input refuses repeated selection or mode flags")
			}
			seen[name] = true
		}
	}
	return nil
}

func readVerificationInput(ctx context.Context, a *App, room, event string) error {
	s, err := a.session()
	if err != nil {
		return err
	}
	if !matrix.ValidVerificationID(s.creds.UserID, '@') {
		return usagef("invalid selected verification receiver")
	}
	// Refuse a known original before any server response can override local
	// knowledge or choose a different refusal diagnostic.
	if s.client.FilterEvents != nil {
		events, filterErr := s.client.FilterEvents(room, []matrix.Event{{EventID: event}})
		if filterErr != nil {
			return filterErr
		}
		if len(events) != 1 || events[0].IsRedacted() {
			return errors.New("read: VERIFICATION_REDACTED")
		}
	}
	v, err := s.client.GetVerificationEvent(ctx, room, event)
	if err != nil {
		return err
	}
	// Consult local tombstones (including explicit edit targets) without
	// changing the strict observation's original raw content bytes.
	if s.client.FilterEvents != nil {
		filtered, filterErr := s.client.FilterEvents(room, []matrix.Event{{EventID: v.EventID, Sender: v.Sender, Type: v.Type, Content: v.Content}})
		if filterErr != nil {
			return filterErr
		}
		if len(filtered) != 1 || filtered[0].IsRedacted() {
			return errors.New("read: VERIFICATION_REDACTED")
		}
	}
	result := VerificationInput{
		Type: "a2a.event-verification-input/1", ObservationStatus: "observed", RequestedRoomID: room, RequestedEventID: event,
		ReturnedEventID: v.EventID, SenderMXID: v.Sender, ReceiverMXID: s.creds.UserID, ReceiverIdentitySource: "selected-stored-session",
		EventType: v.Type, ReturnedRoomIDPresent: v.RoomIDPresent, ReturnedRoomID: v.RoomID, RoomContextSource: "authenticated-room-scoped-endpoint",
		ContentBase64: base64.RawURLEncoding.EncodeToString(v.Content),
	}
	if v.RoomIDPresent {
		result.RoomContextSource = "returned-room-and-original-endpoint"
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err = s.checkConnection(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Unlike the display printer, propagate writer failure and short writes.
	// A consumer must also require successful exit and one complete record.
	data = append(data, '\n')
	n, err := a.Stdout.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
