package e2ee

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"
)

// A direct m.room_key Olm-decrypted from the eligible cross-signed peer device
// is admitted into an isolated fork carrying the Megolm session, the Olm
// session and a direct-olm record bound to that device's keys; the committed
// account keeps its one-time key and gains no session.
func TestToDeviceAcceptsDirectRoomKey(t *testing.T) {
	e := newEnv(t)
	before := checkpoint(t, e.local.w, e.local.key)
	adm := e.admit()
	if !sameState(before, checkpoint(t, e.local.w, e.local.key)) {
		t.Fatal("acceptance mutated the committed account")
	}
	if adm.Prepared.inbound[e.sid] == nil || len(adm.Prepared.account.OTKeys) != 0 || adm.Prepared.olm[adm.OlmSession] == nil {
		t.Fatal("prepared fork lacks the admitted state")
	}
	if len(e.local.w.account.OTKeys) != 1 || len(e.local.w.inbound) != 0 || len(e.local.w.olm) != 0 {
		t.Fatal("committed account changed")
	}
	want := SessionRecord{SessionID: e.sid, RoomID: dmRoom, Originator: peerMXID, Device: peerDev, Ed25519: e.peer.ed, Curve25519: e.peer.curve, Source: KeySourceDirectOlm}
	if adm.Session != want {
		t.Fatal("provenance record does not bind the authenticated device")
	}
}

// rejected runs one to-device negative, asserts its exact stage/reason and
// proves the committed account, OTKs and sessions are byte-identical after.
func (e *env) rejected(raw json.RawMessage, stage string, reason Code) *Refusal {
	e.t.Helper()
	e.budget = &AttemptBudget{}
	before := checkpoint(e.t, e.local.w, e.local.key)
	adm, refusal, err := VerifyToDevice(e.toDevice(raw))
	expectRefusal(e.t, refusal, err, stage, reason)
	if adm != nil {
		e.t.Fatal("refusal returned an admission")
	}
	if !sameState(before, checkpoint(e.t, e.local.w, e.local.key)) || len(e.local.w.account.OTKeys) != 1 || len(e.local.w.inbound) != 0 || len(e.local.w.olm) != 0 {
		e.t.Fatal("rejected attempt mutated the committed account or sessions")
	}
	return refusal
}

// Forwarded, imported and backup keys never become sessions: an Olm payload
// typed m.forwarded_room_key (room_key shaped or real forwarded shape), a
// secret-share payload, an unencrypted m.room_key, and an m.room_key whose
// session_key is in export/backup format all refuse with
// e2ee_key_provenance_unverified; the genuine direct key beside them admits.
func TestToDeviceRefusesForwardedImportedBackupKeys(t *testing.T) {
	e := newEnv(t)
	e.admit()
	exportKey := base64.RawStdEncoding.EncodeToString(append([]byte{1}, make([]byte, 164)...))
	cases := []struct {
		name  string
		raw   json.RawMessage
		stage string
	}{
		{"forwarded-type-room-key-shape", e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { p["type"] = "m.forwarded_room_key" }), nil), "key-type"},
		{"forwarded-real-shape", e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) {
			p["type"] = "m.forwarded_room_key"
			c := contentOf(p)
			c["session_key"] = exportKey
			c["sender_key"] = e.peer.curve
			c["sender_claimed_ed25519_key"] = e.peer.ed
			c["forwarding_curve25519_key_chain"] = []string{e.peer.curve}
		}), nil), "key-type"},
		{"secret-send", e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { p["type"] = "m.secret.send" }), nil), "key-type"},
		{"backup-format-session-key", e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { contentOf(p)["session_key"] = exportKey }), nil), "session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { e.rejected(tc.raw, tc.stage, ErrKeyProvenanceUnverified) })
	}
	// Unencrypted key-carrying to-device types fail at the algorithm step as
	// provenance, before any decrypt attempt.
	plainKey, _ := json.Marshal(map[string]any{"type": TypeRoomKey, "sender": peerMXID, "content": map[string]any{"algorithm": AlgorithmMegolm, "room_id": dmRoom, "session_id": e.sid, "session_key": string(e.shared)}})
	e.rejected(plainKey, "algorithm", ErrKeyProvenanceUnverified)
	forwardedPlain, _ := json.Marshal(map[string]any{"type": "m.forwarded_room_key", "sender": peerMXID, "content": map[string]any{"algorithm": AlgorithmMegolm}})
	e.rejected(forwardedPlain, "algorithm", ErrKeyProvenanceUnverified)
	e.budget = &AttemptBudget{}
	e.admit()
}

