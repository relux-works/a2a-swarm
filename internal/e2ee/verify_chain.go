package e2ee

import (
	"encoding/json"
)

// observedDevice is the stage 3 result: the device the verdict is about, still
// only claimed until direct session provenance (stage 5/6) confirms it.
type observedDevice struct {
	peer   *PeerObservation
	device DeviceObservation
	id     string
}

// stage3 applies the section 7 stage 3 table to one (mxid, device) lookup.
// A missing peer is an unanchored root (device_unverified). An incomplete
// required query is a batch abort (ErrKeyQueryUnavailable), never a verdict.
// A tombstone wins over absence from the current device list.
func stage3(peers map[string]*PeerObservation, mxid, device string) (observedDevice, Code, error) {
	peer := peers[mxid]
	if peer == nil {
		return observedDevice{}, ErrDeviceUnverified, nil
	}
	if !peer.QueryComplete {
		return observedDevice{}, "", ErrKeyQueryUnavailable
	}
	if _, err := publicBase64(peer.AnchoredMaster, 32); err != nil {
		return observedDevice{}, ErrDeviceUnverified, nil
	}
	obs, ok := peer.Devices[device]
	if !ok {
		return observedDevice{}, ErrDeviceUnknown, nil
	}
	switch obs.Status {
	case EligibilityChanged:
		return observedDevice{}, ErrDeviceChanged, nil
	case EligibilityTombstoned, EligibilityUnverified:
		return observedDevice{}, ErrDeviceUnverified, nil
	case EligibilityEligible:
		return observedDevice{peer, obs, device}, "", nil
	}
	// An unknown label is not eligibility.
	return observedDevice{}, ErrDeviceUnverified, nil
}

// deviceBySenderKey resolves the outer sender_key hint to a device in the
// peer's completed list. The hint only selects the lookup; it is confirmed
// by Olm authentication (to-device) or session provenance (room events).
func deviceBySenderKey(peers map[string]*PeerObservation, mxid, senderKey string) (string, bool) {
	peer := peers[mxid]
	if peer == nil {
		return "", false
	}
	for id, obs := range peer.Devices {
		if obs.Keys.Keys["curve25519:"+id] == senderKey {
			return id, true
		}
	}
	return "", false
}

// stage4 verifies the chain for an eligible observed device (section 3 check
// 2 and section 7 stage 4): the master object carries exactly the anchored
// key with master usage for that MXID; the self-signing object is present,
// carries self_signing usage and a master signature that verifies; the device
// object names that MXID/device, carries both algorithm keys, a verifying
// self-signing signature and a verifying device self-signature. Absence of
// the self-signing key or of a required self-signing signature is
// self_signing_missing; any present but failing signature is ciphertext_invalid.
func stage4(obs observedDevice, mxid string) Code {
	peer := obs.peer
	master := peer.Master
	masterID := "ed25519:" + peer.AnchoredMaster
	if master.UserID != mxid || len(master.Usage) != 1 || master.Usage[0] != UsageMaster || len(master.Keys) != 1 || master.Keys[masterID] != peer.AnchoredMaster {
		return ErrCiphertextInvalid
	}
	if peer.SelfSigning == nil {
		return ErrSelfSigningMissing
	}
	ssk := *peer.SelfSigning
	if ssk.UserID != mxid || len(ssk.Usage) != 1 || ssk.Usage[0] != UsageSelfSigning || len(ssk.Keys) != 1 {
		return ErrCiphertextInvalid
	}
	var sskID, sskPublic string
	for k, v := range ssk.Keys {
		sskID, sskPublic = k, v
	}
	if sskID != "ed25519:"+sskPublic {
		return ErrCiphertextInvalid
	}
	if _, err := publicBase64(sskPublic, 32); err != nil {
		return ErrCiphertextInvalid
	}
	sskSignature := ssk.Signatures[mxid][masterID]
	if sskSignature == "" {
		return ErrSelfSigningMissing
	}
	if ok, err := verifySigned(peer.AnchoredMaster, ssk, sskSignature); err != nil || !ok {
		return ErrCiphertextInvalid
	}
	d := obs.device.Keys
	if d.UserID != mxid || d.DeviceID != obs.id {
		return ErrCiphertextInvalid
	}
	if _, err := publicBase64(d.Keys["ed25519:"+obs.id], 32); err != nil {
		return ErrCiphertextInvalid
	}
	if _, err := publicBase64(d.Keys["curve25519:"+obs.id], 32); err != nil {
		return ErrCiphertextInvalid
	}
	deviceSignature := d.Signatures[mxid][sskID]
	if deviceSignature == "" {
		return ErrSelfSigningMissing
	}
	if ok, err := verifySigned(sskPublic, d, deviceSignature); err != nil || !ok {
		return ErrCiphertextInvalid
	}
	if VerifyDeviceSelfSignature(d) != nil {
		return ErrCiphertextInvalid
	}
	return ""
}

// lenientJSON rejects duplicate members, excess depth, invalid UTF-8 and
// trailing input, then decodes known members and ignores unknown ones. Wire
// events legitimately carry members this verifier does not interpret; the
// strict decoder (strictJSON) is reserved for records this package writes.
func lenientJSON(raw []byte, dst any, max int) error {
	if len(raw) == 0 || len(raw) > max {
		return ErrCodec
	}
	if _, err := scanJSON(raw, 0); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return ErrCodec
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c == '{'
	}
	return false
}

func roomID(s string) bool  { return text(s, 255) && s[0] == '!' }
func eventID(s string) bool { return text(s, 255) && s[0] == '$' }
