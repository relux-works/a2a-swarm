package e2ee

import (
	"encoding/json"
	"strings"
	"testing"
)

// A member name that case-folds to a known field but differs is refused at
// every depth, in both decoders and on every ingress path that uses them:
// decrypted Megolm content (M.RELATES_TO beside or instead of m.relates_to:
// stage 6 malformed), the Olm plaintext's nested keys object (ED25519), the
// outer room wire (Sender), the U2a queried master and device objects
// (USER_ID, DEVICE_ID: published conflict) and a strict record (Cursor TOKEN).
// Controls beside each: the exact names decode, and unknown members that fold
// to no field (origin_server_ts, extra) stay ignored.
func TestCaseFoldedMemberNamesRefused(t *testing.T) {
	e := newEnv(t)
	adm := e.admit()
	for name, inner := range map[string]map[string]any{
		"beside":  {"msgtype": MsgTypeText, "body": "x", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$r"}, "M.RELATES_TO": map[string]any{"rel_type": RelationReplace, "event_id": "$r"}},
		"instead": {"msgtype": MsgTypeText, "body": "x", "M.RELATES_TO": map[string]any{"rel_type": RelationReplace, "event_id": "$r"}},
		"nested":  {"msgtype": MsgTypeText, "body": "x", "m.relates_to": map[string]any{"REL_TYPE": RelationReplace, "event_id": "$r"}},
	} {
		t.Run("megolm-"+name, func(t *testing.T) {
			req := e.room(adm, e.megolm(e.peer, e.sid, "$"+name, inner, nil, nil))
			if err := checkCaseFoldRefused(req); err != nil {
				t.Fatal(err)
			}
			noReplayState(t, req)
		})
	}
	thread := map[string]any{"msgtype": MsgTypeText, "body": "x", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$r"}}
	if accepted, refusal, err := VerifyRoomEvent(e.room(adm, e.megolm(e.peer, e.sid, "$ok", thread, nil, nil))); err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("exact relation control", err, refusal)
	}
	// Outer wire: a folded duplicate of sender is malformed at stage 0; the
	// fixture's origin_server_ts (no such field) is ignored.
	good := e.megolm(e.peer, e.sid, "$wire", nil, nil, nil)
	folded := []byte(strings.Replace(string(good), `"sender":`, `"Sender":"@x:example.test","sender":`, 1))
	if string(folded) == string(good) || !json.Valid(folded) {
		t.Fatal("wire plant did not apply")
	}
	_, refusal, err := VerifyRoomEvent(e.room(adm, folded))
	expectRefusal(t, refusal, err, "0", ErrMalformedEvent)
	// Olm plaintext, nested keys object.
	e.budget = &AttemptBudget{}
	nested := e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { p["keys"] = map[string]any{"ED25519": e.peer.ed} }), nil)
	_, refusal, err = VerifyToDevice(e.toDevice(nested))
	expectRefusal(t, refusal, err, "decrypt", ErrMalformedEvent)
	// U2a queried objects through the signingInput path.
	owner, pin, _ := ownerPin(t)
	ev, _, master := agentEvidence(t, pin)
	genuine, err := owner.SignAsUser(master)
	if err != nil {
		t.Fatal(err)
	}
	queried := signedMaster(t, master, ownerMXID, pin.Record.UserSigningKeyID, genuine)
	if _, err := OwnerSignatureOnMaster(pin, agentMXID, ev, queried); err != nil {
		t.Fatal("genuine control", err)
	}
	foldedMaster := json.RawMessage(strings.Replace(string(queried), `{`, `{"USER_ID":"@other:example.test",`, 1))
	if !json.Valid(foldedMaster) {
		t.Fatal("master plant did not apply")
	}
	if _, err := OwnerSignatureOnMaster(pin, agentMXID, ev, foldedMaster); err != ErrPublishedConflict {
		t.Fatal("folded user_id admitted", err)
	}
	if err := SameMasterKey(ev, agentMXID, foldedMaster); err != ErrPublishedConflict {
		t.Fatal("folded user_id admitted by SameMasterKey", err)
	}
	device := DeviceKeys{UserID: agentMXID, DeviceID: agentDev, Keys: map[string]string{"ed25519:" + agentDev: ev.DeviceEd25519, "curve25519:" + agentDev: ev.DeviceCurve25519}}
	deviceRaw, _ := json.Marshal(device)
	if err := SameDeviceKeys(ev, agentMXID, agentDev, deviceRaw); err != nil {
		t.Fatal("device control", err)
	}
	foldedDevice := json.RawMessage(strings.Replace(string(deviceRaw), `{`, `{"DEVICE_ID":"OTHER",`, 1))
	if err := SameDeviceKeys(ev, agentMXID, agentDev, foldedDevice); err != ErrPublishedConflict {
		t.Fatal("folded device_id admitted", err)
	}
	extra := json.RawMessage(strings.Replace(string(deviceRaw), `{`, `{"extra":"member",`, 1))
	if err := SameDeviceKeys(ev, agentMXID, agentDev, extra); err != nil {
		t.Fatal("unrelated unknown member refused", err)
	}
	// The check looks at names only: a value the decoders accept but Go cannot
	// represent (an overflowing number) passes through both decoders unchanged.
	var opaque json.RawMessage
	if err := strictJSON([]byte(`[1e9999, {"a": 1e9999}]`), &opaque, 1024); err != nil {
		t.Fatal("overflowing number refused by strictJSON", err)
	}
	var numbered struct {
		A json.Number `json:"a"`
	}
	if err := lenientJSON([]byte(`{"a": 1e9999, "b": 1e9999}`), &numbered, 1024); err != nil || numbered.A != "1e9999" {
		t.Fatal("overflowing number refused by lenientJSON", err)
	}
	if err := lenientJSON([]byte(`{"A": 1e9999}`), &numbered, 1024); err != ErrCodec {
		t.Fatal("folded member beside an overflowing value admitted", err)
	}
	// Strict record decoder.
	var c Cursor
	if err := strictJSON([]byte(`{"token":"a","revision":0,"batch":"","TOKEN":"b"}`), &c, 20000); err != ErrCodec {
		t.Fatal("strict folded member admitted", err)
	}
	if err := strictJSON([]byte(`{"token":"a","revision":0,"batch":""}`), &c, 20000); err != nil || c.Token != "a" {
		t.Fatal("strict control", err)
	}
}

// checkCaseFoldRefused is the killer assertion of the case-fold plant: a
// decrypted content member folding to m.relates_to (or its nested rel_type)
// must be e2ee_malformed_event at stage 6.
func checkCaseFoldRefused(req RoomEventRequest) error {
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil {
		return err
	}
	if accepted != nil || refusal == nil || refusal.Stage != "6" || refusal.Reason != ErrMalformedEvent {
		return ErrMalformedEvent
	}
	return nil
}