// A room key Olm-decrypted from a device that is not eligible refuses at the
// stage that fails: tombstoned, changed and Q4-disallowed devices at stage 3,
// a device not in the completed list as device_unknown, an unanchored sender
// as sender_not_eligible, and chain faults at stage 4 (absent self-signing
// key or signature: self_signing_missing; forged signatures or a master that
// is not the anchored root: ciphertext_invalid). An incomplete query is a
// batch abort, not a verdict. The unchanged chain beside each case admits.
func TestToDeviceRefusesNonEligibleDevice(t *testing.T) {
	e := newEnv(t)
	raw := e.roomKeyEvent()
	obs := func() *PeerObservation {
		return e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
	}
	impostor := newParty(t, peerMXID, peerDev)
	for _, tc := range []struct {
		name   string
		edit   func(*PeerObservation)
		stage  string
		reason Code
	}{
		{"tombstoned", func(p *PeerObservation) {
			p.Devices[peerDev] = DeviceObservation{EligibilityTombstoned, p.Devices[peerDev].Keys}
		}, "3", ErrDeviceUnverified},
		{"changed", func(p *PeerObservation) {
			p.Devices[peerDev] = DeviceObservation{EligibilityChanged, p.Devices[peerDev].Keys}
		}, "3", ErrDeviceChanged},
		{"q4-disallowed", func(p *PeerObservation) {
			p.Devices[peerDev] = DeviceObservation{EligibilityUnverified, p.Devices[peerDev].Keys}
		}, "3", ErrDeviceUnverified},
		{"unknown-label", func(p *PeerObservation) { p.Devices[peerDev] = DeviceObservation{"trusted", p.Devices[peerDev].Keys} }, "3", ErrDeviceUnverified},
		{"not-in-list", func(p *PeerObservation) { delete(p.Devices, peerDev) }, "3", ErrDeviceUnknown},
		{"other-device-publishes-our-key", func(p *PeerObservation) {
			other := p.Devices[peerDev]
			other.Keys.DeviceID = "OTHERDEV"
			other.Keys.Keys = map[string]string{"ed25519:OTHERDEV": e.peer.ed, "curve25519:OTHERDEV": e.peer.curve}
			delete(p.Devices, peerDev)
			p.Devices["OTHERDEV"] = other
		}, "4", ErrCiphertextInvalid},
		{"unanchored-master", func(p *PeerObservation) { p.AnchoredMaster = impostor.keys.Master().Public }, "4", ErrCiphertextInvalid},
		{"master-wrong-mxid", func(p *PeerObservation) { p.Master.UserID = impostorMXID }, "4", ErrCiphertextInvalid},
		{"master-wrong-usage", func(p *PeerObservation) { p.Master.Usage = []string{UsageSelfSigning} }, "4", ErrCiphertextInvalid},
		{"ssk-absent", func(p *PeerObservation) { p.SelfSigning = nil }, "4", ErrSelfSigningMissing},
		{"ssk-unsigned", func(p *PeerObservation) {
			ssk := *p.SelfSigning
			ssk.Signatures = nil
			p.SelfSigning = &ssk
		}, "4", ErrSelfSigningMissing},
		{"ssk-forged", func(p *PeerObservation) {
			// Present under the anchored master's key ID, but made by another master.
			ssk := *p.SelfSigning
			forged, _ := impostor.keys.SignAsMaster(SignedKey{UserID: ssk.UserID, Usage: ssk.Usage, Keys: ssk.Keys})
			ssk.Signatures = map[string]map[string]string{peerMXID: {e.peer.keys.Master().ID: forged}}
			p.SelfSigning = &ssk
		}, "4", ErrCiphertextInvalid},
		{"device-not-cross-signed", func(p *PeerObservation) {
			d := p.Devices[peerDev]
			d.Keys.Signatures = map[string]map[string]string{peerMXID: {"ed25519:" + peerDev: d.Keys.Signatures[peerMXID]["ed25519:"+peerDev]}}
			p.Devices[peerDev] = d
		}, "4", ErrSelfSigningMissing},
		{"device-cross-signed-by-impostor", func(p *PeerObservation) {
			d := p.Devices[peerDev]
			unsigned := d.Keys
			unsigned.Signatures = map[string]map[string]string{peerMXID: {"ed25519:" + peerDev: d.Keys.Signatures[peerMXID]["ed25519:"+peerDev]}}
			forged, _ := impostor.keys.SignDevice(unsigned)
			forged.Signatures[peerMXID][e.peer.keys.SelfSigning().ID] = forged.Signatures[peerMXID][impostor.keys.SelfSigning().ID]
			delete(forged.Signatures[peerMXID], impostor.keys.SelfSigning().ID)
			d.Keys = forged
			p.Devices[peerDev] = d
		}, "4", ErrCiphertextInvalid},
		{"device-self-signature-forged", func(p *PeerObservation) {
			d := p.Devices[peerDev]
			d.Keys.Signatures = map[string]map[string]string{peerMXID: {}}
			for k, v := range p.Devices[peerDev].Keys.Signatures[peerMXID] {
				d.Keys.Signatures[peerMXID][k] = v
			}
			d.Keys.Signatures[peerMXID]["ed25519:"+peerDev] = impostor.signed.Signatures[peerMXID]["ed25519:"+peerDev]
			p.Devices[peerDev] = d
		}, "4", ErrCiphertextInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := obs()
			tc.edit(p)
			e.peers = map[string]*PeerObservation{peerMXID: p}
			e.rejected(raw, tc.stage, tc.reason)
		})
	}
	// Sender outside the anchored set, and the local identity as sender.
	e.peers = map[string]*PeerObservation{}
	e.rejected(raw, "sender", ErrSenderNotEligible)
	e.peers = map[string]*PeerObservation{peerMXID: obs()}
	self := e.olmEvent(e.peer, e.roomKeyPlaintext(nil), func(w map[string]any) { w["sender"] = localMXID })
	e.rejected(self, "sender", ErrSenderNotEligible)
	// Incomplete query: abort, no verdict, no mutation.
	partial := obs()
	partial.QueryComplete = false
	e.peers = map[string]*PeerObservation{peerMXID: partial}
	before := checkpoint(t, e.local.w, e.local.key)
	if adm, refusal, err := VerifyToDevice(e.toDevice(raw)); err != ErrKeyQueryUnavailable || adm != nil || refusal != nil {
		t.Fatal("partial query produced a verdict", err, refusal)
	}
	if !sameState(before, checkpoint(t, e.local.w, e.local.key)) {
		t.Fatal("aborted batch mutated the account")
	}
	e.peers = map[string]*PeerObservation{peerMXID: obs()}
	e.budget = &AttemptBudget{}
	e.admit()
}

