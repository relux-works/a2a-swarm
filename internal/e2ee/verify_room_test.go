package e2ee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// A Megolm event under a direct-olm session from the eligible cross-signed
// peer, with the outer sender equal to the chain-bound originator, is accepted
// with the exact inner content bytes, originator MXID/device, both key
// fingerprints, key source, session and index; the replay window reserves the
// index; an exact reread is reported as such; and wire JSON cannot populate
// the accepted-event type.
func TestRoomEventAcceptsVerifiedOriginator(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	inner := map[string]any{"msgtype": MsgTypeText, "body": "verified text", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$root"}}
	raw := e.megolm(e.peer, e.sid, "$1", inner, nil, nil)
	req := e.room(adm, raw)
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("verified originator refused", err, refusal)
	}
	wantContent, _ := json.Marshal(inner)
	var got, want any
	if json.Unmarshal(accepted.Content(), &got) != nil || json.Unmarshal(wantContent, &want) != nil || !jsonEqual(got, want) {
		t.Fatal("inner content differs")
	}
	if mxid, device := accepted.Originator(); mxid != peerMXID || device != peerDev {
		t.Fatal("originator")
	}
	edFP, _ := Fingerprint(e.peer.ed)
	curveFP, _ := Fingerprint(e.peer.curve)
	if ed, curve := accepted.Fingerprints(); ed != edFP || curve != curveFP {
		t.Fatal("fingerprints")
	}
	if accepted.KeySource() != KeySourceDirectOlm || accepted.Session() != e.sid || accepted.Index() != 0 || accepted.Reread() || accepted.Room() != dmRoom || accepted.EventID() != "$1" {
		t.Fatal("provenance fields")
	}
	var wire roomWire
	_ = json.Unmarshal(raw, &wire)
	if accepted.CiphertextDigest() != rawDigest(wire.Content) || len(req.Replay[e.sid].Entries) != 1 {
		t.Fatal("digest or replay reservation")
	}
	again, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !again.Populated() || !again.Reread() {
		t.Fatal("exact reread not reported", err, refusal)
	}
	var planted AcceptedEvent
	if json.Unmarshal([]byte(`{"content":"eyJ9","originator":"@x:y","source":"direct-olm","session":"s"}`), &planted) != nil || planted.Populated() || planted.Content() != nil {
		t.Fatal("wire JSON constructed an accepted event")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// The outer sender must equal the MXID the chain binds to the originator
// device: the same valid ciphertext asserted under another anchored peer or
// under the local MXID is e2ee_sender_mismatch at stage 6, while the genuine
// sender beside it is accepted. This is the enforced check of section 1 item 2.
func TestRoomEventOuterSenderMismatch(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	impostor := newParty(t, impostorMXID, impostorDev)
	e.peers[impostorMXID] = impostor.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, impostorMXID)})
	for _, sender := range []string{impostorMXID, localMXID} {
		raw := e.megolm(e.peer, e.sid, "$"+sender, nil, nil, func(w map[string]any) { w["sender"] = sender })
		req := e.room(adm, raw)
		if err := checkOuterSenderRefused(req); err != nil {
			t.Fatal(sender, err)
		}
		if len(req.Replay[e.sid].Entries) != 0 {
			t.Fatal("refusal reserved replay state")
		}
	}
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$genuine", nil, nil, nil))); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("genuine sender control", err, refusal)
	}
}

// checkOuterSenderRefused is the killer assertion of the outer-sender plant:
// a valid ciphertext under a foreign outer sender must be sender_mismatch.
func checkOuterSenderRefused(req RoomEventRequest) error {
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil {
		return err
	}
	if refusal == nil || refusal.Reason != ErrSenderMismatch || refusal.Stage != "6" || accepted != nil {
		return ErrSenderMismatch
	}
	return nil
}

// A session without direct-olm or own-outbound provenance never yields an
// accepted event: a record whose source is forwarded, a missing record, a
// record whose keys disagree with the chain device, and a direct-olm record
// naming the local MXID are e2ee_key_provenance_unverified at stage 5; a
// session absent from the engine is undecryptable; the genuine record admits.
func TestRoomEventRefusesForwardedSessionSource(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	raw := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	for name, edit := range map[string]func(*RoomEventRequest){
		"forwarded-source": func(r *RoomEventRequest) {
			rec := r.Sessions[e.sid]
			rec.Source = "forwarded"
			r.Sessions[e.sid] = rec
		},
		"missing-record": func(r *RoomEventRequest) { delete(r.Sessions, e.sid) },
		"keys-disagree": func(r *RoomEventRequest) {
			rec := r.Sessions[e.sid]
			rec.Ed25519 = e.local.ed
			r.Sessions[e.sid] = rec
		},
		"direct-olm-from-self": func(r *RoomEventRequest) {
			rec := r.Sessions[e.sid]
			rec.Originator, rec.Device = localMXID, localDev
			r.Sessions[e.sid] = rec
			r.Peers[localMXID] = e.local.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := e.room(adm, raw)
			edit(&req)
			if err := checkProvenanceRefused(req); err != nil {
				t.Fatal(err)
			}
		})
	}
	noSession := e.room(adm, raw)
	noSession.Working = e.local.w
	_, refusal, err := VerifyRoomEvent(noSession)
	expectRefusal(t, refusal, err, "5", ErrUndecryptable)
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, raw)); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("direct-olm control", err, refusal)
	}
}

