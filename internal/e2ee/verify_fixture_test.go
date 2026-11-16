package e2ee

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"
)

// Verification fixtures. Every key, session and ciphertext is generated per
// test from real goolm accounts; no fixture carries recorded secret material.
const (
	peerMXID     = "@peer:example.test"
	peerDev      = "PEERDEV"
	impostorMXID = "@impostor:example.test"
	impostorDev  = "IMPOSTORDEV"
	localMXID    = agentMXID
	localDev     = agentDev
	dmRoom       = "!dm:example.test"
	otherRoom    = "!other:example.test"
	testBinding  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// party is one Matrix account with its device account and cross-signing keys.
type party struct {
	mxid, device string
	keys         *CrossSigning
	w            *Working
	key          []byte
	ed, curve    string
	signed       DeviceKeys
}

func newParty(t testing.TB, mxid, device string) *party {
	t.Helper()
	keys, _ := newCrossSigning(t, mxid)
	_, w, key := working(t)
	d, err := w.DeviceKeys(mxid, device)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := keys.SignDevice(d)
	if err != nil {
		t.Fatal(err)
	}
	return &party{mxid: mxid, device: device, keys: keys, w: w, key: key, ed: d.Keys["ed25519:"+device], curve: d.Keys["curve25519:"+device], signed: signed}
}

func (p *party) local() LocalDevice {
	return LocalDevice{MXID: p.mxid, Device: p.device, Ed25519: p.ed, Curve25519: p.curve}
}

// observation renders the party's complete queried public chain as the
// eligibility input a caller would derive, with the given DM rooms.
func (p *party) observation(t testing.TB, rooms map[string]RoomContext) *PeerObservation {
	t.Helper()
	master, err := p.keys.MasterKey(p.mxid)
	if err != nil {
		t.Fatal(err)
	}
	ssk, err := p.keys.SelfSigningKey(p.mxid)
	if err != nil {
		t.Fatal(err)
	}
	return &PeerObservation{
		AnchoredMaster: p.keys.Master().Public, QueryComplete: true, Master: master, SelfSigning: &ssk,
		Devices: map[string]DeviceObservation{p.device: {Status: EligibilityEligible, Keys: p.signed}},
		Rooms:   rooms,
	}
}

func dmContext(a, b string) RoomContext {
	return RoomContext{State: ContextEncrypted, Members: []string{a, b}}
}

// env is the local identity, one anchored peer and the peer's outbound Megolm
// session for the DM; roomKey and megolm render wire events from it.
type env struct {
	t      *testing.T
	local  *party
	peer   *party
	peers  map[string]*PeerObservation
	sid    string
	shared []byte
	otk    id.Curve25519
	budget *AttemptBudget
}

func newEnv(t *testing.T) *env {
	t.Helper()
	local := newParty(t, localMXID, localDev)
	peer := newParty(t, peerMXID, peerDev)
	sid, shared, err := peer.w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	otks, err := local.w.GenerateOneTimeKeys(1)
	if err != nil {
		t.Fatal(err)
	}
	var otk id.Curve25519
	for _, k := range otks {
		otk = k
	}
	rooms := map[string]RoomContext{dmRoom: dmContext(localMXID, peerMXID)}
	e := &env{t: t, local: local, peer: peer, sid: string(sid), shared: shared, otk: otk, budget: &AttemptBudget{}}
	e.peers = map[string]*PeerObservation{peerMXID: peer.observation(t, rooms)}
	return e
}

// roomKeyPlaintext is the standard Olm payload carrying an m.room_key for the
// peer's session; edit may alter any member before encryption.
func (e *env) roomKeyPlaintext(edit func(map[string]any)) []byte {
	e.t.Helper()
	plain := map[string]any{
		"type": TypeRoomKey,
		"content": map[string]any{
			"algorithm": AlgorithmMegolm, "room_id": dmRoom, "session_id": e.sid, "session_key": string(e.shared),
		},
		"sender": peerMXID, "sender_device": peerDev, "keys": map[string]any{"ed25519": e.peer.ed},
		"recipient": localMXID, "recipient_keys": map[string]any{"ed25519": e.local.ed},
	}
	if edit != nil {
		edit(plain)
	}
	raw, err := json.Marshal(plain)
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// olmEvent Olm-encrypts plain from the peer device to the local device over a
// fresh prekey session and renders the to-device wire event. wireEdit may
// alter the outer event; the local committed account never consumes the OTK.
func (e *env) olmEvent(from *party, plain []byte, wireEdit func(map[string]any)) json.RawMessage {
	e.t.Helper()
	osid, err := from.w.NewOutboundOlm(id.Curve25519(e.local.curve), e.otk)
	if err != nil {
		e.t.Fatal(err)
	}
	kind, body, err := from.w.EncryptOlm(osid, plain)
	if err != nil || kind != id.OlmMsgTypePreKey {
		e.t.Fatal("olm prekey control", err)
	}
	wire := map[string]any{
		"type": TypeEncrypted, "sender": from.mxid,
		"content": map[string]any{
			"algorithm": AlgorithmOlm, "sender_key": from.curve,
			"ciphertext": map[string]any{e.local.curve: map[string]any{"type": 0, "body": string(body)}},
		},
	}
	if wireEdit != nil {
		wireEdit(wire)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

func (e *env) roomKeyEvent() json.RawMessage { return e.olmEvent(e.peer, e.roomKeyPlaintext(nil), nil) }

func (e *env) toDevice(raw json.RawMessage) ToDeviceRequest {
	return ToDeviceRequest{
		Binding: testBinding, Raw: raw, Local: e.local.local(), Peers: e.peers,
		Working: e.local.w, PickleKey: e.local.key, Budget: e.budget, NowMS: 60_000,
	}
}

// admit runs the genuine room key through VerifyToDevice and returns the
// prepared fork and record; it is the positive control every room test uses.
func (e *env) admit() *ToDeviceAdmission {
	e.t.Helper()
	admission, refusal, err := VerifyToDevice(e.toDevice(e.roomKeyEvent()))
	if err != nil || refusal != nil || admission == nil {
		e.t.Fatal("direct room key control", err, refusal)
	}
	return admission
}

// megolm encrypts inner under the peer's session and renders the room event.
func (e *env) megolm(from *party, sid, eventID string, inner map[string]any, contentEdit func(map[string]any), wireEdit func(map[string]any)) json.RawMessage {
	e.t.Helper()
	if inner == nil {
		inner = map[string]any{"msgtype": MsgTypeText, "body": "verified text"}
	}
	plain, err := json.Marshal(map[string]any{"type": TypeRoomMessage, "content": inner, "room_id": dmRoom})
	if err != nil {
		e.t.Fatal(err)
	}
	ciphertext, err := from.w.EncryptMegolm(id.SessionID(sid), plain)
	if err != nil {
		e.t.Fatal(err)
	}
	content := map[string]any{"algorithm": AlgorithmMegolm, "sender_key": from.curve, "device_id": from.device, "session_id": sid, "ciphertext": string(ciphertext)}
	if contentEdit != nil {
		contentEdit(content)
	}
	wire := map[string]any{"type": TypeEncrypted, "sender": from.mxid, "room_id": dmRoom, "event_id": eventID, "origin_server_ts": 1, "content": content}
	if wireEdit != nil {
		wireEdit(wire)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

func (e *env) room(adm *ToDeviceAdmission, raw json.RawMessage) RoomEventRequest {
	return RoomEventRequest{
		Binding: testBinding, Raw: raw, Context: dmContext(localMXID, peerMXID), Local: e.local.local(), Peers: e.peers,
		Sessions: map[string]SessionRecord{adm.Session.SessionID: adm.Session}, Working: adm.Prepared,
		Replay: map[string]ReplayWindow{}, Retained: map[string]Code{},
	}
}

// checkpoint is the committed engine state as deterministic pickles, so a
// byte comparison before and after a rejected attempt is meaningful.
func checkpoint(t testing.TB, w *Working, key []byte) EngineState {
	t.Helper()
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func sameState(a, b EngineState) bool { return reflect.DeepEqual(a, b) }

// expectRefusal asserts a final verdict with the exact stage and reason and
// no admission; it is the shared negative assertion.
func expectRefusal(t testing.TB, refusal *Refusal, err error, stage string, reason Code) {
	t.Helper()
	if err != nil {
		t.Fatalf("operation error instead of refusal: %v", err)
	}
	if refusal == nil {
		t.Fatalf("accepted; want %s at stage %s", reason, stage)
	}
	if refusal.Reason != reason || refusal.Stage != stage {
		t.Fatalf("got %s at stage %s, want %s at stage %s", refusal.Reason, refusal.Stage, reason, stage)
	}
	if !hexSize(refusal.ID, 32) {
		t.Fatal("refusal identity is not a SHA256 digest")
	}
}

func randomText(t testing.TB) string {
	t.Helper()
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func contentOf(m map[string]any) map[string]any { return m["content"].(map[string]any) }

func idSession(s string) id.SessionID { return id.SessionID(s) }

// mustContain / mustNotContain are the scrub assertions shared by the marker test.
func mustNotContain(t testing.TB, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			t.Fatal("diagnostic exposed planted material")
		}
	}
}

func flipLastByte(t testing.TB, ciphertext string) string {
	t.Helper()
	raw, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	return base64.RawStdEncoding.EncodeToString(raw)
}