// A room key for a room that is not the reconciled joined two-member DM with
// the authenticated sender refuses as sender_not_eligible: an unknown room, a
// room with a third joined member, a plaintext-confirmed room and a DM with a
// different peer. A retired-session tombstone refuses as history blocked.
// The DM room beside them admits.
func TestToDeviceRefusesDifferentRoom(t *testing.T) {
	e := newEnv(t)
	e.admit()
	rooms := func(r map[string]RoomContext) {
		p := e.peer.observation(t, r)
		e.peers = map[string]*PeerObservation{peerMXID: p}
	}
	otherRoomKey := e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { contentOf(p)["room_id"] = otherRoom }), nil)
	rooms(map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
	e.rejected(otherRoomKey, "room", ErrSenderNotEligible)
	dmKey := e.roomKeyEvent()
	rooms(map[string]RoomContext{dmRoom: {State: ContextEncrypted, Members: []string{localMXID, peerMXID, impostorMXID}}})
	e.rejected(dmKey, "sender", ErrSenderNotEligible)
	rooms(map[string]RoomContext{dmRoom: {State: ContextPlaintext, Members: []string{localMXID, peerMXID}}})
	e.rejected(dmKey, "sender", ErrSenderNotEligible)
	rooms(map[string]RoomContext{dmRoom: dmContext(localMXID, impostorMXID), otherRoom: dmContext(localMXID, peerMXID)})
	e.rejected(dmKey, "room", ErrSenderNotEligible)
	rooms(map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
	var retention Retention
	if err := retention.AdmitRoom(TupleHash(dmRoom)); err != nil {
		t.Fatal(err)
	}
	if err := retention.Retire(TupleHash(dmRoom), TupleHash(e.sid), false); err != nil {
		t.Fatal(err)
	}
	e.budget = &AttemptBudget{}
	req := e.toDevice(dmKey)
	req.Retention = &retention
	adm, refusal, err := VerifyToDevice(req)
	expectRefusal(t, refusal, err, "room", ErrExpired)
	if adm != nil {
		t.Fatal("retired session readmitted")
	}
	e.budget = &AttemptBudget{}
	e.admit()
}

// The Olm-authenticated bindings are enforced after decryption: a plaintext
// whose sender, sender_device, Ed25519 claim, recipient or recipient key
// disagrees with the outer sender, the chain device or the local identity is
// e2ee_sender_mismatch; a session_id not matching the derived session is too.
// A key Olm-sent by a device other than the one the sender_key hint names is
// undecryptable, and a tampered Olm body is ciphertext_invalid.
func TestToDeviceAuthenticatedBindings(t *testing.T) {
	e := newEnv(t)
	e.admit()
	impostor := newParty(t, peerMXID, impostorDev)
	for name, edit := range map[string]func(map[string]any){
		"sender":          func(p map[string]any) { p["sender"] = impostorMXID },
		"sender-device":   func(p map[string]any) { p["sender_device"] = impostorDev },
		"ed25519-claim":   func(p map[string]any) { p["keys"] = map[string]any{"ed25519": impostor.ed} },
		"recipient":       func(p map[string]any) { p["recipient"] = impostorMXID },
		"recipient-key":   func(p map[string]any) { p["recipient_keys"] = map[string]any{"ed25519": impostor.ed} },
		"inner-algorithm": func(p map[string]any) { contentOf(p)["algorithm"] = "m.megolm.v2.aes-sha2" },
		"session-id":      func(p map[string]any) { contentOf(p)["session_id"] = strings.Repeat("A", 43) },
	} {
		t.Run(name, func(t *testing.T) {
			stage, reason := "bindings", ErrSenderMismatch
			switch name {
			case "inner-algorithm":
				stage, reason = "key-type", ErrUnsupportedAlgorithm
			case "session-id":
				stage, reason = "session", ErrSenderMismatch
			}
			e.rejected(e.olmEvent(e.peer, e.roomKeyPlaintext(edit), nil), stage, reason)
		})
	}
	// Olm sent by a different account while the outer hint names the peer
	// device: the prekey identity key does not match, so it cannot decrypt.
	wrongIdentity := e.olmEvent(impostor, e.roomKeyPlaintext(nil), func(w map[string]any) {
		w["sender"] = peerMXID
		contentOf(w)["sender_key"] = e.peer.curve
	})
	e.rejected(wrongIdentity, "decrypt", ErrUndecryptable)
	tampered := e.olmEvent(e.peer, e.roomKeyPlaintext(nil), func(w map[string]any) {
		box := contentOf(w)["ciphertext"].(map[string]any)[e.local.curve].(map[string]any)
		box["body"] = flipLastByte(t, box["body"].(string))
	})
	e.rejected(tampered, "decrypt", ErrCiphertextInvalid)
	e.budget = &AttemptBudget{}
	e.admit()
}

// The to-device prefix is evaluated in exact order: a malformed event wins
// over an ineligible sender; an ineligible sender wins over a budget that is
// already exhausted for every peer and for the identity (and that exhausted
// budget does refuse an eligible peer); the budget wins over an unsupported
// algorithm; the algorithm wins over a device fault; the 21st attempt by one
// peer within a minute is rate limited while the next minute admits again.
// Malformed events carry an identity built from empty components and the raw
// digest.
func TestToDevicePrefixOrderAndBudget(t *testing.T) {
	e := newEnv(t)
	e.admit()
	duplicate := []byte(`{"type":"m.room.encrypted","type":"m.room.encrypted","sender":"@nobody:example.test","content":{}}`)
	r := e.rejected(duplicate, "malformed", ErrMalformedEvent)
	if r.ID != ToDeviceRefusalID(testBinding, "", "", []byte(rawDigest(duplicate))) || r.Sender != "" {
		t.Fatal("malformed identity")
	}
	noContent, _ := json.Marshal(map[string]any{"type": TypeEncrypted, "sender": peerMXID, "content": "text"})
	e.rejected(noContent, "malformed", ErrMalformedEvent)
	// Ineligible sender with an exhausted budget and a bad algorithm: sender
	// wins. The budget is exhausted in place for this minute, for the stranger,
	// the peer and the identity; the peer's own event proves it is live.
	stranger := e.olmEvent(e.peer, e.roomKeyPlaintext(nil), func(w map[string]any) {
		w["sender"] = impostorMXID
		contentOf(w)["algorithm"] = "m.olm.v2"
	})
	exhausted := &AttemptBudget{WindowMS: 60_000, Peers: map[string]int{impostorMXID: MaxAttemptsPeer, peerMXID: MaxAttemptsPeer}, Identity: MaxAttemptsLocal}
	req := e.toDevice(stranger)
	req.Budget = exhausted
	if _, refusal, err := VerifyToDevice(req); err != nil || refusal == nil || refusal.Reason != ErrSenderNotEligible || refusal.Stage != "sender" {
		t.Fatal("stranger under exhausted budget", err, refusal)
	}
	req.Raw = e.roomKeyEvent()
	if _, refusal, err := VerifyToDevice(req); err != nil || refusal == nil || refusal.Reason != ErrRateLimited || refusal.Stage != "budget" {
		t.Fatal("exhausted budget control", err, refusal)
	}
	// Budget before algorithm and device: exhaust the peer, then a bad
	// algorithm from a tombstoned device is still rate_limited.
	e.budget = &AttemptBudget{}
	badAlgorithm := e.olmEvent(e.peer, e.roomKeyPlaintext(nil), func(w map[string]any) { contentOf(w)["algorithm"] = "m.olm.v2" })
	for i := 0; i < MaxAttemptsPeer; i++ {
		if _, refusal, err := VerifyToDevice(e.toDevice(badAlgorithm)); err != nil || refusal == nil || refusal.Reason != ErrUnsupportedAlgorithm || refusal.Stage != "algorithm" {
			t.Fatal("algorithm refusal within budget", err, refusal)
		}
	}
	tomb := e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
	tomb.Devices[peerDev] = DeviceObservation{EligibilityTombstoned, tomb.Devices[peerDev].Keys}
	e.peers = map[string]*PeerObservation{peerMXID: tomb}
	if _, refusal, err := VerifyToDevice(e.toDevice(badAlgorithm)); err != nil || refusal == nil || refusal.Reason != ErrRateLimited || refusal.Stage != "budget" {
		t.Fatal("21st attempt not rate limited", err, refusal)
	}
	req = e.toDevice(badAlgorithm)
	req.NowMS = 120_000
	if _, refusal, err := VerifyToDevice(req); err != nil || refusal == nil || refusal.Reason != ErrUnsupportedAlgorithm {
		t.Fatal("new minute did not reset the budget", err, refusal)
	}
	// Algorithm wins over the tombstoned device; with a good algorithm the
	// tombstone is the verdict.
	e.rejected(badAlgorithm, "algorithm", ErrUnsupportedAlgorithm)
	e.rejected(e.roomKeyEvent(), "3", ErrDeviceUnverified)
	// Identity budget: 100 attempts across peers in one minute.
	e.peers = map[string]*PeerObservation{peerMXID: e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})}
	var b AttemptBudget
	for i := 0; i < MaxAttemptsLocal; i++ {
		if !b.take("@p"+strings.Repeat("x", i%7)+":example.test", 5) {
			t.Fatal("identity budget exhausted early")
		}
	}
	if b.take("@fresh:example.test", 5) {
		t.Fatal("identity budget not enforced")
	}
	e.budget = &AttemptBudget{}
	e.admit()
}