// checkProvenanceRefused is the killer assertion of the forwarded-keys plant
// on the room side: a session lacking direct provenance must refuse at stage 5.
func checkProvenanceRefused(req RoomEventRequest) error {
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil {
		return err
	}
	if refusal == nil || refusal.Reason != ErrKeyProvenanceUnverified || refusal.Stage != "5" || accepted != nil {
		return ErrKeyProvenanceUnverified
	}
	return nil
}

// Integrity is distinguished from unavailability: a ciphertext with one
// flipped byte is e2ee_ciphertext_invalid, a body that is not a Megolm
// message is e2ee_undecryptable, both at stage 6 without replay reservation;
// the untouched ciphertext beside them is accepted.
func TestRoomEventTamperedCiphertext(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	raw := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	flipped := e.megolm(e.peer, e.sid, "$2", nil, func(m map[string]any) { m["ciphertext"] = flipLastByte(t, m["ciphertext"].(string)) }, nil)
	req := e.room(adm, flipped)
	_, refusal, err := VerifyRoomEvent(req)
	expectRefusal(t, refusal, err, "6", ErrCiphertextInvalid)
	garbage := e.megolm(e.peer, e.sid, "$3", nil, func(m map[string]any) { m["ciphertext"] = "QUFBQUFBQUFBQUFB" }, nil)
	_, refusal, err = VerifyRoomEvent(e.room(adm, garbage))
	expectRefusal(t, refusal, err, "6", ErrUndecryptable)
	if len(req.Replay[e.sid].Entries) != 0 {
		t.Fatal("refusal reserved replay state")
	}
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, raw)); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("untouched control", err, refusal)
	}
}

// Stage 8 replay, both identities. By session index: the same index under a
// different event ID, and the same event ID with a different valid ciphertext
// at that index, are e2ee_replay. By event ID: an already accepted ID reused
// by the homeserver for the next genuine index of the same session, and for
// a genuine event of another admitted session, is e2ee_replay; so is an
// accepted ID whose stored index or session differs. The first delivery, an
// exact reread (Reread true, no second reservation) and a later index under a
// fresh ID are accepted; no refusal reserves either identity.
func TestRoomEventReplay(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	// A fork of the peer's outbound session encrypts a second valid message
	// at the same index zero, with a different digest.
	peerFork, err := e.peer.w.Fork(e.peer.key)
	if err != nil {
		t.Fatal(err)
	}
	first := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	forkParty := *e.peer
	forkParty.w = peerFork
	sameIndexOtherBody := e.megolm(&forkParty, e.sid, "$1", map[string]any{"msgtype": MsgTypeText, "body": "other body at index zero"}, nil, nil)
	req := e.room(adm, first)
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !accepted.Populated() || accepted.Index() != 0 {
		t.Fatal("first delivery control", err, refusal)
	}
	if len(req.Events.Entries) != 1 || req.Events.Entries["$1"].Index != 0 || req.Events.Entries["$1"].Session != e.sid {
		t.Fatal("event identity not reserved")
	}
	// Same ciphertext, different event ID.
	var wire map[string]any
	_ = json.Unmarshal(first, &wire)
	wire["event_id"] = "$2"
	renamed, _ := json.Marshal(wire)
	replayed := req
	replayed.Raw = renamed
	if err := checkReplayRefused(replayed, ErrReplay); err != nil {
		t.Fatal("same index other event", err)
	}
	sameEvent := req
	sameEvent.Raw = sameIndexOtherBody
	if err := checkReplayRefused(sameEvent, ErrReplay); err != nil {
		t.Fatal("same event other digest", err)
	}
	// B1: the homeserver reuses "$1" for the peer's next genuine message.
	reusedID := req
	reusedID.Raw = e.megolm(e.peer, e.sid, "$1", map[string]any{"msgtype": MsgTypeText, "body": "second genuine message"}, nil, nil)
	if err := checkEventIDReplayRefused(reusedID); err != nil {
		t.Fatal("event ID reused for next index", err)
	}
	// B1 across sessions: a second admitted session from the same peer, whose
	// genuine first message the homeserver also labels "$1".
	sid2, shared2, err := e.peer.w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adm.Prepared.AcceptDirectMegolm(shared2); err != nil {
		t.Fatal(err)
	}
	rec2 := adm.Session
	rec2.SessionID = string(sid2)
	req.Sessions[string(sid2)] = rec2
	crossSession := req
	crossSession.Raw = e.megolm(e.peer, string(sid2), "$1", nil, nil, nil)
	if err := checkEventIDReplayRefused(crossSession); err != nil {
		t.Fatal("event ID reused across sessions", err)
	}
	if len(req.Replay[string(sid2)].Entries) != 0 || len(req.Replay[e.sid].Entries) != 1 || len(req.Events.Entries) != 1 {
		t.Fatal("a refusal reserved replay state")
	}
	// Exact reread: prior outcome, no second reservation.
	again, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !again.Populated() || !again.Reread() || req.Events.Next != 1 {
		t.Fatal("exact reread", err, refusal)
	}
	// Controls: the same genuine messages under fresh IDs are accepted.
	next := req
	next.Raw = e.megolm(e.peer, e.sid, "$3", nil, nil, nil)
	if accepted, refusal, err := VerifyRoomEvent(next); err != nil || refusal != nil || !accepted.Populated() || accepted.Index() != 2 {
		t.Fatal("next index control", err, refusal)
	}
	other := req
	other.Raw = e.megolm(e.peer, string(sid2), "$4", nil, nil, nil)
	if accepted, refusal, err := VerifyRoomEvent(other); err != nil || refusal != nil || !accepted.Populated() || accepted.Session() != string(sid2) {
		t.Fatal("other session control", err, refusal)
	}
	if len(req.Events.Entries) != 3 {
		t.Fatal("accepted IDs not all reserved")
	}
}

