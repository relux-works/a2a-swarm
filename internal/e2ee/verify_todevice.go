package e2ee

import (
	"encoding/json"

	"maunium.net/go/mautrix/id"
)

// ToDeviceRequest is one to-device event with the authenticated local context
// the caller supplies. Working is the committed engine state; this verifier
// never mutates it. It forks with PickleKey and mutates only the fork, which
// is returned on acceptance and dropped on every refusal.
type ToDeviceRequest struct {
	Binding   string
	Raw       json.RawMessage
	Local     LocalDevice
	Peers     map[string]*PeerObservation
	Working   *Working
	PickleKey []byte
	// OlmSessions maps a sender Curve25519 key to the Olm session IDs already
	// established with it; type-1 Olm messages are tried against these only.
	OlmSessions map[string][]string
	Budget      *AttemptBudget
	Retention   *Retention
	NowMS       int64
}

// ToDeviceAdmission is the isolated prepared result: the fork carrying the
// new Olm/Megolm state, the provenance record and the Olm session that
// carried the key. Nothing here has touched the committed account.
type ToDeviceAdmission struct {
	Prepared   *Working
	Session    SessionRecord
	OlmSession string
}

type toDeviceWire struct {
	Type    string          `json:"type"`
	Sender  string          `json:"sender"`
	Content json.RawMessage `json:"content"`
}
type olmContent struct {
	Algorithm  string             `json:"algorithm"`
	SenderKey  string             `json:"sender_key"`
	Ciphertext map[string]olmBody `json:"ciphertext"`
}
type olmBody struct {
	Type *int   `json:"type"`
	Body string `json:"body"`
}
type olmPlaintext struct {
	Type          string          `json:"type"`
	Content       json.RawMessage `json:"content"`
	Sender        string          `json:"sender"`
	SenderDevice  string          `json:"sender_device"`
	Keys          olmKeys         `json:"keys"`
	Recipient     string          `json:"recipient"`
	RecipientKeys olmKeys         `json:"recipient_keys"`
}
type olmKeys struct {
	Ed25519 string `json:"ed25519"`
}
type roomKeyContent struct {
	Algorithm  string `json:"algorithm"`
	RoomID     string `json:"room_id"`
	SessionID  string `json:"session_id"`
	SessionKey string `json:"session_key"`
}