// M1: the sender_key hint must resolve to exactly one device. Two listed
// devices publishing the same Curve25519 key refuse deterministically as
// e2ee_device_changed at stage 3, whether the second is eligible or
// tombstoned and whichever map order the lookup visits; removing the
// duplicate admits the same event.
func TestToDeviceAmbiguousSenderKey(t *testing.T) {
	e := newEnv(t)
	raw := e.roomKeyEvent()
	for _, status := range []EligibilityStatus{EligibilityEligible, EligibilityTombstoned} {
		for round := 0; round < 8; round++ {
			obs := e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})
			twin := obs.Devices[peerDev]
			twin.Status = status
			twin.Keys.DeviceID = "TWINDEV"
			twin.Keys.Keys = map[string]string{"ed25519:TWINDEV": e.peer.ed, "curve25519:TWINDEV": e.peer.curve}
			obs.Devices["TWINDEV"] = twin
			e.peers = map[string]*PeerObservation{peerMXID: obs}
			if err := checkAmbiguousSenderKeyRefused(e.toDevice(raw)); err != nil {
				t.Fatal(status, err)
			}
		}
	}
	e.peers = map[string]*PeerObservation{peerMXID: e.peer.observation(t, map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)})}
	e.budget = &AttemptBudget{}
	e.admit()
}