// checkReplayRefused is the killer assertion of the replay plant: a replayed
// or expired index must refuse at stage 8 with the given reason.
func checkReplayRefused(req RoomEventRequest, want Code) error {
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil {
		return err
	}
	if refusal == nil || refusal.Reason != want || refusal.Stage != "8" || accepted != nil {
		return want
	}
	return nil
}

// checkEventIDReplayRefused is the killer assertion of the event-ID replay
// plant: a genuine payload under an already accepted event ID must be
// e2ee_replay at stage 8, whatever its session or index.
func checkEventIDReplayRefused(req RoomEventRequest) error {
	return checkReplayRefused(req, ErrReplay)
}

// The replay floor is applied at stage 5 from the unauthenticated header: an
// index at or below the floor is e2ee_replay_window_expired even when the same
// event also carries a stage 6 fault (foreign outer sender) and a stage 7
// fault (m.replace); the header index must equal the authenticated index; the
// floor-free control is accepted and reserves nothing on the expired paths.
func TestRoomEventFloorBeforeDecrypt(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	impostor := newParty(t, impostorMXID, impostorDev)
	e.peers[impostorMXID] = impostor.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, impostorMXID)})
	edit := map[string]any{"msgtype": MsgTypeText, "body": "* edit", "m.relates_to": map[string]any{"rel_type": RelationReplace, "event_id": "$1"}}
	faulty := e.megolm(e.peer, e.sid, "$f", edit, nil, func(w map[string]any) { w["sender"] = impostorMXID })
	req := e.room(adm, faulty)
	req.Replay[e.sid] = ReplayWindow{HasFloor: true, Floor: 3}
	if err := checkFloorRefused(req); err != nil {
		t.Fatal(err)
	}
	// The header reader sees the index goolm will authenticate (0 here).
	var w roomWire
	var c megolmContent
	_ = json.Unmarshal(faulty, &w)
	_ = json.Unmarshal(w.Content, &c)
	var ct string
	_ = json.Unmarshal(c.Ciphertext, &ct)
	if index, ok := megolmHeaderIndex(ct); !ok || index != 0 {
		t.Fatal("header index control", ok, index)
	}
	noReplayState(t, req)
	clean := e.room(adm, e.megolm(e.peer, e.sid, "$ok", nil, nil, nil))
	if accepted, refusal, err := VerifyRoomEvent(clean); err != nil || refusal != nil || !accepted.Populated() || accepted.Index() != 1 {
		t.Fatal("floor-free control", err, refusal)
	}
	// The next genuine message is index 2: exactly at a floor of 2 it expires.
	clean.Replay[e.sid] = ReplayWindow{HasFloor: true, Floor: 2, Entries: map[uint64]ReplayEntry{}}
	clean.Raw = e.megolm(e.peer, e.sid, "$at-floor", nil, nil, nil)
	if _, refusal, err := VerifyRoomEvent(clean); err != nil || refusal == nil || refusal.Stage != "5" || refusal.Reason != ErrExpired {
		t.Fatal("index at the floor admitted", err, refusal)
	}
}

// checkFloorRefused is the killer assertion of the floor-stage plant: an
// index below the floor must be e2ee_replay_window_expired at stage 5, ahead
// of the stage 6 and 7 faults the same event carries.
func checkFloorRefused(req RoomEventRequest) error {
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil {
		return err
	}
	if refusal == nil || refusal.Reason != ErrExpired || refusal.Stage != "5" || accepted != nil {
		return fmt.Errorf("want stage 5 %s, got %v", ErrExpired, refusal)
	}
	return nil
}

