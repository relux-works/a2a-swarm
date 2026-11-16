package e2ee

import (
	"encoding/json"

	"maunium.net/go/mautrix/id"
)

// RoomEventRequest is one timeline event with the authenticated local context
// the caller admits it under. Sessions holds the provenance record of every
// admitted inbound Megolm session. Replay (per session) and Events (this
// room's event-ID identity) are the replay state the caller persists; both
// change only on acceptance. Working is never mutated: decryption runs on a
// copy of the inbound session. Retained maps refusal IDs to their saved reason
// within the retention window; Retention supplies the per-room digest filter
// and retired-session tombstones.
type RoomEventRequest struct {
	Binding   string
	Raw       json.RawMessage
	Context   RoomContext
	Local     LocalDevice
	Peers     map[string]*PeerObservation
	Sessions  map[string]SessionRecord
	Working   *Working
	Replay    map[string]ReplayWindow
	Events    *EventReplay
	Retained  map[string]Code
	Retention *Retention
}

type roomWire struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	RoomID   string          `json:"room_id"`
	EventID  string          `json:"event_id"`
	StateKey *string         `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}
type megolmContent struct {
	Algorithm  string          `json:"algorithm"`
	SenderKey  string          `json:"sender_key"`
	DeviceID   string          `json:"device_id"`
	SessionID  string          `json:"session_id"`
	Ciphertext json.RawMessage `json:"ciphertext"`
}
type megolmPlaintext struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
	RoomID  string          `json:"room_id"`
}
type innerMessage struct {
	MsgType    string          `json:"msgtype"`
	Body       *string         `json:"body"`
	URL        json.RawMessage `json:"url"`
	File       json.RawMessage `json:"file"`
	Info       json.RawMessage `json:"info"`
	NewContent json.RawMessage `json:"m.new_content"`
	RelatesTo  *struct {
		RelType string `json:"rel_type"`
	} `json:"m.relates_to"`
}

// VerifyRoomEvent implements section 7 stages 0-8 in exact order; the first
// failed stage wins. It returns a populated AcceptedEvent, or a Refusal with
// the stage's reason, or a batch-level error (invalid request, incomplete key
// query, capacity) that leaves no verdict. Replay state changes only on
// acceptance. The outer sender MXID is accepted only when it equals the MXID
// the session record (hence the cross-signing chain) binds to the originator.
func VerifyRoomEvent(req RoomEventRequest) (*AcceptedEvent, *Refusal, error) {
	if !hexSize(req.Binding, 32) || !req.Local.valid() || req.Working == nil || req.Replay == nil || req.Events == nil || req.Sessions == nil {
		return nil, nil, ErrInvalid
	}
	var wire roomWire
	identity := func() (room, event, digest string) {
		if !roomID(wire.RoomID) || !eventID(wire.EventID) || len(wire.Content) == 0 {
			return "", "", rawDigest(req.Raw)
		}
		return wire.RoomID, wire.EventID, rawDigest(wire.Content)
	}
	refuse := func(stage string, reason Code, device string) (*AcceptedEvent, *Refusal, error) {
		room, event, digest := identity()
		return nil, &Refusal{Source: SourceRoom, Stage: stage, Reason: reason, ID: RoomRefusalID(room, event, digest), RoomID: room, Event: event, Sender: wire.Sender, Device: device}, nil
	}
	// Stage 0: wire shape. Duplicate members, wrong types and oversized or
	// invalid required fields are malformed; nothing claims a device yet.
	if err := lenientJSON(req.Raw, &wire, MaxWireEvent); err != nil {
		return refuse("0", ErrMalformedEvent, "")
	}
	if !text(wire.Type, 255) || !fullMXID(wire.Sender) || !roomID(wire.RoomID) || !eventID(wire.EventID) || !isObject(wire.Content) {
		return refuse("0", ErrMalformedEvent, "")
	}
	var content megolmContent
	if err := lenientJSON(wire.Content, &content, MaxWireEvent); err != nil {
		return refuse("0", ErrMalformedEvent, "")
	}
	room, event, digest := identity()
	refusalID := RoomRefusalID(room, event, digest)
	// Stage 0a: refusal history. A retained exact refusal returns its saved
	// reason; a room filter hit without detail is refusal_history_blocked.
	if saved, ok := req.Retained[refusalID]; ok {
		return refuse("0a", saved, "")
	}
	if req.Retention != nil {
		if rr := req.Retention.Rooms[TupleHash(room)]; rr != nil {
			switch err := rr.Refusals.Check(refusalID); err {
			case nil:
			case ErrRefused:
				return refuse("0a", ErrRefused, "")
			default:
				return nil, nil, err
			}
		}
	}
	// Stage 1: context.
	switch req.Context.State {
	case ContextEncrypted:
		if !twoMemberDM(req.Context, req.Local.MXID) {
			return refuse("1", ErrRoomScopeUnsupported, "")
		}
	case ContextPlaintext:
		if wire.Type == TypeEncrypted {
			return refuse("1", ErrCiphertextInPlaintextRoom, "")
		}
		// A plaintext event in a confirmed plaintext room is ordinary delivery,
		// outside this verifier.
		return nil, nil, ErrInvalid
	default:
		return refuse("1", ErrContextUnknown, "")
	}
	// Stage 2: outer kind. Control events belong to their own validators; the
	// caller routes them before admission, so any other type here is an
	// application payload in an encrypted room.
	if wire.Type != TypeEncrypted || wire.StateKey != nil {
		return refuse("2", ErrPlaintextInEncryptedRoom, "")
	}
	if content.Algorithm != AlgorithmMegolm {
		return refuse("2", ErrUnsupportedAlgorithm, "")
	}
	var ciphertext string
	if json.Unmarshal(content.Ciphertext, &ciphertext) != nil || !text(ciphertext, MaxWireEvent) || !text(content.SessionID, 255) {
		return refuse("2", ErrMalformedEvent, "")
	}
	// Stage 3: observed device eligibility. The lookup device is the session
	// record's originator when the session is known; otherwise the outer
	// sender/device hint, which stays a claim until stage 5/6.
	record, known := req.Sessions[content.SessionID]
	mxid, device := wire.Sender, content.DeviceID
	if known {
		mxid, device = record.Originator, record.Device
	}
	obs, reason, err := stage3(req.Peers, mxid, device)
	if err != nil {
		return nil, nil, err
	}
	if reason != "" {
		return refuse("3", reason, device)
	}
	// Stage 4: chain.
	if reason = stage4(obs, mxid); reason != "" {
		return refuse("4", reason, device)
	}
	// Stage 5: key availability and origin.
	if req.Retention != nil {
		if rr := req.Retention.Rooms[TupleHash(room)]; rr != nil && rr.Retired[TupleHash(content.SessionID)] {
			return refuse("5", ErrExpired, device)
		}
	}
	if req.Working.inbound[content.SessionID] == nil {
		return refuse("5", ErrUndecryptable, device)
	}
	if !known || !record.valid() || record.SessionID != content.SessionID {
		return refuse("5", ErrKeyProvenanceUnverified, device)
	}
	switch record.Source {
	case KeySourceDirectOlm:
		if record.Originator == req.Local.MXID {
			return refuse("5", ErrKeyProvenanceUnverified, device)
		}
	case KeySourceOwnOutbound:
		// Own echo: the session must be this exact local account/device's and
		// still present among its own outbound sessions.
		if record.Originator != req.Local.MXID || record.Device != req.Local.Device || record.Ed25519 != req.Local.Ed25519 || record.Curve25519 != req.Local.Curve25519 || req.Working.outbound[content.SessionID] == nil {
			return refuse("5", ErrKeyProvenanceUnverified, device)
		}
	default:
		return refuse("5", ErrKeyProvenanceUnverified, device)
	}
	// The chain-verified device must publish exactly the keys the session
	// provenance bound; otherwise the record and the chain disagree.
	keys := obs.device.Keys.Keys
	if keys["ed25519:"+device] != record.Ed25519 || keys["curve25519:"+device] != record.Curve25519 {
		return refuse("5", ErrKeyProvenanceUnverified, device)
	}
	// Stage 5, replay floor: the index is read from the unauthenticated
	// group-message header before decryption. It can only refuse here; the
	// authenticated index is compared to it after decryption.
	window := req.Replay[content.SessionID]
	headerIndex, headerOK := megolmHeaderIndex(ciphertext)
	if headerOK && window.HasFloor && headerIndex <= window.Floor {
		return refuse("5", ErrExpired, device)
	}
	// Stage 6: decrypt on a copy of the session (never committed) and bind.
	plain, index, err := req.Working.PeekMegolm(id.SessionID(content.SessionID), []byte(ciphertext))
	switch err {
	case nil:
	case ErrIntegrity:
		return refuse("6", ErrCiphertextInvalid, device)
	case ErrCapacity:
		return nil, nil, ErrCapacity
	default:
		return refuse("6", ErrUndecryptable, device)
	}
	if !headerOK || uint64(index) != headerIndex {
		return refuse("6", ErrCiphertextInvalid, device)
	}
	// Every string inside the plaintext joins the secret inventory, so a
	// diagnostic that embeds a decrypted body fragment is scrubbed, not only
	// one that embeds the whole payload.
	req.Working.engine.secrets.ObserveJSON(plain)
	if wire.Sender != record.Originator {
		return refuse("6", ErrSenderMismatch, device)
	}
	var inner megolmPlaintext
	if err := lenientJSON(plain, &inner, MaxWireEvent); err != nil || !isObject(inner.Content) || !text(inner.Type, 255) {
		return refuse("6", ErrMalformedEvent, device)
	}
	if inner.RoomID != room || record.RoomID != room {
		return refuse("6", ErrSenderMismatch, device)
	}
	// The originator must still be a joined member of the reconciled room: a
	// former peer's session does not follow the DM to its next member.
	if !isMember(req.Context, record.Originator) {
		return refuse("6", ErrSenderMismatch, device)
	}
	// Inner content structure (invalid decrypted structure is stage 6).
	var message innerMessage
	if lenientJSON(inner.Content, &message, MaxWireEvent) != nil {
		return refuse("6", ErrMalformedEvent, device)
	}
	// Stage 7: inner payload.
	if inner.Type != TypeRoomMessage || message.MsgType != MsgTypeText || message.Body == nil || len(message.URL) > 0 || len(message.File) > 0 || len(message.Info) > 0 || len(message.NewContent) > 0 || (message.RelatesTo != nil && message.RelatesTo.RelType == RelationReplace) {
		return refuse("7", ErrContentUnsupported, device)
	}
	// Stage 8: replay, by session index and by event ID. An exact retained
	// reread (same event, digest, index in both identities) is the prior
	// outcome; the same index under another event ID or digest, or the same
	// event ID under another session, index or digest, is a replay; an index
	// evicted while being reserved is expired. Nothing is reserved on refusal.
	prior, retained := window.Entries[uint64(index)]
	if retained && (prior.Event != event || prior.Digest != digest) {
		return refuse("8", ErrReplay, device)
	}
	seen, err := req.Events.Check(event, content.SessionID, uint64(index), digest)
	if err == ErrReplay {
		return refuse("8", ErrReplay, device)
	}
	if err != nil {
		return nil, nil, err
	}
	if retained != seen {
		// The two replay identities disagree: corrupted replay state, never
		// a verdict about this event.
		return nil, nil, ErrConflict
	}
	if !retained {
		switch err := window.Reserve(uint64(index), event, digest); err {
		case nil:
		case ErrReplay:
			return refuse("8", ErrReplay, device)
		case ErrExpired:
			return refuse("8", ErrExpired, device)
		default:
			return nil, nil, err
		}
		if err := req.Events.Reserve(event, content.SessionID, uint64(index), digest); err != nil {
			return nil, nil, err
		}
		req.Replay[content.SessionID] = window
	}
	ed, err := Fingerprint(record.Ed25519)
	if err != nil {
		return nil, nil, ErrCodec
	}
	curve, err := Fingerprint(record.Curve25519)
	if err != nil {
		return nil, nil, ErrCodec
	}
	return &AcceptedEvent{
		content: append([]byte(nil), inner.Content...), room: room, event: event, digest: digest,
		originator: record.Originator, device: record.Device, ed25519: ed, curve25519: curve,
		source: record.Source, session: content.SessionID, index: index, reread: retained,
	}, nil, nil
}

func isMember(c RoomContext, mxid string) bool {
	for _, m := range c.Members {
		if m == mxid {
			return true
		}
	}
	return false
}

// twoMemberDM reports an encrypted room whose joined members are exactly the
// local identity and one other account.
func twoMemberDM(c RoomContext, local string) bool {
	for _, m := range c.Members {
		if m != local && c.dmWith(local, m) {
			return true
		}
	}
	return false
}

// RecordRefusal persists the digest-only footprint of a final refusal: the
// retained reason by refusal ID and, for room events, the room's nonclearing
// digest filter. It stores no content, ciphertext or sender material beyond
// the identity digest. Filter saturation is reported as ErrCapacity after the
// bits are set, so the room can be suspended without forgetting the refusal.
func RecordRefusal(retained map[string]Code, retention *Retention, r Refusal) error {
	if retained == nil || r.ID == "" || r.Reason == "" {
		return ErrInvalid
	}
	retained[r.ID] = r.Reason
	if r.Source != SourceRoom || retention == nil || r.RoomID == "" {
		return nil
	}
	room := retention.Rooms[TupleHash(r.RoomID)]
	if room == nil {
		return ErrUnavailable
	}
	// The filter keys on the refusal ID: already a SHA256 hex digest over
	// (room, event_id, ciphertext_digest), so it is used directly.
	return room.Refusals.Add(r.ID)
}