// VerifyToDevice implements the section 7 to-device prefix in exact order:
// malformed -> sender outside trust/joined-peer set -> budget -> algorithm ->
// stage 3 -> stage 4 -> Olm decrypt typed errors -> authenticated sender/
// recipient/key conflict -> key type -> room outside the reconciled joined DM
// -> accept into the isolated fork. A Refusal is the final verdict for this
// event; an error is a batch-level failure (invalid request, incomplete key
// query, capacity) that leaves no verdict.
func VerifyToDevice(req ToDeviceRequest) (*ToDeviceAdmission, *Refusal, error) {
	if !hexSize(req.Binding, 32) || !req.Local.valid() || req.Working == nil || len(req.PickleKey) != 32 || req.Budget == nil || req.NowMS < 0 {
		return nil, nil, ErrInvalid
	}
	refuse := func(stage string, reason Code, sender, kind string, content []byte, device string) (*ToDeviceAdmission, *Refusal, error) {
		return nil, &Refusal{Source: SourceToDevice, Stage: stage, Reason: reason, ID: ToDeviceRefusalID(req.Binding, sender, kind, content), Sender: sender, Device: device}, nil
	}
	// Prefix: malformed. Fields that cannot be read become empty identity
	// components and the raw event digest stands in for the content.
	var wire toDeviceWire
	if err := lenientJSON(req.Raw, &wire, MaxWireEvent); err != nil || !isObject(wire.Content) {
		return refuse("malformed", ErrMalformedEvent, "", "", []byte(rawDigest(req.Raw)), "")
	}
	if !text(wire.Type, 255) || !fullMXID(wire.Sender) {
		return refuse("malformed", ErrMalformedEvent, "", "", []byte(rawDigest(req.Raw)), "")
	}
	var content olmContent
	if err := lenientJSON(wire.Content, &content, MaxWireEvent); err != nil {
		return refuse("malformed", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, "")
	}
	// Prefix: sender eligibility. The outer sender must be an anchored peer
	// with at least one reconciled joined two-member DM with this identity.
	peer := req.Peers[wire.Sender]
	if peer == nil || wire.Sender == req.Local.MXID || !hasJoinedDM(peer, req.Local.MXID, wire.Sender) {
		return refuse("sender", ErrSenderNotEligible, wire.Sender, wire.Type, wire.Content, "")
	}
	// Prefix: bounded attempts per peer and per identity.
	if !req.Budget.take(wire.Sender, req.NowMS) {
		return refuse("budget", ErrRateLimited, wire.Sender, wire.Type, wire.Content, "")
	}
	// Prefix: algorithm. An unencrypted room-key type is an unauthenticated
	// key: provenance, not algorithm. Any other non-Olm payload is unsupported.
	if wire.Type != TypeEncrypted {
		if wire.Type == TypeRoomKey || isForwardedKeyType(wire.Type) {
			return refuse("algorithm", ErrKeyProvenanceUnverified, wire.Sender, wire.Type, wire.Content, "")
		}
		return refuse("algorithm", ErrUnsupportedAlgorithm, wire.Sender, wire.Type, wire.Content, "")
	}
	if content.Algorithm != AlgorithmOlm {
		return refuse("algorithm", ErrUnsupportedAlgorithm, wire.Sender, wire.Type, wire.Content, "")
	}
	if _, err := publicBase64(content.SenderKey, 32); err != nil || len(content.Ciphertext) == 0 || len(content.Ciphertext) > maxCiphertextKeys {
		return refuse("malformed", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, "")
	}
	body, ok := content.Ciphertext[req.Local.Curve25519]
	if !ok || body.Type == nil || (*body.Type != 0 && *body.Type != 1) || !text(body.Body, MaxWireEvent) {
		return refuse("malformed", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, "")
	}
	// Stage 3: the sender_key hint must resolve to exactly one device in the
	// completed list, in stage-3 order; its eligibility label decides.
	// Stage 4: the chain must verify. A label refusal carries the matched
	// device ID; the ambiguous and no-match refusals carry none.
	obs, reason, err := stage3BySenderKey(req.Peers, wire.Sender, content.SenderKey)
	if err != nil {
		return nil, nil, err
	}
	if reason != "" {
		return refuse("3", reason, wire.Sender, wire.Type, wire.Content, obs.id)
	}
	device := obs.id
	if reason = stage4(obs, wire.Sender); reason != "" {
		return refuse("4", reason, wire.Sender, wire.Type, wire.Content, device)
	}
	// Olm decrypt on an isolated fork. The fork is the only state mutated from
	// here on; every refusal below drops it, so the committed account, its
	// one-time keys and its sessions are untouched by a rejected attempt.
	fork, err := req.Working.Fork(req.PickleKey)
	if err != nil {
		return nil, nil, err
	}
	var plain []byte
	var olmSession string
	if *body.Type == 0 {
		sid, raw, err := fork.DecryptPrekey(id.Curve25519(content.SenderKey), body.Body)
		switch err {
		case nil:
			plain, olmSession = raw, string(sid)
		case ErrIntegrity:
			return refuse("decrypt", ErrCiphertextInvalid, wire.Sender, wire.Type, wire.Content, device)
		case ErrCapacity:
			return nil, nil, ErrCapacity
		default:
			return refuse("decrypt", ErrUndecryptable, wire.Sender, wire.Type, wire.Content, device)
		}
	} else {
		for _, candidate := range req.OlmSessions[content.SenderKey] {
			raw, err := fork.DecryptOlm(id.SessionID(candidate), id.OlmMsgTypeMsg, body.Body)
			if err == ErrCapacity {
				return nil, nil, ErrCapacity
			}
			if err == nil {
				plain, olmSession = raw, candidate
				break
			}
		}
		if plain == nil {
			// Several candidate sessions make integrity and wrong-session
			// indistinguishable: one consistent code.
			return refuse("decrypt", ErrUndecryptable, wire.Sender, wire.Type, wire.Content, device)
		}
	}
	// Authenticated bindings: the Olm identity key that decrypted equals the
	// device's published Curve25519; the plaintext's sender, sender_device,
	// Ed25519 claim, recipient and recipient key must all agree.
	fork.engine.secrets.ObserveJSON(plain)
	var inner olmPlaintext
	if err := lenientJSON(plain, &inner, MaxWireEvent); err != nil || !isObject(inner.Content) {
		return refuse("decrypt", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, device)
	}
	deviceKeys := obs.device.Keys.Keys
	if deviceKeys["curve25519:"+device] != content.SenderKey || inner.Sender != wire.Sender || inner.SenderDevice != device || inner.Keys.Ed25519 != deviceKeys["ed25519:"+device] || inner.Recipient != req.Local.MXID || inner.RecipientKeys.Ed25519 != req.Local.Ed25519 {
		return refuse("bindings", ErrSenderMismatch, wire.Sender, wire.Type, wire.Content, device)
	}
	// Key type: only a direct m.room_key is provenance. Forwarded, imported,
	// backup, withheld and secret payloads never become sessions.
	if inner.Type != TypeRoomKey {
		return refuse("key-type", ErrKeyProvenanceUnverified, wire.Sender, wire.Type, wire.Content, device)
	}
	var key roomKeyContent
	if err := lenientJSON(inner.Content, &key, MaxWireEvent); err != nil || !text(key.SessionID, 255) || len(key.SessionKey) == 0 || len(key.SessionKey) > MaxItem {
		return refuse("key-type", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, device)
	}
	if key.Algorithm != AlgorithmMegolm {
		return refuse("key-type", ErrUnsupportedAlgorithm, wire.Sender, wire.Type, wire.Content, device)
	}
	// Room: the key must name a reconciled joined two-member DM with exactly
	// this identity and the authenticated sender.
	if !roomID(key.RoomID) || !peer.Rooms[key.RoomID].dmWith(req.Local.MXID, wire.Sender) {
		return refuse("room", ErrSenderNotEligible, wire.Sender, wire.Type, wire.Content, device)
	}
	if req.Retention != nil {
		// A closed room is outside the active set of DMs, whatever its
		// sessions: the same reason as any room outside the reconciled joined
		// DM, and checked first so that it is never reported as an expired
		// replay window.
		if rr := req.Retention.Rooms[TupleHash(key.RoomID)]; rr != nil && rr.Closed {
			return refuse("room", ErrSenderNotEligible, wire.Sender, wire.Type, wire.Content, device)
		}
		// A retired session is a no-reimport tombstone: the same reason the room
		// side gives a retired session at stage 5. CanImport's only remaining
		// ErrRefused is that tombstone.
		switch err := req.Retention.CanImport(TupleHash(key.RoomID), TupleHash(key.SessionID)); err {
		case nil:
		case ErrRefused:
			return refuse("room", ErrExpired, wire.Sender, wire.Type, wire.Content, device)
		default:
			return nil, nil, err
		}
	}
	sid, err := fork.AcceptDirectMegolm([]byte(key.SessionKey))
	switch err {
	case nil:
	case ErrExists:
		return refuse("session", ErrReplay, wire.Sender, wire.Type, wire.Content, device)
	case ErrCapacity:
		return nil, nil, ErrCapacity
	default:
		// Not a direct sharing-format key (export/backup format or corrupt).
		return refuse("session", ErrKeyProvenanceUnverified, wire.Sender, wire.Type, wire.Content, device)
	}
	if string(sid) != key.SessionID {
		return refuse("session", ErrSenderMismatch, wire.Sender, wire.Type, wire.Content, device)
	}
	record := SessionRecord{
		SessionID: key.SessionID, RoomID: key.RoomID, Originator: wire.Sender, Device: device,
		Ed25519: deviceKeys["ed25519:"+device], Curve25519: content.SenderKey, Source: KeySourceDirectOlm,
	}
	if !record.valid() {
		return refuse("session", ErrMalformedEvent, wire.Sender, wire.Type, wire.Content, device)
	}
	return &ToDeviceAdmission{Prepared: fork, Session: record, OlmSession: olmSession}, nil, nil
}

func hasJoinedDM(peer *PeerObservation, local, mxid string) bool {
	for _, room := range peer.Rooms {
		if room.dmWith(local, mxid) {
			return true
		}
	}
	return false
}

// isForwardedKeyType names the unencrypted key-carrying to-device types that
// can never establish provenance, whatever their algorithm claims.
func isForwardedKeyType(kind string) bool {
	switch kind {
	case "m.forwarded_room_key", "m.room_key.withheld", "m.secret.send", "m.room_key_request":
		return true
	}
	return false
}