// B2: VerifyRoomEvent never mutates Working. Decryption runs on a copy of the
// inbound session that is never committed, so a refusal at stage 6 (foreign
// outer sender), 7 (m.replace) and 8 (replayed index) leaves Checkpoint
// byte-identical; the accepted control also leaves it byte-identical and is
// still decryptable afterwards, and a later index decrypts after the earlier
// ones (the stored ratchet stays at its import position).
func TestRoomEventRefusalLeavesWorkingUnchanged(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	impostor := newParty(t, impostorMXID, impostorDev)
	e.peers[impostorMXID] = impostor.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, impostorMXID)})
	// Index 0 is never shown to the verifier, so every verified index is
	// above the stored ratchet position and a live decrypt would advance it.
	if _, err := e.peer.w.EncryptMegolm(idSession(e.sid), []byte(`{"type":"m.room.message","content":{},"room_id":"!dm:example.test"}`)); err != nil {
		t.Fatal(err)
	}
	before := checkpoint(t, adm.Prepared, e.local.key)
	edit := map[string]any{"msgtype": MsgTypeText, "body": "* edit", "m.relates_to": map[string]any{"rel_type": RelationReplace, "event_id": "$1"}}
	stage6 := e.room(adm, e.megolm(e.peer, e.sid, "$6", nil, nil, func(w map[string]any) { w["sender"] = impostorMXID }))
	stage7 := e.room(adm, e.megolm(e.peer, e.sid, "$7", edit, nil, nil))
	stage8 := e.room(adm, e.megolm(e.peer, e.sid, "$8", nil, nil, nil))
	stage8.Replay[e.sid] = ReplayWindow{Entries: map[uint64]ReplayEntry{3: {Event: "$earlier", Digest: TupleHash("x")}}}
	for _, tc := range []struct {
		name   string
		req    RoomEventRequest
		stage  string
		reason Code
	}{{"6", stage6, "6", ErrSenderMismatch}, {"7", stage7, "7", ErrContentUnsupported}, {"8", stage8, "8", ErrReplay}} {
		_, refusal, err := VerifyRoomEvent(tc.req)
		expectRefusal(t, refusal, err, tc.stage, tc.reason)
		if err := checkWorkingUnchanged(before, adm.Prepared, e.local.key); err != nil {
			t.Fatal("stage", tc.name, err)
		}
		if tc.name != "8" {
			noReplayState(t, tc.req)
		}
	}
	accepted := e.room(adm, e.megolm(e.peer, e.sid, "$ok", nil, nil, nil))
	if got, refusal, err := VerifyRoomEvent(accepted); err != nil || refusal != nil || !got.Populated() || got.Index() != 4 {
		t.Fatal("accepted control", err, refusal)
	}
	if err := checkWorkingUnchanged(before, adm.Prepared, e.local.key); err != nil {
		t.Fatal("acceptance", err)
	}
	// A further genuine index under a fresh replay state still decrypts from
	// the unchanged stored ratchet.
	further := stage6
	further.Raw = e.megolmSameIndexAt(t, e.peer, e.sid, "$again")
	if got, refusal, err := VerifyRoomEvent(further); err != nil || refusal != nil || !got.Populated() || got.Index() != 5 {
		t.Fatal("further index decryptable", err, refusal)
	}
	if err := checkWorkingUnchanged(before, adm.Prepared, e.local.key); err != nil {
		t.Fatal("further acceptance", err)
	}
}

// L2: a session of a former peer does not follow the DM to its next member.
// With the reconciled room now {local, impostor}, a genuine event under the
// peer's direct-olm session (outer sender peer, equal to the record) is
// e2ee_sender_mismatch at stage 6; the same event with the peer joined is
// accepted.
func TestRoomEventFormerPeerSession(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	raw := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	moved := e.room(adm, raw)
	moved.Context = dmContext(localMXID, impostorMXID)
	_, refusal, err := VerifyRoomEvent(moved)
	expectRefusal(t, refusal, err, "6", ErrSenderMismatch)
	noReplayState(t, moved)
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, raw)); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("joined peer control", err, refusal)
	}
}

// megolmSameIndexAt renders a genuine text event at the party's current index
// through a fork, leaving the party's live session where it is.
func (e *env) megolmSameIndexAt(t *testing.T, from *party, sid, eventID string) json.RawMessage {
	return e.megolmSameIndex(t, from, sid, eventID, nil, nil, nil)
}

// checkWorkingUnchanged is the killer assertion of the copy-decrypt plant: a
// verified event must not change the engine checkpoint bytes.
func checkWorkingUnchanged(before EngineState, w *Working, key []byte) error {
	after, err := w.Checkpoint(key)
	if err != nil {
		return err
	}
	if !sameState(before, after) {
		return ErrIntegrity
	}
	return nil
}

