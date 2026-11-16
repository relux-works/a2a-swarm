package e2ee

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Planted-marker scrub: a random marker inside the decrypted text and the
// generated Megolm session key never appear in any verifier diagnostic (the
// refusal's Error, its JSON, every fmt verb of the request, admission and
// accepted-event types), and the engine inventory scrubs both from a planted
// diagnostic string. Controls: the marker is present in the accepted content
// bytes (the plant exists) and an unregistered string survives Scrub (the
// scrub is not blanket suppression).
func TestVerificationDiagnosticsScrubPlantedMarker(t *testing.T) {
	e := newEnv(t)
	marker := "PLANTED-" + randomText(t)
	adm := e.admit()
	raw := e.megolm(e.peer, e.sid, "$1", map[string]any{"msgtype": MsgTypeText, "body": marker}, nil, nil)
	req := e.room(adm, raw)
	accepted, refusal, err := VerifyRoomEvent(req)
	if err != nil || refusal != nil || !accepted.Populated() {
		t.Fatal("control", err, refusal)
	}
	if !strings.Contains(string(accepted.Content()), marker) {
		t.Fatal("plant control: marker missing from accepted content")
	}
	sessionKey := string(e.shared)
	planted := []string{marker, sessionKey}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		mustNotContain(t, fmt.Sprintf(verb, *accepted), planted...)
		mustNotContain(t, fmt.Sprintf(verb, accepted), planted...)
		mustNotContain(t, fmt.Sprintf(verb, req), planted...)
		mustNotContain(t, fmt.Sprintf(verb, *adm), planted...)
		mustNotContain(t, fmt.Sprintf(verb, adm.Session), planted...)
		mustNotContain(t, fmt.Sprintf(verb, e.toDevice(e.roomKeyEvent())), planted...)
	}
	// Refusals from every path carry only codes and public identity.
	tampered := e.megolm(e.peer, e.sid, "$2", map[string]any{"msgtype": MsgTypeText, "body": marker}, func(m map[string]any) { m["ciphertext"] = flipLastByte(t, m["ciphertext"].(string)) }, nil)
	_, refusal, err = VerifyRoomEvent(e.room(adm, tampered))
	expectRefusal(t, refusal, err, "6", ErrCiphertextInvalid)
	e.budget = &AttemptBudget{}
	_, keyRefusal, err := VerifyToDevice(e.toDevice(e.olmEvent(e.peer, e.roomKeyPlaintext(func(p map[string]any) { p["type"] = "m.forwarded_room_key" }), nil)))
	expectRefusal(t, keyRefusal, err, "key-type", ErrKeyProvenanceUnverified)
	for _, r := range []*Refusal{refusal, keyRefusal} {
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		mustNotContain(t, r.Error()+string(encoded)+fmt.Sprintf("%+v %#v", *r, *r), planted...)
		if r.Error() != string(r.Reason) {
			t.Fatal("refusal error text is not the bare reason code")
		}
	}
	// The engine inventory registered the plaintext and the session key on
	// decrypt/import; a diagnostic embedding either is scrubbed.
	inv := adm.Prepared.engine.secrets
	diagnostic := "library said: " + marker + " key=" + sessionKey
	scrubbed := inv.Scrub(diagnostic)
	mustNotContain(t, scrubbed, planted...)
	unregistered := "UNREGISTERED-" + randomText(t)
	if !strings.Contains(inv.Scrub("note "+unregistered), unregistered) {
		t.Fatal("scrub control: unregistered text was suppressed")
	}
}
