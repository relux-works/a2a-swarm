package e2ee

import "encoding/json"

// OwnerUserSigningKeyObject is the standard signed USK object the host pin
// describes: user_id = pinned owner, usage = [user_signing], keys = {USK ID: USK}.
func OwnerUserSigningKeyObject(pin HostPin) (SignedKey, error) {
	return keyObject(pin.Owner, UsageUserSigning, PublicKey{ID: pin.Record.UserSigningKeyID, Public: pin.Record.UserSigningPublicKey})
}

// VerifyOwnerChain verifies the pinned owner MSK signature over the pinned USK
// object. Shape and fingerprints were validated by the reader; this is the
// cryptographic step. A missing signature never passes (the reader refuses it).
func VerifyOwnerChain(pin HostPin) error {
	usk, err := OwnerUserSigningKeyObject(pin)
	if err != nil {
		return ErrPinInvalid
	}
	ok, err := verifySigned(pin.Record.MasterPublicKey, usk, pin.Record.UserSigningMasterSignature)
	if err != nil {
		return ErrPinInvalid
	}
	if !ok {
		return ErrOwnerSignature
	}
	return nil
}

// OwnerSignatureOnMaster verifies the owner USK signature over the agent's own
// master key as returned by the agent's own key query. queried is the exact
// server object; its keys must equal the persisted master exactly. The result
// is the verified signature, ErrAwaitingOwner when the pinned USK has not signed
// it, or ErrOwnerSignature when the present signature does not verify. A local
// flag or a signature under any other key never substitutes.
func OwnerSignatureOnMaster(pin HostPin, mxid string, evidence IdentityEvidence, queried json.RawMessage) (string, error) {
	if len(queried) == 0 || len(queried) > MaxPin {
		return "", ErrPublishedConflict
	}
	var master SignedKey
	if err := lenientJSON(queried, &master, MaxPin); err != nil {
		return "", ErrPublishedConflict
	}
	expected, err := evidence.MasterKeyObject(mxid)
	if err != nil {
		return "", err
	}
	if master.UserID != expected.UserID || len(master.Usage) != 1 || master.Usage[0] != UsageMaster || len(master.Keys) != 1 || master.Keys[evidence.MasterKeyID] != evidence.MasterPublicKey {
		return "", ErrPublishedConflict
	}
	signature := master.Signatures[pin.Owner][pin.Record.UserSigningKeyID]
	if signature == "" {
		return "", ErrAwaitingOwner
	}
	// Verify over the exact queried object minus signatures/unsigned, which is
	// what the owner's client signed, and require that object to be ours.
	ok, err := verifySigned(pin.Record.UserSigningPublicKey, queried, signature)
	if err != nil {
		return "", ErrOwnerSignature
	}
	if !ok {
		return "", ErrOwnerSignature
	}
	// The signed bytes must also equal our own persisted master object: an
	// extended object carrying other members is not evidence for this master.
	ours, err := signingInput(expected)
	if err != nil {
		return "", err
	}
	theirs, err := signingInput(queried)
	if err != nil {
		return "", ErrPublishedConflict
	}
	if string(ours) != string(theirs) {
		return "", ErrPublishedConflict
	}
	return signature, nil
}

// VerifyStoredOwnerSignature re-verifies the sealed owner signature against the
// current pin on every use after restart. It refuses a record whose state claims
// owner-verified without a signature that verifies under the pinned USK.
func VerifyStoredOwnerSignature(pin HostPin, mxid string, evidence IdentityEvidence) error {
	if evidence.State != StateOwnerVerified {
		return ErrAwaitingOwner
	}
	if evidence.OwnerUserSigningKeyID != pin.Record.UserSigningKeyID || evidence.Owner != pin.Owner {
		return ErrOwnerSignature
	}
	master, err := evidence.MasterKeyObject(mxid)
	if err != nil {
		return err
	}
	ok, err := verifySigned(pin.Record.UserSigningPublicKey, master, evidence.OwnerSignature)
	if err != nil {
		return ErrOwnerSignature
	}
	if !ok {
		return ErrOwnerSignature
	}
	return nil
}

// SameMasterKey reports whether a queried master-key object publishes exactly
// the persisted master. Any other published master is a conflict, never a
// replacement root.
func SameMasterKey(evidence IdentityEvidence, mxid string, queried json.RawMessage) error {
	var master SignedKey
	if lenientJSON(queried, &master, MaxPin) != nil {
		return ErrPublishedConflict
	}
	if master.UserID != mxid || len(master.Keys) != 1 || master.Keys[evidence.MasterKeyID] != evidence.MasterPublicKey || len(master.Usage) != 1 || master.Usage[0] != UsageMaster {
		return ErrPublishedConflict
	}
	return nil
}

// SameDeviceKeys reports whether queried device keys publish exactly this
// device's Ed25519 and Curve25519 keys.
func SameDeviceKeys(evidence IdentityEvidence, mxid, device string, queried json.RawMessage) error {
	var d DeviceKeys
	if lenientJSON(queried, &d, MaxPin) != nil {
		return ErrPublishedConflict
	}
	if d.UserID != mxid || d.DeviceID != device || d.Keys["ed25519:"+device] != evidence.DeviceEd25519 || d.Keys["curve25519:"+device] != evidence.DeviceCurve25519 {
		return ErrPublishedConflict
	}
	return nil
}