// Context and outer-kind gates: unknown context is e2ee_context_unknown; an
// encrypted room with a third joined member is e2ee_room_scope_unsupported;
// ciphertext in a plaintext-confirmed room is
// e2ee_ciphertext_in_plaintext_room; a plaintext application event or a state
// event in the encrypted DM is e2ee_plaintext_in_encrypted_room; an
// unsupported algorithm is e2ee_unsupported_algorithm; a plaintext event in a
// plaintext room is outside this verifier (ErrInvalid, no verdict). The DM
// context beside them is accepted.
func TestRoomEventContextAndOuterKind(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	raw := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	plaintext, _ := json.Marshal(map[string]any{"type": TypeRoomMessage, "sender": peerMXID, "room_id": dmRoom, "event_id": "$p", "content": map[string]any{"msgtype": MsgTypeText, "body": "injected"}})
	state, _ := json.Marshal(map[string]any{"type": TypeEncrypted, "state_key": "", "sender": peerMXID, "room_id": dmRoom, "event_id": "$s", "content": map[string]any{"algorithm": AlgorithmMegolm}})
	for _, tc := range []struct {
		name    string
		raw     json.RawMessage
		context RoomContext
		stage   string
		reason  Code
	}{
		{"unknown-context", raw, RoomContext{State: ContextUnknown}, "1", ErrContextUnknown},
		{"third-member", raw, RoomContext{State: ContextEncrypted, Members: []string{localMXID, peerMXID, impostorMXID}}, "1", ErrRoomScopeUnsupported},
		{"self-only", raw, RoomContext{State: ContextEncrypted, Members: []string{localMXID}}, "1", ErrRoomScopeUnsupported},
		{"ciphertext-in-plaintext-room", raw, RoomContext{State: ContextPlaintext, Members: []string{localMXID, peerMXID}}, "1", ErrCiphertextInPlaintextRoom},
		{"plaintext-in-encrypted-room", plaintext, dmContext(localMXID, peerMXID), "2", ErrPlaintextInEncryptedRoom},
		{"state-event", state, dmContext(localMXID, peerMXID), "2", ErrPlaintextInEncryptedRoom},
		{"unsupported-algorithm", e.megolm(e.peer, e.sid, "$a", nil, func(m map[string]any) { m["algorithm"] = "m.megolm.v2.aes-sha2" }, nil), dmContext(localMXID, peerMXID), "2", ErrUnsupportedAlgorithm},
		{"missing-session-id", e.megolm(e.peer, e.sid, "$m", nil, func(m map[string]any) { delete(m, "session_id") }, nil), dmContext(localMXID, peerMXID), "2", ErrMalformedEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := e.room(adm, tc.raw)
			req.Context = tc.context
			_, refusal, err := VerifyRoomEvent(req)
			expectRefusal(t, refusal, err, tc.stage, tc.reason)
		})
	}
	outside := e.room(adm, plaintext)
	outside.Context = RoomContext{State: ContextPlaintext, Members: []string{localMXID, peerMXID}}
	if accepted, refusal, err := VerifyRoomEvent(outside); err != ErrInvalid || accepted != nil || refusal != nil {
		t.Fatal("plaintext room event was judged", err, refusal)
	}
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, raw)); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("DM control", err, refusal)
	}
}

// Stage 0 wire shape: duplicate members, a non-object content, a missing or
// malformed event ID or room ID and a partial sender are e2ee_malformed_event
// with an identity that falls back to the raw digest when the room/event
// components are unreadable; the well-formed event beside them is accepted.
func TestRoomEventWireShape(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	good := e.megolm(e.peer, e.sid, "$1", nil, nil, nil)
	var wire map[string]any
	_ = json.Unmarshal(good, &wire)
	render := func(edit func(map[string]any)) json.RawMessage {
		var copy map[string]any
		_ = json.Unmarshal(good, &copy)
		edit(copy)
		raw, _ := json.Marshal(copy)
		return raw
	}
	for name, raw := range map[string]json.RawMessage{
		"duplicate-member": json.RawMessage(`{"type":"m.room.encrypted","type":"m.room.encrypted","sender":"@peer:example.test","room_id":"!dm:example.test","event_id":"$1","content":{}}`),
		"content-string":   render(func(m map[string]any) { m["content"] = "ciphertext" }),
		"missing-event-id": render(func(m map[string]any) { delete(m, "event_id") }),
		"bad-room-id":      render(func(m map[string]any) { m["room_id"] = "dm" }),
		"partial-sender":   render(func(m map[string]any) { m["sender"] = "peer" }),
		"wrong-type-field": render(func(m map[string]any) { contentOf(m)["session_id"] = 7 }),
	} {
		t.Run(name, func(t *testing.T) {
			req := e.room(adm, raw)
			_, refusal, err := VerifyRoomEvent(req)
			expectRefusal(t, refusal, err, "0", ErrMalformedEvent)
			if name == "missing-event-id" && refusal.ID != RoomRefusalID("", "", rawDigest(raw)) {
				t.Fatal("malformed identity does not use the raw digest")
			}
		})
	}
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, good)); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("well-formed control", err, refusal)
	}
}