// checkAmbiguousSenderKeyRefused is the killer assertion of the ambiguity
// plant: a hint matching two devices must be e2ee_device_changed at stage 3.
func checkAmbiguousSenderKeyRefused(req ToDeviceRequest) error {
	req.Budget = &AttemptBudget{}
	adm, refusal, err := VerifyToDevice(req)
	if err != nil {
		return err
	}
	if adm != nil || refusal == nil || refusal.Stage != "3" || refusal.Reason != ErrDeviceChanged {
		return fmt.Errorf("want stage 3 %s, got %v", ErrDeviceChanged, refusal)
	}
	return nil
}

// A second Olm message (type 1) is tried only against the sessions indexed
// for its sender key: with the index it admits a further key; without it or
// with a tampered body it is undecryptable, leaving the committed fork intact.
func TestToDeviceNormalOlmMessageUsesSenderIndex(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	// Continue the same Olm session the admitted prekey message established:
	// the peer's outbound session is the one keyed in its map; find it by
	// trying to encrypt from each until the local fork decrypts.
	sid2, shared2, err := e.peer.w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	plain := e.roomKeyPlaintext(func(p map[string]any) {
		contentOf(p)["session_id"] = string(sid2)
		contentOf(p)["session_key"] = string(shared2)
	})
	// The admitted fork has one Olm session; reply over it so the peer side
	// switches to normal messages, then encrypt the second key.
	kind, reply, err := adm.Prepared.EncryptOlm(idSession(adm.OlmSession), []byte(`{"type":"m.dummy","content":{}}`))
	if err != nil || kind != id.OlmMsgTypeMsg {
		t.Fatal("reply control", err)
	}
	var peerSession string
	for s := range e.peer.w.olm {
		if _, err := e.peer.w.DecryptOlm(idSession(s), kind, string(reply)); err == nil {
			peerSession = s
		}
	}
	if peerSession == "" {
		t.Fatal("peer did not receive the reply on any session")
	}
	kind, body, err := e.peer.w.EncryptOlm(idSession(peerSession), plain)
	if err != nil || kind != id.OlmMsgTypeMsg {
		t.Fatal("normal message control", err)
	}
	wire, _ := json.Marshal(map[string]any{"type": TypeEncrypted, "sender": peerMXID, "content": map[string]any{
		"algorithm": AlgorithmOlm, "sender_key": e.peer.curve,
		"ciphertext": map[string]any{e.local.curve: map[string]any{"type": 1, "body": string(body)}},
	}})
	req := e.toDevice(wire)
	req.Working = adm.Prepared
	before := checkpoint(t, adm.Prepared, e.local.key)
	if _, refusal, err := VerifyToDevice(req); err != nil || refusal == nil || refusal.Reason != ErrUndecryptable {
		t.Fatal("unindexed session decrypted", err, refusal)
	}
	req.OlmSessions = map[string][]string{e.peer.curve: {adm.OlmSession}}
	tampered := req
	tamperedWire, _ := json.Marshal(map[string]any{"type": TypeEncrypted, "sender": peerMXID, "content": map[string]any{
		"algorithm": AlgorithmOlm, "sender_key": e.peer.curve,
		"ciphertext": map[string]any{e.local.curve: map[string]any{"type": 1, "body": flipLastByte(t, string(body))}},
	}})
	tampered.Raw = tamperedWire
	if _, refusal, err := VerifyToDevice(tampered); err != nil || refusal == nil || refusal.Reason != ErrUndecryptable {
		t.Fatal("tampered normal message admitted", err, refusal)
	}
	if !sameState(before, checkpoint(t, adm.Prepared, e.local.key)) {
		t.Fatal("refused normal messages mutated the prepared state")
	}
	second, refusal, err := VerifyToDevice(req)
	if err != nil || refusal != nil || second == nil || second.Session.SessionID != string(sid2) || second.OlmSession != adm.OlmSession {
		t.Fatal("indexed normal message control", err, refusal)
	}
	if second.Prepared.inbound[string(sid2)] == nil || adm.Prepared.inbound[string(sid2)] != nil {
		t.Fatal("second key not isolated in its own fork")
	}
}