// Stage 7 inner payload: an edit relation (m.replace / m.new_content), a
// media msgtype, a text message carrying url/file/info, and an inner type
// other than m.room.message are e2ee_content_unsupported; a text message with
// a thread relation or a formatted body beside them is accepted.
func TestRoomEventInnerContentUnsupported(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	for name, inner := range map[string]map[string]any{
		"replace":        {"msgtype": MsgTypeText, "body": "* edit", "m.new_content": map[string]any{"msgtype": MsgTypeText, "body": "edit"}, "m.relates_to": map[string]any{"rel_type": RelationReplace, "event_id": "$1"}},
		"replace-only":   {"msgtype": MsgTypeText, "body": "* edit", "m.relates_to": map[string]any{"rel_type": RelationReplace, "event_id": "$1"}},
		"image":          {"msgtype": "m.image", "body": "photo.png", "url": "mxc://example.test/abc"},
		"text-with-file": {"msgtype": MsgTypeText, "body": "see", "file": map[string]any{"url": "mxc://example.test/abc"}},
		"text-with-info": {"msgtype": MsgTypeText, "body": "see", "info": map[string]any{"size": 1}},
		"no-body":        {"msgtype": MsgTypeText},
	} {
		t.Run(name, func(t *testing.T) {
			_, refusal, err := VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$"+name, inner, nil, nil)))
			expectRefusal(t, refusal, err, "7", ErrContentUnsupported)
		})
	}
	// Inner type other than m.room.message.
	sticker, _ := json.Marshal(map[string]any{"type": "m.sticker", "content": map[string]any{"body": "s", "url": "mxc://example.test/s"}, "room_id": dmRoom})
	ciphertext, err := e.peer.w.EncryptMegolm(idSession(e.sid), sticker)
	if err != nil {
		t.Fatal(err)
	}
	raw := e.megolm(e.peer, e.sid, "$sticker", nil, func(m map[string]any) { m["ciphertext"] = string(ciphertext) }, nil)
	_, refusal, err := VerifyRoomEvent(e.room(adm, raw))
	expectRefusal(t, refusal, err, "7", ErrContentUnsupported)
	// Inner plaintext that is not an event object.
	notEvent, _ := json.Marshal([]any{"text"})
	ciphertext, _ = e.peer.w.EncryptMegolm(idSession(e.sid), notEvent)
	_, refusal, err = VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$array", nil, func(m map[string]any) { m["ciphertext"] = string(ciphertext) }, nil)))
	expectRefusal(t, refusal, err, "6", ErrMalformedEvent)
	// Inner room_id disagreeing with the outer room: a binding conflict.
	otherRoomPlain, _ := json.Marshal(map[string]any{"type": TypeRoomMessage, "content": map[string]any{"msgtype": MsgTypeText, "body": "x"}, "room_id": otherRoom})
	ciphertext, _ = e.peer.w.EncryptMegolm(idSession(e.sid), otherRoomPlain)
	_, refusal, err = VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$room", nil, func(m map[string]any) { m["ciphertext"] = string(ciphertext) }, nil)))
	expectRefusal(t, refusal, err, "6", ErrSenderMismatch)
	for name, inner := range map[string]map[string]any{
		"thread":    {"msgtype": MsgTypeText, "body": "reply", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$root"}},
		"formatted": {"msgtype": MsgTypeText, "body": "**b**", "format": "org.matrix.custom.html", "formatted_body": "<b>b</b>"},
	} {
		if accepted, refusal, err := VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$ok-"+name, inner, nil, nil))); err != nil || refusal != nil || !accepted.Populated() {
			t.Fatal(name, "control", err, refusal)
		}
	}
}

// A multi-failure fixture pins stage precedence: one event that is malformed,
// then (repaired) retained in refusal history, then fails stages 1, 2, 3, 4,
// 5, 6 and 7 at once with a replay collision at its index, reports each stage
// in order as the previous fault is repaired (0 -> 0a -> 1 -> ... -> 7, where
// 7 wins over the pending stage 8 collision); a second event then fails stage
// 8 and finally is accepted. Refused steps reserve no replay state. Swapping
// any two adjacent stages from 0 to 8 changes at least one of these verdicts.
func TestRoomEventStagePrecedence(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	impostor := newParty(t, impostorMXID, impostorDev)
	e.peers[impostorMXID] = impostor.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, impostorMXID)})
	edit := map[string]any{"msgtype": MsgTypeText, "body": "* edit", "m.relates_to": map[string]any{"rel_type": RelationReplace, "event_id": "$1"}}
	multi := e.megolm(e.peer, e.sid, "$multi", edit, func(m map[string]any) { m["algorithm"] = "m.megolm.v2.aes-sha2" }, func(w map[string]any) { w["sender"] = impostorMXID })
	var wire roomWire
	_ = json.Unmarshal(multi, &wire)
	multiID := RoomRefusalID(dmRoom, "$multi", rawDigest(wire.Content))
	malformed := json.RawMessage(`{"type":"m.room.encrypted","type":"m.room.encrypted","sender":"@peer:example.test","room_id":"!dm:example.test","event_id":"$multi","content":{}}`)
	req := e.room(adm, malformed)
	req.Context = RoomContext{State: ContextUnknown}
	req.Retained[multiID] = ErrDeviceChanged
	tomb := e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
	tomb.Devices[peerDev] = DeviceObservation{EligibilityTombstoned, tomb.Devices[peerDev].Keys}
	tomb.SelfSigning = nil
	req.Peers[peerMXID] = tomb
	rec := req.Sessions[e.sid]
	rec.Source = "forwarded"
	req.Sessions[e.sid] = rec
	// The forked renders below all sit at index 1; a collision waits there.
	req.Replay[e.sid] = ReplayWindow{Entries: map[uint64]ReplayEntry{1: {Event: "$earlier", Digest: TupleHash("x")}}}
	steps := []precedenceStep{
		{"0", ErrMalformedEvent, func() { req.Raw = multi }},
		{"0a", ErrDeviceChanged, func() { delete(req.Retained, multiID) }},
		{"1", ErrContextUnknown, func() { req.Context = dmContext(localMXID, peerMXID) }},
		{"2", ErrUnsupportedAlgorithm, func() {
			req.Raw = e.megolmSameIndex(t, e.peer, e.sid, "$multi", edit, nil, func(w map[string]any) { w["sender"] = impostorMXID })
		}},
		{"3", ErrDeviceUnverified, func() { tomb.Devices[peerDev] = DeviceObservation{EligibilityEligible, tomb.Devices[peerDev].Keys} }},
		{"4", ErrSelfSigningMissing, func() {
			ssk, _ := e.peer.keys.SelfSigningKey(peerMXID)
			tomb.SelfSigning = &ssk
		}},
		{"5", ErrKeyProvenanceUnverified, func() {
			rec.Source = KeySourceDirectOlm
			req.Sessions[e.sid] = rec
		}},
		{"6", ErrSenderMismatch, func() {
			req.Raw = e.megolmSameIndex(t, e.peer, e.sid, "$multi", edit, nil, nil)
		}},
		// Stage 7 fault plus the stage 8 collision at index 1: 7 wins.
		{"7", ErrContentUnsupported, nil},
	}
	if err := checkPrecedence(&req, steps); err != nil {
		t.Fatal(err)
	}
	if len(req.Replay[e.sid].Entries) != 1 || len(req.Events.Entries) != 0 {
		t.Fatal("a refused step reserved replay state")
	}
	// Stage 8 and acceptance on a second genuine event at the colliding index.
	req.Raw = e.megolm(e.peer, e.sid, "$second", nil, nil, nil)
	if err := checkPrecedence(&req, []precedenceStep{{"8", ErrReplay, func() { req.Replay[e.sid] = ReplayWindow{} }}}); err != nil {
		t.Fatal(err)
	}
	if accepted, refusal, err := VerifyRoomEvent(req); err != nil || refusal != nil || !accepted.Populated() || accepted.Index() != 1 {
		t.Fatal("fully repaired event refused", err, refusal)
	}
}

// megolmSameIndex renders a fresh wire event whose ciphertext is encrypted by
// a fork of the party's outbound session, so repeated renders share the index.
func (e *env) megolmSameIndex(t *testing.T, from *party, sid, eventID string, inner map[string]any, contentEdit func(map[string]any), wireEdit func(map[string]any)) json.RawMessage {
	t.Helper()
	fork, err := from.w.Fork(from.key)
	if err != nil {
		t.Fatal(err)
	}
	copy := *from
	copy.w = fork
	return e.megolm(&copy, sid, eventID, inner, contentEdit, wireEdit)
}

// precedenceStep is one expected verdict of the precedence walk and the repair
// that removes exactly that fault; repairs write into the shared request.
type precedenceStep struct {
	stage  string
	reason Code
	repair func()
}

// checkPrecedence is the killer assertion of the stage-order plant: each step
// must report exactly its stage and reason before its repair is applied. The
// request is shared by pointer so value-field repairs (Context, Raw) are seen;
// a mismatch reports both the expected and the observed verdict.
func checkPrecedence(req *RoomEventRequest, steps []precedenceStep) error {
	for _, step := range steps {
		accepted, refusal, err := VerifyRoomEvent(*req)
		if err != nil {
			return fmt.Errorf("want stage %s %s, got operation error %v", step.stage, step.reason, err)
		}
		observed := "accepted"
		if refusal != nil {
			observed = "stage " + refusal.Stage + " " + string(refusal.Reason)
		}
		if accepted != nil || refusal == nil || refusal.Stage != step.stage || refusal.Reason != step.reason {
			return fmt.Errorf("want stage %s %s, got %s", step.stage, step.reason, observed)
		}
		if step.repair != nil {
			step.repair()
		}
	}
	return nil
}

// Stage 0a: a recorded refusal is returned by its saved reason without
// re-decryption; a room filter hit without retained detail is
// e2ee_refusal_history_blocked; an unrecorded event beside them is judged.
func TestRoomEventRefusalHistory(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	var retention Retention
	if err := retention.AdmitRoom(TupleHash(dmRoom)); err != nil {
		t.Fatal(err)
	}
	flipped := e.megolm(e.peer, e.sid, "$1", nil, func(m map[string]any) { m["ciphertext"] = flipLastByte(t, m["ciphertext"].(string)) }, nil)
	req := e.room(adm, flipped)
	req.Retention = &retention
	_, refusal, err := VerifyRoomEvent(req)
	expectRefusal(t, refusal, err, "6", ErrCiphertextInvalid)
	if err := RecordRefusal(req.Retained, &retention, *refusal); err != nil {
		t.Fatal(err)
	}
	_, again, err := VerifyRoomEvent(req)
	expectRefusal(t, again, err, "0a", ErrCiphertextInvalid)
	if again.ID != refusal.ID {
		t.Fatal("retained identity")
	}
	req.Retained = map[string]Code{}
	_, blocked, err := VerifyRoomEvent(req)
	expectRefusal(t, blocked, err, "0a", ErrRefused)
	good := e.room(adm, e.megolm(e.peer, e.sid, "$fresh", nil, nil, nil))
	good.Retention = &retention
	if accepted, refusal, err := VerifyRoomEvent(good); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("unrecorded control", err, refusal)
	}
}

// Own echoes: an event under this account's own outbound session, recorded as
// own-outbound and still present among its outbound sessions, with the outer
// sender equal to the local MXID, is accepted with key source
// own-outbound-session. The same ciphertext under the peer's outer MXID is
// sender_mismatch; an own-outbound record naming another device, or without
// the outbound session, is provenance_unverified.
func TestRoomEventOwnOutboundEcho(t *testing.T) {
	e := newEnv(t)
	osid, oshared, err := e.local.w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.local.w.AcceptDirectMegolm(oshared); err != nil {
		t.Fatal(err)
	}
	rooms := map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)}
	e.peers[localMXID] = e.local.observation(t, rooms)
	own := SessionRecord{SessionID: string(osid), RoomID: dmRoom, Originator: localMXID, Device: localDev, Ed25519: e.local.ed, Curve25519: e.local.curve, Source: KeySourceOwnOutbound}
	echo := e.megolm(e.local, string(osid), "$echo", nil, nil, nil)
	req := RoomEventRequest{Binding: testBinding, Raw: echo, Context: dmContext(localMXID, peerMXID), Local: e.local.local(), Peers: e.peers,
		Sessions: map[string]SessionRecord{string(osid): own}, Working: e.local.w, Replay: map[string]ReplayWindow{}, Events: &EventReplay{}, Retained: map[string]Code{}}
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !accepted.Populated() || accepted.KeySource() != KeySourceOwnOutbound {
		t.Fatal("own echo control", err, refusal)
	}
	forged := req
	forged.Raw = e.megolmSameIndex(t, e.local, string(osid), "$echo", nil, nil, func(w map[string]any) { w["sender"] = peerMXID })
	_, refusal, err = VerifyRoomEvent(forged)
	expectRefusal(t, refusal, err, "6", ErrSenderMismatch)
	otherDevice := own
	otherDevice.Device = "OTHERDEV"
	forged.Raw = echo
	forged.Sessions = map[string]SessionRecord{string(osid): otherDevice}
	_, refusal, err = VerifyRoomEvent(forged)
	expectRefusal(t, refusal, err, "3", ErrDeviceUnknown)
	peerClaim := own
	peerClaim.Originator, peerClaim.Device, peerClaim.Ed25519, peerClaim.Curve25519 = peerMXID, peerDev, e.peer.ed, e.peer.curve
	forged.Sessions = map[string]SessionRecord{string(osid): peerClaim}
	_, refusal, err = VerifyRoomEvent(forged)
	expectRefusal(t, refusal, err, "5", ErrKeyProvenanceUnverified)
	// Without the outbound session the inbound copy alone is not own evidence.
	inboundOnly, err := e.local.w.Fork(e.local.key)
	if err != nil {
		t.Fatal(err)
	}
	delete(inboundOnly.outbound, string(osid))
	forged.Sessions = map[string]SessionRecord{string(osid): own}
	forged.Working = inboundOnly
	_, refusal, err = VerifyRoomEvent(forged)
	expectRefusal(t, refusal, err, "5", ErrKeyProvenanceUnverified)
}
